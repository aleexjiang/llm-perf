package config

import "testing"

func clampCfg() *Config {
	return &Config{MaxPromptTokens: 262_144}
}

func TestClampLadder(t *testing.T) {
	cfg := clampCfg()
	out, clamped := cfg.ClampLadder([]int{4_000, 100_000, 300_000, 1_000_000})
	want := []int{4_000, 100_000, 262_144}
	if !clamped {
		t.Fatal("应报告发生截断")
	}
	if len(out) != len(want) {
		t.Fatalf("截断结果 %v ≠ %v", out, want)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("截断结果 %v ≠ %v", out, want)
		}
	}
	// 去重：多个超限档位收敛为同一个上限
	out, _ = cfg.ClampLadder([]int{300_000, 500_000, 1_000_000})
	if len(out) != 1 || out[0] != 262_144 {
		t.Fatalf("超限档位应去重为一个上限: %v", out)
	}
}

func TestClampDisabled(t *testing.T) {
	cfg := &Config{} // MaxPromptTokens=0 不限制
	out, clamped := cfg.ClampLadder([]int{1_000_000})
	if clamped || out[0] != 1_000_000 {
		t.Fatalf("未配置截止时不应截断: %v clamped=%v", out, clamped)
	}
	if cfg.ClampOne(1_000_000) != 1_000_000 {
		t.Fatal("ClampOne 未配置截止时不应截断")
	}
}

func TestLargestPromptTokens(t *testing.T) {
	cfg := &Config{
		MaxPromptTokens: 262_144,
		Multiturn:       Multiturn{SystemTokens: 800, ToolDefsTokens: 800, Turns: 4, TurnTokens: 100_000},
		Concurrent:      Concurrent{PromptTokens: 10_240},
	}
	// 多轮最大深度超过模型截止，被截到 262144；并发 prompt 为 10240。
	if got := cfg.LargestPromptTokens(); got != 262_144 {
		t.Fatalf("LargestPromptTokens=%d ≠ 262144", got)
	}
	cfg2 := &Config{
		Multiturn:  Multiturn{SystemTokens: 800, ToolDefsTokens: 800, Turns: 4, TurnTokens: 500},
		Concurrent: Concurrent{PromptTokens: 10_240},
	}
	if got := cfg2.LargestPromptTokens(); got != 10_240 {
		t.Fatalf("LargestPromptTokens=%d ≠ 10240", got)
	}
}
