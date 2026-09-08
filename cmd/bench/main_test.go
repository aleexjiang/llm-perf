package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/report"
)

// resolveOutPath 三种输入形态：空 → outputDir/默认名；.json 后缀 → 原样；其他 → 视为目录拼接默认名。
func TestResolveOutPath(t *testing.T) {
	def := report.DefaultName("single")
	if def == "" || !strings.HasSuffix(def, ".json") {
		t.Fatalf("DefaultName 异常: %q", def)
	}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"空 → 配置输出目录 + 默认名", "", filepath.Join("output", def)},
		{".json 后缀 → 原样", "result/r1.json", "result/r1.json"},
		{"目录 → 拼接默认名", "results", filepath.Join("results", def)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveOutPath(c.in, "output", "single"); got != c.want {
				t.Fatalf("resolveOutPath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// modelDirName：取 "/" 后末段 + 非法字符清洗；空段回退 unknown
func TestModelDirName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/models/DeepSeek-V4-Flash-0731", "DeepSeek-V4-Flash-0731"},
		{"Qwen3.8-27B", "Qwen3.8-27B"},
		{"openai/gpt-4:latest", "gpt-4-latest"}, // ":" 清洗
		{"/models/中文 模型", "中文-模型"},              // 空格清洗，中文保留（Unicode 感知，避免碰撞）
		{"/models/..", "unknown"},               // 全是点：防路径逃逸，回退
	}
	for _, c := range cases {
		if got := modelDirName(c.in); got != c.want {
			t.Fatalf("modelDirName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// applyThinkingCLI：-thinking off/on 是合并后的变体名过滤（优先级 CLI > overrides > 全局），
// 不做全局 mode 覆写——回归 2026-09-08 现场 bug（overrides mode=both 反超 CLI off，on 照跑）
func TestApplyThinkingCLIFilter(t *testing.T) {
	newCfg := func() *config.Config {
		return &config.Config{
			Thinking: config.Thinking{Mode: "off"},
			ModelOverrides: map[string]*config.ModelOverride{
				"/models/Qwen3.8-27B": {Thinking: &config.Thinking{Mode: "both"}},
			},
		}
	}
	// off：DeepSeek [off]；Qwen 合并后 [off on] 被过滤成 [off]——overrides 不得反超 CLI
	cfg := newCfg()
	if err := applyThinkingCLI(cfg, "off"); err != nil {
		t.Fatal(err)
	}
	if vs := cfg.ThinkingFor("/models/DeepSeek").Variants(); len(vs) != 1 || vs[0].Name != "off" {
		t.Fatalf("DeepSeek 应只剩 off: %+v", vs)
	}
	if vs := cfg.ThinkingFor("/models/Qwen3.8-27B").Variants(); len(vs) != 1 || vs[0].Name != "off" {
		t.Fatalf("Qwen 应被过滤成只剩 off: %+v", vs)
	}
	// on：DeepSeek 无 on 变体 → 空（整模型跳过）；Qwen 只剩 on
	cfg = newCfg()
	if err := applyThinkingCLI(cfg, "on"); err != nil {
		t.Fatal(err)
	}
	if vs := cfg.ThinkingFor("/models/DeepSeek").Variants(); len(vs) != 0 {
		t.Fatalf("DeepSeek 无 on 变体应为空: %+v", vs)
	}
	if vs := cfg.ThinkingFor("/models/Qwen3.8-27B").Variants(); len(vs) != 1 || vs[0].Name != "on" {
		t.Fatalf("Qwen 应只剩 on: %+v", vs)
	}
	// both：不过滤
	cfg = newCfg()
	if err := applyThinkingCLI(cfg, "both"); err != nil {
		t.Fatal(err)
	}
	if vs := cfg.ThinkingFor("/models/Qwen3.8-27B").Variants(); len(vs) != 2 {
		t.Fatalf("both 应跑全部变体: %+v", vs)
	}
	// 未知名报错
	if err := applyThinkingCLI(newCfg(), "low"); err == nil {
		t.Fatal("不存在的变体名应报错")
	}
}
