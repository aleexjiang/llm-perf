package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_Defaults(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Thinking.Mode != "both" {
		t.Errorf("mode default = %q", cfg.Thinking.Mode)
	}
	if cfg.Single.Runs != 3 || cfg.Concurrent.MaxTokens != 256 {
		t.Error("scenario defaults missing")
	}
	if !cfg.StreamEnabled() || !*cfg.IncludeUsage {
		t.Error("stream/include_usage should default true")
	}
}

func TestLoad_UnknownFieldRejected(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
maxtokens: 999
`)
	if _, err := Load(p); err == nil {
		t.Fatal("typo'd field should be rejected")
	}
}

func TestLoad_InvalidThinkingMode(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
thinking: {mode: "maybe"}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("invalid thinking.mode should be rejected")
	}
}

// 认证 key 解析优先级：环境变量 > 字面量 api_key > api_key_env
func TestAPIKeyResolution(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
api_key: "literal-key"
api_key_env: "SOME_MISSING_VAR"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "literal-key" {
		t.Fatalf("api_key literal should be used when env unset: %q", cfg.APIKey)
	}
	t.Setenv("LLM_PERF_API_KEY", "env-key")
	cfg2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.APIKey != "env-key" {
		t.Fatalf("LLM_PERF_API_KEY should have highest priority: %q", cfg2.APIKey)
	}
}

// 随仓库的每份配置都必须能通过严格解析（防止加字段后忘了同步模板）
func TestShippedConfigsParse(t *testing.T) {
	for _, name := range []string{"example.yaml", "smoke.yaml", "smoke-all.yaml", "qwen3.8-27b.yaml"} {
		p := filepath.Join("..", "..", "configs", name)
		if _, err := Load(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// thinking.levels 自定义变体：取代 mode 展开 + CLI 按档位名过滤
func TestThinkingLevelsVariants(t *testing.T) {
	th := Thinking{
		Levels: []LevelVariant{
			{Name: "off", Enabled: false},
			{Name: "low", Enabled: true, ExtraBody: map[string]any{"reasoning_effort": "low"}},
			{Name: "HIGH", Enabled: true, ExtraBody: map[string]any{"reasoning_effort": "high"}},
		},
	}
	all := th.Variants()
	if len(all) != 3 || all[0].Name != "off" || all[2].Name != "HIGH" {
		t.Fatalf("levels 应按声明顺序全量展开: %+v", all)
	}
	// 过滤大小写不敏感
	th.SetFilter("high")
	got := th.Variants()
	if len(got) != 1 || got[0].Name != "HIGH" || got[0].Enabled != true {
		t.Fatalf("filter=high 应命中 HIGH 变体: %+v", got)
	}
	// 过滤不命中 → 空
	th.SetFilter("ultra")
	if got := th.Variants(); len(got) != 0 {
		t.Fatalf("filter=ultra 不应命中任何变体: %+v", got)
	}
	// 未配 levels 时 mode 展开不受影响
	m := Thinking{Mode: "both"}
	if got := m.Variants(); len(got) != 2 || got[0].Name != "off" || got[1].Name != "on" {
		t.Fatalf("mode=both 应展开 off+on: %+v", got)
	}
}

// model_thinking 按模型覆盖：只写要改的字段，其余继承全局；CLI filter 始终继承
func TestThinkingForModelOverride(t *testing.T) {
	c := &Config{
		Thinking: Thinking{
			Mode:           "both",
			MaxTokensFloor: 8192,
			ExtraBodyOn:    map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}},
		},
		ModelThinking: map[string]*Thinking{
			"/models/Qwen3.8-27B": {
				Levels: []LevelVariant{{Name: "low", Enabled: true, ExtraBody: map[string]any{"reasoning_effort": "low"}}},
			},
			"/models/DeepSeek": {MaxTokensFloor: 16384},
		},
	}
	q := c.ThinkingFor("/models/Qwen3.8-27B")
	if len(q.Levels) != 1 || q.Mode != "both" || q.MaxTokensFloor != 8192 {
		t.Fatalf("Qwen: levels 覆盖，mode/floor 应继承全局: %+v", q)
	}
	d := c.ThinkingFor("/models/DeepSeek")
	if d.MaxTokensFloor != 16384 || len(d.Levels) != 0 || d.Mode != "both" {
		t.Fatalf("DeepSeek: 只覆盖 floor，其余继承: %+v", d)
	}
	g := c.ThinkingFor("/models/未配置的模型")
	if g.Mode != "both" || g.MaxTokensFloor != 8192 || len(g.Levels) != 0 {
		t.Fatalf("未覆盖模型应返回全局: %+v", g)
	}
	// CLI filter 继承到每个模型的生效配置
	c.Thinking.SetFilter("low")
	if got := c.ThinkingFor("/models/Qwen3.8-27B").Variants(); len(got) != 1 || got[0].Name != "low" {
		t.Fatalf("filter 应继承进 ThinkingFor: %+v", got)
	}
}

// model_thinking 键不在 models 列表 → 配置报错
func TestModelThinkingUnknownModelError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte("endpoint: \"http://x/v1\"\nmodels: [\"m1\"]\nmodel_thinking:\n  \"m2\":\n    mode: \"on\"\n"), 0644)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "m2") {
		t.Fatalf("未知模型键应报错且提到 m2: %v", err)
	}
}
