package scenario

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/contract"
	"github.com/aleexjiang/llm-perf/internal/engine"
)

const testProfileJSON = `{
  "version": 1,
  "generated_at": "2026-09-18T00:00:00Z",
  "source": "test",
  "first_turn_tokens": [35000, 40000],
  "profiles": {
    "light":  {"weight": 0.6, "turns_range": [2, 4], "user_input_tokens": [20, 120], "context_tokens": [0, 0], "context_burst_probability": 0.01, "context_burst_tokens": [8000, 30000]},
    "medium": {"weight": 0.3, "turns_range": [5, 8], "user_input_tokens": [30, 200], "context_tokens": [0, 0], "context_burst_probability": 0.05, "context_burst_tokens": [3000, 8000]},
    "heavy":  {"weight": 0.1, "turns_range": [9],    "user_input_tokens": [30, 200], "context_tokens": [0, 0], "context_burst_probability": 0.10, "context_burst_tokens": [10000, 25000]}
  }
}`

func writeProfile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLoadProfile 校验 profile 合法性门槛（版本/权重/轮次/首轮约束）。
func TestLoadProfile(t *testing.T) {
	p, err := LoadProfile(writeProfile(t, testProfileJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Profiles) != 3 || p.FirstTurnTokens[0] < 35000 {
		t.Fatalf("profile 解析错误: %+v", p)
	}
	bad := []string{
		`{"version":1,"first_turn_tokens":[35000,40000],"profiles":{"light":{"weight":1,"turns_range":[1,2],"user_input_tokens":[1,2],"context_tokens":[1,2],"context_burst_probability":0,"context_burst_tokens":[1,2]}}}`,
		`{"version":1,"first_turn_tokens":[35000,40000],"profiles":{"light":{"weight":1,"turns_range":[2,3],"user_input_tokens":[1,2],"context_tokens":[0,0],"context_burst_probability":1.5,"context_burst_tokens":[1,2]}}}`,
		`{"version":1,"first_turn_tokens":[35000,40000],"profiles":{"light":{"weight":1,"turns_range":[2,3],"user_input_tokens":[1,2],"context_tokens":[0,0],"attachment_probability":0.1,"attachment_tokens":[1,2]}}}`,
		`{"version":1,"first_turn_tokens":[35000,40000],"profiles":{"light":{"weight":1,"turns_range":[2,3],"user_input_tokens":[1,2],"context_tokens":[0,0],"context_burst_probability":0}}} {}`,
		`{"version":1,"first_turn_tokens":[35000,40000],"profiles":{}}`,
	}
	for i, b := range bad {
		if _, err := LoadProfile(writeProfile(t, b)); err == nil {
			t.Fatalf("非法 profile #%d 应被拒绝", i)
		}
	}
	if err := applyFirstTurnConfig(p, []int{27000, 30000}, 27000); err == nil {
		t.Fatal("首轮下限小于基座+最大用户输入时应拒绝")
	}
}

func TestUserScenarioFirstTurnConfigOverridesProfile(t *testing.T) {
	srv := sseStub(t, &stubState{})
	cfg := testCfg(t, srv.URL)
	cfg.User = config.User{
		ProfilePath:      writeProfile(t, testProfileJSON),
		Levels:           []int{1},
		MaxTokens:        config.IntList{16},
		FirstTurnTokens:  []int{12000, 12000},
		SharedBaseTokens: 8000,
	}

	rep, err := UserScenario(context.Background(), cfg, engine.NewClient(srv.URL, "", 30*time.Second, true), "", RunOptions{})
	if err != nil {
		t.Fatalf("UserScenario: %v", err)
	}
	level := rep.UserLevels[0]
	if got := level.Workload.FirstTurnTokens; len(got) != 2 || got[0] != 12000 || got[1] != 12000 {
		t.Fatalf("workload first_turn_tokens=%v, want [12000 12000]", got)
	}
	if level.Workload.SharedBaseTokens != 8000 {
		t.Fatalf("workload shared_base_tokens=%d, want 8000", level.Workload.SharedBaseTokens)
	}
	run := level.Sessions[0]
	if first := run.Turns[0].PromptTokens; first < 11000 || first > 13500 {
		t.Fatalf("首轮 prompt=%d，应接近配置的 12000", first)
	}
	if got := run.InputPlan[0]; got.ContextBurst || got.UserInputTokens+got.ContextTokens+8000 != 12000 {
		t.Fatalf("首轮计划应补齐到 12000: %+v", got)
	}
}

// TestUserScenario 集成：profile 驱动的生成式多轮会话。
// 断言：权重 6:3:1 的 SWRR 分派、轮次范围、真实 assistant 进 history、Profile 标签落盘。
// fixture 的 first_turn_tokens 是共享基座尺寸；普通轮由 profile 输入长度决定。
func TestUserScenario(t *testing.T) {
	srv := sseStub(t, &stubState{})
	cfg := testCfg(t, srv.URL)
	cfg.User = config.User{
		ProfilePath: writeProfile(t, testProfileJSON),
		Levels:      []int{10},
		MaxTokens:   config.IntList{16},
	}

	rep, err := UserScenario(context.Background(), cfg, engine.NewClient(srv.URL, "", 30*time.Second, true), "", RunOptions{})
	if err != nil {
		t.Fatalf("UserScenario: %v", err)
	}
	if rep.Scenario != "user" {
		t.Fatalf("scenario = %q, want user", rep.Scenario)
	}
	if len(rep.UserLevels) != 1 || rep.UserLevels[0].Users != 10 {
		t.Fatalf("应有 1 个 user level=10，得到 %+v", rep.UserLevels)
	}
	sessions := rep.UserLevels[0].Sessions

	dist := map[string]int{}
	for _, run := range sessions {
		if run.Profile == "" {
			t.Fatalf("会话缺 profile 标签: %+v", run)
		}
		dist[run.Profile]++
		// 轮次必须在档位范围内（heavy 单元素 = 下限 9，运行时上限 defaultTurnsUpper=32）
		want := map[string][2]int{"light": {2, 4}, "medium": {5, 8}, "heavy": {9, 32}}[run.Profile]
		if len(run.Turns) < want[0] || len(run.Turns) > want[1] {
			t.Fatalf("profile=%s 轮次 %d 超出 [%d,%d]", run.Profile, len(run.Turns), want[0], want[1])
		}
		// fixture 基座 27K（stub usage=chars/4）；首轮不应固定填满 35K。
		if first := run.Turns[0].PromptTokens; first < 34000 || first > 41000 {
			t.Fatalf("profile=%s 首轮 prompt %dtk 不在 first_turn_tokens 约束附近", run.Profile, first)
		}
		if len(run.InputPlan) != len(run.Turns) || run.InputPlan[0].ContextBurst {
			t.Fatalf("profile=%s input_plan 应与 turns 对齐且首轮不突增: %+v", run.Profile, run.InputPlan)
		}
		// 真实 assistant 回复进 history：turn2 的 prompt 必须大于 turn1
		if len(run.Turns) > 1 {
			if run.Turns[1].PromptTokens <= run.Turns[0].PromptTokens {
				t.Fatalf("profile=%s turn2 prompt 未增长（assistant 未进 history？）: %d → %d",
					run.Profile, run.Turns[0].PromptTokens, run.Turns[1].PromptTokens)
			}
		}
	}
	if dist["light"] != 6 || dist["medium"] != 3 || dist["heavy"] != 1 {
		t.Fatalf("6:3:1 分派错误: %v", dist)
	}
	w := rep.UserLevels[0].Workload
	if w.Profile != "test" || len(w.FirstTurnTokens) != 2 || len(w.Tiers) != 3 {
		t.Fatalf("workload 元数据不完整: %+v", w)
	}
	for name, want := range map[string]contract.UserWorkloadTier{
		"light":  {Weight: 0.6, ContextBurstProbability: 0.01, ContextBurstTokens: []int{8000, 30000}},
		"medium": {Weight: 0.3, ContextBurstProbability: 0.05, ContextBurstTokens: []int{3000, 8000}},
		"heavy":  {Weight: 0.1, ContextBurstProbability: 0.10, ContextBurstTokens: []int{10000, 25000}},
	} {
		got := w.Tiers[name]
		if got.Weight != want.Weight || got.ContextBurstProbability != want.ContextBurstProbability ||
			len(got.ContextBurstTokens) != 2 || got.ContextBurstTokens[0] != want.ContextBurstTokens[0] ||
			got.ContextBurstTokens[1] != want.ContextBurstTokens[1] {
			t.Fatalf("workload tier %s = %+v, want %+v", name, got, want)
		}
	}
}

func TestUserScenarioContextBurstAddsToRegularIncrement(t *testing.T) {
	srv := sseStub(t, &stubState{})
	cfg := testCfg(t, srv.URL)
	cfg.User = config.User{
		ProfilePath: writeProfile(t, `{
  "version": 1, "source": "burst-test", "first_turn_tokens": [35000, 35000],
  "profiles": {"light": {"weight": 1, "turns_range": [3, 3], "user_input_tokens": [20, 20],
    "context_tokens": [100, 100], "context_burst_probability": 1, "context_burst_tokens": [1000, 1000]}}
}`),
		Levels:    []int{1},
		MaxTokens: config.IntList{16},
	}

	rep, err := UserScenario(context.Background(), cfg, engine.NewClient(srv.URL, "", 30*time.Second, true), "", RunOptions{})
	if err != nil {
		t.Fatalf("UserScenario: %v", err)
	}
	plan := rep.UserLevels[0].Sessions[0].InputPlan
	if len(plan) != 3 {
		t.Fatalf("input_plan 长度=%d，want 3: %+v", len(plan), plan)
	}
	if plan[0].ContextBurst || plan[0].ContextTokens != 35000-config.DefaultSharedBaseTokens-20 {
		t.Fatalf("首轮应由 first_turn_tokens 决定且不突增: %+v", plan[0])
	}
	for _, p := range plan[1:] {
		if !p.ContextBurst || p.ContextTokens != 100 || p.ContextBurstTokens != 1000 || p.PlannedIncrementTokens != 1120 {
			t.Fatalf("突增应在常规增量之外追加: %+v", p)
		}
	}
}

func TestEstMaxContextIncludesContextBurst(t *testing.T) {
	p := &Profile{
		FirstTurnTokens: []int{35000, 40000},
		Profiles: map[string]*ProfileSpec{"heavy": {
			TurnsRange: []int{3, 3}, UserInputTokens: []int{10, 20}, ContextTokens: []int{0, 100},
			ContextBurstProbability: 0.1, ContextBurstTokens: []int{1000, 5000},
		}},
	}
	if got, want := estMaxContext(p), 40000+2*(20+100+5000); got != want {
		t.Fatalf("estMaxContext=%d, want %d", got, want)
	}
}

// TestUserScenarioRequiresProfile 未配置 profile_path 应报错。
func TestUserScenarioRequiresProfile(t *testing.T) {
	srv := sseStub(t, &stubState{})
	cfg := testCfg(t, srv.URL)
	if _, err := UserScenario(context.Background(), cfg, engine.NewClient(srv.URL, "", 5*time.Second, true), "", RunOptions{}); err == nil {
		t.Fatal("缺 profile_path 应报错")
	}
}

func TestUserScenarioRunsConfiguredLevels(t *testing.T) {
	srv := sseStub(t, &stubState{})
	cfg := testCfg(t, srv.URL)
	cfg.User = config.User{
		ProfilePath: writeProfile(t, testProfileJSON),
		Levels:      []int{2, 3},
		MaxTokens:   config.IntList{16},
	}

	rep, err := UserScenario(context.Background(), cfg, engine.NewClient(srv.URL, "", 30*time.Second, true), "", RunOptions{})
	if err != nil {
		t.Fatalf("UserScenario: %v", err)
	}
	if len(rep.UserLevels) != 2 || rep.UserLevels[0].Users != 2 || rep.UserLevels[1].Users != 3 {
		t.Fatalf("user levels 顺序或数量错误: %+v", rep.UserLevels)
	}
	if len(rep.UserLevels[0].Sessions) != 2 || len(rep.UserLevels[1].Sessions) != 3 {
		t.Fatalf("user level 会话数量错误: %+v", rep.UserLevels)
	}
	for _, level := range rep.UserLevels {
		if level.Metrics == nil || len(level.Metrics.TPSSeries) == 0 {
			t.Fatalf("user level 缺少独立 bucket_tps: %+v", level)
		}
	}
	if rep.Metrics != nil {
		t.Fatalf("user 不应再写顶层重复 metrics: %+v", rep.Metrics)
	}
}

func TestUserScenarioUsesModelOverrideTokenBudget(t *testing.T) {
	srv := sseStub(t, &stubState{})
	cfg := testCfg(t, srv.URL)
	cfg.ContextBudgetTokens = 35000
	cfg.User = config.User{
		ProfilePath: writeProfile(t, testProfileJSON),
		Levels:      []int{1},
		MaxTokens:   config.IntList{16},
	}
	overrideBudget := 40000
	cfg.ModelOverrides = map[string]*config.ModelOverride{
		"stub-model": {ContextBudgetTokens: &overrideBudget},
	}

	rep, err := UserScenario(context.Background(), cfg, engine.NewClient(srv.URL, "", 30*time.Second, true), "", RunOptions{})
	if err != nil {
		t.Fatalf("UserScenario: %v", err)
	}
	if len(rep.UserLevels) != 1 || len(rep.UserLevels[0].Sessions) != 1 {
		t.Fatalf("user level/session 数量错误: %+v", rep.UserLevels)
	}
	if got := rep.UserLevels[0].Sessions[0].TokenBudget; got != overrideBudget-userContextSafetyMargin {
		t.Fatalf("model override token budget=%d, want %d", got, overrideBudget-userContextSafetyMargin)
	}
}

func TestUserScenarioUsesUnifiedSafetyBudget(t *testing.T) {
	srv := sseStub(t, &stubState{})
	cfg := testCfg(t, srv.URL)
	cfg.ContextBudgetTokens = 40000
	cfg.User = config.User{
		ProfilePath: writeProfile(t, testProfileJSON),
		Levels:      []int{1},
		MaxTokens:   config.IntList{256},
	}

	rep, err := UserScenario(context.Background(), cfg, engine.NewClient(srv.URL, "", 30*time.Second, true), "", RunOptions{})
	if err != nil {
		t.Fatalf("UserScenario: %v", err)
	}
	if got := rep.UserLevels[0].Sessions[0].TokenBudget; got != 40000-userContextSafetyMargin {
		t.Fatalf("token_budget=%d, want %d", got, 40000-userContextSafetyMargin)
	}
}

// TestUserScenarioStopsWithinTokenBudget 回归：context_budget_tokens 是统一 prompt+output 总预算基准。
// 上一轮实测历史 + 本轮计划增量 + max_tokens 超过预算时，必须在发出请求前止损，
// 不能像真机 r1-r3 那样用 400 消耗最后一轮。
func TestUserScenarioStopsWithinTokenBudget(t *testing.T) {
	srv := sseStub(t, &stubState{})
	cfg := testCfg(t, srv.URL)
	cfg.User = config.User{
		ProfilePath: writeProfile(t, testProfileJSON),
		Levels:      []int{1},
		MaxTokens:   config.IntList{256},
	}
	// fixture 共享基座约 27K；预算 30K 扣掉输出和安全余量后首轮就稳定触发。
	cfg.ContextBudgetTokens = 30000

	rep, err := UserScenario(context.Background(), cfg, engine.NewClient(srv.URL, "", 30*time.Second, true), "", RunOptions{})
	if err != nil {
		t.Fatalf("UserScenario: %v", err)
	}
	if len(rep.UserLevels) != 1 || len(rep.UserLevels[0].Sessions) != 1 {
		t.Fatalf("应有 1 个 level 和 1 个会话，实际 %+v", rep.UserLevels)
	}
	run := rep.UserLevels[0].Sessions[0]
	if run.TokenBudget != cfg.ContextBudgetTokens-userContextSafetyMargin {
		t.Fatalf("token_budget=%d，want %d", run.TokenBudget, cfg.ContextBudgetTokens-userContextSafetyMargin)
	}
	stopped := false
	for _, m := range run.Turns {
		if m.Error != "" {
			t.Fatalf("预算止损后不应发出失败请求: %+v", m)
		}
		if m.Cancelled && !m.SentAt.Equal(m.EndAt) {
			t.Fatalf("预算止损标记不应伪造请求墙钟: %+v", m)
		}
		for _, w := range m.Warnings {
			if strings.Contains(w, "token_budget_exhausted") {
				stopped = true
			}
		}
	}
	if !stopped {
		t.Fatalf("应触发 token_budget_exhausted；turns=%d", len(run.Turns))
	}
}

// TestSWRR 大样本下平滑轮转比例收敛到权重，且小用户数精确分派。
func TestSWRR(t *testing.T) {
	var p Profile
	if err := json.Unmarshal([]byte(testProfileJSON), &p); err != nil {
		t.Fatal(err)
	}
	s := newSWRR(&p)
	count := map[string]int{}
	n := 10000
	for i := 0; i < n; i++ {
		count[s.next()]++
	}
	for name, want := range map[string]float64{"light": 0.6, "medium": 0.3, "heavy": 0.1} {
		got := float64(count[name]) / float64(n)
		if got < want-0.01 || got > want+0.01 {
			t.Fatalf("%s 分派比例 %.3f 偏离 %.2f", name, got, want)
		}
	}
	// 小用户数：前 10 个用户 6:3:1 精确分派（回归：字母序导致 heavy 霸占前段）
	s2 := newSWRR(&p)
	small := map[string]int{}
	for i := 0; i < 10; i++ {
		small[s2.next()]++
	}
	if small["light"] != 6 || small["medium"] != 3 || small["heavy"] != 1 {
		t.Fatalf("10 用户 6:3:1 分派错误: %v", small)
	}
}
