// Package scenario 实现评测场景矩阵：
//   执行方式（单发/并发） × 轮次（单轮/多轮） × 思考模式（off/on，由 config.Thinking 展开），
//   stream 为请求级开关（config.stream）。
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

// runOne 发起一次请求（流式/非流式、思考变体由 opts 决定）。
func runOne(ctx context.Context, client *engine.Client, cfg *config.Config, model string,
	msgs []engine.Message, maxTokens int, v config.ThinkingVariant) *engine.TurnMetrics {

	m, err := client.Chat(ctx, engine.ChatOptions{
		Model:     model,
		Messages:  msgs,
		MaxTokens: maxTokens,
		Stream:    cfg.StreamEnabled(),
		Thinking:  v.Enabled,
		ExtraBody: v.ExtraBody,
	})
	if err != nil {
		log.Printf("    失败: %v", err)
		return m
	}
	if m.ThinkingNoContent {
		log.Printf("    ⚠️ 思考吃光 max_tokens=%d，全程无 content（finish=length）——本次 ThinkMS/DecodeMS 不可测，建议调大 thinking.max_tokens_floor", maxTokens)
	}
	for _, w := range m.Warnings {
		log.Printf("    ⚠️ 兼容性告警: %s", w)
	}
	if cfg.StreamEnabled() {
		log.Printf("    TTFT=%.0fms think=%.0fms decode=%.0fms tok/s=%.0f finish=%s",
			m.TTFT, m.ThinkMS, m.DecodeMS, m.TokensPerSec, m.FinishReason)
	} else {
		log.Printf("    E2E=%.0fms tok/s=%.0f（非流式，TTFT/思考拆分 N/A）", m.E2EMS, m.TokensPerSec)
	}
	return m
}

// Single 单发单轮：模型 × 思考变体 × token 档位 × runs。
func Single(ctx context.Context, cfg *config.Config, client *engine.Client, modelFilter string) (*report.Report, error) {
	rep := &report.Report{
		Scenario:    "single",
		GeneratedAt: time.Now(),
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("单发单轮 runs=%d fixed_seed=%v stream=%v thinking=%s；思考开启时 max_tokens 下限 %d",
			cfg.Single.Runs, cfg.Single.FixedSeed, cfg.StreamEnabled(), cfg.Thinking.Mode, cfg.Thinking.MaxTokensFloor),
	}
	for _, model := range filterModels(cfg.Models, modelFilter) {
		for _, v := range cfg.Thinking.Variants() {
			maxTok := cfg.Thinking.MaxTokens(cfg.Single.MaxTokens, v)
			for _, tokens := range cfg.Single.PromptTokens {
				row := report.SingleRow{Model: model, Thinking: v.Name, PromptTokens: tokens}
				for run := 0; run < cfg.Single.Runs; run++ {
					var seed int64
					if cfg.Single.FixedSeed {
						seed = 1000
					} else {
						seed = int64(tokens*100 + run)
					}
					msgs := []engine.Message{engine.UserMsg(tokens, seed, cfg.Fillers())}
					log.Printf("[single] %s thinking=%s %dtk run%d", model, v.Name, tokens, run+1)
					row.Runs = append(row.Runs, runOne(ctx, client, cfg, model, msgs, maxTok, v))
				}
				rep.Single = append(rep.Single, row)
			}
		}
	}
	return rep, nil
}

// Multiturn 单发多轮：模型 × 思考变体 × sessions，每 turn 记录思考时间。
func Multiturn(ctx context.Context, cfg *config.Config, client *engine.Client, modelFilter string) (*report.Report, error) {
	mt := cfg.Multiturn
	rep := &report.Report{
		Scenario:    "multiturn",
		GeneratedAt: time.Now(),
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("单发多轮 sessions=%d turns=%d system≈%dtk tool_defs≈%dtk 每轮+≈%dtk stream=%v thinking=%s",
			mt.Sessions, mt.Turns, mt.SystemTokens, mt.ToolDefsTokens, mt.TurnTokens, cfg.StreamEnabled(), cfg.Thinking.Mode),
	}
	for _, model := range filterModels(cfg.Models, modelFilter) {
		for _, v := range cfg.Thinking.Variants() {
			for s := 0; s < mt.Sessions; s++ {
				run := report.MultiturnRun{Model: model, Thinking: v.Name, Session: s + 1}
				log.Printf("[multiturn] %s thinking=%s session%d", model, v.Name, s+1)
				baseSeed := int64(5000 + s*10000)
				msgs := []engine.Message{}
				if sys := engine.SystemMsg(mt.SystemTokens, mt.ToolDefsTokens, baseSeed, cfg.Fillers()); sys.Content != "" {
					msgs = append(msgs, sys)
				}
				maxTok := cfg.Thinking.MaxTokens(mt.MaxTokens, v)
				for turn := 0; turn < mt.Turns; turn++ {
					msgs = append(msgs, engine.UserMsg(mt.TurnTokens, baseSeed+int64(turn), cfg.Fillers()))
					m := runOne(ctx, client, cfg, model, msgs, maxTok, v)
					log.Printf("    turn%d (ctx≈%dtk)", turn+1, m.PromptTokens)
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
	}
	return rep, nil
}

// Concurrent 并发场景：模型 × 思考变体 × 阶梯并发。
// multiturn=false：每个虚拟用户发独立单轮请求；true：每个虚拟用户各自跑完整会话重放。
func Concurrent(ctx context.Context, cfg *config.Config, client *engine.Client, modelFilter string) (*report.Report, error) {
	cc := cfg.Concurrent
	mode := "单轮"
	if cc.Multiturn {
		mode = "多轮会话重放"
	}
	rep := &report.Report{
		Scenario:    "concurrent",
		GeneratedAt: time.Now(),
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("并发%s levels=%v runs_per_worker=%d prompt≈%dtk stream=%v thinking=%s；每用户独立 prompt/会话（不同 seed）",
			mode, cc.Levels, cc.RunsPerWorker, cc.PromptTokens, cfg.StreamEnabled(), cfg.Thinking.Mode),
	}
	for _, model := range filterModels(cfg.Models, modelFilter) {
		for _, v := range cfg.Thinking.Variants() {
			for _, level := range cc.Levels {
				lv := report.ConcurrentLevel{Model: model, Thinking: v.Name, Level: level}
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
							baseSeed := int64(5000 + workerID*10000) // 每用户不同会话内容
							s.Turns = collectSessionTurns(ctx, client, cfg, model, v, baseSeed, 0)
							mu.Lock()
							lv.Sessions = append(lv.Sessions, s)
							mu.Unlock()
							return
						}
						maxTok := cfg.Thinking.MaxTokens(cc.MaxTokens, v)
						for r := 0; r < cc.RunsPerWorker; r++ {
							seed := int64(90000 + workerID*100 + r) // 每用户不同 prompt
							msgs := []engine.Message{engine.UserMsg(cc.PromptTokens, seed, cfg.Fillers())}
							m := runOne(ctx, client, cfg, model, msgs, maxTok, v)
							mu.Lock()
							lv.Requests = append(lv.Requests, m)
							mu.Unlock()
						}
					}(w)
				}
				close(startBarrier)
				wg.Wait()
				lv.WallSeconds = time.Since(start).Seconds()
				throughput := 0.0
				for _, m := range lv.Requests {
					if m != nil {
						throughput += float64(m.CompletionTokens)
					}
				}
				for _, s := range lv.Sessions {
					for _, m := range s.Turns {
						if m != nil {
							throughput += float64(m.CompletionTokens)
						}
					}
				}
				if lv.WallSeconds > 0 {
					lv.ThroughputTPS = throughput / lv.WallSeconds
				}
				log.Printf("[concurrent] %s thinking=%s level%d: wall=%.1fs throughput=%.0f tok/s", model, v.Name, level, lv.WallSeconds, lv.ThroughputTPS)
				rep.Concurrent = append(rep.Concurrent, lv)
			}
		}
	}
	return rep, nil
}

// collectSessionTurns 执行一次完整会话重放并采集逐 turn 指标（并发多轮用：每个虚拟用户一次）。
func collectSessionTurns(ctx context.Context, client *engine.Client, cfg *config.Config,
	model string, v config.ThinkingVariant, baseSeed int64, turnLimit int) []*engine.TurnMetrics {

	mt := cfg.Multiturn
	maxTok := cfg.Thinking.MaxTokens(mt.MaxTokens, v)
	msgs := []engine.Message{}
	if sys := engine.SystemMsg(mt.SystemTokens, mt.ToolDefsTokens, baseSeed, cfg.Fillers()); sys.Content != "" {
		msgs = append(msgs, sys)
	}
	turns := turnLimit
	if turns <= 0 {
		turns = mt.Turns
	}
	var out []*engine.TurnMetrics
	for turn := 0; turn < turns; turn++ {
		msgs = append(msgs, engine.UserMsg(mt.TurnTokens, baseSeed+int64(turn), cfg.Fillers()))
		m := runOne(ctx, client, cfg, model, msgs, maxTok, v)
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
