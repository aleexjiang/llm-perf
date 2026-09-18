// requests.go：rps / concurrency 两种冻结请求快照模式的公共执行核心。
//
// 两者的差别只在调度器（谁控制节奏），请求源与执行路径完全一致：
//   - rps：到达率控制（Poisson/burstiness），可选在飞上限——测排队-延迟曲线；
//   - concurrency：在飞上限控制（request_rate=inf 时齐射）——测总吞吐拐点，对齐 vLLM bench serve。
//
// 数据契约：输出复用 report.ConcurrentLevel（Level/RequestRate/Requests/...），
// scenario 字段区分 "rps" / "concurrency"。
package scenario

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/engine"
	"github.com/aleexjiang/llm-perf/internal/report"
)

// loadRequestSamples 加载冻结请求集（rps/concurrency 共用）。
func loadRequestSamples(cfg *config.Config) ([]engine.RequestSample, error) {
	rs := cfg.RequestSet
	if rs.ShareGPTPath == "" {
		return nil, fmt.Errorf("需要 request_set.sharegpt_path（冻结请求快照数据源）")
	}
	n := rs.NumPrompts
	if n <= 0 {
		n = 100
	}
	samples, err := engine.LoadShareGPTRequests(rs.ShareGPTPath, n, rs.Seed, rs.MaxOutputTokens)
	if err != nil {
		return nil, err
	}
	log.Printf("请求集: %d 条冻结快照（source=%s seed=%d）", len(samples), rs.ShareGPTPath, rs.Seed)
	return samples, nil
}

// requestRunner 单条请求执行（冻结快照：messages 不演化，直接发给被测模型）。
func requestRunner(ctx context.Context, e *env, model string, v config.ThinkingVariant,
	maxTok int, sample engine.RequestSample) *engine.TurnMetrics {

	m, err := e.client.Chat(ctx, engine.ChatOptions{
		Model:     model,
		Messages:  sample.Messages,
		MaxTokens: maxTok,
		Stream:    e.cfg.StreamEnabled(),
		Thinking:  v.Enabled,
		ExtraBody: v.ExtraBody,
	})
	if m == nil {
		m = &engine.TurnMetrics{
			Model: model, Stream: e.cfg.StreamEnabled(), Thinking: v.Enabled, Phase: "benchmark",
			Error: "request returned no metrics", EndAt: time.Now(),
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
		log.Printf("    失败[%s]: %v", sample.ID, err)
	}
	return m
}

// finishLevel 汇总一个档位的吞吐/成败计数。
func finishLevel(lv *report.ConcurrentLevel) {
	wall := lv.WallSeconds
	completed, failed, cancelled, tokens := 0, 0, 0, 0.0
	for _, m := range lv.Requests {
		switch {
		case m.Cancelled:
			cancelled++
		case m.Error != "":
			failed++
		default:
			completed++
			tokens += float64(m.CompletionTokens)
		}
	}
	lv.CompletedRequests = completed
	lv.FailedRequests = failed
	lv.CancelledRequests = cancelled
	if wall > 0 {
		lv.ThroughputTPS = tokens / wall
	}
}

// newEnvSilent 构造不带 trace 的场景 env（请求快照模式不用 dataset/filler）。
func newEnvSilent(ctx context.Context, cfg *config.Config, client *engine.Client) (*env, error) {
	e := &env{cfg: cfg, client: client, perReqSrv: false}
	if err := setupServerMetrics(ctx, e, cfg); err != nil {
		return nil, err
	}
	return e, nil
}

func init() {
	Register(funcScenario{"rps", RPSScenario})
	Register(funcScenario{"concurrency", ConcurrencyScenario})
}

// RPSScenario rps 模式：冻结请求快照的开环到达（到达率控制节奏）。
func RPSScenario(ctx context.Context, cfg *config.Config, client *engine.Client, modelFilter string) (*report.Report, error) {
	samples, err := loadRequestSamples(cfg)
	if err != nil {
		return nil, err
	}
	if len(cfg.RPS.Rates) == 0 {
		return nil, fmt.Errorf("rps 模式需要 rps.rates（到达率档位，req/s）")
	}
	e, err := newEnvSilent(ctx, cfg, client)
	if err != nil {
		return nil, err
	}
	rep := &report.Report{
		Tool:        report.Version,
		Scenario:    "rps",
		GeneratedAt: time.Now(),
		Test:        cfg.TestKind(),
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("冻结请求快照开环到达 rates=%v req/s max_concurrency=%d burstiness=%.2f num_prompts=%d stream=%v thinking=%s（口径对齐 vLLM bench serve）%s",
			cfg.RPS.Rates, cfg.RPS.MaxConcurrency, cfg.RPS.GetBurstiness(), len(samples),
			cfg.StreamEnabled(), cfg.Thinking.Mode, thinkingNoteSuffix(cfg)),
	}
	applySLO(e, rep)
	attachKVCapacity(e, rep)
	before, poller, winStart := startWindow(ctx, e)
	defer func() { rep.Server = finishWindow(e, before, poller, winStart) }()

	for _, model := range cfg.ActiveModels() {
		if modelFilter != "" && !strings.Contains(model, modelFilter) {
			continue
		}
		em, mc := forModel(e, cfg, model)
		th := mc.Thinking
		if len(th.Variants()) == 0 {
			log.Printf("  %s: 无匹配的思考变体，跳过", model)
			continue
		}
		for _, v := range th.Variants() {
			aborted := false
			for _, rate := range cfg.RPS.Rates {
				if aborted || interrupted(ctx) {
					break
				}
				lv := runRequestArrival(ctx, em, model, v, samples, rate,
					cfg.RPS.MaxConcurrency, cfg.RPS.GetBurstiness())
				log.Printf("[rps] %s thinking=%s rate=%.1f/s: wall=%.1fs throughput=%.0f tok/s ok=%d fail=%d",
					model, v.Name, rate, lv.WallSeconds, lv.ThroughputTPS, lv.CompletedRequests, lv.FailedRequests)
				rep.Concurrent = append(rep.Concurrent, lv)
				if lv.Aborted != "" {
					log.Printf("🛑 %s——停止后续到达率档位", lv.Aborted)
					aborted = true
				}
			}
		}
	}
	return rep, nil
}

// runRequestArrival 开环到达一轮：arrivals 决定发射时刻，可选在飞上限（信号量）。
func runRequestArrival(ctx context.Context, e *env, model string, v config.ThinkingVariant,
	samples []engine.RequestSample, rate float64, maxConcurrency int, burstiness float64) report.ConcurrentLevel {

	lv := report.ConcurrentLevel{Model: model, Thinking: v.Name, Level: 0, RequestRate: rate}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	arrivals := poissonDelays(len(samples), rate, burstiness, rng)
	base := time.Now()
	start := time.Now()

	slots := make(chan struct{}, maxConcurrency)
	if maxConcurrency <= 0 {
		slots = nil // 不限在飞
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, delay := range arrivals {
		if interrupted(ctx) {
			break
		}
		// 等到发射时刻（ctx 取消即退出）
		if wake := base.Add(time.Duration(delay * float64(time.Second))); time.Now().Before(wake) {
			select {
			case <-ctx.Done():
			case <-time.After(time.Until(wake)):
			}
		}
		if interrupted(ctx) {
			break
		}
		sample := samples[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if slots != nil {
				select {
				case slots <- struct{}{}:
					defer func() { <-slots }()
				case <-ctx.Done():
					return // 排队中放弃：cancelled 语义（未发即弃）
				}
			}
			m := requestRunner(ctx, e, model, v, sample.OutputTokens, sample)
			mu.Lock()
			lv.Requests = append(lv.Requests, m)
			mu.Unlock()
		}()
	}
	wg.Wait()
	lv.WallSeconds = time.Since(start).Seconds()
	finishLevel(&lv)
	return lv
}

// ConcurrencyScenario concurrency 模式：固定在飞上限齐射（对齐 vLLM bench serve）。
func ConcurrencyScenario(ctx context.Context, cfg *config.Config, client *engine.Client, modelFilter string) (*report.Report, error) {
	samples, err := loadRequestSamples(cfg)
	if err != nil {
		return nil, err
	}
	if len(cfg.Concurrency.Levels) == 0 {
		return nil, fmt.Errorf("concurrency 模式需要 concurrency.levels（在飞请求数档位）")
	}
	e, err := newEnvSilent(ctx, cfg, client)
	if err != nil {
		return nil, err
	}
	rep := &report.Report{
		Tool:        report.Version,
		Scenario:    "concurrency",
		GeneratedAt: time.Now(),
		Test:        cfg.TestKind(),
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("冻结请求快照固定在飞 levels=%v request_rate=%v burstiness=%.2f num_prompts=%d stream=%v thinking=%s（对齐 vLLM bench serve：0=inf 齐射）%s",
			cfg.Concurrency.Levels, cfg.Concurrency.RequestRate, cfg.Concurrency.GetBurstiness(), len(samples),
			cfg.StreamEnabled(), cfg.Thinking.Mode, thinkingNoteSuffix(cfg)),
	}
	applySLO(e, rep)
	attachKVCapacity(e, rep)
	before, poller, winStart := startWindow(ctx, e)
	defer func() { rep.Server = finishWindow(e, before, poller, winStart) }()

	for _, model := range cfg.ActiveModels() {
		if modelFilter != "" && !strings.Contains(model, modelFilter) {
			continue
		}
		em, mc := forModel(e, cfg, model)
		th := mc.Thinking
		if len(th.Variants()) == 0 {
			log.Printf("  %s: 无匹配的思考变体，跳过", model)
			continue
		}
		for _, v := range th.Variants() {
			aborted := false
			for _, level := range cfg.Concurrency.Levels {
				if aborted || interrupted(ctx) {
					break
				}
				lv := runRequestBarrier(ctx, em, model, v, samples, level,
					cfg.Concurrency.RequestRate, cfg.Concurrency.GetBurstiness())
				log.Printf("[concurrency] %s thinking=%s level=%d: wall=%.1fs throughput=%.0f tok/s ok=%d fail=%d",
					model, v.Name, level, lv.WallSeconds, lv.ThroughputTPS, lv.CompletedRequests, lv.FailedRequests)
				rep.Concurrent = append(rep.Concurrent, lv)
				if lv.Aborted != "" {
					log.Printf("🛑 %s——停止后续并发档位", lv.Aborted)
					aborted = true
				}
			}
		}
	}
	return rep, nil
}

// runRequestBarrier 固定在飞一轮：level 个 worker 从共享游标拉请求；
// request_rate>0 时 worker 发射前按泊松间隔等待（有限到达率 + 在飞上限组合）。
func runRequestBarrier(ctx context.Context, e *env, model string, v config.ThinkingVariant,
	samples []engine.RequestSample, level int, requestRate, burstiness float64) report.ConcurrentLevel {

	lv := report.ConcurrentLevel{Model: model, Thinking: v.Name, Level: level}
	if requestRate > 0 {
		lv.RequestRate = requestRate
	}
	start := time.Now()
	var idx atomic.Int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(level)
	for w := 0; w < level; w++ {
		go func(worker int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(worker)))
			for {
				if interrupted(ctx) {
					return
				}
				i := int(idx.Add(1)) - 1
				if i >= len(samples) {
					return
				}
				if requestRate > 0 {
					// 有限到达率：worker 内按泊松节奏发射（共享总速率 / worker 数）
					d := gammaSample(rng, burstiness) / (requestRate / float64(level) * burstiness)
					select {
					case <-ctx.Done():
						return
					case <-time.After(time.Duration(d * float64(time.Second))):
					}
				}
				sample := samples[i]
				m := requestRunner(ctx, e, model, v, sample.OutputTokens, sample)
				mu.Lock()
				lv.Requests = append(lv.Requests, m)
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	lv.WallSeconds = time.Since(start).Seconds()
	finishLevel(&lv)
	return lv
}
