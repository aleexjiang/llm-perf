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
}

// Thinking 思考模式配置。
// 通用透传设计（对齐 vLLM --extra-body）：不硬编码参数名，on/off 两套 JSON 合并进请求体。
type Thinking struct {
	Mode           string         `yaml:"mode"`              // both（默认，A/B 对照）| on | off
	ExtraBodyOn    map[string]any `yaml:"extra_body_on"`     // 思考开启时合并进请求体
	ExtraBodyOff   map[string]any `yaml:"extra_body_off"`    // 思考关闭时合并进请求体
	MaxTokensFloor int            `yaml:"max_tokens_floor"`  // 思考开启时 max_tokens 下限保护（防思考吃光输出预算）
}

// ThinkingVariant 是一个思考模式变体。
type ThinkingVariant struct {
	Name      string         // "off" / "on"
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
	Endpoint       string `yaml:"endpoint"`
	APIKeyEnv      string `yaml:"api_key_env"`
	OutputDir      string `yaml:"output_dir"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	IncludeUsage   *bool  `yaml:"include_usage"`
	FillerLang     string `yaml:"filler_lang"`
	FillerCorpus   string `yaml:"filler_corpus"` // "":合成词表 | "en"/"zh":内置公版书语料 | 文件路径(.txt/.txt.gz):自定义语料
	Stream         *bool  `yaml:"stream"`        // 默认 true；false 时 TTFT/ITL/思考拆分不可测（N/A）
	Debug          bool   `yaml:"debug"`         // true: 每个请求的原始响应留存到 <output_dir>/raw/，日志同步写 run.log（排查魔改引擎用）
	Models         []string `yaml:"models"`

	// MaxPromptTokens 上下文截止（tokens）：>0 时所有请求的 prompt 规模都不超过该值。
	// single 档位超限截到该值并去重；多轮会话 ctx 到顶后停止加轮。0 = 不限制。
	// CLI --max-ctx 可覆盖。建议同时参考 bench probe 报告的模型 max_model_len。
	MaxPromptTokens int `yaml:"max_prompt_tokens"`

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
