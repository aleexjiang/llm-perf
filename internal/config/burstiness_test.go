package config

import (
	"strings"
	"testing"
)

// burstiness（开环到达突发度）：默认 1（标准泊松）、非法值报错、闭环模式作用域提示。
func TestLoad_BurstinessDefaults(t *testing.T) {
	p := writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrent.EffBurstiness() != 1 {
		t.Fatalf("burstiness 默认应为 1（标准泊松），得到 %v", cfg.Concurrent.EffBurstiness())
	}
}

func TestLoad_BurstinessNegativeRejected(t *testing.T) {
	p := writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\nconcurrent:\n  burstiness: -0.5\n")
	if _, err := Load(p); err == nil {
		t.Fatal("burstiness 为负应报错")
	}
}

func TestLoad_BurstinessClosedLoopScopeWarns(t *testing.T) {
	p := writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\nconcurrent:\n  levels: [2]\n  burstiness: 4\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, ";"), "burstiness 仅在开环模式") {
		t.Errorf("闭环下配置 burstiness 应有作用域提示: %v", cfg.Warnings)
	}
}

func TestLoad_BurstinessExplicitOneKept(t *testing.T) {
	p := writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\nconcurrent:\n  burstiness: 2.5\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrent.EffBurstiness() != 2.5 {
		t.Fatalf("显式 2.5 应保持，得到 %v", cfg.Concurrent.EffBurstiness())
	}
}

// raw_timings 原始 chunk 序列开关：nil = 默认开；显式 false 生效。
func TestLoad_RawTimingsDefaultOn(t *testing.T) {
	p := writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RawTimingsEnabled() {
		t.Fatal("raw_timings 未配置应默认开")
	}
}

func TestLoad_RawTimingsExplicitOff(t *testing.T) {
	p := writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\nraw_timings: false\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RawTimingsEnabled() {
		t.Fatal("raw_timings: false 应生效")
	}
}
