// Package scenario 实现评测场景矩阵：
//
//	执行方式（单发/并发/开环到达率） × 轮次（单轮/多轮） × 思考模式（off/on，由 config.Thinking 展开），
//	stream 为请求级开关（config.stream）。
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	Register(funcScenario{"single", Single})
	Register(funcScenario{"multiturn", Multiturn})
	Register(funcScenario{"concurrent", Concurrent})
}

// env 承载一次场景执行的共享资源与横切能力。
type env struct {
	cfg      *config.Config
	client   *engine.Client
	srv      *smetrics.Scraper        // nil = 观测层关闭/不可用
	provider smetrics.MetricsProvider // 观测层指标命名（按服务端指标前缀自动识别）
	trace    *engine.TraceSet         // nil = filler 模式
}

func newEnv(ctx context.Context, cfg *config.Config, client *engine.Client) (*env, error) {
	e := &env{cfg: cfg, client: client}
	if cfg.ServerMetrics {
		s := smetrics.NewScraperAt(cfg.Endpoint, cfg.MetricsPath)
		// /metrics 常与业务接口同一套认证保护——认证格式与 chat 请求保持一致
		s.Auth = auth.Auth{Scheme: cfg.AuthScheme, Header: cfg.AuthHeader}
		s.APIKey = cfg.APIKey
		// 判定口径与 probe 一致：HTTP 200 但 0 项 vLLM 指标（网关占位响应）也算不可用，
		// 否则观测层会带着空指标集白跑，报告里出现假"可用"
		ok, detail := s.Available(ctx)
		if !ok {
			log.Printf("⚠️ server_metrics=true 但 /metrics 不可用（%v）——降级为纯客户端计时", detail)
		} else {
			sample, err := s.Scrape(ctx)
			if err != nil {
				log.Printf("⚠️ server_metrics=true 但 /metrics 抓取失败（%v）——降级为纯客户端计时", err)
			} else {
				name := smetrics.DetectProviderName(sample)
				if name == "" {
					// 自研引擎指标名不带 vllm:/sglang: 前缀——不静默套错命名，显式告知
					log.Printf("⚠️ 无法识别服务端指标命名（无 vllm:/sglang: 前缀，自研网关属预期）——按 vLLM 命名尝试，服务端指标大概率拿不到数")
					name = "vllm"
				}
				e.srv = s
				e.provider = smetrics.DetectProvider(sample)
				n := len(sample.Counters) + len(sample.Gauges) + len(sample.Hists)
				log.Printf("服务端观测层: %s 可用（%d 项指标，%s 命名）", cfg.MetricsPath, n, name)
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
	if e.srv != nil {
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
			log.Printf("    TTFT=%.0fms think=%.0fms decode=%.0fms tok/s=%.0f finish=%s",
				m.TTFT, m.ThinkMS, m.DecodeMS, m.TokensPerSec, m.FinishReason)
		} else {
			log.Printf("    E2E=%.0fms tok/s=%.0f（非流式，TTFT/思考拆分 N/A）", m.E2EMS, m.TokensPerSec)
		}
	}
	if e.srv != nil && before != nil {
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

func warmup(ctx context.Context, e *env, model string) {
	n := e.cfg.WarmupRequests
	if n <= 0 {
		return
	}
	vOff := config.ThinkingVariant{Name: "off", Enabled: false, ExtraBody: e.cfg.ThinkingFor(model).ExtraBodyOff}
	now := time.Now().UnixNano()
	for i := 0; i < n; i++ {
		msgs := []engine.Message{engine.UserMsg(warmupPromptTokens, now+int64(i), e.cfg.FillerLang)}
		e.client.Chat(ctx, engine.ChatOptions{
			Model: model, Messages: msgs, MaxTokens: warmupMaxTokens,
			Stream: e.cfg.StreamEnabled(), ExtraBody: vOff.ExtraBody,
		})
	}
	log.Printf("  预热 %d 条请求完成（不计入统计）", n)
}

// startWindow / finishWindow 场景窗口的服务端观测：开始快照+gauge 轮询 → 结束差值汇总。
func startWindow(ctx context.Context, e *env) (*smetrics.Sample, *smetrics.GaugePoller) {
	if e.srv == nil {
		return nil, nil
	}
	before, err := e.srv.Scrape(ctx)
	if err != nil {
		log.Printf("⚠️ %s 起始快照失败（%v）——本场景无服务端观测", e.cfg.MetricsPath, err)
		return nil, nil
	}
	poller := smetrics.StartGaugePoller(ctx, e.srv, time.Duration(e.cfg.MetricsIntervalMS)*time.Millisecond, e.provider)
	return before, poller
}

func finishWindow(ctx context.Context, e *env, before *smetrics.Sample, poller *smetrics.GaugePoller) *report.ServerMetricsSummary {
	if e.srv == nil || before == nil {
		if poller != nil {
			poller.Stop()
		}
		return nil
	}
	summary := &report.ServerMetricsSummary{Available: true}
	if poller != nil {
		summary.Gauges = poller.Summary() // Summary 内部会 Stop
		if h := poller.Health(); h.Degraded() {
			summary.ObservationDegraded = true
			summary.ObservationNote = fmt.Sprintf("gauge 轮询降级：成功 %d 次，连续失败 %d 次，最后错误 %s",
				h.Samples, h.ConsecutiveFailures, h.LastError)
		}
	}
	after, err := e.srv.Scrape(ctx)
	if err != nil {
		summary.Note = "结束快照抓取失败: " + err.Error()
		return summary
	}
	d := smetrics.DiffCounters(before, after, e.provider)
	summary.CacheHitTokens = d.PrefixCacheHitTokens
	summary.CacheQueryTokens = d.PrefixCacheQueryTokens
	summary.Preemptions = d.Preemptions
	summary.SpecDrafts = d.SpecDrafts
	summary.SpecAcceptedTokens = d.SpecAcceptedTokens
	summary.Hists = smetrics.HistDeltas(before, after, e.provider)
	return summary
}

// applySLO 把 goodput 配置挂到 Report（报告侧按 turn 级 TTFT/TPOT 计算达标率）。
func applySLO(e *env, rep *report.Report) {
	if e.cfg.Goodput != nil {
		rep.SLO = &report.SLO{TTFTMS: e.cfg.Goodput.TTFTMS, TPOTMS: e.cfg.Goodput.TPOTMS}
	}
}

// goodputOf 请求是否同时满足 SLO（TTFT 与 TPOT 双约束；非流式 TPOT 不可测视为不达标）。
func goodputOf(e *env, m *engine.TurnMetrics) bool {
	if e.cfg.Goodput == nil || m == nil || m.Error != "" {
		return false
	}
	if m.TTFT > e.cfg.Goodput.TTFTMS {
		return false
	}
	if m.TPOTMS <= 0 || m.TPOTMS > e.cfg.Goodput.TPOTMS {
		return false
	}
	return true
}

// runCorrectness 数字转写金丝雀：验证服务返回的是真实生成内容（结构 200 但内容异常能被揪出）。
func runCorrectness(ctx context.Context, e *env, model string) []report.CorrectnessRow {
	n := 0
	if e.cfg.Correctness != nil {
		n = e.cfg.Correctness.Samples
	}
	if n <= 0 {
		return nil
	}
	vOff := config.ThinkingVariant{Name: "off", Enabled: false, ExtraBody: e.cfg.ThinkingFor(model).ExtraBodyOff}
	rng := rand.New(rand.NewSource(4242)) // 固定种子：金丝雀可复现
	rows := []report.CorrectnessRow{}
	for i := 0; i < n; i++ {
		num := 10000 + rng.Intn(89999)
		var prompt string
		if e.cfg.FillerLang == "zh" {
			prompt = fmt.Sprintf("请只输出这个数字本身，不要任何其他内容：%d", num)
		} else {
			prompt = fmt.Sprintf("Reply with exactly this number and nothing else: %d", num)
		}
		m := runOne(ctx, e, model, []engine.Message{{Role: "user", Content: prompt}}, 16, vOff)
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
	log.Printf("  正确性抽查: %d/%d 通过", pass, len(rows))
	return rows
}

// runCorrectnessFor 金丝雀结果带上模型归属（数据按模型分区落盘时据此分桶）。
func runCorrectnessFor(ctx context.Context, e *env, model string) []report.CorrectnessRow {
	rows := runCorrectness(ctx, e, model)
	for i := range rows {
		rows[i].Model = model
	}
	return rows
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
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("单发单轮 runs=%d fixed_seed=%v stream=%v thinking=%s；思考开启时 max_tokens 下限 %d；数据源=%s%s",
			cfg.Single.Runs, cfg.Single.FixedSeed, cfg.StreamEnabled(), cfg.Thinking.Mode, cfg.Thinking.MaxTokensFloor, cfg.Dataset.Mode, thinkingNoteSuffix(cfg)),
	}
	applySLO(e, rep)
	before, poller := startWindow(ctx, e)
	defer func() { rep.Server = finishWindow(ctx, e, before, poller) }()

	for _, model := range filterModels(cfg.ActiveModels(), modelFilter) {
		_, mc := forModel(e, cfg, model) // 该模型生效配置（model_overrides 差异覆盖）
		th := mc.Thinking
		if len(th.Variants()) == 0 { // thinking 过滤后无匹配变体：整模型跳过（不发 warmup）
			log.Printf("  %s: 无匹配的思考变体，跳过", model)
			continue
		}
		warmup(ctx, e, model)
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
						log.Printf("🛑 收到中断信号——停止新请求，已完成数据全部保留")
						return rep, nil
					}
					if ctxAborted {
						break
					}
				}
			}
		}
		rep.Correctness = append(rep.Correctness, runCorrectnessFor(ctx, e, model)...)
	}
	return rep, nil
}

// ── Multiturn 单发多轮 ──

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
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("单发多轮 sessions=%d turns=%d stream=%v thinking=%s 数据源=%s%s（trace 模式下轮次来自回放会话，system/turn_tokens 不生效）%s",
			mt.Sessions, mt.Turns, cfg.StreamEnabled(), cfg.Thinking.Mode, dataSrc, ctxShape, thinkingNoteSuffix(cfg)),
	}
	applySLO(e, rep)
	before, poller := startWindow(ctx, e)
	defer func() { rep.Server = finishWindow(ctx, e, before, poller) }()

	for _, model := range filterModels(cfg.ActiveModels(), modelFilter) {
		_, mc := forModel(e, cfg, model) // 该模型生效配置（model_overrides 差异覆盖）
		mt := mc.Multiturn               // 遮蔽外层通用值（Note 仍描述通用基线；覆盖差异见 thinkingNoteSuffix）
		th := mc.Thinking
		if len(th.Variants()) == 0 { // thinking 过滤后无匹配变体：整模型跳过（不发 warmup）
			log.Printf("  %s: 无匹配的思考变体，跳过", model)
			continue
		}
		warmup(ctx, e, model)
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
					baseSeed := sessionSeed(s, mc.SeedSalt)
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
							tt := nextTurnTokens(mc, mt.TurnTokens, lastPrompt)
							if tt <= 0 {
								log.Printf("    已达 max_prompt_tokens=%d 截止，提前结束会话（%d/%d 轮）", mc.MaxPromptTokens, turn, mt.Turns)
								break
							}
							msgs = append(msgs, engine.UserMsg(tt, baseSeed+int64(turn), mc.FillerLang))
						}
						m := runOne(ctx, e, model, msgs, maxTok, v)
						if m.PromptTokens > 0 {
							if lastPrompt > 0 {
								m.NewTokens = m.PromptTokens - lastPrompt
							} else {
								m.NewTokens = m.PromptTokens
							}
							// 只有成功的轮次才推进基准：失败的轮次（ctx=0）不能把 lastPrompt 清零，
							// 否则下一轮会把整条 history 都算成"新增"，增量 prefill 指标错乱
							lastPrompt = m.PromptTokens
						}
						log.Printf("    turn%d (ctx≈%dtk +%dtk)", turn+1, m.PromptTokens, m.NewTokens)
						if mt.KeepAssistant && m.ReplyText != "" && !e.fullReplay() {
							// full 模式 history 来自 trace 原文，不追加生成回复（追加会与原始 assistant 重复）
							msgs = append(msgs, engine.Message{Role: "assistant", Content: engine.TruncateRunes(m.ReplyText, mt.MaxReplyChars)})
						}
						run.Turns = append(run.Turns, m)
						if interrupted(ctx) {
							log.Printf("    🛑 收到中断信号——提前结束会话（已完成 %d/%d 轮保留）", len(run.Turns), mt.Turns)
							break
						}
						if limit := ctxLimitHit(m); limit != "" {
							// 会话 history 已超限，继续加轮必然失败——结束该会话并跳过剩余会话
							log.Printf("    🛑 触发模型上下文上限（limit=%stk，ctx≈%dtk）——提前结束会话，跳过剩余会话", limit, m.PromptTokens)
							ctxAborted = true
							break
						}
					}
					rep.Multiturn = append(rep.Multiturn, run)
					if interrupted(ctx) {
						log.Printf("🛑 收到中断信号——停止新请求，已完成会话全部保留")
						return rep, nil
					}
					if ctxAborted {
						break
					}
				}
			}
		}
		rep.Correctness = append(rep.Correctness, runCorrectnessFor(ctx, e, model)...)
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

// collectSessionTurns 执行一次完整多轮会话并采集逐 turn 指标（并发多轮用：每个虚拟用户一次）。
// filler 模式为合成模拟对话（逐轮滚动 history）；trace 数据源才是真实会话重放。
// maxTok 由调用方传入（输出长度扫描维度，已含思考 floor 抬高）。
func collectSessionTurns(ctx context.Context, e *env, cfg *config.Config,
	model string, v config.ThinkingVariant, sessionIdx int, turnLimit int, maxTok int) []*engine.TurnMetrics {

	mt := cfg.Multiturn
	baseSeed := sessionSeed(sessionIdx, cfg.SeedSalt)
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
	}
	var out []*engine.TurnMetrics
	lastPrompt := 0
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
			tt := nextTurnTokens(cfg, mt.TurnTokens, lastPrompt)
			if tt <= 0 {
				log.Printf("    worker 会话已达 max_prompt_tokens=%d 截止，提前结束（%d/%d 轮）", cfg.MaxPromptTokens, turn, turns)
				break
			}
			msgs = append(msgs, engine.UserMsg(tt, baseSeed+int64(turn), cfg.FillerLang))
		}
		m := runOne(ctx, e, model, msgs, maxTok, v)
		if m.PromptTokens > 0 {
			if lastPrompt > 0 {
				m.NewTokens = m.PromptTokens - lastPrompt
			} else {
				m.NewTokens = m.PromptTokens
			}
			// 与 Multiturn 同语义：失败轮不推进基准
			lastPrompt = m.PromptTokens
		}
		out = append(out, m)
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
	if len(cc.Mix) > 0 {
		labels := make([]string, len(cc.Mix))
		for i, s := range cc.Mix {
			labels[i] = fmt.Sprintf("%s×%d", s.Label, s.Weight)
		}
		loadModel += " 混跑[" + strings.Join(labels, " ") + "]"
	}
	rep := &report.Report{
		Tool:        report.Version,
		Scenario:    "concurrent",
		GeneratedAt: time.Now(),
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("并发%s %s prompt≈%dtk stream=%v thinking=%s；每用户独立 prompt/会话（不同 seed）%s",
			mode, loadModel, cc.PromptTokens, cfg.StreamEnabled(), cfg.Thinking.Mode, thinkingNoteSuffix(cfg)),
	}
	applySLO(e, rep)
	before, poller := startWindow(ctx, e)
	defer func() { rep.Server = finishWindow(ctx, e, before, poller) }()

	for _, model := range filterModels(cfg.ActiveModels(), modelFilter) {
		_, mc := forModel(e, cfg, model) // 该模型生效配置（model_overrides 差异覆盖）
		cc := mc.Concurrent              // 遮蔽外层通用值：模型层可覆盖 levels/开环参数（Note 仍描述通用基线）
		rates := openRates(cc)
		th := mc.Thinking
		if len(th.Variants()) == 0 { // thinking 过滤后无匹配变体：整模型跳过（不发 warmup）
			log.Printf("  %s: 无匹配的思考变体，跳过", model)
			continue
		}
		warmup(ctx, e, model)
		for _, v := range th.Variants() {
			tiers := th.MaxTokensList(cc.MaxTokens, v)
			if len(cc.Mix) > 0 {
				tiers = []int{0} // 混跑：输出上限由各形状自带（floor 在 plan 内逐形状应用），不走外层扫描
			}
			for _, maxTok := range tiers {
				if rates != nil {
					for _, rate := range rates {
						lv := runOpenRound(ctx, e, mc, model, v, rate, maxTok)
						logConcurrent(&lv)
						rep.Concurrent = append(rep.Concurrent, lv)
						if interrupted(ctx) {
							log.Printf("🛑 收到中断信号——停止新请求，已完成档位全部保留")
							return rep, nil
						}
					}
					continue
				}
				for _, level := range cc.Levels {
					lv := runClosedRound(ctx, e, mc, model, v, level, maxTok)
					logConcurrent(&lv)
					rep.Concurrent = append(rep.Concurrent, lv)
					if interrupted(ctx) {
						log.Printf("🛑 收到中断信号——停止新请求，已完成档位全部保留")
						return rep, nil
					}
				}
			}
		}
		rep.Correctness = append(rep.Correctness, runCorrectnessFor(ctx, e, model)...)
	}
	return rep, nil
}

func logConcurrent(lv *report.ConcurrentLevel) {
	extra := ""
	if lv.RequestRate > 0 {
		extra = fmt.Sprintf(" rate=%.1f/s", lv.RequestRate)
	}
	outDesc := fmt.Sprintf("out=%dtk", lv.MaxTokens)
	if len(lv.Shapes) > 0 {
		outDesc = "mix"
	}
	log.Printf("[concurrent] %s thinking=%s %s level%d%s: wall=%.1fs throughput=%.0f tok/s%s",
		lv.Model, lv.Thinking, outDesc, lv.Level, extra, lv.WallSeconds, lv.ThroughputTPS, goodputLog(lv))
}

func goodputLog(lv *report.ConcurrentLevel) string {
	if lv.SLOTotal == 0 {
		return ""
	}
	return fmt.Sprintf(" goodput=%d/%d", lv.SLOMeet, lv.SLOTotal)
}

// mixPlan 混合负载（concurrent.mix，5.6）的每轮形状计划。
// 平滑加权轮转（nginx 同款算法）把权重展开成确定性交错序列：请求按发射序取模对号入座
// （闭环=发车序，开环=到达序），不引入随机——同配置重跑形状分布一致，run 间可复现。
type mixPlan struct {
	shapes  []config.MixShape
	maxToks []int // 每形状已过思考 floor 的输出上限
	seq     []int // 展开的形状下标序列（长度 = 权重之和）
}

func newMixPlan(cfg *config.Config, model string, v config.ThinkingVariant) *mixPlan {
	shapes := cfg.Concurrent.Mix
	if len(shapes) == 0 {
		return nil
	}
	total := 0
	for _, s := range shapes {
		total += s.Weight
	}
	maxToks := make([]int, len(shapes))
	for i, s := range shapes {
		maxToks[i] = cfg.ThinkingFor(model).MaxTokens(s.MaxTokens, v)
	}
	cur := make([]int, len(shapes))
	seq := make([]int, 0, total)
	for j := 0; j < total; j++ {
		best, bestVal := 0, -1
		for i := range shapes {
			cur[i] += shapes[i].Weight
			if cur[i] > bestVal {
				best, bestVal = i, cur[i]
			}
		}
		cur[best] -= total
		seq = append(seq, best)
	}
	return &mixPlan{shapes: shapes, maxToks: maxToks, seq: seq}
}

// at 返回第 i 个发射请求的形状与输出上限（已含 floor）。
func (p *mixPlan) at(i int) (config.MixShape, int) {
	k := p.seq[i%len(p.seq)]
	return p.shapes[k], p.maxToks[k]
}

// aggregateShapes 把本轮请求按形状聚合出中位数统计（idxs 与 Requests 一一对应，混跑时记录形状下标）。
func aggregateShapes(mp *mixPlan, reqs []*engine.TurnMetrics, idxs []int) []report.ShapeStat {
	if mp == nil {
		return nil
	}
	by := map[int][]*engine.TurnMetrics{}
	for i, m := range reqs {
		if i < len(idxs) && idxs[i] >= 0 {
			by[idxs[i]] = append(by[idxs[i]], m)
		}
	}
	out := make([]report.ShapeStat, 0, len(mp.shapes))
	for i, sh := range mp.shapes {
		ms := by[i]
		if len(ms) == 0 {
			continue
		}
		med := func(get func(*engine.TurnMetrics) float64) float64 {
			vals := make([]float64, 0, len(ms))
			for _, m := range ms {
				vals = append(vals, get(m))
			}
			sort.Float64s(vals)
			// 偶数样本取两中值平均（与报告侧 Python st.median 口径一致，
			// 否则小样本混跑下形状中位系统性偏高半步）
			n := len(vals)
			if n%2 == 0 {
				return (vals[n/2-1] + vals[n/2]) / 2
			}
			return vals[n/2]
		}
		out = append(out, report.ShapeStat{
			Label:        sh.Label,
			Weight:       sh.Weight,
			PromptTokens: sh.PromptTokens,
			MaxTokens:    mp.maxToks[i],
			Count:        len(ms),
			TTFTS:        med(func(m *engine.TurnMetrics) float64 { return m.TTFT / 1000 }),
			E2ES:         med(func(m *engine.TurnMetrics) float64 { return m.E2EMS / 1000 }),
			TokPS:        med(func(m *engine.TurnMetrics) float64 { return m.TokensPerSec }),
		})
	}
	return out
}

// runClosedRound 闭环并发档位：level 个 worker 同时发车，各自跑 runs_per_worker 次请求（或一次完整会话）。
// maxTok 输出长度由调用方传入（输出长度扫描维度，已含思考 floor 抬高）。
func runClosedRound(ctx context.Context, e *env, cfg *config.Config,
	model string, v config.ThinkingVariant, level int, maxTok int) report.ConcurrentLevel {

	cc := cfg.Concurrent
	if cc.Multiturn {
		e.warnTraceWrap(level)
	}
	lv := &report.ConcurrentLevel{Model: model, Thinking: v.Name, MaxTokens: maxTok, Level: level}
	mp := newMixPlan(cfg, model, v)
	if mp != nil {
		lv.MaxTokens = 0 // 混跑：输出上限由各形状自带（Shapes 内逐形状记录）
	}
	var shapeIdxs []int // 与 Requests 一一对应的形状下标（-1 = 非混跑）
	start := time.Now()
	var mu sync.Mutex
	var wg sync.WaitGroup
	startBarrier := make(chan struct{})
	var reqSeq atomic.Int64
	for w := 0; w < level; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-startBarrier // 所有 worker 就绪后同时发车
			if cc.Multiturn {
				s := report.MultiturnRun{Model: model, Thinking: v.Name, Session: workerID + 1, MaxTokens: maxTok}
				s.Turns = collectSessionTurns(ctx, e, cfg, model, v, workerID, 0, maxTok)
				mu.Lock()
				lv.Sessions = append(lv.Sessions, s)
				mu.Unlock()
				return
			}
			for r := 0; r < cc.RunsPerWorker; r++ {
				promptTokens := cfg.ClampOne(cc.PromptTokens)
				reqMaxTok := maxTok
				si := -1
				if mp != nil {
					seqNo := int(reqSeq.Add(1)) - 1
					sh, mt := mp.at(seqNo)
					promptTokens = cfg.ClampOne(sh.PromptTokens)
					reqMaxTok = mt
					si = mp.seq[seqNo%len(mp.seq)]
				}
				seed := workerSeed(workerID, r, cfg.SeedSalt) // 每用户不同 prompt
				msgs := []engine.Message{engine.UserMsg(promptTokens, seed, cfg.FillerLang)}
				m := runOne(ctx, e, model, msgs, reqMaxTok, v)
				mu.Lock()
				lv.Requests = append(lv.Requests, m)
				shapeIdxs = append(shapeIdxs, si)
				mu.Unlock()
			}
		}(w)
	}
	close(startBarrier)
	wg.Wait()
	lv.Shapes = aggregateShapes(mp, lv.Requests, shapeIdxs)
	finalizeLevel(e, lv, time.Since(start).Seconds())
	return *lv
}

// runOpenRound 开环到达率：请求按 Poisson 过程到达（对齐 vLLM bench serve），测排队-延迟曲线。
// maxTok 输出长度由调用方传入（输出长度扫描维度，已含思考 floor 抬高）。
func runOpenRound(ctx context.Context, e *env, cfg *config.Config,
	model string, v config.ThinkingVariant, rate float64, maxTok int) report.ConcurrentLevel {

	cc := cfg.Concurrent
	lv := &report.ConcurrentLevel{Model: model, Thinking: v.Name, MaxTokens: maxTok, Level: 0, RequestRate: rate}
	mp := newMixPlan(cfg, model, v)
	if mp != nil {
		lv.MaxTokens = 0
	}
	var shapeIdxs []int
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
				sem <- struct{}{}
				defer func() { <-sem }()
			}
			if cc.Multiturn {
				s := report.MultiturnRun{Model: model, Thinking: v.Name, Session: i + 1, MaxTokens: maxTok}
				s.Turns = collectSessionTurns(ctx, e, cfg, model, v, i, 0, maxTok)
				mu.Lock()
				lv.Sessions = append(lv.Sessions, s)
				mu.Unlock()
				return
			}
			promptTokens := cfg.ClampOne(cc.PromptTokens)
			reqMaxTok := maxTok
			si := -1
			if mp != nil {
				sh, mt := mp.at(i)
				promptTokens = cfg.ClampOne(sh.PromptTokens)
				reqMaxTok = mt
				si = mp.seq[i%len(mp.seq)]
			}
			seed := openWorkerSeed(i, cfg.SeedSalt)
			msgs := []engine.Message{engine.UserMsg(promptTokens, seed, cfg.FillerLang)}
			m := runOne(ctx, e, model, msgs, reqMaxTok, v)
			mu.Lock()
			lv.Requests = append(lv.Requests, m)
			shapeIdxs = append(shapeIdxs, si)
			mu.Unlock()
		}(i)
	}
	schedDone := make(chan struct{})
	go func() { // Poisson 调度：指数分布到达间隔
		defer close(schedDone)
		for i := 0; i < n; i++ {
			if i > 0 {
				time.Sleep(time.Duration(rng.ExpFloat64() / rate * float64(time.Second)))
			}
			launch(i)
		}
	}()
	<-schedDone // 全部请求已按到达序列发射
	wg.Wait()
	lv.Shapes = aggregateShapes(mp, lv.Requests, shapeIdxs)
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
			throughput += float64(m.CompletionTokens)
			if e.cfg.Goodput != nil {
				lv.SLOTotal++
				if goodputOf(e, m) {
					meet++
					meetTokens += float64(m.CompletionTokens)
				}
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

// sessionSeed 多轮会话：不同会话（含并发多轮的不同虚拟用户）内容互异。
func sessionSeed(sessionIdx, salt int) int64 { return int64(5000 + sessionIdx*10000 + salt) }

// workerSeed 闭环并发单轮：不同 worker / 不同 run 内容互异。
func workerSeed(workerIdx, run, salt int) int64 { return int64(90000 + workerIdx*100 + run + salt) }

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
