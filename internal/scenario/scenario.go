// Package scenario 实现三种评测场景：single / multiturn / concurrent。
package scenario

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/engine"
	"github.com/aleexjiang/llm-perf/internal/report"
)

// Single 跑单请求基线：模型 × token 档位 × runs。
// fixed_seed=true 时各 run 使用相同 prompt（观察前缀缓存命中，Run2+ 的 TTFT 应明显低于 Run1）。
func Single(ctx context.Context, cfg *config.Config, client *engine.Client, modelFilter string) (*report.Report, error) {
	rep := &report.Report{
		Scenario:    "single",
		GeneratedAt: time.Now(),
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("runs=%d, fixed_seed=%v（同 prompt 重复可观察前缀缓存命中；Run2+ TTFT 显著低于 Run1 即命中）",
			cfg.Single.Runs, cfg.Single.FixedSeed),
	}
	for _, model := range filterModels(cfg.Models, modelFilter) {
		for _, tokens := range cfg.Single.PromptTokens {
			row := report.SingleRow{Model: model, PromptTokens: tokens}
			for run := 0; run < cfg.Single.Runs; run++ {
				var seed int64
				if cfg.Single.FixedSeed {
					seed = 1000 // 同一 prompt
				} else {
					seed = int64(tokens*100 + run)
				}
				msgs := []engine.Message{engine.UserMsg(tokens, seed, cfg.Fillers())}
				m, err := client.Stream(ctx, model, msgs, cfg.Single.MaxTokens)
				if err != nil {
					log.Printf("[single] %s %dtk run%d 失败: %v", model, tokens, run+1, err)
				} else {
					log.Printf("[single] %s %dtk run%d: TTFT=%.0fms think=%.0fms decode=%.0fms tok/s=%.0f",
						model, tokens, run+1, m.TTFT, m.ThinkMS, m.DecodeMS, m.TokensPerSec)
				}
				row.Runs = append(row.Runs, m)
			}
			rep.Single = append(rep.Single, row)
		}
	}
	return rep, nil
}

// Multiturn 跑多轮会话重放：system + tool defs + 逐轮滚大的 history，模拟真实 agent。
// 判定：turn N 的 TTFT 若只比 turn N-1 增加一小截 ⇒ 前缀缓存命中；若接近全量 prefill ⇒ 未命中。
func Multiturn(ctx context.Context, cfg *config.Config, client *engine.Client, modelFilter string) (*report.Report, error) {
	mt := cfg.Multiturn
	rep := &report.Report{
		Scenario:    "multiturn",
		GeneratedAt: time.Now(),
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("sessions=%d turns=%d system≈%dtk tool_defs≈%dtk 每轮新增 user≈%dtk；累计上下文到 %dtk 量级",
			mt.Sessions, mt.Turns, mt.SystemTokens, mt.ToolDefsTokens, mt.TurnTokens,
			mt.SystemTokens+mt.ToolDefsTokens+mt.Turns*mt.TurnTokens),
	}
	for _, model := range filterModels(cfg.Models, modelFilter) {
		for s := 0; s < mt.Sessions; s++ {
			run := report.MultiturnRun{Model: model, Session: s + 1}
			baseSeed := int64(5000 + s*10000)
			msgs := []engine.Message{}
			if sys := engine.SystemMsg(mt.SystemTokens, mt.ToolDefsTokens, baseSeed, cfg.Fillers()); sys.Content != "" {
				msgs = append(msgs, sys)
			}
			for turn := 0; turn < mt.Turns; turn++ {
				msgs = append(msgs, engine.UserMsg(mt.TurnTokens, baseSeed+int64(turn), cfg.Fillers()))
				m, err := client.Stream(ctx, model, msgs, mt.MaxTokens)
				if err != nil {
					log.Printf("[multiturn] %s session%d turn%d 失败: %v", model, s+1, turn+1, err)
				} else {
					log.Printf("[multiturn] %s session%d turn%d (ctx≈%dtk): TTFT=%.0fms think=%.0fms",
						model, s+1, turn+1, m.PromptTokens, m.TTFT, m.ThinkMS)
					if mt.KeepAssistant && m.ReplyText != "" {
						reply := m.ReplyText
						if len(reply) > 2000 {
							reply = reply[:2000]
						}
						msgs = append(msgs, engine.Message{Role: "assistant", Content: reply})
					}
				}
				run.Turns = append(run.Turns, m)
			}
			rep.Multiturn = append(rep.Multiturn, run)
		}
	}
	return rep, nil
}

// Concurrent 跑阶梯并发：每个档位 level 个虚拟用户同时发起独立请求。
// 各用户 prompt 用不同 seed，避免伪缓存命中。
func Concurrent(ctx context.Context, cfg *config.Config, client *engine.Client, modelFilter string) (*report.Report, error) {
	cc := cfg.Concurrent
	rep := &report.Report{
		Scenario:    "concurrent",
		GeneratedAt: time.Now(),
		Endpoint:    cfg.Endpoint,
		Note:        fmt.Sprintf("levels=%v runs_per_worker=%d prompt≈%dtk；每用户独立 prompt（不同 seed）", cc.Levels, cc.RunsPerWorker, cc.PromptTokens),
	}
	for _, model := range filterModels(cfg.Models, modelFilter) {
		for _, level := range cc.Levels {
			lv := report.ConcurrentLevel{Model: model, Level: level}
			start := time.Now()
			results := make([]*engine.TurnMetrics, level*cc.RunsPerWorker)
			var wg sync.WaitGroup
			startBarrier := make(chan struct{})
			for w := 0; w < level; w++ {
				wg.Add(1)
				go func(workerID int) {
					defer wg.Done()
					<-startBarrier // 所有 worker 就绪后同时发车
					for r := 0; r < cc.RunsPerWorker; r++ {
						idx := workerID*cc.RunsPerWorker + r
						seed := int64(90000 + workerID*100 + r) // 每用户不同 prompt
						msgs := []engine.Message{engine.UserMsg(cc.PromptTokens, seed, cfg.Fillers())}
						m, err := client.Stream(ctx, model, msgs, cc.MaxTokens)
						if err != nil {
							log.Printf("[concurrent] %s level%d worker%d r%d 失败: %v", model, level, workerID, r+1, err)
						}
						results[idx] = m
					}
				}(w)
			}
			close(startBarrier)
			wg.Wait()
			lv.WallSeconds = time.Since(start).Seconds()
			lv.Requests = results
			throughput := 0.0
			for _, m := range results {
				if m != nil {
					throughput += float64(m.CompletionTokens)
				}
			}
			if lv.WallSeconds > 0 {
				lv.ThroughputTPS = throughput / lv.WallSeconds
			}
			log.Printf("[concurrent] %s level%d: wall=%.1fs throughput=%.0f tok/s", model, level, lv.WallSeconds, lv.ThroughputTPS)
			rep.Concurrent = append(rep.Concurrent, lv)
		}
	}
	return rep, nil
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
