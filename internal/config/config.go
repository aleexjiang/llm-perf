// Package config 负责加载 yaml 配置并用环境变量覆盖。
package config

import (
	"bytes"
	"fmt"
	"os"
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
}

// ThinkingVariant 是一个思考模式变体。
type ThinkingVariant struct {
	Name      string // "off" / "on"
	Enabled   bool
	ExtraBody map[string]any
}

// Variants 按 mode 展开成变体列表（both 时先 off 后 on，便于报告对照）。
func (t Thinking) Variants() []ThinkingVariant {
	switch t.Mode {
	case "on":
		return []ThinkingVariant{{Name: "on", Enabled: true, ExtraBody: t.ExtraBodyOn}}
	case "off":
		return []ThinkingVariant{{Name: "off", Enabled: false, ExtraBody: t.ExtraBodyOff}}
	default: // both
		return []ThinkingVariant{
			{Name: "off", Enabled: false, ExtraBody: t.ExtraBodyOff},
			{Name: "on", Enabled: true, ExtraBody: t.ExtraBodyOn},
		}
	}
}

// MaxTokens 对思考开启的变体应用 max_tokens 下限保护。
func (t Thinking) MaxTokens(maxTokens int, v ThinkingVariant) int {
	if v.Enabled && t.MaxTokensFloor > 0 && maxTokens < t.MaxTokensFloor {
		return t.MaxTokensFloor
	}
	return maxTokens
}

type Config struct {
	Endpoint       string   `yaml:"endpoint"`
	APIKeyEnv      string   `yaml:"api_key_env"`
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

	Dataset     DatasetCfg      `yaml:"dataset"`
	Goodput     *GoodputCfg     `yaml:"goodput"`
	Retry       *RetryCfg       `yaml:"retry"`
	Correctness *CorrectnessCfg `yaml:"correctness"`

	Thinking Thinking `yaml:"thinking"`

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
	if cfg.APIKeyEnv != "" {
		cfg.APIKey = os.Getenv(cfg.APIKeyEnv)
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
	return cfg, nil
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
