package scenario

import (
	"strings"
	"testing"

	"github.com/aleexjiang/llm-perf/internal/config"
)

// PlanSummary 请求估算与场景循环同构：变体 × 输出档 × (档位×runs | sessions×turns | levels×每档请求)。
func TestPlanSummary(t *testing.T) {
	cfg := &config.Config{
		Endpoint:       "http://x:1/v1",
		Models:         []string{"m1", "m2"},
		TimeoutSeconds: 300,
		Single:         config.Single{Runs: 2, PromptTokens: []int{4000, 10000}, MaxTokens: config.IntList{256}},
		Multiturn: config.Multiturn{Sessions: 2, Turns: 8, SystemTokens: 18000, ToolDefsTokens: 3000,
			TurnTokens: 10000, MaxTokens: config.IntList{256}},
		Concurrent: config.Concurrent{RunsPerWorker: 2, MaxTokens: config.IntList{256}}, // Load 后的默认形状
	}
	cfg.Thinking.Mode = "both" // off + on 两个变体
	items := []PlanItem{
		{Name: "single"},
		{Name: "multiturn"},
		{Name: "concurrent", Highs: []int{2, 4}},
	}
	p := PlanSummary(cfg, "", items)
	if p == nil {
		t.Fatal("plan 为 nil")
	}
	if p.NumModels != 2 || len(p.Models) != 2 {
		t.Fatalf("模型数: %d/%d", p.NumModels, len(p.Models))
	}
	m := p.Models[0]
	if len(m.Scenarios) != 3 {
		t.Fatalf("场景数 %d ≠ 3", len(m.Scenarios))
	}
	// single: 2 变体 × 1 输出档 × 2 档位 × 2 runs = 8
	if got := m.Scenarios[0].Requests; got != 8 {
		t.Fatalf("single 估算 %d ≠ 8", got)
	}
	// multiturn: 2 变体 × 1 输出档 × 2 sessions × 8 turns = 32
	if got := m.Scenarios[1].Requests; got != 32 {
		t.Fatalf("multiturn 估算 %d ≠ 32", got)
	}
	// concurrent: 2 变体 × 1 输出档 × (2×2 + 4×2) = 24
	if got := m.Scenarios[2].Requests; got != 24 {
		t.Fatalf("concurrent 估算 %d ≠ 24", got)
	}
	if m.Requests != 64 || p.TotalRequests != 128 {
		t.Fatalf("汇总: 模型 %d / 总 %d，want 64/128", m.Requests, p.TotalRequests)
	}
	if len(p.Render()) != 8 { // 总览 1 行 + 2 模型 × 3 场景 + 总计
		t.Fatalf("Render 行数 %d ≠ 8", len(p.Render()))
	}
}

// max_prompt_tokens 截止：多轮有效轮数提前（与场景层停轮口径一致）。
func TestPlanSummaryCtxCutoff(t *testing.T) {
	cfg := &config.Config{
		Endpoint: "http://x:1/v1",
		Models:   []string{"m1"},
		Single:   config.Single{Runs: 1, PromptTokens: []int{4000}, MaxTokens: config.IntList{64}},
		Multiturn: config.Multiturn{Sessions: 1, Turns: 8, SystemTokens: 18000, ToolDefsTokens: 3000,
			TurnTokens: 10000, MaxTokens: config.IntList{64}},
		MaxPromptTokens: 50000, // base 21k + 10k×2 → 第 3 轮起超限，有效轮数 2
	}
	cfg.Thinking.Mode = "off"
	p := PlanSummary(cfg, "", []PlanItem{{Name: "multiturn"}})
	got := p.Models[0].Scenarios[0]
	if got.Requests != 2 {
		t.Fatalf("截止后 multiturn 估算 %d ≠ 2（1 变体 × 1 session × 2 轮）", got.Requests)
	}
}

// 开环（rate_sweep/request_rate）：画像必须按到达率档位算，不能报 levels 矩阵。
// 回归背景：默认 levels=[1,2,4,8,16] 在 Load 后始终填充，开环配置下仍被画像当成执行口径，
// 把 8 个请求的小配置估成 60+ 请求，与实际执行量差一个数量级。
func TestPlanSummaryOpenLoop(t *testing.T) {
	cfg := &config.Config{
		Endpoint: "http://x:1/v1",
		Models:   []string{"m1"},
		Single:   config.Single{Runs: 1, PromptTokens: []int{4000}, MaxTokens: config.IntList{64}},
		// 多轮默认值（Load 后填充）；num_prompts 在多轮语义下是会话数，请求量按轮计
		Multiturn: config.Multiturn{Sessions: 1, Turns: 2, SystemTokens: 100, ToolDefsTokens: 100,
			TurnTokens: 200, MaxTokens: config.IntList{32}},
		// Load 后的形态：levels 有默认值，但 rate_sweep 生效 → levels 被忽略
		Concurrent: config.Concurrent{Levels: []int{1, 2, 4, 8, 16}, RunsPerWorker: 2,
			RateSweep: []float64{2, 8}, NumPrompts: 4, MaxTokens: config.IntList{32}},
	}
	cfg.Thinking.Mode = "off"
	p := PlanSummary(cfg, "", []PlanItem{{Name: "concurrent", Highs: []int{1, 2, 4, 8, 16}}})
	got := p.Models[0].Scenarios[0]
	if got.Requests != 8 { // 1 变体 × 1 输出档 × 2 到达率档 × 4 请求/档
		t.Fatalf("开环估算 %d ≠ 8（%s）", got.Requests, got.Detail)
	}
	if !strings.Contains(got.Detail, "开环到达率") {
		t.Fatalf("开环画像明细应说明到达率档位，实际: %s", got.Detail)
	}
	// 开环多轮：num_prompts 是会话数，请求量按轮计（2 档 × 4 会话 × 2 轮 = 16）
	pMT := PlanSummary(cfg, "", []PlanItem{{Name: "concurrent-multi", MT: true, Highs: []int{1, 2, 4, 8, 16}}})
	if gotMT := pMT.Models[0].Scenarios[0]; gotMT.Requests != 16 {
		t.Fatalf("开环多轮估算 %d ≠ 16（%s）", gotMT.Requests, gotMT.Detail)
	}
	// request_rate 单档同样走开环口径
	cfg.Concurrent.RateSweep = nil
	cfg.Concurrent.RequestRate = 4
	p2 := PlanSummary(cfg, "", []PlanItem{{Name: "concurrent", Highs: []int{1, 2, 4, 8, 16}}})
	if got2 := p2.Models[0].Scenarios[0]; got2.Requests != 4 {
		t.Fatalf("单档开环估算 %d ≠ 4（%s）", got2.Requests, got2.Detail)
	}
}

// 混跑：不走外层输出扫描，请求数 = levels × runs/worker（tiers=1）。
func TestPlanSummaryMix(t *testing.T) {
	cfg := &config.Config{
		Endpoint: "http://x:1/v1",
		Models:   []string{"m1"},
		Single:   config.Single{Runs: 1, PromptTokens: []int{4000}, MaxTokens: config.IntList{64}},
		Concurrent: config.Concurrent{Levels: []int{4}, RunsPerWorker: 2, PromptTokens: 10000,
			MaxTokens: config.IntList{256},
			Mix: []config.MixShape{
				{Weight: 7, Label: "short", PromptTokens: 1000, MaxTokens: 128},
				{Weight: 3, Label: "long", PromptTokens: 8000, MaxTokens: 256},
			}},
	}
	cfg.Thinking.Mode = "on"
	p := PlanSummary(cfg, "", []PlanItem{{Name: "concurrent", Highs: []int{4}}})
	got := p.Models[0].Scenarios[0]
	if got.Requests != 8 { // 1 变体 × level4 × runs2（mix 下输出档不乘）
		t.Fatalf("mix 估算 %d ≠ 8", got.Requests)
	}
}

// 时长制（10.5 duration_seconds）：请求量由墙钟决定，画像不报数——Requests=0 +
// DurationS 标记（渲染层以「—」呈现），Detail 说明时长制口径。回归背景：时长制下
// runs_per_worker 被场景层忽略，画像曾仍按它估算，日志/JSON 给出不会执行的请求数。
func TestPlanSummaryDuration(t *testing.T) {
	cfg := &config.Config{
		Endpoint: "http://x:1/v1",
		Models:   []string{"m1"},
		Multiturn: config.Multiturn{Sessions: 2, Turns: 8, SystemTokens: 100, ToolDefsTokens: 100,
			TurnTokens: 200, MaxTokens: config.IntList{32}},
		Concurrent: config.Concurrent{Levels: []int{2}, RunsPerWorker: 2, DurationSeconds: 60,
			Renew: true, MaxTokens: config.IntList{32}},
	}
	cfg.Thinking.Mode = "off"
	p := PlanSummary(cfg, "", []PlanItem{{Name: "concurrent-multi", MT: true, Highs: []int{2}}})
	got := p.Models[0].Scenarios[0]
	if got.DurationS != 60 || got.Requests != 0 {
		t.Fatalf("时长制画像应 Requests=0 + DurationS=60，实际 %d/%d（%s）",
			got.Requests, got.DurationS, got.Detail)
	}
	if !strings.Contains(got.Detail, "时长制") {
		t.Fatalf("时长制画像明细应说明口径: %s", got.Detail)
	}
	if p.TotalRequests != 0 {
		t.Fatalf("总请求不应计入时长制档位: %d", p.TotalRequests)
	}
	// Render：时长制行不带「≈ N 请求」，总行说明不含时长制档位（数字不可估 ≠ 0 个请求）
	outs := strings.Join(p.Render(), "\n")
	if strings.Contains(outs, "≈ 0 请求") {
		t.Fatalf("时长制行不应渲染「≈ 0 请求」: %s", outs)
	}
	if !strings.Contains(outs, "不含时长制档位") {
		t.Fatalf("总行应说明不含时长制档位: %s", outs)
	}
}
