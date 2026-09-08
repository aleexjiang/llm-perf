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
	if cfg.Single.Runs != 3 || len(cfg.Concurrent.MaxTokens) != 1 || cfg.Concurrent.MaxTokens[0] != 256 {
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

// model_overrides 按模型覆盖思考配置：只写要改的字段，其余继承全局；CLI filter 始终继承
func TestThinkingForModelOverride(t *testing.T) {
	c := &Config{
		Thinking: Thinking{
			Mode:           "both",
			MaxTokensFloor: 8192,
			ExtraBodyOn:    map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}},
		},
		ModelOverrides: map[string]*ModelOverride{
			"/models/Qwen3.8-27B": {Thinking: &Thinking{
				Levels: []LevelVariant{{Name: "low", Enabled: true, ExtraBody: map[string]any{"reasoning_effort": "low"}}},
			}},
			"/models/DeepSeek": {Thinking: &Thinking{MaxTokensFloor: 16384}},
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
	// ForModel 视图的 thinking 与 ThinkingFor 一致（重复调用幂等）
	fm := c.ForModel("/models/DeepSeek")
	if fm.Thinking.MaxTokensFloor != 16384 || len(fm.Thinking.Levels) != 0 {
		t.Fatalf("ForModel thinking 覆盖错误: %+v", fm.Thinking)
	}
}

// model_overrides 键不在 models 列表 → 配置报错
func TestModelOverridesUnknownModelError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte("endpoint: \"http://x/v1\"\nmodels: [\"m1\"]\nmodel_overrides:\n  \"m2\":\n    thinking: {mode: \"on\"}\n"), 0644)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "m2") {
		t.Fatalf("未知模型键应报错且提到 m2: %v", err)
	}
}

// enabled 开关：EnabledFor/ActiveModels 语义（未写默认 true、显式 false 剔除、保序）
func TestEnabledForAndActiveModels(t *testing.T) {
	c := &Config{
		Models: []string{"/models/A", "/models/B", "/models/C"},
		ModelOverrides: map[string]*ModelOverride{
			"/models/B": {Enabled: boolPtr(false)},
			"/models/C": {Stream: boolPtr(false)}, // 有覆盖但没写 enabled → 默认参与
		},
	}
	if !c.EnabledFor("/models/A") || c.EnabledFor("/models/B") || !c.EnabledFor("/models/C") {
		t.Fatal("EnabledFor 语义错误：未写默认 true，显式 false 剔除")
	}
	got := c.ActiveModels()
	if len(got) != 2 || got[0] != "/models/A" || got[1] != "/models/C" {
		t.Fatalf("ActiveModels 应剔除 B 且保持声明顺序: %v", got)
	}
}

func boolPtr(b bool) *bool { return &b }

// enabled 全部禁用 → Load 报错；部分禁用 → 提示跳过名单
func TestLoadEnabledSwitch(t *testing.T) {
	all := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1", "m2"]
model_overrides:
  m1: {enabled: false}
  m2: {enabled: false}
`)
	if _, err := Load(all); err == nil || !strings.Contains(err.Error(), "enabled") {
		t.Fatalf("全部禁用应报错: %v", err)
	}
	partial := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1", "m2"]
model_overrides:
  m1: {enabled: false}
`)
	cfg, err := Load(partial)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ActiveModels()) != 1 || cfg.ActiveModels()[0] != "m2" {
		t.Fatalf("部分禁用后应只剩 m2: %v", cfg.ActiveModels())
	}
	joined := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(joined, "m1") {
		t.Fatalf("部分禁用应有跳过提示: %v", cfg.Warnings)
	}
}

// normalizeLadder：排序去重 + 增量 <10% 拒绝 + 非正值报错（模型覆盖段复用同一校验）
func TestNormalizeLadder(t *testing.T) {
	got, changed, err := normalizeLadder([]int{10000, 4000, 10000}, "x")
	if err != nil || !changed {
		t.Fatalf("排序去重: %v %v", got, err)
	}
	if len(got) != 2 || got[0] != 4000 || got[1] != 10000 {
		t.Fatalf("应排序去重为 [4000 10000]: %v", got)
	}
	if _, _, err := normalizeLadder([]int{0}, "x"); err == nil {
		t.Fatal("非正值应报错")
	}
	if _, _, err := normalizeLadder([]int{40000, 41000}, "x"); err == nil {
		t.Fatal("增量 <10% 应报错")
	}
}

// 测量守卫：短输出档的方向性偏悲观提示 + 输入/输出只扫一维的归因提示
func TestLoad_MeasurementGuards(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
single:
  runs: 1
  prompt_tokens: [500, 1000, 2000]
  max_tokens: 64
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(joined, "single.max_tokens=64 偏小") {
		t.Errorf("短输出档应有守卫告警，got: %v", cfg.Warnings)
	}
	if !strings.Contains(joined, "输出长度也做多档对照") {
		t.Errorf("输入多档/输出单档应有归因提示，got: %v", cfg.Warnings)
	}

	// 正常工作点不应误报
	p2 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
single:
  runs: 3
  prompt_tokens: [500]
  max_tokens: 512
`)
	cfg2, err := Load(p2)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range cfg2.Warnings {
		if strings.Contains(w, "系统性偏悲观") || strings.Contains(w, "只扫了一个") {
			t.Errorf("正常配置不应有测量守卫告警: %q", w)
		}
	}
}

// 5.1 输出长度扫描：max_tokens 接受标量或列表；列表排序去重、非正值报错；守卫逐档生效。
func TestLoad_MaxTokensList(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
single:
  prompt_tokens: [500]
  max_tokens: [512, 128, 512]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Single.MaxTokens) != 2 || cfg.Single.MaxTokens[0] != 128 || cfg.Single.MaxTokens[1] != 512 {
		t.Errorf("max_tokens 列表应排序去重为 [128 512]，got %v", cfg.Single.MaxTokens)
	}
	joined := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(joined, "single.max_tokens 已排序去重") {
		t.Errorf("应提示排序去重，got: %v", cfg.Warnings)
	}

	p2 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
single:
  max_tokens: [128, 0]
`)
	if _, err := Load(p2); err == nil {
		t.Error("max_tokens 列表含非正值应报错")
	}

	// 标量写法不变；thinking floor 告警逐档判断（列表中只有 <floor 的档位才提示）
	p3 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
single:
  max_tokens: [64, 4096]
`)
	cfg3, err := Load(p3)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range cfg3.Warnings {
		if strings.Contains(w, "single.max_tokens=64 < max_tokens_floor") {
			found = true
		}
	}
	if !found {
		t.Errorf("列表中 <floor 的档位应触发 floor 告警，got: %v", cfg3.Warnings)
	}
}

// 5.6 混合负载：mix 校验（权重/长度为正、label 补默认查重、与 multiturn 互斥、单值失效告警）。
func TestLoad_MixShapes(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
concurrent:
  levels: [2]
  multiturn: true
  mix:
    - {weight: 7, label: short, prompt_tokens: 500, max_tokens: 64}
    - {weight: 3, label: long, prompt_tokens: 8000, max_tokens: 256}
`)
	if _, err := Load(p); err == nil {
		t.Error("mix 与 multiturn: true 应互斥报错")
	}

	p2 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
concurrent:
  levels: [2]
  mix:
    - {weight: 7, prompt_tokens: 500, max_tokens: 64}
    - {weight: 0, label: bad, prompt_tokens: 8000, max_tokens: 256}
`)
	if _, err := Load(p2); err == nil {
		t.Error("weight=0 应报错")
	}

	p3 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
concurrent:
  levels: [2]
  prompt_tokens: 10000
  mix:
    - {weight: 7, prompt_tokens: 500, max_tokens: 64}
    - {weight: 3, prompt_tokens: 8000, max_tokens: 256}
`)
	cfg, err := Load(p3)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrent.Mix[0].Label != "shape1" || cfg.Concurrent.Mix[1].Label != "shape2" {
		t.Errorf("label 留空应补 shapeN，got %q / %q", cfg.Concurrent.Mix[0].Label, cfg.Concurrent.Mix[1].Label)
	}
	joined := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(joined, "concurrent.mix 已配置") {
		t.Errorf("应提示单值维度失效，got: %v", cfg.Warnings)
	}
}
