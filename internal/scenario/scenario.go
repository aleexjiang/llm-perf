// Package scenario 实现评测场景矩阵（2026-09-18 新架构，见 docs/workload-refactor-plan.md）：
//
//	user        生成式多轮用户会话（profile 驱动 + 经典书语料 + 被测模型真实回复）
//	rps         冻结请求快照的开环到达（排队-延迟曲线、体验拐点）
//	concurrency 固定在飞齐射（总吞吐拐点，对齐 vLLM bench serve）
//	probe       能力与健康探针（internal/engine，独立入口）
//
// 本文件只承载各场景共享的横切基建：
//   - 场景注册表
//   - runOne：单请求执行 + 完整指标（失败/取消一条不丢）
//   - 服务端观测窗口（startWindow/finishWindow）与两源一致性（applySourceCheck）
//   - SLO / KV 容量画像挂载
//
// 旧的 filler/trace 回放路径与 --turns × --concurrency 组合入口已于 2026-09-18 下线。
package scenario

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aleexjiang/llm-perf/internal/auth"
	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/engine"
	"github.com/aleexjiang/llm-perf/internal/report"
	"github.com/aleexjiang/llm-perf/internal/smetrics"
)

// Scenario 是评测场景的统一抽象：注册表分发——新增场景实现该接口并 Register 即可。
type Scenario interface {
	Name() string
	Run(ctx context.Context, cfg *config.Config, client *engine.Client, modelFilter string) (*report.Report, error)
}

type funcScenario struct {
	name string
	fn   func(context.Context, *config.Config, *engine.Client, string) (*report.Report, error)
}

func (s funcScenario) Name() string { return s.name }

func (s funcScenario) Run(ctx context.Context, cfg *config.Config, c *engine.Client, filter string) (*report.Report, error) {
	return s.fn(ctx, cfg, c, filter)
}

var scenarioRegistry = map[string]Scenario{}

// Register 注册场景（init 期调用，无并发）。
func Register(s Scenario) {
	scenarioRegistry[s.Name()] = s
}

// Lookup 按名字查场景（main 按子命令精确取名）。
func Lookup(name string) (Scenario, bool) {
	s, ok := scenarioRegistry[name]
	return s, ok
}

// env 承载一次场景执行的共享资源与横切能力。
type env struct {
	cfg      *config.Config
	client   *engine.Client
	srv      *smetrics.Scraper        // nil = 观测层关闭/不可用
	provider smetrics.MetricsProvider // 观测层指标命名（按服务端指标前缀自动识别）
	// kv 是场景开始快照提取的 KV 容量画像（12.12）：nil = 观测层不可用或引擎未暴露
	// vllm:cache_config_info。仅作容量归因的并列参照，不影响任何结论口径。
	kv *smetrics.KVCapacity
	// gauge 是场景窗口的后台 gauge 轮询器；档位级 running/waiting 峰值从它读取。
	gauge *smetrics.GaugePoller
	// perReqSrv 逐请求 /metrics 前后抓取（SrvDelta）开关：仅串行路径启用。
	// 并发/开环下每请求抓取落在计时窗口内（压低 wall_seconds 口径的吞吐）、
	// 各请求差值窗口互相重叠无归因意义，且给服务端叠加可观测负载——
	// 并发路径只保留场景窗口级差分（startWindow/finishWindow），口径更干净。
	perReqSrv bool
}

// setupServerMetrics 装配服务端观测层（server_metrics: true 时）：probe 同口径判定
// 可用性，DetectProvider 识别指标命名。所有场景共用。
func setupServerMetrics(ctx context.Context, e *env, cfg *config.Config) error {
	if !cfg.ServerMetrics {
		return nil
	}
	s := smetrics.NewScraperAt(cfg.Endpoint, cfg.MetricsPath)
	// /metrics 常与业务接口同一套认证保护——认证格式与 chat 请求保持一致
	s.Auth = auth.Auth{Scheme: cfg.AuthScheme, Header: cfg.AuthHeader}
	s.APIKey = cfg.APIKey
	// 判定口径与 probe 一致：HTTP 200 但 0 项 vLLM 指标（网关占位响应）也算不可用，
	// 否则观测层会带着空指标集白跑，报告里出现假"可用"
	ok, detail := s.Available(ctx)
	if !ok {
		// /metrics 是引擎实现细节，不是标准端点（网关照不到、代理剥掉都属常见形态）。
		// 客户端实测是本工具唯一的基线口径，服务端观测只是可选增强——
		// 因此这里既不是错误，也不是"降级"：缺它不影响任何结论。
		log.Printf("ℹ️ 未提供 %s（%v）——全部结论按客户端实测口径给出", cfg.MetricsPath, detail)
		return nil
	}
	sample, err := s.Scrape(ctx)
	if err != nil {
		log.Printf("ℹ️ %s 抓取失败（%v）——本次按客户端实测口径给出结论", cfg.MetricsPath, err)
		return nil
	}
	name := smetrics.DetectProviderName(sample)
	if name == "" {
		// 自研引擎指标名不带 vllm:/sglang: 前缀——未知命名只记录端点可达，
		// 不静默套用 vLLM 语义，避免生成假 server_metrics 数据。
		log.Printf("⚠️ 无法识别服务端指标命名（无 vllm:/sglang: 前缀）——跳过语义化 /metrics 采集，结论基线仍是客户端实测")
		return nil
	}
	e.srv = s
	e.provider = smetrics.DetectProvider(sample)
	e.kv = smetrics.ExtractKVCapacity(sample) // 12.12：KV 容量画像（未暴露则 nil）
	n := len(sample.Counters) + len(sample.Gauges) + len(sample.Hists)
	log.Printf("ℹ️ 服务端观测 %s 可用（%d 项指标，%s 命名）——额外采集一份作辅助，结论基线仍是客户端实测", cfg.MetricsPath, n, name)
	if e.kv != nil {
		// 容量归因的静态参照：报告会把「实测拐点 vs KV 上界」并列
		log.Printf("ℹ️ %s", e.kv.Describe())
	}
	return nil
}

// ctxLimitHit 识别"请求超过模型上下文上限"类失败（vLLM 对超限返回 400 且错误信息带
// "maximum context length is N tokens"），返回解析出的上限数字；非该类错误返回 ""。
// 现场语义：这类错误是确定性的（同档位重试必然再失败，更大档位更超），应立即止损。
var ctxLimitRe = regexp.MustCompile(`(?i)context length is (\d+)`)

func ctxLimitHit(m *engine.TurnMetrics) string {
	if m == nil || m.Error == "" || !strings.Contains(m.Error, "HTTP 400") {
		return ""
	}
	if !strings.Contains(strings.ToLower(m.Error), "context length") {
		return ""
	}
	if mm := ctxLimitRe.FindStringSubmatch(m.Error); mm != nil {
		return mm[1]
	}
	return "未知"
}

// runOne 发起一次请求（流式/非流式、思考变体由 opts 决定），并做 /metrics counter 前后差值。
// 契约：任何路径都返回非 nil 的 TurnMetrics——失败/取消也是数据（"原始请求一条不丢"）。
func runOne(ctx context.Context, e *env, model string,
	msgs []engine.Message, maxTokens int, v config.ThinkingVariant) *engine.TurnMetrics {

	var before *smetrics.Sample
	if e.srv != nil && e.perReqSrv {
		before, _ = e.srv.Scrape(ctx)
	}
	m, err := e.client.Chat(ctx, engine.ChatOptions{
		Model:       model,
		Messages:    msgs,
		MaxTokens:   maxTokens,
		Stream:      e.cfg.StreamEnabled(),
		Thinking:    v.Enabled,
		ExtraBody:   v.ExtraBody,
		Temperature: e.cfg.Sampling.Temperature,
		TopP:        e.cfg.Sampling.TopP,
	})
	if m == nil {
		// Client.Chat 在请求构造失败等边界路径可能只有 error；场景层仍必须保留一条
		// 可序列化的失败指标，不能让主路径解引用 nil 丢掉整个场景。
		m = &engine.TurnMetrics{
			Model:    model,
			Stream:   e.cfg.StreamEnabled(),
			Thinking: v.Enabled,
			Phase:    "benchmark",
			Error:    "request returned no metrics",
			EndAt:    time.Now(),
		}
		if err != nil {
			m.Error = err.Error()
		}
		m.Finalize()
	} else {
		m.Phase = "benchmark"
		if m.Error != "" && ctx.Err() != nil {
			m.Cancelled = true
		}
	}
	if err != nil {
		log.Printf("    失败: %v", err)
	} else {
		// 输入/输出全量打印（用户要求）：长内容掐头 120 + 掐尾 120，逐请求留痕便于现场排错
		if n := len(msgs); n > 0 {
			last := msgs[n-1]
			log.Printf("    输入[%d条消息,末条 %s %d字]: %s",
				n, last.Role, utf8.RuneCountInString(last.Content), engine.PreviewHeadTail(last.Content))
		}
		if m.ReasoningChars > 0 {
			log.Printf("    思考[%d字]: %s", m.ReasoningChars, engine.PreviewHeadTail(m.ReasoningText()))
		}
		if m.ContentChars > 0 {
			log.Printf("    输出[content %d字]: %s", m.ContentChars, engine.PreviewHeadTail(m.ReplyText))
		}
		if m.ThinkingNoContent {
			log.Printf("    ⚠️ 思考吃光 max_tokens=%d，全程无 content（finish=length）——本次 ThinkMS/DecodeMS 不可测，建议调大 thinking.max_tokens_floor", maxTokens)
		}
		for _, w := range m.Warnings {
			log.Printf("    ⚠️ 兼容性告警: %s", w)
		}
		if e.cfg.StreamEnabled() {
			// 不可测的时长打印 "—" 而不是 "0ms"：思考吃光输出预算时 m.ThinkMS 是零值
			// （JSON 里因 omitempty 连键都不存在），打成 0ms 会被读成「这个请求没思考」，
			// 而真实情况恰好相反。上面那条 ⚠️ 说得对，但日志行才是被扫读的那一行。
			think, decode := fmt.Sprintf("%.0fms", m.ThinkMS), fmt.Sprintf("%.0fms", m.DecodeMS)
			if m.ThinkingNoContent {
				think, decode = "—", "—"
			}
			log.Printf("    TTFT=%.0fms think=%s decode=%s tok/s=%.0f finish=%s",
				m.TTFT, think, decode, m.TokensPerSec, m.FinishReason)
		} else {
			log.Printf("    E2E=%.0fms tok/s=%.0f（非流式，TTFT/思考拆分 N/A）", m.E2EMS, m.TokensPerSec)
		}
	}
	if e.srv != nil && e.perReqSrv && before != nil {
		if after, err := e.srv.Scrape(ctx); err == nil {
			m.SrvDelta = smetrics.DiffCounters(before, after, e.provider)
		}
	}
	return m
}

// thinkingNoteSuffix 存在按模型覆盖时，报告备注追加标记（Note 描述的是通用基线）
func thinkingNoteSuffix(cfg *config.Config) string {
	if len(cfg.ModelOverrides) == 0 {
		return ""
	}
	return "；部分模型的场景/思考配置按模型覆盖（model_overrides），与通用基线不一致"
}

// interrupted 中断检查：SIGINT 取消 ctx 后返回 true，外层循环据此停止并保留已完成数据。
func interrupted(ctx context.Context) bool { return ctx.Err() != nil }

// startWindow / finishWindow 场景窗口的服务端观测：开始快照+gauge 轮询 → 结束差值汇总。
// 起始时刻一并返回：窗口时长是两源一致性（客户端 tok/s vs 服务端 tok/s）的共同分母。
func startWindow(ctx context.Context, e *env) (*smetrics.Sample, *smetrics.GaugePoller, time.Time) {
	if e.srv == nil {
		return nil, nil, time.Time{}
	}
	start := time.Now()
	before, err := e.srv.Scrape(ctx)
	if err != nil {
		log.Printf("⚠️ %s 起始快照失败（%v）——本场景无服务端观测", e.cfg.MetricsPath, err)
		return nil, nil, time.Time{}
	}
	poller := smetrics.StartGaugePoller(ctx, e.srv, time.Duration(e.cfg.MetricsIntervalMS)*time.Millisecond, e.provider)
	e.gauge = poller
	return before, poller, start
}

// gaugeSampleStarts 记录当前轮询样本累计值，供档位结束后做区间峰值。
// 观测层未启用/未采到对应指标时返回 0，applyGaugePeaks 会自然跳过。
func (e *env) gaugeSampleStarts() (waiting, running int) {
	if e.gauge == nil {
		return 0, 0
	}
	return e.gauge.SampleTotal("waiting"), e.gauge.SampleTotal("running")
}

// applyGaugePeaks 将本档位区间内的 running/waiting 峰值写回档位结果。
func (e *env) applyGaugePeaks(lv *report.ConcurrentLevel, waitingStart, runningStart int) {
	if e.gauge == nil || lv == nil {
		return
	}
	if v, ok := e.gauge.MaxSince("waiting", waitingStart); ok {
		lv.WaitingMax = v
	}
	if v, ok := e.gauge.MaxSince("running", runningStart); ok {
		lv.RunningMax = v
	}
}

// finalScrapeCtx 结束快照的独立 ctx（12.11）：场景 ctx 此时可能已被取消——运行中断
// （SIGHUP/Ctrl+C）或场景控制停止——而结束快照恰恰是窗口差值的唯一来源。
// 独立限时 ctx 不被取消传播波及（正常路径无行为差异）；超时（5s 上限）如实记失败，不阻塞退出。
func finalScrapeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// finishWindow 汇总场景窗口的服务端观测。
//
// Available 的语义严格限定为「**窗口差值**（counter/hist）是否取到」：结束快照失败时窗口差值
// 无从计算，此时 Available=false 并保留 Note 说明原因——已轮询到的 gauges 仍然有效，照常挂回。
// 这样报告侧能如实区分「已采集 / 已启用但未取到 / 未提供」，不会把一次失败渲染成全零面板。
func finishWindow(e *env, before *smetrics.Sample,
	poller *smetrics.GaugePoller, start time.Time) *report.ServerMetricsSummary {
	if e.srv == nil || before == nil {
		if poller != nil {
			poller.Stop()
		}
		return nil
	}
	summary := &report.ServerMetricsSummary{}
	if poller != nil {
		summary.Gauges = poller.Summary() // Summary 内部会 Stop
		if h := poller.Health(); h.Degraded() {
			summary.ObservationDegraded = true
			summary.ObservationNote = fmt.Sprintf("gauge 轮询降级：成功 %d 次，连续失败 %d 次，最后错误 %s",
				h.Samples, h.ConsecutiveFailures, h.LastError)
		}
	}
	sctx, cancel := finalScrapeCtx()
	after, err := e.srv.Scrape(sctx)
	cancel()
	if err != nil {
		summary.Note = "结束快照抓取失败（本场景无窗口差值）: " + err.Error()
		return summary
	}
	summary.Available = true
	if !start.IsZero() {
		summary.WindowSeconds = time.Since(start).Seconds()
	}
	d := smetrics.DiffCounters(before, after, e.provider)
	summary.CacheHitTokens = d.PrefixCacheHitTokens
	summary.CacheQueryTokens = d.PrefixCacheQueryTokens
	summary.Preemptions = d.Preemptions
	summary.SpecDrafts = d.SpecDrafts
	summary.SpecAcceptedTokens = d.SpecAcceptedTokens
	summary.PromptTokens = d.PromptTokens
	summary.GenerationTokens = d.GenerationTokens
	summary.Hists = smetrics.HistDeltas(before, after, e.provider)
	return summary
}

// applySourceCheck 两源一致性（10.1）：客户端实测聚合吞吐 vs 服务端生成吞吐。
//
// 只服务诊断层（不产生评测指标）：危险形态是「数百并发流 + 高 chunk 率下客户端自身成瓶颈」，
// 那时客户端读数系统性偏低，只看客户端会把客户端问题误归因成服务变慢。
//
// 两边同分母：本场景各档位墙钟之和（客户端侧吞吐本就是这个口径），因此比较等价于 token 量比较。
// 观测层缺失 / 引擎不暴露生成 token 数 / 无有效档位 → 只写 Note 记 NA，不改任何结论。
func applySourceCheck(e *env, rep *report.Report, before *smetrics.Sample) {
	if rep == nil || len(rep.Concurrent) == 0 {
		return
	}
	sc := &report.SourceCheck{}
	var wall, tokens float64
	for i := range rep.Concurrent {
		lv := &rep.Concurrent[i]
		wall += lv.WallSeconds
		tokens += lv.ThroughputTPS * lv.WallSeconds
	}
	if wall <= 0 || tokens <= 0 {
		sc.Note = "客户端侧无可比吞吐（本轮无有效档位数据）"
		rep.SourceCheck = sc
		return
	}
	sc.ClientTPS = tokens / wall
	sc.ClientTokens = tokens
	sc.WindowSeconds = wall
	if e.srv == nil || before == nil {
		sc.Note = "服务端观测不可用（未启用 /metrics 或起始快照失败）"
		rep.SourceCheck = sc
		return
	}
	sctx, cancel := finalScrapeCtx() // 12.11：中断场景同样要保住两源一致性差值（独立 ctx）
	after, err := e.srv.Scrape(sctx)
	cancel()
	if err != nil {
		sc.Note = "末档位后快照抓取失败：" + err.Error()
		rep.SourceCheck = sc
		return
	}
	d := smetrics.DiffCounters(before, after, e.provider)
	if d.GenerationTokens <= 0 {
		sc.Note = "引擎未暴露生成 token 数（generation_tokens），无法交叉校验"
		rep.SourceCheck = sc
		return
	}
	sc.ServerTokens = d.GenerationTokens
	sc.ServerTPS = d.GenerationTokens / wall
	if sc.ServerTPS > 0 {
		sc.Deviation = (sc.ClientTPS - sc.ServerTPS) / sc.ServerTPS
	}
	if sc.Deviation < -0.15 {
		log.Printf("⚠️ 两源一致性偏差 %.0f%%：客户端聚合 %.0f tok/s 低于服务端生成 %.0f tok/s"+
			"——客户端侧可能有损（高并发 + 高 chunk 率下客户端自身成瓶颈）",
			sc.Deviation*100, sc.ClientTPS, sc.ServerTPS)
	}
	rep.SourceCheck = sc
}

// applySLO 把 goodput 配置与基线评估阈值挂到 Report（报告侧按 turn 级 TTFT/TPOT 计算达标率；
// 基线阈值随 JSON 透出供外部报告替代内置默认，未配置时不透出 = 报告用内置 SLO_TIERS）。
func applySLO(e *env, rep *report.Report) {
	if g := e.cfg.EffGoodput(); g != nil {
		rep.SLO = &report.SLO{TTFTMS: g.TTFTMS, TPOTMS: g.TPOTMS}
	}
	if b := e.cfg.EffBaseline(); b != nil {
		rep.SLOBaseline = &report.SLOBaseline{
			Enabled:        b.BaselineEnabled(),
			ShortMaxTokens: b.ShortMaxTokens,
			LongMinTokens:  b.LongMinTokens,
			ShortGoodTTFT:  b.ShortGoodTTFT,
			ShortPassTTFT:  b.ShortPassTTFT,
			LongGoodTTFT:   b.LongGoodTTFT,
			LongPassTTFT:   b.LongPassTTFT,
			GoodTPOT:       b.GoodTPOT,
			PassTPOT:       b.PassTPOT,
			GoodTPS:        b.GoodTPS,
			PassTPS:        b.PassTPS,
		}
	}
}

// attachKVCapacity 把 KV 容量画像挂到 Report（12.12）：观测层可用且引擎暴露
// vllm:cache_config_info 时填充，其余情况省略（报告侧并列项自然消失）。
// 与 applySLO 同为「env → rep 的横切挂载」，各场景统一。
func attachKVCapacity(e *env, rep *report.Report) {
	if e.kv != nil {
		rep.KVCapacity = e.kv
	}
}

// goodputOf 请求是否满足 SLO。对齐 vLLM goodput 语义：只判定"已配置的"SLO 子集
// （阈值为 0 的维度不参与）；配置了 TTFT 阈值时要求 TTFT 可测（>0）——非流式
// TTFT 不可测（N/A），不应凭 0 值白拿达标。非流式在配置了 TPOT 阈值时天然不达标。
func goodputOf(e *env, m *engine.TurnMetrics) bool {
	g := e.cfg.EffGoodput()
	if g == nil || m == nil || m.Error != "" {
		return false
	}
	if g.TTFTMS > 0 && (m.TTFT <= 0 || m.TTFT > g.TTFTMS) {
		return false
	}
	if g.TPOTMS > 0 && (m.TPOTMS <= 0 || m.TPOTMS > g.TPOTMS) {
		return false
	}
	return true
}

// poissonDelays 生成开环到达的累计时刻序列（秒，[0]=0，共 n 项）：请求 i 应在
// start + ts[i] 发射。间隔 ~ Gamma(shape=burstiness, scale=1/(rate·burstiness))，
// burstiness=1 退化为指数分布（标准泊松）；<1 更突发；>1 趋向恒定间隔（均匀到达）。
// 采样后按理论总量 (n-1)/rate **整体重整**——随机抽样的间隔总和天然有 1-2% 偏差，
// 不重整则不同 seed 的到达总量不同，吞吐数据跨 run 不可比（对齐 vLLM bench serve
// 的 normalize_factor，方法论唯一"必抄"项）。
func poissonDelays(n int, rate, burstiness float64, rng *rand.Rand) []float64 {
	ts := make([]float64, n)
	if n <= 1 || rate <= 0 {
		return ts // rate<=0（等效满并发）＝零间隔齐射
	}
	sum := 0.0
	for i := 1; i < n; i++ {
		d := gammaSample(rng, burstiness) / (rate * burstiness)
		ts[i] = ts[i-1] + d
		sum += d
	}
	if sum > 0 {
		k := float64(n-1) / rate / sum
		for i := 1; i < n; i++ {
			ts[i] *= k
		}
	}
	return ts
}

// gammaSample Marsaglia-Tsang (2000) gamma(shape) 随机数（scale=1）；shape<1 用
// boost 法 G(shape) = G(shape+1)·U^(1/shape)。仅开环到达调度使用。
func gammaSample(rng *rand.Rand, shape float64) float64 {
	if shape < 1 {
		return gammaSample(rng, shape+1) * math.Pow(rng.Float64(), 1/shape)
	}
	d := shape - 1.0/3
	c := 1 / math.Sqrt(9*d)
	for {
		var x, v float64
		for {
			x = rng.NormFloat64()
			if v = 1 + c*x; v > 0 {
				break
			}
		}
		v = v * v * v
		u := rng.Float64()
		if u < 1-0.0331*x*x*x*x {
			return d * v
		}
		if math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}

// forModel 切换到某模型生效的配置视图（model_overrides 差异覆盖）：
// 场景内所有经 cfg/e.cfg 读配置的路径随之取到该模型的实际生效值。
// 模型循环串行且档位内 worker 全部 join 后才进下一模型，e.cfg 不会被并发改写。
func forModel(e *env, cfg *config.Config, model string) (*env, *config.Config) {
	e.cfg = cfg.ForModel(model)
	return e, e.cfg
}
