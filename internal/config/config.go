// Package config 负责加载 yaml 配置并用环境变量覆盖。
package config

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Single struct {
	Runs         int   `yaml:"runs"`
	PromptTokens []int `yaml:"prompt_tokens"`
	MaxTokens    int   `yaml:"max_tokens"`
	FixedSeed    bool  `yaml:"fixed_seed"`
}

type Multiturn struct {
	Sessions       int  `yaml:"sessions"`
	Turns          int  `yaml:"turns"`
	SystemTokens   int  `yaml:"system_tokens"`
	ToolDefsTokens int  `yaml:"tool_defs_tokens"`
	TurnTokens     int  `yaml:"turn_tokens"`
	MaxTokens      int  `yaml:"max_tokens"`
	KeepAssistant  bool `yaml:"keep_assistant"`
}

type Concurrent struct {
	Levels        []int `yaml:"levels"`
	RunsPerWorker int   `yaml:"runs_per_worker"`
	PromptTokens  int   `yaml:"prompt_tokens"`
	MaxTokens     int   `yaml:"max_tokens"`
	Multiturn     bool  `yaml:"multiturn"` // true=每个虚拟用户各自跑完整多轮会话（会话重放）

	// 开环到达率模式（对齐 vLLM bench serve / inference-perf）：request_rate>0 或 rate_sweep
	// 非空时替代 levels 闭环——请求按 Poisson 过程到达，能测出排队-延迟曲线
	RequestRate    float64   `yaml:"request_rate"`    // 到达率（req/s），>0 启用开环模式
	RateSweep      []float64 `yaml:"rate_sweep"`      // 多档到达率扫描（饱和点寻找），每档跑一轮开环
	NumPrompts     int       `yaml:"num_prompts"`     // 开环模式总请求数（multiturn 时为总会话数）
	MaxConcurrency int       `yaml:"max_concurrency"` // 开环模式并发上限（0=不限）
}

// DatasetCfg 数据源：filler（默认，token 精确的合成/语料填充，用于变量控制实验）
// 或 trace（真实会话回放，贴近客户实际流量分布）。
type DatasetCfg struct {
	Mode        string `yaml:"mode"`         // filler | trace
	Path        string `yaml:"path"`         // trace 文件路径（.json / .json.gz）
	Format      string `yaml:"format"`       // sharegpt | sessions（空=自动识别）
	MinTurns    int    `yaml:"min_turns"`    // 会话最少 user 轮数（sharegpt 过滤），默认 2
	MaxSessions int    `yaml:"max_sessions"` // 最多加载多少会话，0=不限
}

// GoodputCfg SLO 约束（goodput 口径）：同时满足 TTFT 与 TPOT 上限的请求才算有效吞吐。
type GoodputCfg struct {
	TTFTMS float64 `yaml:"ttft_ms"` // 如 2000
	TPOTMS float64 `yaml:"tpot_ms"` // 如 100
}

// RetryCfg 连接层重试策略（默认关闭）：只重试瞬时失败（TCP/流被 reset、HTTP 5xx/429），
// 4xx 不重试。重试会记入 warnings 与 retry_count——计时窗口干净，但服务端不稳定仍可见。
type RetryCfg struct {
	MaxAttempts int `yaml:"max_attempts"` // 总尝试次数；0/1 = 不重试
	BackoffMS   int `yaml:"backoff_ms"`   // 退避基数，默认 300ms，指数退避封顶 5s
}

// CorrectnessCfg 正确性抽查（llmperf 式防"假成功"）：向服务发数字转写金丝雀请求，
// 验证回复确实包含目标数字——结构上 200 但内容异常（缓存污染/截断/网关伪响应）能被揪出。
type CorrectnessCfg struct {
	Samples int `yaml:"samples"` // 每个模型抽查条数，0=关闭
}

// Thinking 思考模式配置。
// 通用透传设计（对齐 vLLM --extra-body）：不硬编码参数名，on/off 两套 JSON 合并进请求体。
type Thinking struct {
	Mode           string         `yaml:"mode"`             // both（默认，A/B 对照）| on | off
	ExtraBodyOn    map[string]any `yaml:"extra_body_on"`    // 思考开启时合并进请求体
	ExtraBodyOff   map[string]any `yaml:"extra_body_off"`   // 思考关闭时合并进请求体
	MaxTokensFloor int            `yaml:"max_tokens_floor"` // 思考开启时 max_tokens 下限保护（防思考吃光输出预算）

	// Levels 自定义思考变体（低/中/高/极高等任意档位）：配置后取代 mode 展开。
	// 不同推理框架参数名不同（OpenAI reasoning_effort / Qwen chat_template_kwargs.thinking_budget /
	// GLM thinking.type 等），统一用 extra_body 透传；probe 会自动探测哪个参数可控并给出建议。
	Levels []LevelVariant `yaml:"levels"`

	filter string // CLI --thinking 变体名过滤（非序列化字段）
}

// LevelVariant 一个自定义思考档位。
type LevelVariant struct {
	Name      string         `yaml:"name"`       // 档位名（进日志与报告分组，如 low/medium/high/ultra）
	Enabled   bool           `yaml:"enabled"`    // 是否处于思考开启状态（true 时享受 max_tokens_floor 保护）
	ExtraBody map[string]any `yaml:"extra_body"` // 合并进请求体的参数
}

// SetFilter CLI 指定变体名过滤（大小写不敏感；不匹配任何变体时 Variants 返回空）。
func (t *Thinking) SetFilter(name string) { t.filter = name }

// VariantNames 返回全部变体名（忽略 CLI 过滤）——CLI --thinking 校验用。
func (t Thinking) VariantNames() []string {
	saved := t.filter
	t.filter = ""
	var names []string
	for _, v := range t.Variants() {
		names = append(names, v.Name)
	}
	t.filter = saved
	return names
}

// ThinkingVariant 是一个思考模式变体。
type ThinkingVariant struct {
	Name      string // "off" / "on" / 自定义档位名
	Enabled   bool
	ExtraBody map[string]any
}

// Variants 展开成变体列表：配了 levels 用 levels（保持声明顺序），否则按 mode 展开
// （both 时先 off 后 on，便于报告对照）；CLI filter 非空时只留名字匹配的变体。
// validateThinkingLevels 校验 levels 变体名非空且唯一（报告与日志按 name 分组，重名会串数据）。
func validateThinkingLevels(th Thinking, where string) error {
	seen := map[string]bool{}
	for _, lv := range th.Levels {
		if lv.Name == "" {
			return fmt.Errorf("%s thinking.levels 变体缺少 name（报告与日志按 name 分组，必须显式命名）", where)
		}
		if seen[lv.Name] {
			return fmt.Errorf("%s thinking.levels 变体名 %q 重复——档位名必须唯一", where, lv.Name)
		}
		seen[lv.Name] = true
	}
	return nil
}

func (t Thinking) Variants() []ThinkingVariant {
	var vs []ThinkingVariant
	if len(t.Levels) > 0 {
		for _, lv := range t.Levels {
			vs = append(vs, ThinkingVariant{Name: lv.Name, Enabled: lv.Enabled, ExtraBody: lv.ExtraBody})
		}
	} else {
		switch t.Mode {
		case "on":
			vs = []ThinkingVariant{{Name: "on", Enabled: true, ExtraBody: t.ExtraBodyOn}}
		case "off":
			vs = []ThinkingVariant{{Name: "off", Enabled: false, ExtraBody: t.ExtraBodyOff}}
		default: // both
			vs = []ThinkingVariant{
				{Name: "off", Enabled: false, ExtraBody: t.ExtraBodyOff},
				{Name: "on", Enabled: true, ExtraBody: t.ExtraBodyOn},
			}
		}
	}
	if t.filter != "" {
		var out []ThinkingVariant
		for _, v := range vs {
			if strings.EqualFold(v.Name, t.filter) {
				out = append(out, v)
			}
		}
		vs = out
	}
	return vs
}

// MaxTokens 对思考开启的变体应用 max_tokens 下限保护。
func (t Thinking) MaxTokens(maxTokens int, v ThinkingVariant) int {
	if v.Enabled && t.MaxTokensFloor > 0 && maxTokens < t.MaxTokensFloor {
		return t.MaxTokensFloor
	}
	return maxTokens
}

// ThinkingFor 返回某模型生效的思考配置：全局 thinking 为底，model_thinking[model] 的
// 非零字段覆盖（零值 = 未写 = 继承全局）；CLI --thinking 的变体过滤始终继承。
func (c *Config) ThinkingFor(model string) *Thinking {
	t := c.Thinking // 值拷贝（map/slice 共享底层数组但只读，安全）
	if ov := c.ModelThinking[model]; ov != nil {
		if ov.Mode != "" {
			t.Mode = ov.Mode
		}
		if ov.ExtraBodyOn != nil {
			t.ExtraBodyOn = ov.ExtraBodyOn
		}
		if ov.ExtraBodyOff != nil {
			t.ExtraBodyOff = ov.ExtraBodyOff
		}
		if ov.MaxTokensFloor > 0 {
			t.MaxTokensFloor = ov.MaxTokensFloor
		}
		if len(ov.Levels) > 0 {
			t.Levels = ov.Levels
		}
	}
	t.filter = c.Thinking.filter
	return &t
}

type Config struct {
	Endpoint       string   `yaml:"endpoint"`
	APIKeyLiteral  string   `yaml:"api_key"`     // 字面量 key，直接写配置文件（该配置文件应避免入库）；环境变量 LLM_PERF_API_KEY 优先级更高
	APIKeyEnv      string   `yaml:"api_key_env"` // 从哪个环境变量读 key（留空则跳过）；字面量 api_key 与环境变量都未提供时不带认证头
	OutputDir      string   `yaml:"output_dir"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
	IncludeUsage   *bool    `yaml:"include_usage"`
	FillerLang     string   `yaml:"filler_lang"`
	FillerCorpus   string   `yaml:"filler_corpus"` // "":合成词表 | "en"/"zh":内置公版书语料 | 文件路径(.txt/.txt.gz):自定义语料
	Stream         *bool    `yaml:"stream"`        // 默认 true；false 时 TTFT/ITL/思考拆分不可测（N/A）
	Debug          bool     `yaml:"debug"`         // true: 每个请求的原始响应留存到 <output_dir>/raw/，日志同步写 run.log（排查魔改引擎用）
	Models         []string `yaml:"models"`

	// MaxPromptTokens 上下文截止（tokens）：>0 时所有请求的 prompt 规模都不超过该值。
	// single 档位超限截到该值并去重；多轮会话 ctx 到顶后停止加轮。0 = 不限制。
	// CLI --max-ctx 可覆盖。建议同时参考 bench probe 报告的模型 max_model_len。
	MaxPromptTokens int `yaml:"max_prompt_tokens"`

	// ServerMetrics 服务端观测层：抓取推理服务原生 /metrics（vLLM 默认暴露），
	// 补充前缀缓存命中率、排队深度、prefill/decode 分解、MTP 接受率（不可达时自动降级并告警）
	ServerMetrics     bool `yaml:"server_metrics"`
	MetricsIntervalMS int  `yaml:"metrics_interval_ms"` // gauge 轮询间隔，默认 500

	// WarmupRequests 每场景开始前的预热请求数（不计入统计）：
	// 暖连接池/首包路径；用唯一内容避免污染被测前缀的缓存对照
	WarmupRequests int `yaml:"warmup_requests"`

	// SeedSalt 种子盐值：所有场景的 prompt 种子都叠加该值。服务端 prefix cache 是内存态、
	// 跨请求存活——同一配置重跑时 prompt 与上次完全相同，"冷缓存"测量会被上次战役污染。
	// 每次测试战役（改代码/改配置后的重测）递增盐值即可隔离；不改服务端也能拿到干净的冷缓存。
	SeedSalt int `yaml:"seed_salt"`

	// Warnings 配置诊断提示（Load 时生成，非序列化字段）：不阻止运行，
	// 但启动时打印——数量级不合理、轮次不足、覆盖关系等"合法但值得知道"的事
	Warnings []string `yaml:"-"`

	Dataset     DatasetCfg      `yaml:"dataset"`
	Goodput     *GoodputCfg     `yaml:"goodput"`
	Retry       *RetryCfg       `yaml:"retry"`
	Correctness *CorrectnessCfg `yaml:"correctness"`

	Thinking Thinking `yaml:"thinking"`

	// ModelThinking 按模型覆盖思考配置（键=模型名，必须在 models 列表内）。
	// 覆盖语义：只写要改的字段，未写的字段继承全局 thinking（零值 = 未写）。
	// 不同模型/引擎的思考参数与档位词汇不同（Qwen thinking_budget / OpenAI reasoning_effort / GLM thinking.type），
	// probe 会探测哪个参数可控；每个模型配各自己的 levels/extra_body。
	ModelThinking map[string]*Thinking `yaml:"model_thinking"`

	Single     Single     `yaml:"single"`
	Multiturn  Multiturn  `yaml:"multiturn"`
	Concurrent Concurrent `yaml:"concurrent"`

	// 运行时解析
	APIKey string `yaml:"-"`
}

// Load 读取配置文件，应用默认值，再用环境变量覆盖。
// 环境变量优先级最高：LLM_PERF_ENDPOINT、LLM_PERF_API_KEY。
func Load(path string) (*Config, error) {
	cfg := &Config{
		OutputDir:      "output",
		TimeoutSeconds: 300,
		FillerLang:     "en",
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		// KnownFields(true)：未知字段报错——配置项拼错（如 maxtokens）会被静默忽略，
		// 现场跑完才发现没生效是最贵的错误
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}

	// env 覆盖
	if v := os.Getenv("LLM_PERF_ENDPOINT"); v != "" {
		cfg.Endpoint = v
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("endpoint 未配置（yaml endpoint 或 LLM_PERF_ENDPOINT）")
	}
	// 认证 key 解析优先级：环境变量 LLM_PERF_API_KEY > 配置字面量 api_key > api_key_env 指向的变量
	if cfg.APIKeyEnv != "" {
		cfg.APIKey = os.Getenv(cfg.APIKeyEnv)
	}
	if cfg.APIKeyLiteral != "" {
		cfg.APIKey = cfg.APIKeyLiteral
	}
	if v := os.Getenv("LLM_PERF_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if len(cfg.Models) == 0 {
		return nil, fmt.Errorf("models 未配置")
	}
	if cfg.IncludeUsage == nil {
		t := true
		cfg.IncludeUsage = &t
	}
	if cfg.Stream == nil {
		t := true
		cfg.Stream = &t
	}
	if cfg.Thinking.Mode == "" {
		cfg.Thinking.Mode = "both"
	}
	switch cfg.Thinking.Mode {
	case "both", "on", "off":
	default:
		return nil, fmt.Errorf("thinking.mode 无效值 %q（可选 both/on/off）", cfg.Thinking.Mode)
	}
	if cfg.Thinking.MaxTokensFloor <= 0 {
		cfg.Thinking.MaxTokensFloor = 2048
	}
	// model_thinking 覆盖校验：模型必须在 models 列表内；mode 值合法
	inModels := make(map[string]bool, len(cfg.Models))
	for _, m := range cfg.Models {
		inModels[m] = true
	}
	for name, ov := range cfg.ModelThinking {
		if ov == nil {
			continue
		}
		if !inModels[name] {
			return nil, fmt.Errorf("model_thinking 键 %q 不在 models 列表中（models: %v）", name, cfg.Models)
		}
		switch ov.Mode {
		case "", "both", "on", "off":
		default:
			return nil, fmt.Errorf("model_thinking[%s].mode 无效值 %q（可选 both/on/off；留空继承全局）", name, ov.Mode)
		}
	}
	// 各场景默认值
	if cfg.Single.Runs <= 0 {
		cfg.Single.Runs = 3
	}
	if len(cfg.Single.PromptTokens) == 0 {
		cfg.Single.PromptTokens = []int{4000, 10000, 20000, 40000}
	}
	if cfg.Single.MaxTokens <= 0 {
		cfg.Single.MaxTokens = 512
	}
	if cfg.Multiturn.Sessions <= 0 {
		cfg.Multiturn.Sessions = 2
	}
	if cfg.Multiturn.Turns <= 0 {
		cfg.Multiturn.Turns = 8
	}
	if cfg.Multiturn.TurnTokens <= 0 {
		cfg.Multiturn.TurnTokens = 2000
	}
	if cfg.Multiturn.MaxTokens <= 0 {
		cfg.Multiturn.MaxTokens = 256
	}
	if len(cfg.Concurrent.Levels) == 0 {
		cfg.Concurrent.Levels = []int{1, 2, 4, 8, 16}
	}
	if cfg.Concurrent.RunsPerWorker <= 0 {
		cfg.Concurrent.RunsPerWorker = 2
	}
	if cfg.Concurrent.PromptTokens <= 0 {
		cfg.Concurrent.PromptTokens = 10000
	}
	if cfg.Concurrent.MaxTokens <= 0 {
		cfg.Concurrent.MaxTokens = 256
	}
	// 新增能力默认值与校验
	if cfg.MetricsIntervalMS <= 0 {
		cfg.MetricsIntervalMS = 500
	}
	switch cfg.Dataset.Mode {
	case "":
		cfg.Dataset.Mode = "filler"
	case "filler", "trace":
	default:
		return nil, fmt.Errorf("dataset.mode 无效值 %q（可选 filler/trace）", cfg.Dataset.Mode)
	}
	if cfg.Dataset.Mode == "trace" && cfg.Dataset.Path == "" {
		return nil, fmt.Errorf("dataset.mode=trace 需要 dataset.path（trace 文件路径）")
	}
	if cfg.Concurrent.RequestRate < 0 {
		return nil, fmt.Errorf("concurrent.request_rate 不能为负")
	}

	// ── 输入合理性校验：错误在开跑前暴露，而不是跑完才发现 ──

	// 单发档位：拒绝非正值；排序去重（被修正时提示）；相邻增量 <10% 拒绝
	if len(cfg.Single.PromptTokens) > 0 {
		for _, t := range cfg.Single.PromptTokens {
			if t <= 0 {
				return nil, fmt.Errorf("single.prompt_tokens 含非正值 %d——档位必须是正整数 token 数", t)
			}
		}
		orig := append([]int(nil), cfg.Single.PromptTokens...)
		sort.Ints(cfg.Single.PromptTokens)
		ded := cfg.Single.PromptTokens[:0]
		for i, t := range cfg.Single.PromptTokens {
			if i == 0 || t != ded[len(ded)-1] {
				ded = append(ded, t)
			}
		}
		cfg.Single.PromptTokens = ded
		changed := len(orig) != len(ded)
		for i := 0; !changed && i < len(orig); i++ {
			changed = orig[i] != ded[i]
		}
		if changed {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"single.prompt_tokens 已排序去重 → %v（原顺序/重复档位不影响结果，但图表与日志按修正后顺序展示）", ded))
		}
		for i := 1; i < len(ded); i++ {
			prev, cur := ded[i-1], ded[i]
			if cur-prev < prev/10 {
				return nil, fmt.Errorf(
					"single.prompt_tokens 档位 %d 与前一档 %d 增量仅 %.1f%%（<10%%）——同量级档位的 TTFT 差异会淹没在请求间抖动里，测了也测不出结论；请拉开差距或删除多余档位（如 40000 之后想探更深，用 60000/80000 而不是 41000）",
					cur, prev, float64(cur-prev)/float64(prev)*100)
			}
		}
	}

	// 多轮可行性：单轮消息过大直接拒绝；轮次不足与可达深度用提示
	if cfg.Multiturn.TurnTokens > 200000 {
		return nil, fmt.Errorf(
			"multiturn.turn_tokens=%d 过大：单条 user 消息大概率超过模型上下文上限，该会话形状无法成立。多轮深度应通过增加轮次实现（如 turns: 16 + turn_tokens: 12300 → 末轮 ~200k），而不是增大单轮",
			cfg.Multiturn.TurnTokens)
	}
	if cfg.Multiturn.Turns < 4 {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
			"multiturn.turns=%d 偏少：TTFT 逐轮斜率与前缀缓存判定至少需要 4 轮才可靠（建议 8–16 轮）", cfg.Multiturn.Turns))
	}
	if cfg.Multiturn.TurnTokens > 0 {
		base := cfg.Multiturn.SystemTokens + cfg.Multiturn.ToolDefsTokens
		reach := base + cfg.Multiturn.Turns*cfg.Multiturn.TurnTokens
		w := fmt.Sprintf("多轮可达深度：base %d + %d 轮 × %d ≈ 末轮 %d token",
			base, cfg.Multiturn.Turns, cfg.Multiturn.TurnTokens, reach)
		if cfg.MaxPromptTokens > 0 && reach > cfg.MaxPromptTokens {
			w += fmt.Sprintf("（超过 max_prompt_tokens=%d，到顶后提前停轮）", cfg.MaxPromptTokens)
		}
		cfg.Warnings = append(cfg.Warnings, w)
	}

	// thinking levels：变体名唯一；levels 生效时提示 mode/extra_body 被覆盖
	if err := validateThinkingLevels(cfg.Thinking, "全局"); err != nil {
		return nil, err
	}
	if len(cfg.Thinking.Levels) > 0 {
		cfg.Warnings = append(cfg.Warnings,
			"thinking.levels 已配置：mode 与 extra_body_on/off 不再参与思考展开（以 levels 为准）")
	}
	for name, ov := range cfg.ModelThinking {
		if ov == nil {
			continue
		}
		if err := validateThinkingLevels(*ov, "model_thinking["+name+"]"); err != nil {
			return nil, err
		}
	}

	// ── 联动与全局合理性校验 ──

	// endpoint 形状：必须带协议头；指向具体接口路径是常见误填
	if !strings.HasPrefix(cfg.Endpoint, "http://") && !strings.HasPrefix(cfg.Endpoint, "https://") {
		return nil, fmt.Errorf("endpoint %q 缺少 http:// 或 https:// 前缀", cfg.Endpoint)
	}
	if strings.HasSuffix(strings.TrimRight(cfg.Endpoint, "/"), "/chat/completions") {
		cfg.Warnings = append(cfg.Warnings,
			"endpoint 指向 /chat/completions——工具会自动拼接接口路径，endpoint 应填到 /v1 为止")
	}
	for _, m := range cfg.Models {
		if strings.TrimSpace(m) == "" {
			return nil, fmt.Errorf("models 含空模型名")
		}
	}

	// 数据源与语言联动
	switch cfg.FillerLang {
	case "", "en", "zh":
	default:
		return nil, fmt.Errorf("filler_lang 无效值 %q（可选 en/zh）", cfg.FillerLang)
	}
	if cfg.Dataset.Mode == "trace" {
		if cfg.FillerCorpus != "" {
			cfg.Warnings = append(cfg.Warnings,
				"dataset.mode=trace：filler_corpus 不生效（语料只用于 filler 模式的 token 填充）")
		}
		if _, err := os.Stat(cfg.Dataset.Path); err != nil {
			return nil, fmt.Errorf("dataset.path 文件不可读: %w", err)
		}
	} else if cfg.FillerCorpus == "en" && cfg.FillerLang == "zh" || cfg.FillerCorpus == "zh" && cfg.FillerLang == "en" {
		cfg.Warnings = append(cfg.Warnings,
			fmt.Sprintf("filler_lang=%s 与 filler_corpus=%s 语言不一致：token 计数口径会偏差，建议两者一致（en 语料配 en，zh 语料配 zh，或语料直接给文件路径）",
				cfg.FillerLang, cfg.FillerCorpus))
	}

	// 超时：缺省/非法兜底；深上下文 + 思考时给足量提示
	if cfg.TimeoutSeconds <= 0 {
		cfg.Warnings = append(cfg.Warnings, "timeout_seconds 未配置或非正，回退 300s")
		cfg.TimeoutSeconds = 300
	}
	thinkMayOn := cfg.Thinking.Mode == "on" || cfg.Thinking.Mode == "both" ||
		(len(cfg.Thinking.Levels) > 0 && anyLevelEnabled(cfg.Thinking))
	deepCtx := false
	if n := len(cfg.Single.PromptTokens); n > 0 && cfg.Single.PromptTokens[n-1] >= 100000 {
		deepCtx = true
	}
	if cfg.Multiturn.TurnTokens > 0 {
		if reach := cfg.Multiturn.SystemTokens + cfg.Multiturn.ToolDefsTokens + cfg.Multiturn.Turns*cfg.Multiturn.TurnTokens; reach >= 100000 {
			deepCtx = true
		}
	}
	if cfg.TimeoutSeconds > 3600 {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("timeout_seconds=%d 超过 1 小时：确认不是把毫秒当秒填了", cfg.TimeoutSeconds))
	}
	if thinkMayOn && deepCtx && cfg.TimeoutSeconds < 300 {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
			"timeout_seconds=%d 偏小：思考开启 + 100k 级上下文时单请求可达 3–8 分钟，会被误判超时", cfg.TimeoutSeconds))
	}

	// runs/sessions 统计充分性
	if cfg.Single.Runs < 2 {
		cfg.Warnings = append(cfg.Warnings,
			"single.runs=1 无法做缓存冷/热对照（run1 vs run2+ 至少要 2 次，建议 3 次）")
	}
	if cfg.Single.Runs >= 2 && !cfg.Single.FixedSeed {
		cfg.Warnings = append(cfg.Warnings,
			"single.fixed_seed=false：每个 run 换 prompt，测的是冷启动分布而非前缀缓存对照（确认是本意）")
	}
	if cfg.Multiturn.Sessions < 2 {
		cfg.Warnings = append(cfg.Warnings,
			"multiturn.sessions=1 无法评估会话间方差，缓存判定建议 ≥2 个会话取中位")
	}

	// 思考 floor 联动：on 时 max_tokens 会被抬高，off/on 的 E2E 口径不同
	if thinkMayOn && cfg.Thinking.MaxTokensFloor > 0 {
		if cfg.Single.MaxTokens > 0 && cfg.Single.MaxTokens < cfg.Thinking.MaxTokensFloor {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"single.max_tokens=%d < max_tokens_floor=%d：thinking=on 的请求会被抬高到 floor，off/on 的 E2E 不可直接横向比（off 受 512 钳制、on 受 floor 抬高）",
				cfg.Single.MaxTokens, cfg.Thinking.MaxTokensFloor))
		}
		if cfg.Multiturn.MaxTokens > 0 && cfg.Multiturn.MaxTokens < cfg.Thinking.MaxTokensFloor {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"multiturn.max_tokens=%d < max_tokens_floor=%d：thinking=on 的请求会被抬高到 floor",
				cfg.Multiturn.MaxTokens, cfg.Thinking.MaxTokensFloor))
		}
	}

	// 多轮深度与单发档位的衔接
	if cfg.Multiturn.TurnTokens > 0 && len(cfg.Single.PromptTokens) > 0 {
		reach := cfg.Multiturn.SystemTokens + cfg.Multiturn.ToolDefsTokens + cfg.Multiturn.Turns*cfg.Multiturn.TurnTokens
		if top := cfg.Single.PromptTokens[len(cfg.Single.PromptTokens)-1]; reach < top {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"多轮可达深度（~%d）低于单发最大档位（%d）：多轮曲线无法覆盖单发最深处，两场景在最深处的形态差异测不到", reach, top))
		}
	}

	// 并发：levels 非法值 / 重复；闭环与开环互斥提示；参数作用域提示
	openLoop := cfg.Concurrent.RequestRate > 0 || len(cfg.Concurrent.RateSweep) > 0
	seenLevel := map[int]bool{}
	deduped := cfg.Concurrent.Levels[:0]
	for _, lv := range cfg.Concurrent.Levels {
		if lv <= 0 {
			return nil, fmt.Errorf("concurrent.levels 含非正值 %d——并发度必须是正整数", lv)
		}
		if seenLevel[lv] {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("concurrent.levels 重复档位 %d 已去重", lv))
			continue
		}
		seenLevel[lv] = true
		deduped = append(deduped, lv)
	}
	cfg.Concurrent.Levels = deduped
	if openLoop {
		if len(cfg.Concurrent.Levels) > 0 {
			cfg.Warnings = append(cfg.Warnings,
				"request_rate/rate_sweep 已配置：开环到达率模式生效，levels 被忽略")
		}
		if cfg.Concurrent.RequestRate > 0 && len(cfg.Concurrent.RateSweep) > 0 {
			cfg.Warnings = append(cfg.Warnings,
				"request_rate 与 rate_sweep 同时配置：rate_sweep 多档扫描优先")
		}
		if cfg.Concurrent.NumPrompts <= 0 {
			cfg.Warnings = append(cfg.Warnings,
				"开环模式未配 num_prompts，将使用默认 32（multiturn 时为总会话数）")
		}
	} else if cfg.Concurrent.MaxConcurrency > 0 {
		cfg.Warnings = append(cfg.Warnings,
			"max_concurrency 仅在开环模式（request_rate/rate_sweep）下生效，闭环 levels 模式会忽略")
	}
	if !cfg.Concurrent.Multiturn && cfg.Concurrent.PromptTokens > 200000 {
		return nil, fmt.Errorf(
			"concurrent.prompt_tokens=%d 过大：单条消息大概率超过模型上下文上限（阈值 200k，与 single 档位同一约束）",
			cfg.Concurrent.PromptTokens)
	}

	// goodput / retry / correctness / warmup / salt
	if cfg.Goodput != nil {
		if cfg.Goodput.TTFTMS < 0 || cfg.Goodput.TPOTMS < 0 {
			return nil, fmt.Errorf("goodput 阈值不能为负（ttft_ms=%v tpot_ms=%v）", cfg.Goodput.TTFTMS, cfg.Goodput.TPOTMS)
		}
		if cfg.Goodput.TTFTMS == 0 && cfg.Goodput.TPOTMS == 0 {
			return nil, fmt.Errorf("goodput 已启用但 ttft_ms/tpot_ms 均为 0——至少配置一项才有判定意义（不打算用请整段注释掉）")
		}
	}
	if cfg.Retry != nil {
		if cfg.Retry.MaxAttempts < 0 || cfg.Retry.BackoffMS < 0 {
			return nil, fmt.Errorf("retry.max_attempts / retry.backoff_ms 不能为负")
		}
		if cfg.Retry.MaxAttempts > 5 {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"retry.max_attempts=%d 过多：重试会掩盖服务端不稳定，压测语义下建议 ≤2", cfg.Retry.MaxAttempts))
		}
	}
	if cfg.Correctness != nil && cfg.Correctness.Samples < 0 {
		return nil, fmt.Errorf("correctness.samples 不能为负")
	}
	if cfg.WarmupRequests < 0 {
		return nil, fmt.Errorf("warmup_requests 不能为负")
	}
	if cfg.SeedSalt < 0 {
		return nil, fmt.Errorf("seed_salt 不能为负")
	}

	// max_prompt_tokens 联动：档位截断预告
	if cfg.MaxPromptTokens > 0 {
		if cfg.MaxPromptTokens < 1000 {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"max_prompt_tokens=%d 过小（<1k）：所有场景都会被截到该值，确认单位是 token 而非其它", cfg.MaxPromptTokens))
		}
		clamped, didClamp := cfg.ClampLadder(cfg.Single.PromptTokens)
		if didClamp {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"single 档位超过 max_prompt_tokens=%d，截断去重后实际档位 → %v；多轮会话到顶后提前停轮",
				cfg.MaxPromptTokens, clamped))
		}
	}

	return cfg, nil
}

// anyLevelEnabled 判断 levels 里是否存在思考开启的变体。
func anyLevelEnabled(th Thinking) bool {
	for _, lv := range th.Levels {
		if lv.Enabled {
			return true
		}
	}
	return false
}

// Timeout 返回超时 Duration。
func (c *Config) Timeout() time.Duration { return time.Duration(c.TimeoutSeconds) * time.Second }

// Fillers 返回填充文本语言设置。
func (c *Config) Fillers() string { return c.FillerLang }

// StreamEnabled 返回是否使用流式请求。
func (c *Config) StreamEnabled() bool { return *c.Stream }

// ClampLadder 将 token 档位截到 MaxPromptTokens（>0 时生效）：超限档位收敛到上限，去重保序。
// 返回 (截断后档位, 是否发生了截断)。
func (c *Config) ClampLadder(tokens []int) ([]int, bool) {
	if c.MaxPromptTokens <= 0 {
		return tokens, false
	}
	clamped := false
	seen := map[int]bool{}
	out := make([]int, 0, len(tokens))
	for _, t := range tokens {
		if t > c.MaxPromptTokens {
			t = c.MaxPromptTokens
			clamped = true
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, clamped
}

// ClampOne 将单个 token 规模截到 MaxPromptTokens（>0 时生效）。
func (c *Config) ClampOne(tokens int) int {
	if c.MaxPromptTokens > 0 && tokens > c.MaxPromptTokens {
		return c.MaxPromptTokens
	}
	return tokens
}

// LargestPromptTokens 返回配置中最大的单请求 prompt 规模（三场景取最大，用于超时提示与 probe 对比）。
func (c *Config) LargestPromptTokens() int {
	mx := 0
	for _, t := range c.Single.PromptTokens {
		if t > mx {
			mx = t
		}
	}
	if est := c.Multiturn.SystemTokens + c.Multiturn.ToolDefsTokens + c.Multiturn.Turns*c.Multiturn.TurnTokens; est > mx {
		mx = est
	}
	if c.Concurrent.PromptTokens > mx {
		mx = c.Concurrent.PromptTokens
	}
	return c.ClampOne(mx)
}
