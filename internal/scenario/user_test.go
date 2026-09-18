package scenario

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/engine"
)

const testProfileJSON = `{
  "version": 1,
  "generated_at": "2026-09-18T00:00:00Z",
  "source": "test",
  "first_turn_tokens": [35000, 40000],
  "profiles": {
    "light":  {"weight": 0.6, "turns_range": [2, 4], "user_input_tokens": [80, 300], "context_tokens": [500, 2000]},
    "medium": {"weight": 0.3, "turns_range": [5, 8], "user_input_tokens": [80, 300], "context_tokens": [500, 2000]},
    "heavy":  {"weight": 0.1, "turns_range": [9],    "user_input_tokens": [80, 300], "context_tokens": [500, 2000]}
  },
  "cleaning": {}
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
		`{"version":1,"first_turn_tokens":[35000,40000],"profiles":{"light":{"weight":1,"turns_range":[1,2],"user_input_tokens":[1,2],"context_tokens":[1,2]}}}`,
		`{"version":1,"first_turn_tokens":[1000,2000],"profiles":{"light":{"weight":1,"turns_range":[2,3],"user_input_tokens":[1,2],"context_tokens":[1,2]}}}`,
		`{"version":1,"first_turn_tokens":[35000,40000],"profiles":{}}`,
	}
	for i, b := range bad {
		if _, err := LoadProfile(writeProfile(t, b)); err == nil {
			t.Fatalf("非法 profile #%d 应被拒绝", i)
		}
	}
}

// TestUserScenario 集成：profile 驱动的生成式多轮会话。
// 断言：权重 6:3:1 的 SWRR 分派、轮次范围、真实 assistant 进 history（prompt 逐轮增长）、
// 首轮 ≥35K（agent 形状硬约束）、报告 scenario=user 且 Profile 标签落盘。
func TestUserScenario(t *testing.T) {
	srv := sseStub(t, &stubState{})
	cfg := testCfg(t, srv.URL)
	cfg.User = config.User{
		ProfilePath: writeProfile(t, testProfileJSON),
		Users:       10,
		MaxTokens:   config.IntList{16},
	}

	rep, err := UserScenario(context.Background(), cfg, engine.NewClient(srv.URL, "", 30*time.Second, true), "")
	if err != nil {
		t.Fatalf("UserScenario: %v", err)
	}
	if rep.Scenario != "user" {
		t.Fatalf("scenario = %q, want user", rep.Scenario)
	}
	if len(rep.Multiturn) != 10 {
		t.Fatalf("应有 10 个用户会话，得到 %d", len(rep.Multiturn))
	}

	dist := map[string]int{}
	for _, run := range rep.Multiturn {
		if run.Profile == "" {
			t.Fatalf("会话缺 profile 标签: %+v", run)
		}
		dist[run.Profile]++
		// 轮次必须在档位范围内（heavy 单元素 = 下限 9，运行时上限 defaultTurnsUpper=32）
		want := map[string][2]int{"light": {2, 4}, "medium": {5, 8}, "heavy": {9, 32}}[run.Profile]
		if len(run.Turns) < want[0] || len(run.Turns) > want[1] {
			t.Fatalf("profile=%s 轮次 %d 超出 [%d,%d]", run.Profile, len(run.Turns), want[0], want[1])
		}
		// 首轮 prompt ≥35K（stub usage = 字符数/4，包含 system 基座 + 首轮 user/context）
		if first := run.Turns[0].PromptTokens; first < 35000 {
			t.Fatalf("profile=%s 首轮 prompt %dtk < 35K（agent 形状约束被破坏）", run.Profile, first)
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
}

// TestUserScenarioRequiresProfile 未配置 profile_path 应报错。
func TestUserScenarioRequiresProfile(t *testing.T) {
	srv := sseStub(t, &stubState{})
	cfg := testCfg(t, srv.URL)
	if _, err := UserScenario(context.Background(), cfg, engine.NewClient(srv.URL, "", 5*time.Second, true), ""); err == nil {
		t.Fatal("缺 profile_path 应报错")
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
