// Package scenario 实现评测场景矩阵：
//
//	执行方式（单发多轮/多用户多轮/开环 RPS） × 思考模式（off/on，由 config.Thinking 展开），
//	stream 为请求级开关（config.stream）；公共入口不再运行单发单轮。
//
// 横切能力（所有场景共用）：
//   - warmup：场景开始前的预热请求（不计入统计，唯一内容避免污染被测前缀）
//   - server_metrics：请求前后抓服务端 /metrics counter 差值（缓存命中/preemption/MTP），
//     场景窗口内 gauge 轮询（排队深度/KV 占用）与 histogram 差值（queue/prefill/decode 分解）
//   - dataset：filler（token 精确填充）或 trace（真实会话回放）
//   - goodput：SLO 约束吞吐（TTFT/TPOT 双达标才算有效请求）
//   - correctness：数字转写金丝雀抽查（防"HTTP 200 但内容异常"的假成功）
package scenario

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aleexjiang/llm-perf/internal/auth"
	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/engine"
	"github.com/aleexjiang/llm-perf/internal/report"
	"github.com/aleexjiang/llm-perf/internal/smetrics"
)

// Scenario 是评测场景的统一抽象：注册表分发——新增场景实现该接口并 Register 即可，
// main 无需改动，bench all 按注册顺序执行。
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

// Lookup 按名字查场景（main 按 turns×concurrency 组合精确取名，不遍历）。
func Lookup(name string) (Scenario, bool) {
	s, ok := scenarioRegistry[name]
	return s, ok
}

func init() {
	// 公共 bench 只注册 agent 多轮路径；Single 保留为旧数据/内部回归辅助，不作为产品入口。
	Register(funcScenario{"multiturn", Multiturn})
	Register(funcScenario{"concurrent", Concurrent})
}

// env 承载一次场景执行的共享资源与横切能力。
type env struct {
	cfg      *config.Config
	client   *engine.Client
	srv      *smetrics.Scraper        // nil = 观测层关闭/不可用
	provider smetrics.MetricsProvider // 观测层指标命名（按服务端指标前缀自动识别）
	// kv 是场景开始快照提取的 KV 容量画像（12.12）：nil = 观测层不可用或引擎未暴露
	// vllm:cache_config_info。仅作容量归因的并列参照，不影响任何结论口径。
	kv    *smetrics.KVCapacity
	trace *engine.TraceSet // nil = filler 模式
	// perReqSrv 逐请求 /metrics 前后抓取（SrvDelta）开关：仅单发/串行多轮启用。
	// 并发/开环下每请求抓取落在计时窗口内（压低 wall_seconds 口径的吞吐）、
	// 各请求差值窗口互相重叠无归因意义，且给服务端叠加可观测负载——
	// 并发路径只保留场景窗口级差分（startWindow/finishWindow），口径更干净。
	perReqSrv bool
}

func newEnv(ctx context.Context, cfg *config.Config, client *engine.Client) (*env, error) {
	e := &env{cfg: cfg, client: client, perReqSrv: true}
	if cfg.ServerMetrics {
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
		} else {
			sample, err := s.Scrape(ctx)
			if err != nil {
				log.Printf("ℹ️ %s 抓取失败（%v）——本次按客户端实测口径给出结论", cfg.MetricsPath, err)
			} else {
				name := smetrics.DetectProviderName(sample)
				if name == "" {
					// 自研引擎指标名不带 vllm:/sglang: 前缀——未知命名只记录端点可达，
					// 不静默套用 vLLM 语义，避免生成假 server_metrics 数据。
					log.Printf("⚠️ 无法识别服务端指标命名（无 vllm:/sglang: 前缀）——跳过语义化 /metrics 采集，结论基线仍是客户端实测")
				} else {
					e.srv = s
					e.provider = smetrics.DetectProvider(sample)
					e.kv = smetrics.ExtractKVCapacity(sample) // 12.12：KV 容量画像（未暴露则 nil）
					n := len(sample.Counters) + len(sample.Gauges) + len(sample.Hists)
					log.Printf("ℹ️ 服务端观测 %s 可用（%d 项指标，%s 命名）——额外采集一份作辅助，结论基线仍是客户端实测", cfg.MetricsPath, n, name)
					if e.kv != nil {
						// 容量归因的静态参照：报告会把「实测拐点 vs KV 上界」并列
						log.Printf("ℹ️ %s", e.kv.Describe())
					}
				}
			}
		}
	}
	if cfg.Dataset.Mode == "trace" {
		ts, err := engine.LoadTrace(cfg.Dataset.Path, cfg.Dataset.Format, cfg.Dataset.ReplayMode, cfg.Dataset.MinTurns, cfg.Dataset.MaxSessions)
		if err != nil {
			return nil, err
		}
		e.trace = ts
		mode := "user_only"
		if ts.FullReplay {
			mode = "full"
		}
		log.Printf("trace 回放: %s（%s 格式，%d 个会话，replay_mode=%s）", ts.Source, ts.Format, len(ts.Sessions), mode)
		if ts.MissingToolCallID > 0 {
			log.Printf("    ⚠️ full 回放：%d 条 role=tool 消息缺 tool_call_id，已跳过（OpenAI 协议要求 tool 消息必须带该字段，缺失会被服务端 400）", ts.MissingToolCallID)
		}
		if ts.NoAssistantContent {
			log.Printf("    ⚠️ full 回放：数据源不含 assistant/tool 消息（%s 格式只有 user 轮）——实际回放深度与 user_only 相同，请改用 sharegpt 等含完整会话的数据", ts.Format)
		}
	}
	return e, nil
}

// warnTraceTraceWrap trace 会话数不足所需时提示回绕复用（覆盖多样性受限）。
func (e *env) warnTraceWrap(need int) {
	if e.trace != nil && need > len(e.trace.Sessions) {
		log.Printf("⚠️ trace 会话数 %d 少于需要的 %d——将按序回绕复用，会话多样性受限",
			len(e.trace.Sessions), need)
	}
}

// runOne 发起一次请求（流式/非流式、思考变体由 opts 决定），并做 /metrics counter 前后差值。
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

func runOne(ctx context.Context, e *env, model string,
	msgs []engine.Message, maxTokens int, v config.ThinkingVariant) *engine.TurnMetrics {

	var before *smetrics.Sample
	if e.srv != nil && e.perReqSrv {
		before, _ = e.srv.Scrape(ctx)
	}
	m, err := e.client.Chat(ctx, engine.ChatOptions{
		Model:     model,
		Messages:  msgs,
		MaxTokens: maxTokens,
		Stream:    e.cfg.StreamEnabled(),
		Thinking:  v.Enabled,
		ExtraBody: v.ExtraBody,
	})
	if m != nil {
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
				n, last.Role, len([]rune(last.Content)), engine.PreviewHeadTail(last.Content))
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

// warmup 场景开始前的预热：暖连接池/首包路径；唯一内容（时间戳 seed）避免污染被测前缀的缓存对照。
// 预热请求形状：小 prompt + 1 token 输出，只为建连/暖机，不构成有效负载。
const (
	warmupPromptTokens = 128
	warmupMaxTokens    = 1
)

// traceSingleSampleLimit trace 模式下单发档位最多取样的回放会话数：
// 单发矩阵的价值在"首轮长度分布"，取前 N 个即可代表分布，避免大数据集拖长单发矩阵。
const traceSingleSampleLimit = 16

func warmup(ctx context.Context, e *env, model string) []*engine.TurnMetrics {
	n := e.cfg.WarmupRequests
	if n <= 0 {
		return nil
	}
	vOff := config.ThinkingVariant{Name: "off", Enabled: false, ExtraBody: e.cfg.ThinkingFor(model).ExtraBodyOff}
	now := time.Now().UnixNano()
	out := make([]*engine.TurnMetrics, 0, n)
	for i := 0; i < n; i++ {
		msgs := []engine.Message{engine.UserMsg(warmupPromptTokens, now+int64(i), e.cfg.FillerLang)}
		m, _ := e.client.Chat(ctx, engine.ChatOptions{
			Model: model, Messages: msgs, MaxTokens: warmupMaxTokens,
			Stream: e.cfg.StreamEnabled(), ExtraBody: vOff.ExtraBody,
		})
		if m != nil {
			m.Phase = "warmup"
			out = append(out, m)
		}
	}
	log.Printf("  预热 %d 条请求完成（不计入主统计，完整数据已落盘）", len(out))
	return out
}

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
	return before, poller, start
}

// finalScrapeCtx 结束快照的独立 ctx（12.11）：场景 ctx 此时可能已被取消——运行中断
// （SIGHUP/Ctrl+C）或场景控制停止——而结束快照恰恰是窗口差值的唯一来源。
// r1-off S5 实测：中断场景 server_metrics.available=false、queue/prefill/decode 分解与
// preemptions 差值整段丢失，只能靠实时 /metrics 手工回溯。独立限时 ctx 不被取消传播
// 波及（正常路径无行为差异）；超时（5s 上限）如实记失败，不阻塞退出。
func finalScrapeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// finishWindow 汇总场景窗口的服务端观测。
//
// Available 的语义严格限定为「**窗口差值**（counter/hist）是否取到」：结束快照失败时窗口差值
// 无从计算，此时 Available=false 并保留 Note 说明原因——已轮询到的 gauges 仍然有效，照常挂回。
// 这样报告侧能如实区分「已采集 / 已启用但未取到 / 未提供」，不会把一次失败渲染成全零面板
// （历史 bug：中断场景下 available=true 且计数器全空，报告输出一句「服务端观测（single）：。」）。
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
		return // 非并发场景（single/multiturn 无聚合吞吐口径）不做该检查
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
// 基线阈值随 JSON 透出供 HTML 报告替代内置默认，未配置时不透出 = 报告用内置 SLO_TIERS）。
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
// 与 applySLO 同为「env → rep 的横切挂载」，三个场景统一。
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

// runCorrectness 数字转写金丝雀：验证服务返回的是真实生成内容（结构 200 但内容异常能被揪出）。
func runCorrectness(ctx context.Context, e *env, model string) ([]report.CorrectnessRow, []*engine.TurnMetrics) {
	n := 0
	if e.cfg.Correctness != nil {
		n = e.cfg.Correctness.Samples
	}
	if n <= 0 {
		return nil, nil
	}
	vOff := config.ThinkingVariant{Name: "off", Enabled: false, ExtraBody: e.cfg.ThinkingFor(model).ExtraBodyOff}
	rng := rand.New(rand.NewSource(4242)) // 固定种子：金丝雀可复现
	rows := []report.CorrectnessRow{}
	metrics := make([]*engine.TurnMetrics, 0, n)
	for i := 0; i < n; i++ {
		num := 10000 + rng.Intn(89999)
		var prompt string
		if e.cfg.FillerLang == "zh" {
			prompt = fmt.Sprintf("请只输出这个数字本身，不要任何其他内容：%d", num)
		} else {
			prompt = fmt.Sprintf("Reply with exactly this number and nothing else: %d", num)
		}
		m := runOne(ctx, e, model, []engine.Message{{Role: "user", Content: prompt}}, 16, vOff)
		if m == nil {
			continue
		}
		m.Phase = "correctness"
		metrics = append(metrics, m)
		reply := engine.TruncateRunes(m.ReplyText, 200)
		rows = append(rows, report.CorrectnessRow{
			Number: strconv.Itoa(num), Reply: reply,
			Match: m.Error == "" && strings.Contains(m.ReplyText, strconv.Itoa(num)),
			E2EMS: m.E2EMS, Error: m.Error,
		})
	}
	pass := 0
	for _, r := range rows {
		if r.Match {
			pass++
		}
	}
	log.Printf("  正确性抽查: %d/%d 通过（完整指标已落盘）", pass, len(rows))
	return rows, metrics
}

// runCorrectnessFor 金丝雀结果带上模型归属（数据按模型分区落盘时据此分桶）。
func runCorrectnessFor(ctx context.Context, e *env, model string) ([]report.CorrectnessRow, []*engine.TurnMetrics) {
	rows, metrics := runCorrectness(ctx, e, model)
	for i := range rows {
		rows[i].Model = model
	}
	return rows, metrics
}

// appendAuxiliary 把不进入 benchmark KPI 的请求完整挂到报告；原始数据不丢，
// 统计时由 phase 区分 warmup/correctness 与主压测。
func appendAuxiliary(rep *report.Report, ms []*engine.TurnMetrics) {
	for i, m := range ms {
		if m == nil {
			continue
		}
		rep.AuxiliaryRequests = append(rep.AuxiliaryRequests, report.AuxiliaryRequest{
			Phase: m.Phase, Index: i, Metrics: m,
		})
	}
}

// forModel 切换到某模型生效的配置视图（model_overrides 差异覆盖）：
// 场景内所有经 cfg/e.cfg 读配置的路径（runOne 的 stream、warmup、goodput、
// nextTurnTokens 的上下文截止等）随之取到该模型的实际生效值。
// 模型循环串行且档位内 worker 全部 join 后才进下一模型，e.cfg 不会被并发改写。
func forModel(e *env, cfg *config.Config, model string) (*env, *config.Config) {
	e.cfg = cfg.ForModel(model)
	return e, e.cfg
}

// ── Single 单发单轮 ──

func Single(ctx context.Context, cfg *config.Config, client *engine.Client, modelFilter string) (*report.Report, error) {
	e, err := newEnv(ctx, cfg, client)
	if err != nil {
		return nil, err
	}
	rep := &report.Report{
		Tool:        report.Version,
		Scenario:    "single",
		GeneratedAt: time.Now(),
		Test:        cfg.TestKind(),
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("单发单轮 runs=%d fixed_seed=%v stream=%v thinking=%s；思考开启时 max_tokens 下限 %d；数据源=%s%s",
			cfg.Single.Runs, cfg.Single.FixedSeed, cfg.StreamEnabled(), cfg.Thinking.Mode, cfg.Thinking.MaxTokensFloor, cfg.Dataset.Mode, thinkingNoteSuffix(cfg)),
	}
	applySLO(e, rep)
	attachKVCapacity(e, rep)
	before, poller, winStart := startWindow(ctx, e)
	defer func() { rep.Server = finishWindow(e, before, poller, winStart) }()

	for _, model := range filterModels(cfg.ActiveModels(), modelFilter) {
		_, mc := forModel(e, cfg, model) // 该模型生效配置（model_overrides 差异覆盖）
		th := mc.Thinking
		if len(th.Variants()) == 0 { // thinking 过滤后无匹配变体：整模型跳过（不发 warmup）
			log.Printf("  %s: 无匹配的思考变体，跳过", model)
			continue
		}
		appendAuxiliary(rep, warmup(ctx, e, model))
		for _, v := range th.Variants() {
			maxToks := th.MaxTokensList(mc.Single.MaxTokens, v) // 输出长度扫描维度（列表多档 / 标量单档）
			if e.trace != nil {
				// trace 模式：用回放会话的首轮 user 消息做档位（prompt_tokens 粗估，服务端 usage 为准）
				n := len(e.trace.Sessions)
				if n > traceSingleSampleLimit {
					log.Printf("  trace 单发档位只取样前 %d 个会话（数据集共 %d 个，避免拖长单发矩阵）", traceSingleSampleLimit, n)
					n = traceSingleSampleLimit
				}
				ctxAborted := false
				for _, maxTok := range maxToks {
					if ctxAborted {
						break
					}
					for i := 0; i < n; i++ {
						content := e.trace.Pick(i).UserTurns[0]
						row := report.SingleRow{Model: model, Thinking: v.Name, MaxTokens: maxTok, PromptTokens: len(content) / 4}
						log.Printf("[single] %s thinking=%s out=%dtk trace#%d (~%dtk)", model, v.Name, maxTok, i+1, row.PromptTokens)
						for run := 0; run < mc.Single.Runs; run++ {
							msgs := []engine.Message{{Role: "user", Content: content}}
							log.Printf("[single] %s thinking=%s out=%dtk trace#%d (~%dtk) run%d", model, v.Name, maxTok, i+1, row.PromptTokens, run+1)
							m := runOne(ctx, e, model, msgs, maxTok, v)
							row.Runs = append(row.Runs, m)
							if interrupted(ctx) {
								break
							}
						}
						rep.Single = append(rep.Single, row)
						if interrupted(ctx) {
							return rep, nil
						}
					}
				}
				continue
			}
			ladder, clamped := mc.ClampLadder(mc.Single.PromptTokens)
			if clamped {
				log.Printf("[single] 档位已按 max_prompt_tokens=%d 截断: %v", mc.MaxPromptTokens, ladder)
			}
			ctxAborted := false
			for _, maxTok := range maxToks {
				if ctxAborted {
					break
				}
				for _, tokens := range ladder {
					row := report.SingleRow{Model: model, Thinking: v.Name, MaxTokens: maxTok, PromptTokens: tokens}
					for run := 0; run < mc.Single.Runs; run++ {
						seed := singleSeed(mc.Single.FixedSeed, tokens, run, mc.SeedSalt)
						msgs := []engine.Message{engine.UserMsg(tokens, seed, mc.FillerLang)}
						log.Printf("[single] %s thinking=%s out=%dtk %dtk run%d", model, v.Name, maxTok, tokens, run+1)
						m := runOne(ctx, e, model, msgs, maxTok, v)
						row.Runs = append(row.Runs, m)
						if limit := ctxLimitHit(m); limit != "" {
							log.Printf("    🛑 触发模型上下文上限（limit=%stk）——跳过 %dtk 剩余 run 及更大档位（重试必然同样超限）", limit, tokens)
							ctxAborted = true
							break
						}
						if interrupted(ctx) {
							break
						}
					}
					rep.Single = append(rep.Single, row)
					if interrupted(ctx) {
						log.Printf("🛑 已中止（中断或场景停止）——停止新请求，已完成数据全部保留")
						return rep, nil
					}
					if ctxAborted {
						break
					}
				}
			}
		}
		rows, metrics := runCorrectnessFor(ctx, e, model)
		rep.Correctness = append(rep.Correctness, rows...)
		appendAuxiliary(rep, metrics)
	}
	return rep, nil
}

// ── Multiturn 单发多轮 ──

// nominalLastPrompt 12.3：filler 口径的名义末轮上下文（基座 + 轮数×每轮增量，×1.07 模板开销，
// 与开跑画像的估算口径一致）。trace 模式轮次来自回放会话，名义值无意义，返回 0
// （报告侧据此跳过实测 vs 名义对照）。
func nominalLastPrompt(trace bool, mt config.Multiturn) int {
	if trace || mt.TurnTokens <= 0 || mt.Turns <= 0 {
		return 0
	}
	return int(float64(mt.SystemTokens+mt.ToolDefsTokens+mt.Turns*mt.TurnTokens) * 1.07)
}

// profileAt 混合档（5.11）：按平滑加权轮转把会话序号映射到档位；无 profiles 时 ok=false。
// 使用 nginx smooth weighted round-robin：确定性分配、
// 同配置重跑结果一致（不引入随机）。返回的档位切片为浅拷贝，只读使用。
func profileAt(mt config.Multiturn, sessionIdx int) (config.MixProfile, bool) {
	n := len(mt.Profiles)
	if n == 0 {
		return config.MixProfile{}, false
	}
	total := 0
	for _, p := range mt.Profiles {
		total += p.Weight
	}
	if total <= 0 {
		return config.MixProfile{}, false // Load 已拦（weight>0），兜底防除零
	}
	pos := sessionIdx % total // 序号 -1 的偏移落在首轮内，序号≥0 才是合法输入
	if pos < 0 {
		pos += total
	}
	cur := make([]int, n)
	best := 0
	for j := 0; j <= pos; j++ {
		bestVal := -1
		for i := 0; i < n; i++ {
			cur[i] += mt.Profiles[i].Weight
			if cur[i] > bestVal {
				best, bestVal = i, cur[i]
			}
		}
		cur[best] -= total
	}
	return mt.Profiles[best], true
}

// nominalLastPromptAt 混合档口径的名义末轮深度：按会话所属档位（增量/轮数）计算；
// 无 profiles 时与 nominalLastPrompt 全等。
func nominalLastPromptAt(trace bool, mt config.Multiturn, sessionIdx int) int {
	if p, ok := profileAt(mt, sessionIdx); ok {
		if p.Turns > 0 {
			mt.Turns = p.Turns
		}
		mt.TurnTokens = p.TurnTokens
	}
	return nominalLastPrompt(trace, mt)
}

// sessionUserTurns 取会话的 user 消息序列：trace 模式来自回放会话，filler 模式返回 nil（走 token 填充）。
func (e *env) sessionUserTurns(sessionIdx int) []string {
	if e.trace == nil {
		return nil
	}
	return e.trace.Pick(sessionIdx).UserTurns
}

// fullReplay full 回放是否生效（replay_mode=full 且数据源为 trace 且含非 user 消息）。
func (e *env) fullReplay() bool {
	return e.trace != nil && e.trace.FullReplay && !e.trace.NoAssistantContent
}

// fullPrefixes full 回放：返回会话按 user 消息边界的累计消息前缀——
// 第 i 轮请求 = 原序消息到第 i 条 user 消息为止的全部内容（含其前的 assistant/tool 消息）。
// 这是"真实 agent 会话"的忠实回放：工具结果与助手回复都占着真实上下文深度。
func (e *env) fullPrefixes(sessionIdx int) [][]engine.Message {
	s := e.trace.Pick(sessionIdx)
	var out [][]engine.Message
	for i, m := range s.Messages {
		if m.Role == "user" {
			prefix := make([]engine.Message, 0, i+1)
			for _, mm := range s.Messages[:i+1] {
				prefix = append(prefix, engine.Message{Role: mm.Role, Content: mm.Content, ToolCallID: mm.ToolCallID})
			}
			out = append(out, prefix)
		}
	}
	return out
}

func Multiturn(ctx context.Context, cfg *config.Config, client *engine.Client, modelFilter string) (*report.Report, error) {
	e, err := newEnv(ctx, cfg, client)
	if err != nil {
		return nil, err
	}
	mt := cfg.Multiturn
	dataSrc := "filler"
	if e.trace != nil {
		dataSrc = "trace:" + e.trace.Source + "（replay_mode=" + cfg.Dataset.ReplayMode + "）"
	}
	// 上下文口径（5.9）：filler 模式注明起步/增量形状——逐轮增量比早前 12.3k 小是刻意设计
	// （模拟"每轮新增工具结果 + 追问"），报告中可追溯，防止被当成配置失误
	ctxShape := ""
	if e.trace == nil && mt.TurnTokens > 0 {
		base := mt.SystemTokens + mt.ToolDefsTokens
		ctxShape = fmt.Sprintf("，上下文 ~%.0fk 起步 → 末轮 ~%.0fk（每轮增量 %dtk：模拟每轮新增工具结果+追问）",
			float64(base+mt.TurnTokens)*1.07/1000, float64(base+mt.Turns*mt.TurnTokens)*1.07/1000, mt.TurnTokens)
	}
	rep := &report.Report{
		Tool:        report.Version,
		Scenario:    "multiturn",
		GeneratedAt: time.Now(),
		Test:        cfg.TestKind(),
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("单发多轮 sessions=%d turns=%d stream=%v thinking=%s 数据源=%s%s 基座=%s（trace 模式下轮次来自回放会话，system/turn_tokens 不生效）%s",
			mt.Sessions, mt.Turns, cfg.StreamEnabled(), cfg.Thinking.Mode, dataSrc, ctxShape,
			baseSharingDesc(mt), thinkingNoteSuffix(cfg)),
	}
	applySLO(e, rep)
	attachKVCapacity(e, rep)
	models := filterModels(cfg.ActiveModels(), modelFilter)
	for _, model := range models {
		_, mc := forModel(e, cfg, model)
		if len(mc.Thinking.Variants()) > 0 {
			appendAuxiliary(rep, warmup(ctx, e, model))
		}
	}
	before, poller, winStart := startWindow(ctx, e)
	serverDone := false
	finishServer := func() {
		if !serverDone {
			rep.Server = finishWindow(e, before, poller, winStart)
			serverDone = true
		}
	}
	defer finishServer()

	for _, model := range models {
		_, mc := forModel(e, cfg, model) // 该模型生效配置（model_overrides 差异覆盖）
		mt := mc.Multiturn               // 遮蔽外层通用值（Note 仍描述通用基线；覆盖差异见 thinkingNoteSuffix）
		if len(mt.Profiles) > 0 {
			log.Printf("⚠️ multiturn.profiles 仅并发多轮生效：本次单发多轮忽略档位，会话统一用 turn_tokens=%d（--concurrency 2,4,… 时按档位分配）", mt.TurnTokens)
		}
		th := mc.Thinking
		if len(th.Variants()) == 0 { // thinking 过滤后无匹配变体：整模型跳过（不发 warmup）
			log.Printf("  %s: 无匹配的思考变体，跳过", model)
			continue
		}
		e.warnTraceWrap(mt.Sessions)
		for _, v := range th.Variants() {
			ctxAborted := false // 触发模型上下文上限：剩余会话必然同样超限，全部跳过
			for _, maxTok := range th.MaxTokensList(mt.MaxTokens, v) {
				if ctxAborted {
					break
				}
				for s := 0; s < mt.Sessions; s++ {
					run := report.MultiturnRun{Model: model, Thinking: v.Name, Session: s + 1, MaxTokens: maxTok}
					log.Printf("[multiturn] %s thinking=%s out=%dtk session%d", model, v.Name, maxTok, s+1)
					baseSeed, turnSeed := sessionSeeds(mc, s)
					msgs := []engine.Message{}
					if e.trace == nil {
						if sys := engine.SystemMsg(mt.SystemTokens, mt.ToolDefsTokens, baseSeed, mc.FillerLang); sys.Content != "" {
							msgs = append(msgs, sys)
						}
					}
					userTurns := e.sessionUserTurns(s)
					var fullPrefixes [][]engine.Message
					if e.fullReplay() {
						fullPrefixes = e.fullPrefixes(s)
						if len(fullPrefixes) < len(userTurns) {
							userTurns = userTurns[:len(fullPrefixes)] // 两视图按 user 消息对齐
						}
					}
					lastPrompt := 0 // 上一轮服务端实测 prompt_tokens（截止计算与新增 tokens 计算）
					estPrompt := 0  // usage 缺失时的生成侧估算：filler 每轮累加本轮 turn tokens，usage 恢复即对齐实测
					for turn := 0; turn < mt.Turns; turn++ {
						if e.fullReplay() {
							if turn >= len(fullPrefixes) {
								log.Printf("    回放会话只有 %d 轮 user 消息，提前结束", len(fullPrefixes))
								break
							}
							msgs = fullPrefixes[turn] // full：原序全部 role，assistant/tool 都在上下文里
						} else if e.trace != nil {
							if turn >= len(userTurns) {
								log.Printf("    回放会话只有 %d 轮 user 消息，提前结束", len(userTurns))
								break
							}
							msgs = append(msgs, engine.Message{Role: "user", Content: userTurns[turn]})
						} else {
							tt := nextTurnTokens(mc, mt.TurnTokens, effPromptOf(lastPrompt, estPrompt))
							if tt <= 0 {
								log.Printf("    已达 max_prompt_tokens=%d 截止，提前结束会话（%d/%d 轮）", mc.MaxPromptTokens, turn, mt.Turns)
								break
							}
							msgs = append(msgs, engine.UserMsg(tt, turnSeed+int64(turn), mc.FillerLang))
							estPrompt += tt // usage 缺失时的估算基线：本轮已追加进 history，下一轮的 prompt 必然包含它
						}
						m := runOne(ctx, e, model, msgs, maxTok, v)
						if m.PromptTokens > 0 {
							// tokenizer 对累积 history 重新切分等场景下 prompt 可能不增反减，
							// 负增量会污染增量 prefill 斜率——钳 0（原始值在 prompt_tokens 可核查）
							if lastPrompt > 0 {
								m.NewTokens = m.PromptTokens - lastPrompt
								if m.NewTokens < 0 {
									m.NewTokens = 0
								}
							} else {
								m.NewTokens = m.PromptTokens
							}
							// 只有成功的轮次才推进基准：失败的轮次（ctx=0）不能把 lastPrompt 清零，
							// 否则下一轮会把整条 history 都算成"新增"，增量 prefill 指标错乱
							lastPrompt = m.PromptTokens
							estPrompt = m.PromptTokens // usage 恢复：估算基线对齐实测
						}
						log.Printf("    turn%d (ctx≈%dtk +%dtk)", turn+1, m.PromptTokens, m.NewTokens)
						if mt.KeepAssistant && m.ReplyText != "" && !e.fullReplay() {
							// full 模式 history 来自 trace 原文，不追加生成回复（追加会与原始 assistant 重复）
							msgs = append(msgs, engine.Message{Role: "assistant", Content: engine.TruncateRunes(m.ReplyText, mt.MaxReplyChars)})
						}
						run.Turns = append(run.Turns, m)
						if interrupted(ctx) {
							// 被 ctx 取消的这一轮是作废样本（error 非空、prompt_tokens=0），
							// 不能算进「已完成」——否则日志报「3/8」而落盘数据里只有 2 轮能用，
							// 读数的人会高估有效样本量。
							done, dropped := 0, 0
							for _, t := range run.Turns {
								if t.Error == "" {
									done++
								} else {
									dropped++
								}
							}
							extra := ""
							if dropped > 0 {
								extra = fmt.Sprintf("，另 %d 轮被中断作废（error 非空，未计入）", dropped)
							}
							log.Printf("    🛑 已中止（中断或场景停止）——提前结束会话（有效 %d/%d 轮保留%s）",
								done, mt.Turns, extra)
							break
						}
						if limit := ctxLimitHit(m); limit != "" {
							// 会话 history 已超限，继续加轮必然失败——结束该会话并跳过剩余会话
							log.Printf("    🛑 触发模型上下文上限（limit=%stk，ctx≈%dtk）——提前结束会话，跳过剩余会话", limit, m.PromptTokens)
							ctxAborted = true
							break
						}
					}
					run.FillLastPromptTokens() // 12.3：末轮实测深度（名义外推偏乐观 10–15%，落盘实测供报告对照）
					run.NominalLastPrompt = nominalLastPrompt(e.trace != nil, mt)
					rep.Multiturn = append(rep.Multiturn, run)
					if interrupted(ctx) {
						log.Printf("🛑 已中止（中断或场景停止）——停止新请求，已完成会话全部保留")
						return rep, nil
					}
					if ctxAborted {
						break
					}
				}
			}
		}
	}
	// benchmark 服务端窗口在 correctness 之前结束；correctness 仍完整采集，但不混入 server 对账。
	finishServer()
	for _, model := range models {
		_, mc := forModel(e, cfg, model)
		if len(mc.Thinking.Variants()) == 0 {
			continue
		}
		rows, metrics := runCorrectnessFor(ctx, e, model)
		rep.Correctness = append(rep.Correctness, rows...)
		appendAuxiliary(rep, metrics)
	}
	return rep, nil
}

// ── Concurrent 并发（闭环 levels / 开环到达率） ──

// openRates 返回开环模式的到达率列表；nil = 走闭环 levels。
func openRates(cc config.Concurrent) []float64 {
	if len(cc.RateSweep) > 0 {
		return cc.RateSweep
	}
	if cc.RequestRate > 0 {
		return []float64{cc.RequestRate}
	}
	return nil
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

// sessionTurnHook 每轮完成后的回调（drain/soak 截止用）：turn 为 0 基轮次、m 为该轮指标；
// 返回 false = 提前终止该会话（已完成轮保留）。nil = 无钩子。
type sessionTurnHook func(turn int, m *engine.TurnMetrics) bool

// collectSessionTurns 执行一次完整多轮会话并采集逐 turn 指标（并发多轮用：每个虚拟用户一次）。
// filler 模式为合成模拟对话（逐轮滚动 history）；trace 数据源才是真实会话重放。
// maxTok 由调用方传入（输出长度扫描维度，已含思考 floor 抬高）。
func collectSessionTurns(ctx context.Context, e *env, cfg *config.Config,
	model string, v config.ThinkingVariant, sessionIdx int, turnLimit int, maxTok int,
	hook sessionTurnHook) []*engine.TurnMetrics {

	mt := cfg.Multiturn
	prof, hasProf := profileAt(mt, sessionIdx) // 混合档（5.11）：按会话序号取档位（无配置则零值）
	turnTokens := mt.TurnTokens
	if hasProf {
		turnTokens = prof.TurnTokens
	}
	baseSeed, turnSeed := sessionSeeds(cfg, sessionIdx)
	msgs := []engine.Message{}
	userTurns := e.sessionUserTurns(sessionIdx)
	var fullPrefixes [][]engine.Message
	if e.fullReplay() {
		fullPrefixes = e.fullPrefixes(sessionIdx)
		if len(fullPrefixes) < len(userTurns) {
			userTurns = userTurns[:len(fullPrefixes)]
		}
	}
	if e.trace == nil {
		if sys := engine.SystemMsg(mt.SystemTokens, mt.ToolDefsTokens, baseSeed, cfg.FillerLang); sys.Content != "" {
			msgs = append(msgs, sys)
		}
	}
	turns := turnLimit
	if turns <= 0 {
		turns = mt.Turns
		if hasProf && prof.Turns > 0 {
			turns = prof.Turns // 混合档：档位轮数覆盖全局（0 = 继承）
		}
	}
	var out []*engine.TurnMetrics
	lastPrompt := 0
	estPrompt := 0 // usage 缺失时的生成侧估算（与 Multiturn 同语义）
	for turn := 0; turn < turns; turn++ {
		if e.fullReplay() {
			if turn >= len(fullPrefixes) {
				break
			}
			msgs = fullPrefixes[turn]
		} else if userTurns != nil {
			if turn >= len(userTurns) {
				break
			}
			msgs = append(msgs, engine.Message{Role: "user", Content: userTurns[turn]})
		} else {
			tt := nextTurnTokens(cfg, turnTokens, effPromptOf(lastPrompt, estPrompt))
			if tt <= 0 {
				log.Printf("    worker 会话已达 max_prompt_tokens=%d 截止，提前结束（%d/%d 轮）", cfg.MaxPromptTokens, turn, turns)
				break
			}
			msgs = append(msgs, engine.UserMsg(tt, turnSeed+int64(turn), cfg.FillerLang))
			estPrompt += tt // 本轮已追加进 history，下一轮的 prompt 必然包含它
		}
		m := runOne(ctx, e, model, msgs, maxTok, v)
		if m.PromptTokens > 0 {
			if lastPrompt > 0 {
				// 与 Multiturn 同语义：失败轮不推进基准；tokenizer 重切分致 prompt
				// 不增反减时钳 0，避免负增量污染增量 prefill 斜率（原始值在 prompt_tokens 可核查）
				m.NewTokens = m.PromptTokens - lastPrompt
				if m.NewTokens < 0 {
					m.NewTokens = 0
				}
			} else {
				m.NewTokens = m.PromptTokens
			}
			lastPrompt = m.PromptTokens
			estPrompt = m.PromptTokens // usage 恢复：估算基线对齐实测
		}
		out = append(out, m)
		if hook != nil && !hook(turn, m) {
			break
		}
		if mt.KeepAssistant && m.ReplyText != "" && !e.fullReplay() {
			msgs = append(msgs, engine.Message{Role: "assistant", Content: engine.TruncateRunes(m.ReplyText, mt.MaxReplyChars)})
		}
	}
	return out
}

func Concurrent(ctx context.Context, cfg *config.Config, client *engine.Client, modelFilter string) (*report.Report, error) {
	e, err := newEnv(ctx, cfg, client)
	if err != nil {
		return nil, err
	}
	// 并发/开环关闭逐请求 /metrics 抓取：抓取耗时曾落在计时窗口内压低吞吐口径，
	// 且并发下各请求差值窗口互相重叠无归因意义——只保留场景窗口级差分（P3-6）
	e.perReqSrv = false
	cc := cfg.Concurrent
	rates := openRates(cc)
	mode := "单轮"
	if cc.Multiturn {
		mode = "多轮会话模拟" // filler=合成模拟对话；trace 数据源才是"重放"（见 collectSessionTurns）
		if e.trace != nil {
			mode = "多轮会话重放(trace)"
		}
	}
	loadModel := "闭环并发"
	if rates != nil {
		loadModel = fmt.Sprintf("开环到达率 %v req/s", rates)
	}
	if cc.Multiturn && len(cfg.Multiturn.Profiles) > 0 {
		// 混合档（5.11）：会话按权重分属不同增量档
		labels := make([]string, len(cfg.Multiturn.Profiles))
		for i, p := range cfg.Multiturn.Profiles {
			labels[i] = fmt.Sprintf("%s×%d", p.Name, p.Weight)
		}
		loadModel += " 混跑[" + strings.Join(labels, " ") + "]"
	}
	perUser := "每用户独立 prompt（不同 seed）"
	if cc.Multiturn {
		// 多轮下要说清基座形态：shared_base 默认 true = 全部会话同一套 system/tool defs
		// 前缀（贴"一套部署一套提示词"），逐轮 user 内容仍按会话独立。
		perUser = "每用户独立会话（逐轮内容不同 seed；基座" + baseSharingDesc(cfg.Multiturn) + "）"
	}
	rep := &report.Report{
		Tool:        report.Version,
		Scenario:    "concurrent",
		GeneratedAt: time.Now(),
		Test:        cfg.TestKind(),
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("并发%s %s prompt≈%dtk stream=%v thinking=%s；%s%s",
			mode, loadModel, cc.PromptTokens, cfg.StreamEnabled(), cfg.Thinking.Mode, perUser, thinkingNoteSuffix(cfg)),
	}
	applySLO(e, rep)
	attachKVCapacity(e, rep)
	// 预热先对全部待测模型完成并落盘，随后才开启 benchmark 两源一致性窗口，
	// 避免后续模型的 warmup 混入服务端 generation_tokens 差值。
	models := filterModels(cfg.ActiveModels(), modelFilter)
	for _, model := range models {
		_, mc := forModel(e, cfg, model)
		if len(mc.Thinking.Variants()) > 0 {
			appendAuxiliary(rep, warmup(ctx, e, model))
		}
	}
	before, poller, winStart := startWindow(ctx, e)
	serverDone := false
	finishServer := func() {
		if !serverDone {
			rep.Server = finishWindow(e, before, poller, winStart)
			serverDone = true
		}
	}
	defer finishServer()

	// 10.1 两源一致性窗口：只覆盖 benchmark 档位，不包含 warmup/correctness。
	var chkBefore *smetrics.Sample
	if e.srv != nil {
		chkBefore, _ = e.srv.Scrape(ctx)
	}
	sourceCheckDone := false
	finishSourceCheck := func() {
		if sourceCheckDone {
			return
		}
		sourceCheckDone = true
		applySourceCheck(e, rep, chkBefore)
	}
	// 饱和止损判据一依赖 waiting 排队深度（场景级 GaugePoller）；观测层不可用时提前
	// 说一声——waiting 判定不生效，墙钟上限（判据二）仍有效
	if cfg.SaturationGuard.SatEnabled() && cfg.SaturationGuard.MaxWaiting > 0 && poller == nil {
		log.Printf("⚠️ saturation_guard.max_waiting 需要 server_metrics 观测（waiting 排队深度不可得）——waiting 判定不生效，墙钟上限仍有效")
	}

	for _, model := range models {
		_, mc := forModel(e, cfg, model) // 该模型生效配置（model_overrides 差异覆盖）
		cc := mc.Concurrent              // 遮蔽外层通用值：模型层可覆盖 levels/开环参数（Note 仍描述通用基线）
		rates := openRates(cc)
		th := mc.Thinking
		if len(th.Variants()) == 0 { // thinking 过滤后无匹配变体：整模型跳过（不发 warmup）
			log.Printf("  %s: 无匹配的思考变体，跳过", model)
			continue
		}
		for _, v := range th.Variants() {
			tiers := th.MaxTokensList(cc.MaxTokens, v)
			for _, maxTok := range tiers {
				if rates != nil {
					for _, rate := range rates {
						lv := runOpenRound(ctx, e, mc, model, v, rate, maxTok, poller)
						logConcurrent(&lv)
						rep.Concurrent = append(rep.Concurrent, lv)
						if lv.Aborted != "" {
							// 饱和止损：本到达率已饱和/超时，更高档只会更糟——停止后续档位
							log.Printf("🛑 %s——停止后续到达率档位，已完成数据全部保留", lv.Aborted)
							finishSourceCheck()
							return rep, nil
						}
						if interrupted(ctx) {
							log.Printf("🛑 已中止（中断或场景停止）——停止新请求，已完成档位全部保留")
							finishSourceCheck()
							return rep, nil
						}
					}
					continue
				}
				for _, level := range cc.Levels {
					lv := runClosedRound(ctx, e, mc, model, v, level, maxTok, poller)
					logConcurrent(&lv)
					rep.Concurrent = append(rep.Concurrent, lv)
					if lv.Aborted != "" {
						// saturation/drain 或外部中断：后续档位不再发压，已采集数据照常保留
						log.Printf("🛑 %s——停止后续档位与场景，已完成数据全部保留", lv.Aborted)
						finishSourceCheck()
						return rep, nil
					}
					if interrupted(ctx) {
						log.Printf("🛑 已中止（中断或场景停止）——停止新请求，已完成档位全部保留")
						finishSourceCheck()
						return rep, nil
					}
				}
			}
		}
	}
	// benchmark 窗口在 correctness 之前结束，避免金丝雀 token 混入 server_metrics/source_check。
	finishSourceCheck()
	finishServer()
	for _, model := range models {
		_, mc := forModel(e, cfg, model)
		if len(mc.Thinking.Variants()) == 0 {
			continue
		}
		rows, metrics := runCorrectnessFor(ctx, e, model)
		rep.Correctness = append(rep.Correctness, rows...)
		appendAuxiliary(rep, metrics)
	}
	return rep, nil
}

func logConcurrent(lv *report.ConcurrentLevel) {
	extra := ""
	if lv.RequestRate > 0 {
		extra = fmt.Sprintf(" rate=%.1f/s", lv.RequestRate)
	}
	outDesc := fmt.Sprintf("out=%dtk", lv.MaxTokens)
	log.Printf("[concurrent] %s thinking=%s %s level%d%s: wall=%.1fs throughput=%.0f tok/s%s",
		lv.Model, lv.Thinking, outDesc, lv.Level, extra, lv.WallSeconds, lv.ThroughputTPS, goodputLog(lv))
}

func goodputLog(lv *report.ConcurrentLevel) string {
	if lv.SLOTotal == 0 {
		return ""
	}
	return fmt.Sprintf(" goodput=%d/%d", lv.SLOMeet, lv.SLOTotal)
}

// runClosedRound 闭环并发档位：level 个 worker 各自跑 runs_per_worker 次请求（或一次完整会话）。
// maxTok 输出长度由调用方传入（输出长度扫描维度，已含思考 floor 抬高）。
// pol 为场景级 gauge 轮询器（饱和止损判据一的观测源；nil = 观测层不可用）。
//
// saturation_guard（2026-09-12，drain 语义）：waiting 持续超阈或墙钟到点 → 停止发新
// 请求（在飞跑完保留全量），Aborted 留痕，场景层停止后续档位（饱和之后更高档只会更糟）。
func runClosedRound(ctx context.Context, e *env, cfg *config.Config,
	model string, v config.ThinkingVariant, level int, maxTok int, pol *smetrics.GaugePoller) report.ConcurrentLevel {

	cc := cfg.Concurrent
	if cc.Multiturn {
		e.warnTraceWrap(level)
	}
	lv := &report.ConcurrentLevel{Model: model, Thinking: v.Name, MaxTokens: maxTok, Level: level,
		DurationSeconds: float64(cc.DurationSeconds), Renew: cc.Renew}
	start := time.Now()
	// 10.5 时长制 soak：dur>0 时各 worker 跑满墙钟（runs_per_worker 被忽略）；
	// capped = 到点该收手（所有发新请求/新轮的入口都要查——drain 语义，在飞跑完保留）
	dur := time.Duration(cc.DurationSeconds) * time.Second
	capped := func() bool { return dur > 0 && time.Since(start) >= dur }
	var mu sync.Mutex
	var wg sync.WaitGroup
	startBarrier := make(chan struct{})

	// 档位控制器：饱和/墙钟触发 = 关发射闸门（drain），不取消在飞
	sat := cfg.SaturationGuard
	lr := newLevelRun(sat)
	defer lr.finish()
	roundCtx, roundCancel := context.WithCancel(ctx)
	defer roundCancel()
	waitingMax, runningMax, satWait := startSaturationWatch(roundCtx, sat, pol, lr)

	worker := func(workerID int) {
		defer wg.Done()
		<-startBarrier // 所有 worker 就绪后同时发车
		if cc.Multiturn {
			// 10.5 renew 时长制 soak：会话滚完 turns 轮后换新 seed 重开（序号递增——seed 与
			// Session 编号同源，上下文清零重涨），直到时长满；非时长制保持原行为（单会话即终点）。
			for sr := 0; ; sr++ {
				idx := workerID + sr*level // 会话序号全局唯一（seed/Session 编号共用）
				s := report.MultiturnRun{Model: model, Thinking: v.Name, Session: idx + 1, MaxTokens: maxTok}
				if p, ok := profileAt(cfg.Multiturn, idx); ok {
					s.Profile = p.Name // 混合档（5.11）：会话档位标签
				}
				if dur > 0 {
					// 时长制记录会话启动偏移，供外部分析首末时段漂移。
					s.StartOffsetS = time.Since(start).Seconds()
				}
				// hook 只负责 saturation/drain 与 soak 截止；服务端失败不提前终止会话，
				// 每一轮的失败指标都完整留存。
				hook := func(int, *engine.TurnMetrics) bool {
					return !lr.Stop() && !capped()
				}
				s.Turns = collectSessionTurns(roundCtx, e, cfg, model, v, idx, 0, maxTok, hook)
				s.FillLastPromptTokens() // 12.3：末轮实测深度
				s.NominalLastPrompt = nominalLastPromptAt(e.trace != nil, cfg.Multiturn, idx)
				mu.Lock()
				lv.Sessions = append(lv.Sessions, s)
				mu.Unlock()
				if dur <= 0 || !cc.Renew {
					return // 次数制 / 非续跑：单会话即 worker 终点
				}
				if roundCtx.Err() != nil || lr.Stop() || capped() {
					return // 取消 / drain / 到点：不再重开会话
				}
			}
		}
		for r := 0; ; r++ {
			// 10.5 时长制：到点不再发新请求（drain——在飞跑完保留）；次数制按 runs_per_worker
			if roundCtx.Err() != nil || lr.Stop() || capped() {
				return // 外部中断、saturation drain 或时长到点：不再发新请求
			}
			if dur <= 0 && r >= cc.RunsPerWorker {
				return
			}
			promptTokens := cfg.ClampOne(cc.PromptTokens)
			seed := workerSeed(workerID, r, cfg.SeedSalt) // 每用户不同 prompt
			msgs := []engine.Message{engine.UserMsg(promptTokens, seed, cfg.FillerLang)}
			m := runOne(roundCtx, e, model, msgs, maxTok, v)
			mu.Lock()
			lv.Requests = append(lv.Requests, m)
			mu.Unlock()
		}
	}

	for w := 0; w < level; w++ {
		wg.Add(1)
		go worker(w)
	}
	close(startBarrier)
	wg.Wait()
	// 本档位已结束：停观测器、收 waiting/running 峰值（标定数据），再组装终止原因
	roundCancel()
	satWait()
	lv.WaitingMax = waitingMax()
	lv.RunningMax = runningMax()
	if r := lr.Reason(); r != "" {
		lv.Aborted = r // 饱和/墙钟触发的原因（lr.Trip 记录时已打日志）
	} else if ctx.Err() != nil {
		// 12.11：运行中断（SIGHUP/Ctrl+C）——真实原因必须留痕，别让「没跑完」看起来像服务端问题
		lv.Aborted = "运行中断（SIGHUP/Ctrl+C），场景未跑完（已完成数据照常保留）"
		log.Printf("⛔ %s", lv.Aborted)
	}
	finalizeLevel(e, lv, time.Since(start).Seconds())
	return *lv
}

// runOpenRound 开环到达率：请求按 Poisson 过程到达（对齐 vLLM bench serve），测排队-延迟曲线。
// maxTok 输出长度由调用方传入（输出长度扫描维度，已含思考 floor 抬高）。
// pol 为场景级 gauge 轮询器（饱和止损判据一的观测源；nil = 观测层不可用）。
// saturation_guard：waiting 持续超阈或墙钟超限 → 取消本档位（停止发新到达 + 取消在飞），
// Aborted 留痕，场景层停止后续档位。
func runOpenRound(ctx context.Context, e *env, cfg *config.Config,
	model string, v config.ThinkingVariant, rate float64, maxTok int, pol *smetrics.GaugePoller) report.ConcurrentLevel {

	cc := cfg.Concurrent
	lv := &report.ConcurrentLevel{Model: model, Thinking: v.Name, MaxTokens: maxTok, Level: 0, RequestRate: rate}
	n := cc.NumPrompts
	if n <= 0 {
		n = 32
	}
	if cc.Multiturn {
		n := cc.NumPrompts
		if n <= 0 {
			n = 32
		}
		e.warnTraceWrap(n)
	}
	start := time.Now()
	// 档位控制器：饱和/墙钟触发 = 关发射闸门（drain），不取消在飞
	sat := cfg.SaturationGuard
	lr := newLevelRun(sat)
	defer lr.finish()
	roundCtx, roundCancel := context.WithCancel(ctx)
	defer roundCancel()
	waitingMax, runningMax, satWait := startSaturationWatch(roundCtx, sat, pol, lr)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var sem chan struct{}
	if cc.MaxConcurrency > 0 {
		sem = make(chan struct{}, cc.MaxConcurrency)
	}
	// 固定种子：同一 rate 重复跑到达序列一致（可复现）。
	// 用 Float64bits 而非 rate*1000——浮点截断会让 0.5001/0.5002 这类相邻档位碰撞出同一种子
	rng := rand.New(rand.NewSource(int64(math.Float64bits(rate))))
	launch := func(i int) {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if sem != nil {
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-roundCtx.Done():
					return // 本档位已取消：不再占用并发槽
				}
			}
			if cc.Multiturn {
				s := report.MultiturnRun{Model: model, Thinking: v.Name, Session: i + 1, MaxTokens: maxTok}
				if p, ok := profileAt(cfg.Multiturn, i); ok {
					s.Profile = p.Name // 混合档（5.11）：会话档位标签
				}
				// drain 闸门要求会话中途也能停（已完成轮保留）
				stopHook := sessionTurnHook(func(int, *engine.TurnMetrics) bool { return !lr.Stop() })
				s.Turns = collectSessionTurns(roundCtx, e, cfg, model, v, i, 0, maxTok, stopHook)
				s.FillLastPromptTokens() // 12.3：末轮实测深度
				s.NominalLastPrompt = nominalLastPromptAt(e.trace != nil, cfg.Multiturn, i)
				mu.Lock()
				lv.Sessions = append(lv.Sessions, s)
				mu.Unlock()
				return
			}
			promptTokens := cfg.ClampOne(cc.PromptTokens)
			seed := openWorkerSeed(i, cfg.SeedSalt)
			msgs := []engine.Message{engine.UserMsg(promptTokens, seed, cfg.FillerLang)}
			m := runOne(roundCtx, e, model, msgs, maxTok, v)
			mu.Lock()
			lv.Requests = append(lv.Requests, m)
			mu.Unlock()
		}(i)
	}
	schedDone := make(chan struct{})
	go func() { // Poisson 调度：预生成到达时刻线，逐请求睡到绝对时刻（补偿发射循环自身滞后）
		defer close(schedDone)
		delays := poissonDelays(n, rate, cfg.Concurrent.EffBurstiness(), rng)
		startAt := time.Now()
		for i := 0; i < n; i++ {
			if d := time.Duration(delays[i]*float64(time.Second)) - time.Since(startAt); d > 0 {
				select {
				case <-roundCtx.Done():
					return // 本档位已取消（场景中止）：停止发新到达
				case <-time.After(d):
				}
			}
			if lr.Stop() {
				return // drain 触发或本档位取消：尚未发出的到达不再发射
			}
			launch(i)
		}
	}()
	<-schedDone // 全部请求已按到达序列发射（或本档位被提前停止）
	wg.Wait()
	// 本档位已结束：停观测器、收 waiting/running 峰值（标定数据），再组装终止原因
	roundCancel()
	satWait()
	lv.WaitingMax = waitingMax()
	lv.RunningMax = runningMax()
	if r := lr.Reason(); r != "" {
		lv.Aborted = r
	} else if ctx.Err() != nil {
		// 12.11：运行中断（SIGHUP/Ctrl+C）——真实原因留痕（同闭环口径）
		lv.Aborted = "运行中断（SIGHUP/Ctrl+C），场景未跑完（已完成数据照常保留）"
	}
	finalizeLevel(e, lv, time.Since(start).Seconds())
	return *lv
}

// finalizeLevel 计算墙钟吞吐与 goodput。
func finalizeLevel(e *env, lv *report.ConcurrentLevel, wall float64) {
	lv.WallSeconds = wall
	throughput := 0.0
	meet, meetTokens := 0, 0.0
	collect := func(ms []*engine.TurnMetrics) {
		for _, m := range ms {
			if m == nil {
				continue
			}
			if m.Cancelled {
				lv.CancelledRequests++
				continue
			}
			if e.cfg.EffGoodput() != nil {
				// 服务端/网络失败是已发出的请求，仍进入 SLO 分母；主动取消是工具
				// 控制行为，不是服务端结果，前面已单独排除。
				lv.SLOTotal++
			}
			if m.Error != "" {
				lv.FailedRequests++
				continue
			}
			lv.CompletedRequests++
			throughput += float64(m.CompletionTokens)
			if e.cfg.EffGoodput() != nil && goodputOf(e, m) {
				meet++
				meetTokens += float64(m.CompletionTokens)
			}
		}
	}
	collect(lv.Requests)
	for _, s := range lv.Sessions {
		collect(s.Turns)
	}
	if lv.WallSeconds > 0 {
		lv.ThroughputTPS = throughput / lv.WallSeconds
		if meet > 0 {
			lv.GoodputRPS = float64(meet) / lv.WallSeconds
			lv.GoodputTPS = meetTokens / lv.WallSeconds
		}
	}
	lv.SLOMeet = meet
}

// ── 种子派生（表驱动测试锁定语义：盐值隔离测试、fixed_seed 档内复用/档间独立、worker 间互异） ──

// singleSeed 单发场景：fixed_seed 时每档独立种子（档位内各 run 复用同一 prompt 测缓存对照）。
// 不要让不同档位共享种子——语料窗口同起点会使档位间 prompt 互为嵌套前缀，
// 上一档的缓存会"预热"下一档的 run1，冷启动测量就不干净了。
func singleSeed(fixed bool, tokens, run, salt int) int64 {
	if fixed {
		return int64(1000 + tokens + salt)
	}
	return int64(tokens*100 + run + salt)
}

// baseSharingDesc 基座共享形态的人读描述（多轮场景 Note 用）。
// 这是 10.3 的拍板点：默认共享，等于"一套部署一套提示词"的线上形态。
func baseSharingDesc(mt config.Multiturn) string {
	if mt.GetSharedBase() {
		return "跨会话共享（同一套 system/tools，逐轮内容仍按会话独立）"
	}
	return "每会话独立（不同 seed，缓存不可跨会话复用）"
}

// sessionSeed 多轮会话：不同会话（含并发多轮的不同虚拟用户）内容互异。
func sessionSeed(sessionIdx, salt int) int64 { return int64(5000 + sessionIdx*10000 + salt) }

// sharedBaseSeed 共享基座（`multiturn.shared_base: true`，默认）的基座种子：
// 与会话编号无关，只随盐值变化（换盐 = 换基座内容 = 重新冷）。
//
// 刻意不复用 sessionSeed(0)：两个形态的基座内容必须互不相同，否则「共享 vs 独立」
// 的对照里，独立形态的 session0 会与共享形态的基座同前缀，被上一轮的 prefix cache
// 提前热身，对照就不干净了。
func sharedBaseSeed(salt int) int64 { return int64(7000 + salt) }

// sessionSeeds 返回一个多轮会话的（基座种子, 逐轮种子）。
//
// SharedBase=true（默认）：基座跨会话共享——全部会话顶着同一套 system/tool defs 前缀，
// 贴近"一套部署一套提示词"的真实形态，测的是**跨用户共享前缀值多少 TTFT**。
// 逐轮种子始终按会话独立：否则会话之间会变成逐字节相同，跨会话对比与 per-session
// 统计（会话间方差、失败隔离）都失去意义。
func sessionSeeds(cfg *config.Config, sessionIdx int) (base int64, turn int64) {
	turn = sessionSeed(sessionIdx, cfg.SeedSalt)
	base = turn
	if cfg.Multiturn.GetSharedBase() {
		base = sharedBaseSeed(cfg.SeedSalt)
	}
	return base, turn
}

// workerSeed 闭环并发单轮：不同 worker / 不同 run 内容互异。
// stride 取 1_000_000（≥ runs_per_worker 实际可达上限）：曾用 100，
// runs_per_worker ≥ 100 时相邻 worker 撞种子（相同 prompt 破坏缓存对照）。
func workerSeed(workerIdx, run, salt int) int64 {
	return int64(90000 + workerIdx*1_000_000 + run + salt)
}

// openWorkerSeed 开环并发单轮。
func openWorkerSeed(i, salt int) int64 { return int64(90000 + i + salt) }

// nextTurnTokens 根据上下文截止计算本轮 user 消息的 token 规模。
// lastPrompt 为上一轮服务端实测 prompt_tokens（首轮传 0）；返回 0 表示已达上限应停轮。
// 未配置截止（MaxPromptTokens<=0）时原样返回 turnTokens。
func nextTurnTokens(cfg *config.Config, turnTokens, lastPrompt int) int {
	if cfg.MaxPromptTokens <= 0 {
		return turnTokens
	}
	remaining := cfg.MaxPromptTokens - lastPrompt
	if remaining < 200 { // 剩余空间不足一个最小 turn，停止加轮
		return 0
	}
	if turnTokens > remaining {
		return remaining
	}
	return turnTokens
}

// effPromptOf 截止计算用的有效上下文基线：优先服务端实测 prompt_tokens；
// 引擎不回 usage 时（恒为 0）回退到生成侧估算——否则 remaining 恒等于
// MaxPromptTokens，max_prompt_tokens 截止形同虚设，上下文无界增长撞模型上限。
func effPromptOf(server, est int) int {
	if server > 0 {
		return server
	}
	return est
}

func filterModels(models []string, filter string) []string {
	if filter == "" {
		return models
	}
	var out []string
	for _, m := range models {
		if strings.Contains(m, filter) {
			out = append(out, m)
		}
	}
	return out
}

// FirstMatchedModel 返回按 -m 子串语义过滤后的首个模型（恢复探针等 main 侧消费）；
// 无匹配（或列表为空）返回空串。与 filterModels 同一语义，避免过滤口径两处漂移。
func FirstMatchedModel(models []string, filter string) string {
	for _, m := range filterModels(models, filter) {
		return m
	}
	return ""
}
