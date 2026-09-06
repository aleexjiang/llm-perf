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
	"strconv"
	"strings"
	"sync"
	"time"

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

var (
	scenarioRegistry = map[string]Scenario{}
	scenarioOrder    []string
)

// Register 注册场景（init 期调用，无并发）。
func Register(s Scenario) {
	scenarioRegistry[s.Name()] = s
	scenarioOrder = append(scenarioOrder, s.Name())
}

// Lookup 按名字查场景。
func Lookup(name string) (Scenario, bool) {
	s, ok := scenarioRegistry[name]
	return s, ok
}

// All 返回注册顺序的场景列表（bench all 的执行顺序）。
func All() []Scenario {
	out := make([]Scenario, 0, len(scenarioOrder))
	for _, n := range scenarioOrder {
		out = append(out, scenarioRegistry[n])
	}
	return out
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
		s := smetrics.NewScraper(cfg.Endpoint)
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
				e.srv = s
				e.provider = smetrics.DetectProvider(sample)
				n := len(sample.Counters) + len(sample.Gauges) + len(sample.Hists)
				log.Printf("服务端观测层: /metrics 可用（%d 项指标，%s 命名）", n, e.provider.Name())
			}
		}
	}
	if cfg.Dataset.Mode == "trace" {
		ts, err := engine.LoadTrace(cfg.Dataset.Path, cfg.Dataset.Format, cfg.Dataset.MinTurns, cfg.Dataset.MaxSessions)
		if err != nil {
			return nil, err
		}
		e.trace = ts
		log.Printf("trace 回放: %s（%s 格式，%d 个会话）", ts.Source, ts.Format, len(ts.Sessions))
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

// warmup 场景开始前的预热：暖连接池/首包路径；唯一内容（时间戳 seed）避免污染被测前缀的缓存对照。
func warmup(ctx context.Context, e *env, model string) {
	n := e.cfg.WarmupRequests
	if n <= 0 {
		return
	}
	vOff := config.ThinkingVariant{Name: "off", Enabled: false, ExtraBody: e.cfg.Thinking.ExtraBodyOff}
	now := time.Now().UnixNano()
	for i := 0; i < n; i++ {
		msgs := []engine.Message{engine.UserMsg(128, now+int64(i), e.cfg.Fillers())}
		e.client.Chat(ctx, engine.ChatOptions{
			Model: model, Messages: msgs, MaxTokens: 1,
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
		log.Printf("⚠️ /metrics 起始快照失败（%v）——本场景无服务端观测", err)
		return nil, nil
	}
	poller := smetrics.StartGaugePoller(ctx, e.cfg.Endpoint, time.Duration(e.cfg.MetricsIntervalMS)*time.Millisecond, e.provider)
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
	vOff := config.ThinkingVariant{Name: "off", Enabled: false, ExtraBody: e.cfg.Thinking.ExtraBodyOff}
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
		reply := m.ReplyText
		if len(reply) > 200 {
			reply = reply[:200]
		}
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
		Note: fmt.Sprintf("单发单轮 runs=%d fixed_seed=%v stream=%v thinking=%s；思考开启时 max_tokens 下限 %d；数据源=%s",
			cfg.Single.Runs, cfg.Single.FixedSeed, cfg.StreamEnabled(), cfg.Thinking.Mode, cfg.Thinking.MaxTokensFloor, cfg.Dataset.Mode),
	}
	applySLO(e, rep)
	before, poller := startWindow(ctx, e)
	defer func() { rep.Server = finishWindow(ctx, e, before, poller) }()

	for _, model := range filterModels(cfg.Models, modelFilter) {
		warmup(ctx, e, model)
		for _, v := range cfg.Thinking.Variants() {
			maxTok := cfg.Thinking.MaxTokens(cfg.Single.MaxTokens, v)
			if e.trace != nil {
				// trace 模式：用回放会话的首轮 user 消息做档位（prompt_tokens 粗估，服务端 usage 为准）
				n := len(e.trace.Sessions)
				if n > 16 {
					n = 16
				}
				for i := 0; i < n; i++ {
					content := e.trace.Pick(i).UserTurns[0]
					row := report.SingleRow{Model: model, Thinking: v.Name, PromptTokens: len(content) / 4}
					log.Printf("[single] %s thinking=%s trace#%d (~%dtk)", model, v.Name, i+1, row.PromptTokens)
					for run := 0; run < cfg.Single.Runs; run++ {
						msgs := []engine.Message{{Role: "user", Content: content}}
						row.Runs = append(row.Runs, runOne(ctx, e, model, msgs, maxTok, v))
					}
					rep.Single = append(rep.Single, row)
				}
				continue
			}
			ladder, clamped := cfg.ClampLadder(cfg.Single.PromptTokens)
			if clamped {
				log.Printf("[single] 档位已按 max_prompt_tokens=%d 截断: %v", cfg.MaxPromptTokens, ladder)
			}
			for _, tokens := range ladder {
				row := report.SingleRow{Model: model, Thinking: v.Name, PromptTokens: tokens}
				for run := 0; run < cfg.Single.Runs; run++ {
					seed := singleSeed(cfg.Single.FixedSeed, tokens, run, cfg.SeedSalt)
					msgs := []engine.Message{engine.UserMsg(tokens, seed, cfg.Fillers())}
					log.Printf("[single] %s thinking=%s %dtk run%d", model, v.Name, tokens, run+1)
					m := runOne(ctx, e, model, msgs, maxTok, v)
					row.Runs = append(row.Runs, m)
					if limit := ctxLimitHit(m); limit != "" {
						log.Printf("    🛑 触发模型上下文上限（limit=%stk）——跳过 %dtk 剩余 run 及更大档位（重试必然同样超限）", limit, tokens)
						break
					}
				}
				rep.Single = append(rep.Single, row)
				if len(row.Runs) > 0 && ctxLimitHit(row.Runs[len(row.Runs)-1]) != "" {
					break // 更大档位必然同样超限，跳过该模型该变体的剩余档位
				}
			}
		}
		rep.Correctness = append(rep.Correctness, runCorrectness(ctx, e, model)...)
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

func Multiturn(ctx context.Context, cfg *config.Config, client *engine.Client, modelFilter string) (*report.Report, error) {
	e, err := newEnv(ctx, cfg, client)
	if err != nil {
		return nil, err
	}
	mt := cfg.Multiturn
	dataSrc := "filler"
	if e.trace != nil {
		dataSrc = "trace:" + e.trace.Source
	}
	rep := &report.Report{
		Tool:        report.Version,
		Scenario:    "multiturn",
		GeneratedAt: time.Now(),
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("单发多轮 sessions=%d turns=%d stream=%v thinking=%s 数据源=%s（trace 模式下轮次来自回放会话，system/turn_tokens 不生效）",
			mt.Sessions, mt.Turns, cfg.StreamEnabled(), cfg.Thinking.Mode, dataSrc),
	}
	applySLO(e, rep)
	before, poller := startWindow(ctx, e)
	defer func() { rep.Server = finishWindow(ctx, e, before, poller) }()

	for _, model := range filterModels(cfg.Models, modelFilter) {
		warmup(ctx, e, model)
		e.warnTraceWrap(mt.Sessions)
		for _, v := range cfg.Thinking.Variants() {
			ctxAborted := false // 触发模型上下文上限：剩余会话必然同样超限，全部跳过
			for s := 0; s < mt.Sessions; s++ {
				run := report.MultiturnRun{Model: model, Thinking: v.Name, Session: s + 1}
				log.Printf("[multiturn] %s thinking=%s session%d", model, v.Name, s+1)
				baseSeed := sessionSeed(s, cfg.SeedSalt)
				msgs := []engine.Message{}
				if e.trace == nil {
					if sys := engine.SystemMsg(mt.SystemTokens, mt.ToolDefsTokens, baseSeed, cfg.Fillers()); sys.Content != "" {
						msgs = append(msgs, sys)
					}
				}
				userTurns := e.sessionUserTurns(s)
				maxTok := cfg.Thinking.MaxTokens(mt.MaxTokens, v)
				lastPrompt := 0 // 上一轮服务端实测 prompt_tokens（截止计算与新增 tokens 计算）
				for turn := 0; turn < mt.Turns; turn++ {
					if e.trace != nil {
						if turn >= len(userTurns) {
							log.Printf("    回放会话只有 %d 轮 user 消息，提前结束", len(userTurns))
							break
						}
						msgs = append(msgs, engine.Message{Role: "user", Content: userTurns[turn]})
					} else {
						tt := nextTurnTokens(cfg, mt.TurnTokens, lastPrompt)
						if tt <= 0 {
							log.Printf("    已达 max_prompt_tokens=%d 截止，提前结束会话（%d/%d 轮）", cfg.MaxPromptTokens, turn, mt.Turns)
							break
						}
						msgs = append(msgs, engine.UserMsg(tt, baseSeed+int64(turn), cfg.Fillers()))
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
					if mt.KeepAssistant && m.ReplyText != "" {
						reply := m.ReplyText
						if len(reply) > 2000 {
							reply = reply[:2000]
						}
						msgs = append(msgs, engine.Message{Role: "assistant", Content: reply})
					}
					run.Turns = append(run.Turns, m)
					if limit := ctxLimitHit(m); limit != "" {
						// 会话 history 已超限，继续加轮必然失败——结束该会话并跳过剩余会话
						log.Printf("    🛑 触发模型上下文上限（limit=%stk，ctx≈%dtk）——提前结束会话，跳过剩余会话", limit, m.PromptTokens)
						ctxAborted = true
						break
					}
				}
				rep.Multiturn = append(rep.Multiturn, run)
				if ctxAborted {
					break
				}
			}
		}
		rep.Correctness = append(rep.Correctness, runCorrectness(ctx, e, model)...)
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

// collectSessionTurns 执行一次完整会话重放并采集逐 turn 指标（并发多轮用：每个虚拟用户一次）。
func collectSessionTurns(ctx context.Context, e *env, cfg *config.Config,
	model string, v config.ThinkingVariant, sessionIdx int, turnLimit int) []*engine.TurnMetrics {

	mt := cfg.Multiturn
	maxTok := cfg.Thinking.MaxTokens(mt.MaxTokens, v)
	baseSeed := sessionSeed(sessionIdx, cfg.SeedSalt)
	msgs := []engine.Message{}
	userTurns := e.sessionUserTurns(sessionIdx)
	if e.trace == nil {
		if sys := engine.SystemMsg(mt.SystemTokens, mt.ToolDefsTokens, baseSeed, cfg.Fillers()); sys.Content != "" {
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
		if userTurns != nil {
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
			msgs = append(msgs, engine.UserMsg(tt, baseSeed+int64(turn), cfg.Fillers()))
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
		if mt.KeepAssistant && m.ReplyText != "" {
			reply := m.ReplyText
			if len(reply) > 2000 {
				reply = reply[:2000]
			}
			msgs = append(msgs, engine.Message{Role: "assistant", Content: reply})
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
		mode = "多轮会话重放"
	}
	loadModel := "闭环并发"
	if rates != nil {
		loadModel = fmt.Sprintf("开环到达率 %v req/s", rates)
	}
	rep := &report.Report{
		Tool:        report.Version,
		Scenario:    "concurrent",
		GeneratedAt: time.Now(),
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("并发%s %s prompt≈%dtk stream=%v thinking=%s；每用户独立 prompt/会话（不同 seed）",
			mode, loadModel, cc.PromptTokens, cfg.StreamEnabled(), cfg.Thinking.Mode),
	}
	applySLO(e, rep)
	before, poller := startWindow(ctx, e)
	defer func() { rep.Server = finishWindow(ctx, e, before, poller) }()

	for _, model := range filterModels(cfg.Models, modelFilter) {
		warmup(ctx, e, model)
		for _, v := range cfg.Thinking.Variants() {
			if rates != nil {
				for _, rate := range rates {
					lv := runOpenRound(ctx, e, cfg, model, v, rate)
					logConcurrent(&lv)
					rep.Concurrent = append(rep.Concurrent, lv)
				}
				continue
			}
			for _, level := range cc.Levels {
				lv := runClosedRound(ctx, e, cfg, model, v, level)
				logConcurrent(&lv)
				rep.Concurrent = append(rep.Concurrent, lv)
			}
		}
		rep.Correctness = append(rep.Correctness, runCorrectness(ctx, e, model)...)
	}
	return rep, nil
}

func logConcurrent(lv *report.ConcurrentLevel) {
	extra := ""
	if lv.RequestRate > 0 {
		extra = fmt.Sprintf(" rate=%.1f/s", lv.RequestRate)
	}
	log.Printf("[concurrent] %s thinking=%s level%d%s: wall=%.1fs throughput=%.0f tok/s%s",
		lv.Model, lv.Thinking, lv.Level, extra, lv.WallSeconds, lv.ThroughputTPS, goodputLog(lv))
}

func goodputLog(lv *report.ConcurrentLevel) string {
	if lv.SLOTotal == 0 {
		return ""
	}
	return fmt.Sprintf(" goodput=%d/%d", lv.SLOMeet, lv.SLOTotal)
}

// runClosedRound 闭环并发档位：level 个 worker 同时发车，各自跑 runs_per_worker 次请求（或一次完整会话）。
func runClosedRound(ctx context.Context, e *env, cfg *config.Config,
	model string, v config.ThinkingVariant, level int) report.ConcurrentLevel {

	cc := cfg.Concurrent
	if cc.Multiturn {
		e.warnTraceWrap(level)
	}
	lv := &report.ConcurrentLevel{Model: model, Thinking: v.Name, Level: level}
	start := time.Now()
	var mu sync.Mutex
	var wg sync.WaitGroup
	startBarrier := make(chan struct{})
	for w := 0; w < level; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			<-startBarrier // 所有 worker 就绪后同时发车
			if cc.Multiturn {
				s := report.MultiturnRun{Model: model, Thinking: v.Name, Session: workerID + 1}
				s.Turns = collectSessionTurns(ctx, e, cfg, model, v, workerID, 0)
				mu.Lock()
				lv.Sessions = append(lv.Sessions, s)
				mu.Unlock()
				return
			}
			maxTok := cfg.Thinking.MaxTokens(cc.MaxTokens, v)
			promptTokens := cfg.ClampOne(cc.PromptTokens)
			for r := 0; r < cc.RunsPerWorker; r++ {
				seed := workerSeed(workerID, r, cfg.SeedSalt) // 每用户不同 prompt
				msgs := []engine.Message{engine.UserMsg(promptTokens, seed, cfg.Fillers())}
				m := runOne(ctx, e, model, msgs, maxTok, v)
				mu.Lock()
				lv.Requests = append(lv.Requests, m)
				mu.Unlock()
			}
		}(w)
	}
	close(startBarrier)
	wg.Wait()
	finalizeLevel(e, lv, time.Since(start).Seconds())
	return *lv
}

// runOpenRound 开环到达率：请求按 Poisson 过程到达（对齐 vLLM bench serve），测排队-延迟曲线。
func runOpenRound(ctx context.Context, e *env, cfg *config.Config,
	model string, v config.ThinkingVariant, rate float64) report.ConcurrentLevel {

	cc := cfg.Concurrent
	lv := &report.ConcurrentLevel{Model: model, Thinking: v.Name, Level: 0, RequestRate: rate}
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
	maxTok := cfg.Thinking.MaxTokens(cc.MaxTokens, v)
	launch := func(i int) {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if sem != nil {
				sem <- struct{}{}
				defer func() { <-sem }()
			}
			if cc.Multiturn {
				s := report.MultiturnRun{Model: model, Thinking: v.Name, Session: i + 1}
				s.Turns = collectSessionTurns(ctx, e, cfg, model, v, i, 0)
				mu.Lock()
				lv.Sessions = append(lv.Sessions, s)
				mu.Unlock()
				return
			}
			seed := openWorkerSeed(i, cfg.SeedSalt)
			msgs := []engine.Message{engine.UserMsg(cfg.ClampOne(cc.PromptTokens), seed, cfg.Fillers())}
			m := runOne(ctx, e, model, msgs, maxTok, v)
			mu.Lock()
			lv.Requests = append(lv.Requests, m)
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

// ── 种子派生（表驱动测试锁定语义：盐值隔离战役、fixed_seed 档内复用/档间独立、worker 间互异） ──

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
