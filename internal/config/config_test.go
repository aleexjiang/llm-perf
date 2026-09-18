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

// UnknownFields(true)：配置项拼错（如 maxtokens）会被静默忽略，现场跑完才发现没生效
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

func TestThinkingMaxTokensFloorZeroDisablesProtection(t *testing.T) {
	cfg, err := Load(writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
thinking:
  max_tokens_floor: 0
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Thinking.MaxTokensFloorValue(); got != 0 {
		t.Fatalf("显式 floor=0 应保持关闭，得到 %d", got)
	}
	if got := cfg.Thinking.MaxTokens(16, ThinkingVariant{Enabled: true}); got != 16 {
		t.Fatalf("显式 floor=0 不应抬高 max_tokens，得到 %d", got)
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

func TestLoadRedactsConfigSecrets(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
api_key: "literal-secret"
api_key_env: "PRIVATE_KEY"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "literal-secret" {
		t.Fatalf("运行时认证 key 不应被脱敏: %q", cfg.APIKey)
	}
	if strings.Contains(cfg.Raw, "literal-secret") || strings.Contains(cfg.Raw, "PRIVATE_KEY") {
		t.Fatalf("配置存档仍包含敏感值: %q", cfg.Raw)
	}
	if strings.Count(cfg.Raw, "<redacted>") != 2 {
		t.Fatalf("应脱敏两个认证字段: %q", cfg.Raw)
	}
}

func TestLoadMissingAPIKeyEnvFails(t *testing.T) {
	t.Setenv("LLM_PERF_API_KEY", "")
	_, err := Load(writeTemp(t, `
endpoint: http://x:1/v1
models: [m1]
api_key_env: MISSING_KEY_FOR_TEST
`))
	if err == nil || !strings.Contains(err.Error(), "api_key_env") {
		t.Fatalf("缺失 api_key_env 应失败，得到: %v", err)
	}
}

// 随仓库的每份配置都必须能通过严格解析（防止加字段后忘了同步模板）
func TestShippedConfigsParse(t *testing.T) {
	for _, name := range []string{"example.yaml", "smoke-probe.yaml", "smoke-user.yaml", "smoke-rps.yaml", "smoke-conc.yaml"} {
		p := filepath.Join("..", "..", "configs", name)
		if _, err := Load(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// corpus_path 的「按配置目录解析相对路径」只对文件路径生效；
// en/zh 是内置语料哨兵，必须原样透传。
func TestCorpusPathBuiltinNotJoined(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
corpus_path: "my.txt.gz"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(p), "my.txt.gz"); cfg.CorpusPath != want {
		t.Errorf("文件路径应按配置目录解析: got %q want %q", cfg.CorpusPath, want)
	}
	// 内置哨兵不拼接
	cfg2, err := Load(writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\ncorpus_path: \"en\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.CorpusPath != "en" {
		t.Errorf("内置语料哨兵被当成路径拼接了: %q", cfg2.CorpusPath)
	}
}

// user/request_set 的相对路径以配置目录为基准（profile/sharegpt 都可能放配置旁）
func TestUserAndRequestSetPathJoin(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
user:
  profile_path: "profiles/agent-v1.json"
request_set:
  sharegpt_path: "data/sharegpt.json"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(p)
	if want := filepath.Join(dir, "profiles/agent-v1.json"); cfg.User.ProfilePath != want {
		t.Errorf("profile_path 应按配置目录解析: got %q want %q", cfg.User.ProfilePath, want)
	}
	if want := filepath.Join(dir, "data/sharegpt.json"); cfg.RequestSet.ShareGPTPath != want {
		t.Errorf("sharegpt_path 应按配置目录解析: got %q want %q", cfg.RequestSet.ShareGPTPath, want)
	}
}

// levels 模式下 extra_body_on/off 为空；probe 靠 ProbeExtraBodies 从 levels 兜底取开关两态。
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
			MaxTokensFloor: intPtr(8192),
			ExtraBodyOn:    map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}},
		},
		ModelOverrides: map[string]*ModelOverride{
			"/models/Qwen3.8-27B": {Thinking: &Thinking{
				Levels: []LevelVariant{{Name: "low", Enabled: true, ExtraBody: map[string]any{"reasoning_effort": "low"}}},
			}},
			"/models/DeepSeek": {Thinking: &Thinking{MaxTokensFloor: intPtr(16384)}},
		},
	}
	q := c.ThinkingFor("/models/Qwen3.8-27B")
	if len(q.Levels) != 1 || q.Mode != "both" || q.MaxTokensFloorValue() != 8192 {
		t.Fatalf("Qwen: levels 覆盖，mode/floor 应继承全局: %+v", q)
	}
	d := c.ThinkingFor("/models/DeepSeek")
	if d.MaxTokensFloorValue() != 16384 || len(d.Levels) != 0 || d.Mode != "both" {
		t.Fatalf("DeepSeek: 只覆盖 floor，其余继承: %+v", d)
	}
	g := c.ThinkingFor("/models/未配置的模型")
	if g.Mode != "both" || g.MaxTokensFloorValue() != 8192 || len(g.Levels) != 0 {
		t.Fatalf("未覆盖模型应返回全局: %+v", g)
	}
	// CLI filter 继承到每个模型的生效配置
	c.Thinking.SetFilter("low")
	if got := c.ThinkingFor("/models/Qwen3.8-27B").Variants(); len(got) != 1 || got[0].Name != "low" {
		t.Fatalf("filter 应继承进 ThinkingFor: %+v", got)
	}
	// ForModel 视图的 thinking 与 ThinkingFor 一致（重复调用幂等）
	fm := c.ForModel("/models/DeepSeek")
	if fm.Thinking.MaxTokensFloorValue() != 16384 || len(fm.Thinking.Levels) != 0 {
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
func intPtr(v int) *int    { return &v }

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

// normalizeMaxTokens：排序去重 + 非正值报错
func TestNormalizeMaxTokens(t *testing.T) {
	got, changed, err := normalizeMaxTokens(IntList{512, 128, 512}, "x")
	if err != nil || !changed {
		t.Fatalf("排序去重: %v %v", got, err)
	}
	if len(got) != 2 || got[0] != 128 || got[1] != 512 {
		t.Fatalf("应排序去重为 [128 512]: %v", got)
	}
	if _, _, err := normalizeMaxTokens(IntList{0}, "x"); err == nil {
		t.Fatal("非正值应报错")
	}
	if _, changed, _ := normalizeMaxTokens(IntList{256}, "x"); changed {
		t.Fatal("单值不应有修正")
	}
}

// user 模式启用时 max_tokens 默认 256 + 归一化生效
func TestLoad_UserDefaults(t *testing.T) {
	p := writeTemp(t, `
endpoint: "http://x:1/v1"
models: ["m1"]
user:
  profile_path: "profile.json"
  max_tokens: [512, 256, 512]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.User.MaxTokens) != 2 || cfg.User.MaxTokens[0] != 256 || cfg.User.MaxTokens[1] != 512 {
		t.Fatalf("user.max_tokens 应排序去重: %v", cfg.User.MaxTokens)
	}
	joined := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(joined, "user.max_tokens") {
		t.Fatalf("应有排序去重提示: %v", cfg.Warnings)
	}
	// users 缺省 = 1；shared_base 缺省 = true
	if cfg.User.GetUsers() != 1 || !cfg.User.GetSharedBase() {
		t.Fatalf("user 默认值错误: users=%d shared=%v", cfg.User.GetUsers(), cfg.User.GetSharedBase())
	}
}

// request_set 默认值与校验
func TestLoad_RequestSetDefaults(t *testing.T) {
	cfg, err := Load(writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\nrequest_set:\n  sharegpt_path: \"x.json\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequestSet.NumPrompts != 100 {
		t.Fatalf("num_prompts 默认应 100: %d", cfg.RequestSet.NumPrompts)
	}
	if cfg.RequestSet.Seed != 0 {
		t.Fatalf("seed 默认应 0: %d", cfg.RequestSet.Seed)
	}
	// 负值拒绝
	if _, err := Load(writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\nrequest_set:\n  num_prompts: -1\n")); err == nil {
		t.Fatal("num_prompts 负值应报错")
	}
}

// rps.rates / concurrency.levels 值域校验
func TestLoad_RPSAndConcurrencyValidation(t *testing.T) {
	if _, err := Load(writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\nrps:\n  rates: [2, 0]\n")); err == nil {
		t.Fatal("rps.rates 含 0 应报错")
	}
	if _, err := Load(writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\nconcurrency:\n  levels: [1, -2]\n")); err == nil {
		t.Fatal("concurrency.levels 含负值应报错")
	}
	cfg, err := Load(writeTemp(t, "endpoint: \"http://x:1/v1\"\nmodels: [\"m1\"]\nconcurrency:\n  levels: [2, 2, 4]\nrps:\n  rates: [1]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Concurrency.Levels) != 2 || cfg.Concurrency.Levels[0] != 2 || cfg.Concurrency.Levels[1] != 4 {
		t.Fatalf("levels 应去重保序: %v", cfg.Concurrency.Levels)
	}
	joined := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(joined, "重复档位") {
		t.Fatalf("重复档位应有提示: %v", cfg.Warnings)
	}
	// burstiness 缺省 = 1（泊松）
	if got := cfg.RPS.GetBurstiness(); got != 1 {
		t.Fatalf("rps.burstiness 缺省应 1: %v", got)
	}
}

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

	// 旧顶层 goodput 已合流：KnownFields(true) 应直接拒绝（schema 不保兼容）
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

// thinking.levels 保留字（on/off/both）fail-fast：CLI 按变体名过滤时永远无法选中
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

// 12.8：levels 模式下 ExtraBodyOff 从 enabled=false 档兜底；显式 extra_body_off 优先
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
