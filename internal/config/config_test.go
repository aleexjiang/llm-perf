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

// test 类别：留空 = performance（默认不改行为）；大小写不敏感；非法值报错。
// 断言同时查 cfg.Test 与 cfg.TestKind()——报告侧读的是落盘的 Test 键，
// 归一化必须在 Load 里完成，不能只靠读侧的 TestKind() 兜。
func TestLoad_TestKind(t *testing.T) {
	base := "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\n"
	cases := []struct {
		line string
		want string
	}{
		{"", TestPerformance},
		{"test: benchmark\n", TestBenchmark},
		{"test: SOAK\n", TestSoak},
		{"test: Performance\n", TestPerformance},
	}
	for _, c := range cases {
		cfg, err := Load(writeTemp(t, base+c.line))
		if err != nil {
			t.Fatalf("test=%q: %v", c.line, err)
		}
		if cfg.TestKind() != c.want || cfg.Test != c.want {
			t.Errorf("test=%q: TestKind=%q Test=%q, want %q", c.line, cfg.TestKind(), cfg.Test, c.want)
		}
	}
	// 值域错误：KnownFields 只抓未知键，拼错的**值**要在这里兜住
	if _, err := Load(writeTemp(t, base+"test: benchmar\n")); err == nil {
		t.Error("test 非法值应报错")
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
	for _, name := range []string{"example.yaml", "smoke.yaml", "smoke-all.yaml"} {
		p := filepath.Join("..", "..", "configs", name)
		if _, err := Load(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// filler_corpus 的「按配置目录解析相对路径」只对文件路径生效；
// en/zh 是内置语料哨兵，必须原样透传给 corpus.Load。
// 回归：曾无条件 join，把 en 变成 configs/en，语料加载直接失败。
func TestFillerCorpusBuiltinNotJoined(t *testing.T) {
	p := filepath.Join("..", "..", "configs", "example.yaml")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FillerCorpus != "en" {
		t.Errorf("内置语料哨兵被当成路径拼接了: %q", cfg.FillerCorpus)
	}

	// 文件路径仍以配置文件所在目录为基准
	p2 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
filler_corpus: "my.txt.gz"
`)
	cfg2, err := Load(p2)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(p2), "my.txt.gz"); cfg2.FillerCorpus != want {
		t.Errorf("文件路径应按配置目录解析: got %q want %q", cfg2.FillerCorpus, want)
	}
}

// levels 模式下 extra_body_on/off 为空；probe 靠 ProbeExtraBodies 从 levels 兜底取开关两态。
// 不兜底会让整块思考探测被静默跳过——部署侧关思考这类问题就查不出来。
func TestProbeExtraBodiesFromLevels(t *testing.T) {
	th := Thinking{
		Levels: []LevelVariant{
			{Name: "off", Enabled: false, ExtraBody: map[string]any{"k": "off"}},
			{Name: "on", Enabled: true, ExtraBody: map[string]any{"k": "on"}},
		},
	}
	on, off := th.ProbeExtraBodies()
	if on == nil || off == nil {
		t.Fatalf("levels 兜底失败: on=%v off=%v", on, off)
	}
	if on["k"] != "on" || off["k"] != "off" {
		t.Errorf("应从 levels 取到开启态/关闭态: on=%v off=%v", on, off)
	}

	// 显式 extra_body_on/off 优先于 levels 兜底
	th2 := Thinking{
		ExtraBodyOn:  map[string]any{"k": "explicit-on"},
		ExtraBodyOff: map[string]any{"k": "explicit-off"},
		Levels:       []LevelVariant{{Name: "low", Enabled: true, ExtraBody: map[string]any{"k": "level"}}},
	}
	on2, off2 := th2.ProbeExtraBodies()
	if on2["k"] != "explicit-on" || off2["k"] != "explicit-off" {
		t.Errorf("显式 extra_body_on/off 应优先: %v %v", on2, off2)
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

	// 标量非正值与列表口径一致：显式写 0 应报错而非静默走默认
	p2b := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
single:
  max_tokens: 0
`)
	if _, err := Load(p2b); err == nil {
		t.Error("max_tokens 标量非正值应报错（与列表口径一致）")
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

// 降速熔断（stall_guard）：默认值填充、显式覆盖、关闭语义、非法值报错。
func TestLoad_StallGuard(t *testing.T) {
	// 只写 enabled 与阈值：窗口/冷却/采样周期应补默认值（600/300/2）
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
stall_guard:
  min_tps: 35
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	sg := cfg.StallGuard
	if sg == nil || !sg.StallEnabled() {
		t.Fatal("配置了 stall_guard 段应默认启用")
	}
	if sg.MinTPS != 35 || sg.WindowSeconds != 600 || sg.CooldownSeconds != 300 || sg.SampleSeconds != 2 {
		t.Errorf("默认值填充不符：%+v", sg)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "降速熔断已启用") {
		t.Errorf("应打印启用提示，got: %v", cfg.Warnings)
	}

	// 未配置该段 = 不启用
	p2 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
`)
	cfg2, err := Load(p2)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.StallGuard.StallEnabled() {
		t.Error("未配置 stall_guard 时不应启用")
	}

	// enabled: false = 保留配置但不启用（也不填默认值、不告警）
	p3 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
stall_guard:
  enabled: false
  min_tps: 20
  window_seconds: 600
`)
	cfg3, err := Load(p3)
	if err != nil {
		t.Fatal(err)
	}
	if cfg3.StallGuard.StallEnabled() {
		t.Error("enabled:false 应不启用")
	}

	// 采样间隔比判定窗口还长：永远判不出持续降速，直接报错
	p4 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
stall_guard:
  min_tps: 20
  window_seconds: 5
  sample_seconds: 30
`)
	if _, err := Load(p4); err == nil {
		t.Error("sample_seconds 大于 window_seconds 应报错")
	}

	// 负值报错
	p5 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
stall_guard:
  min_tps: -1
`)
	if _, err := Load(p5); err == nil {
		t.Error("min_tps 为负应报错")
	}
}

// ── stall_guard（降速熔断）：默认值、校验、探针三态 ──

func TestLoad_StallGuardDefaults(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
stall_guard:
  enabled: true
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	sg := cfg.StallGuard
	if sg == nil {
		t.Fatal("stall_guard 段应被解析")
	}
	if !sg.StallEnabled() {
		t.Fatal("enabled: true 应生效")
	}
	if sg.MinTPS != 10 {
		t.Errorf("min_tps default = %v, want 10", sg.MinTPS)
	}
	if sg.WindowSeconds != 600 {
		t.Errorf("window_seconds default = %v, want 600", sg.WindowSeconds)
	}
	if sg.SampleSeconds != 2 {
		t.Errorf("sample_seconds default = %v, want 2", sg.SampleSeconds)
	}
	if sg.CooldownSeconds != 300 {
		t.Errorf("cooldown_seconds default = %v, want 300", sg.CooldownSeconds)
	}
	if len(cfg.Warnings) == 0 || !strings.Contains(strings.Join(cfg.Warnings, ";"), "降速熔断已启用") {
		t.Errorf("启用熔断应有 Warnings 提示: %v", cfg.Warnings)
	}
}

func TestLoad_StallGuardDisabledKeepsZeros(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
stall_guard:
  enabled: false
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	sg := cfg.StallGuard
	if sg.StallEnabled() {
		t.Fatal("enabled: false 应不生效")
	}
	if sg.MinTPS != 0 || sg.WindowSeconds != 0 {
		t.Errorf("禁用时不应填默认值（保留配置原样）: %+v", sg)
	}
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "降速熔断已启用") {
			t.Errorf("禁用时不应有熔断启用提示: %v", cfg.Warnings)
		}
	}
}

func TestLoad_StallGuardAbsentByDefault(t *testing.T) {
	p := writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StallGuard != nil {
		t.Fatalf("未配置段应为 nil: %+v", cfg.StallGuard)
	}
}

func TestLoad_StallGuardNegativeRejected(t *testing.T) {
	cases := map[string]string{
		"min_tps":          "stall_guard:\n  min_tps: -1\n  enabled: false\n",
		"window_seconds":   "stall_guard:\n  window_seconds: -5\n  enabled: false\n",
		"cooldown_seconds": "stall_guard:\n  cooldown_seconds: -3\n  enabled: false\n",
		"sample_seconds":   "stall_guard:\n  sample_seconds: -0.5\n  enabled: false\n",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\n"+y)); err == nil {
				t.Fatalf("%s 为负应报错", name)
			}
		})
	}
}

func TestLoad_StallGuardProbeFactorNegativeRejected(t *testing.T) {
	p := writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\nstall_guard:\n  enabled: false\n  probe_factor: -0.1\n")
	if _, err := Load(p); err == nil {
		t.Fatal("probe_factor 为负应报错（0 = 关闭探针，是合法值）")
	}
}

func TestLoad_StallGuardSampleLargerThanWindow(t *testing.T) {
	p := writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\nstall_guard:\n  window_seconds: 600\n  sample_seconds: 700\n")
	if _, err := Load(p); err == nil {
		t.Fatal("sample_seconds > window_seconds 应报错（永远判不出持续降速）")
	}
}

func TestStallEnabledAndProbeFactorMethods(t *testing.T) {
	var nilCfg *StallGuardCfg
	if nilCfg.StallEnabled() {
		t.Fatal("nil 配置应视为未启用（StallEnabled 需 nil 安全）")
	}
	disabled := false
	if (&StallGuardCfg{Enabled: &disabled}).StallEnabled() {
		t.Fatal("enabled: false 应视为未启用")
	}
	if !(&StallGuardCfg{}).StallEnabled() {
		t.Fatal("Enabled 未配置应默认启用")
	}
	// EffProbeFactor 三态：nil → 默认 2；显式 0 → 关闭；负数 → 防御性归 0
	if got := (&StallGuardCfg{}).EffProbeFactor(); got != 2 {
		t.Errorf("EffProbeFactor 默认 = %v, want 2", got)
	}
	zero := 0.0
	if got := (&StallGuardCfg{ProbeFactor: &zero}).EffProbeFactor(); got != 0 {
		t.Errorf("EffProbeFactor 显式 0 = %v, want 0", got)
	}
	neg := -1.0
	if got := (&StallGuardCfg{ProbeFactor: &neg}).EffProbeFactor(); got != 0 {
		t.Errorf("EffProbeFactor 负数防御 = %v, want 0", got)
	}
	pf := 1.5
	if got := (&StallGuardCfg{ProbeFactor: &pf}).EffProbeFactor(); got != 1.5 {
		t.Errorf("EffProbeFactor 显式 1.5 = %v", got)
	}
}

// ── 5.7 ramp：默认开 / 显式关 / factor 校验与回落 ──

func TestLoad_RampDefaults(t *testing.T) {
	p := writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Concurrent.RampEnabled() {
		t.Fatal("ramp 未配置应默认开启（nil = true）")
	}
	if cfg.Concurrent.EffRampFactor() != 2 {
		t.Fatalf("ramp_factor 默认 = %d, want 2", cfg.Concurrent.EffRampFactor())
	}
}

func TestLoad_RampNegativeRejected(t *testing.T) {
	p := writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\nconcurrent:\n  ramp_factor: -1\n")
	if _, err := Load(p); err == nil {
		t.Fatal("ramp_factor 为负应报错")
	}
}

func TestLoad_RampFactorOneWarns(t *testing.T) {
	p := writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\nconcurrent:\n  ramp_factor: 1\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, ";"), "串行发车") {
		t.Errorf("ramp_factor=1 应有串行发车 Warnings: %v", cfg.Warnings)
	}
	if cfg.Concurrent.EffRampFactor() != 2 {
		t.Fatalf("factor=1 应回落 2（EffRampFactor），得到 %d", cfg.Concurrent.EffRampFactor())
	}
}

func TestRampEnabledExplicitFalse(t *testing.T) {
	off := false
	if (&Concurrent{Ramp: &off}).RampEnabled() {
		t.Fatal("ramp: false 应关闭爬坡（退回齐射）")
	}
	if !(Concurrent{}).RampEnabled() {
		t.Fatal("Ramp nil 应默认开")
	}
	if got := (Concurrent{RampFactor: 3}).EffRampFactor(); got != 3 {
		t.Fatalf("显式 3 应保持 3，得到 %d", got)
	}
}

// ── slo 段（goodput 合流 + 基线阈值透出，2026-09-12）──

func TestLoad_SLOGoodput(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
slo:
  goodput:
    ttft_ms: 2000
    tpot_ms: 200
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.EffGoodput()
	if g == nil || g.TTFTMS != 2000 || g.TPOTMS != 200 {
		t.Fatalf("slo.goodput 解析不符：%+v", g)
	}

	// 旧顶层 goodput 已合流：KnownFields(true) 应直接拒绝（schema 不保兼容，2026-09-08 拍板）
	p2 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
goodput:
  ttft_ms: 2000
`)
	if _, err := Load(p2); err == nil {
		t.Error("顶层 goodput: 已迁入 slo.goodput，旧写法应报未知字段错误")
	}

	// 全 0 阈值拒绝
	p3 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
slo:
  goodput:
    ttft_ms: 0
    tpot_ms: 0
`)
	if _, err := Load(p3); err == nil {
		t.Error("goodput 全 0 阈值应报错")
	}
}

func TestLoad_SLOBaseline(t *testing.T) {
	off := false
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
slo:
  baseline:
    short_max_tokens: 8000
    long_min_tokens: 48000
    short_good_ttft: 0.3
    good_tpot: 50
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.EffBaseline()
	if b == nil {
		t.Fatal("slo.baseline 应解析出配置")
	}
	if !b.BaselineEnabled() {
		t.Error("enabled 未写应默认开")
	}
	if b.ShortMaxTokens != 8000 || b.LongMinTokens != 48000 || b.ShortGoodTTFT != 0.3 || b.GoodTPOT != 50 {
		t.Fatalf("baseline 字段解析不符：%+v", b)
	}
	// 未写阈值 = 零值（报告侧回落内置默认）
	if b.ShortPassTTFT != 0 || b.PassTPS != 0 {
		t.Errorf("未写字段应保持零值待报告回落：%+v", b)
	}

	// enabled: false = 保留阈值但不渲染
	p2 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
slo:
  baseline:
    enabled: false
`)
	cfg2, err := Load(p2)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.EffBaseline().BaselineEnabled() {
		t.Error("enabled:false 应不渲染基线评估")
	}
	_ = off

	// long_min_tokens ≤ short_max_tokens 报错
	p3 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
slo:
  baseline:
    short_max_tokens: 8000
    long_min_tokens: 8000
`)
	if _, err := Load(p3); err == nil {
		t.Error("long_min_tokens ≤ short_max_tokens 应报错")
	}

	// 负值报错
	p4 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
slo:
  baseline:
    good_tps: -1
`)
	if _, err := Load(p4); err == nil {
		t.Error("baseline 阈值为负应报错")
	}
}

// ── saturation_guard（饱和止损，2026-09-12）──

func TestLoad_SaturationGuard(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
server_metrics: true
saturation_guard:
  max_waiting: 32
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	sg := cfg.SaturationGuard
	if sg == nil || !sg.SatEnabled() {
		t.Fatal("配置了 saturation_guard 段应默认启用")
	}
	if sg.MaxWaiting != 32 || sg.WindowSeconds != 120 || sg.SampleSeconds != 5 {
		t.Errorf("默认值填充不符：%+v", sg)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "饱和止损已启用") {
		t.Errorf("应打印启用提示: %v", cfg.Warnings)
	}

	// 未配 server_metrics：waiting 判定不生效的提示
	p2 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
saturation_guard:
  max_waiting: 32
`)
	cfg2, err := Load(p2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cfg2.Warnings, "\n"), "server_metrics") {
		t.Errorf("未开 server_metrics 应有提示: %v", cfg2.Warnings)
	}

	// 段写了但两个阈值都为 0：不生效提示
	p3 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
saturation_guard:
  window_seconds: 60
`)
	cfg3, err := Load(p3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cfg3.Warnings, "\n"), "没有判定阈值") {
		t.Errorf("无阈值应有提示: %v", cfg3.Warnings)
	}

	// enabled: false = 保留配置但不启用
	p4 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
saturation_guard:
  enabled: false
  max_waiting: 32
`)
	cfg4, err := Load(p4)
	if err != nil {
		t.Fatal(err)
	}
	if cfg4.SaturationGuard.SatEnabled() {
		t.Error("enabled:false 应不启用")
	}

	// 负值 / 采样长于窗口报错
	p5 := writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\nsaturation_guard:\n  max_waiting: -1\n")
	if _, err := Load(p5); err == nil {
		t.Error("max_waiting 为负应报错")
	}
	p6 := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
saturation_guard:
  max_waiting: 32
  window_seconds: 5
  sample_seconds: 30
`)
	if _, err := Load(p6); err == nil {
		t.Error("sample_seconds 大于 window_seconds 应报错")
	}
}

// 12.7：levels 档位名不得使用保留字（on/off/both，大小写不敏感）——CLI --thinking
// 按变体名过滤，档位恰好叫保留字时永远无法通过 CLI 选中。fail-fast 在加载时报错。
func TestThinkingLevelsReservedNames(t *testing.T) {
	base := "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\n"
	for _, name := range []string{"off", "ON", "Both", "oFf"} {
		yaml := base + "thinking:\n  levels:\n    - name: " + name + "\n      enabled: false\n"
		p := writeTemp(t, yaml)
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "保留字") {
			t.Errorf("档位名 %q 应报保留字错误，got %v", name, err)
		}
	}
	// model_overrides 里的 levels 同样校验
	yaml := base + "model_overrides:\n  \"m1\":\n    thinking:\n      levels:\n        - name: both\n          enabled: true\n"
	p := writeTemp(t, yaml)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "model_overrides") {
		t.Errorf("model_overrides 内保留字应报错且带位置，got %v", err)
	}
	// 普通名字不受影响
	ok := writeTemp(t, base+"thinking:\n  levels:\n    - name: none\n      enabled: false\n    - name: low\n      enabled: true\n")
	if _, err := Load(ok); err != nil {
		t.Errorf("普通档位名不应报错: %v", err)
	}
}

// 12.8：levels 模式下 ExtraBodyOn/Off 废弃不回填 → 金丝雀/预热裸发 → 思考吃光
// max_tokens → match=false（r4 实测）。ThinkingFor 从 levels 第一个 enabled=false
// 的变体兜底 ExtraBodyOff；显式 extra_body_off 优先；无关闭档时保持 nil。
func TestThinkingForBackfillsExtraBodyOff(t *testing.T) {
	c := &Config{
		Models: []string{"m1"},
		Thinking: Thinking{
			Levels: []LevelVariant{
				{Name: "none", Enabled: false, ExtraBody: map[string]any{"k": "off-body"}},
				{Name: "low", Enabled: true, ExtraBody: map[string]any{"k": "low-body"}},
			},
		},
	}
	q := c.ThinkingFor("m1")
	if q.ExtraBodyOff == nil || q.ExtraBodyOff["k"] != "off-body" {
		t.Fatalf("levels 模式应从 enabled=false 档兜底 ExtraBodyOff: %+v", q)
	}
	// 显式 extra_body_off 优先，不被兜底覆盖
	c2 := &Config{Models: []string{"m1"}, Thinking: Thinking{
		ExtraBodyOff: map[string]any{"k": "explicit"},
		Levels:       []LevelVariant{{Name: "none", Enabled: false, ExtraBody: map[string]any{"k": "off-body"}}},
	}}
	if got := c2.ThinkingFor("m1").ExtraBodyOff["k"]; got != "explicit" {
		t.Fatalf("显式 extra_body_off 应优先，got %v", got)
	}
	// 全 enabled 档：无兜底来源，保持 nil
	c3 := &Config{Models: []string{"m1"}, Thinking: Thinking{
		Levels: []LevelVariant{{Name: "low", Enabled: true}},
	}}
	if got := c3.ThinkingFor("m1").ExtraBodyOff; got != nil {
		t.Fatalf("无关闭档时 ExtraBodyOff 应保持 nil，got %v", got)
	}
}

// ── 10.5 时长制 soak：duration_seconds + renew 校验（fail-fast 五态）──

func TestLoad_SoakDurationEnablesAndWarns(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
concurrent:
  levels: [2]
  duration_seconds: 30
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrent.DurationSeconds != 30 || cfg.Concurrent.Renew {
		t.Fatalf("duration_seconds=%d renew=%v, want 30/false",
			cfg.Concurrent.DurationSeconds, cfg.Concurrent.Renew)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, ";"), "时长制") {
		t.Errorf("duration 生效应有 Warnings: %v", cfg.Warnings)
	}
}

func TestLoad_SoakDurationOpenLoopRejected(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
concurrent:
  request_rate: 4
  duration_seconds: 30
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "闭环") {
		t.Fatalf("开环 + duration_seconds 应报闭环专属错误, got %v", err)
	}
}

func TestLoad_SoakDurationTraceRejected(t *testing.T) {
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "trace.json")
	if err := os.WriteFile(tracePath, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
dataset:
  mode: trace
  path: `+tracePath+`
concurrent:
  levels: [2]
  duration_seconds: 30
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "trace") {
		t.Fatalf("trace + duration_seconds 应报错（filler 先行）, got %v", err)
	}
}

func TestLoad_SoakMultiturnDurationNeedsRenew(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
concurrent:
  multiturn: true
  levels: [2]
  duration_seconds: 30
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "renew") {
		t.Fatalf("多轮 + duration_seconds 未开 renew 应报错, got %v", err)
	}
}

func TestLoad_SoakRenewNeedsDuration(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
concurrent:
  multiturn: true
  levels: [2]
  renew: true
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "duration_seconds") {
		t.Fatalf("renew 无 duration_seconds 应报错, got %v", err)
	}
}

func TestLoad_SoakRenewNeedsMultiturn(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
concurrent:
  levels: [2]
  duration_seconds: 30
  renew: true
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "多轮") {
		t.Fatalf("renew 非多轮应报错, got %v", err)
	}
}
