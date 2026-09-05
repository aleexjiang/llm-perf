// Package scenario 实现评测场景矩阵：
//   执行方式（单发/并发/开环到达率） × 轮次（单轮/多轮） × 思考模式（off/on，由 config.Thinking 展开），
//   stream 为请求级开关（config.stream）。
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
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/engine"
	"github.com/aleexjiang/llm-perf/internal/report"
	"github.com/aleexjiang/llm-perf/internal/smetrics"
)

// env 承载一次场景执行的共享资源与横切能力。
type env struct {
	cfg    *config.Config
	client *engine.Client
	srv    *smetrics.Scraper // nil = 观测层关闭/不可用
	trace  *engine.TraceSet  // nil = filler 模式
}

func newEnv(ctx context.Context, cfg *config.Config, client *engine.Client) (*env, error) {
	e := &env{cfg: cfg, client: client}
	if cfg.ServerMetrics {
		s := smetrics.NewScraper(cfg.Endpoint)
		if ok, detail := s.Available(ctx); ok {
			e.srv = s
			log.Printf("服务端观测层: /metrics 可用（%s）", detail)
		} else {
			log.Printf("⚠️ server_metrics=true 但 /metrics 不可达（%s）——降级为纯客户端计时", detail)
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

// runOne 发起一次请求（流式/非流式、思考变体由 opts 决定），并做 /metrics counter 前后差值。
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
			m.SrvDelta = smetrics.DiffCounters(before, after)
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
	poller := smetrics.StartGaugePoller(ctx, e.cfg.Endpoint, time.Duration(e.cfg.MetricsIntervalMS)*time.Millisecond)
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
	}
	after, err := e.srv.Scrape(ctx)
	if err != nil {
		summary.Note = "结束快照抓取失败: " + err.Error()
		return summary
	}
	d := smetrics.DiffCounters(before, after)
	summary.CacheHitTokens = d.PrefixCacheHitTokens
	summary.CacheQueryTokens = d.PrefixCacheQueryTokens
	summary.Preemptions = d.Preemptions
	summary.SpecDrafts = d.SpecDrafts
	summary.SpecAcceptedTokens = d.SpecAcceptedTokens
	summary.Hists = smetrics.HistDeltas(before, after)
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
					var seed int64
					// fixed_seed：每档一个独立种子（档位内各 run 复用同一 prompt 测缓存对照）。
					// 不要让不同档位共享种子——语料窗口同起点会使档位间 prompt 互为嵌套前缀，
					// 上一档的缓存会"预热"下一档的 run1，冷启动测量就不干净了。
					if cfg.Single.FixedSeed {
						seed = int64(1000 + tokens + cfg.SeedSalt)
					} else {
						seed = int64(tokens*100 + run + cfg.SeedSalt)
					}
					msgs := []engine.Message{engine.UserMsg(tokens, seed, cfg.Fillers())}
					log.Printf("[single] %s thinking=%s %dtk run%d", model, v.Name, tokens, run+1)
					row.Runs = append(row.Runs, runOne(ctx, e, model, msgs, maxTok, v))
				}
				rep.Single = append(rep.Single, row)
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
		for _, v := range cfg.Thinking.Variants() {
			for s := 0; s < mt.Sessions; s++ {
				run := report.MultiturnRun{Model: model, Thinking: v.Name, Session: s + 1}
				log.Printf("[multiturn] %s thinking=%s session%d", model, v.Name, s+1)
				baseSeed := int64(5000 + s*10000 + cfg.SeedSalt)
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
					}
					lastPrompt = m.PromptTokens
					log.Printf("    turn%d (ctx≈%dtk +%dtk)", turn+1, m.PromptTokens, m.NewTokens)
					if mt.KeepAssistant && m.ReplyText != "" {
						reply := m.ReplyText
						if len(reply) > 2000 {
							reply = reply[:2000]
						}
						msgs = append(msgs, engine.Message{Role: "assistant", Content: reply})
					}
					run.Turns = append(run.Turns, m)
				}
				rep.Multiturn = append(rep.Multiturn, run)
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
	baseSeed := int64(5000 + sessionIdx*10000 + cfg.SeedSalt)
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
		}
		lastPrompt = m.PromptTokens
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
				seed := int64(90000 + workerID*100 + r + cfg.SeedSalt) // 每用户不同 prompt
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
	start := time.Now()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var sem chan struct{}
	if cc.MaxConcurrency > 0 {
		sem = make(chan struct{}, cc.MaxConcurrency)
	}
	// 固定种子：同一 rate 重复跑到达序列一致（可复现）
	rng := rand.New(rand.NewSource(int64(rate * 1000)))
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
			seed := int64(90000 + i + cfg.SeedSalt)
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
