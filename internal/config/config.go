// Package config 负责加载 yaml 配置并用环境变量覆盖。
// 配置按"通用 + 模型差异"组织：一份文件顶部写所有模型共享的通用配置，
// 底部用 model_overrides 按模型只写差异项（缺省继承通用配置），见 ForModel。
//
// 场景配置段（2026-09-18 新架构，见 docs/workload-refactor-plan.md）：
//   - user：生成式多轮用户会话（profile 驱动 + 经典书语料 + 被测模型真实回复）
//   - request_set：冻结独立请求快照数据源（rps/concurrency 共用，ShareGPT 直接读取）
//   - rps：开环到达参数；concurrency：固定在飞参数
//
// 旧的 single/multiturn/concurrent/dataset/filler/saturation_guard 配置段已随
// filler 正式路径下线删除（schema 不保向后兼容，拍板见 AGENTS.md）。
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// IntList max_tokens 类字段：YAML 接受标量（max_tokens: 512）或列表（max_tokens: [128, 256, 512]）。
// 列表 = 输出长度扫描：输出长度是 decode 指标的一级变量（同输入下输出 64→512 可使 TPOT
// 变化 54–77%），多档对照才能把 prefill/decode 效应分开归因。
type IntList []int

func (l *IntList) UnmarshalYAML(node *yaml.Node) error {
	var one int
	if err := node.Decode(&one); err == nil {
		*l = IntList{one}
		return nil
	}
	var many []int
	if err := node.Decode(&many); err != nil {
		return fmt.Errorf("应为整数或整数列表（如 512 或 [128, 512]）: %w", err)
	}
	*l = many
	return nil
}

// First 返回第一个值；Load 的默认值逻辑保证列表非空，这里防御性返回 0。
func (l IntList) First() int {
	if len(l) == 0 {
		return 0
	}
	return l[0]
}

// Max 返回最大值：probe 上下文探测按最大输出预算（prompt+output 最坏组合）发请求。
func (l IntList) Max() int {
	mx := 0
	for _, v := range l {
		if v > mx {
			mx = v
		}
	}
	return mx
}

// GoodputCfg SLO 约束（goodput 口径）：同时满足 TTFT 与 TPOT 上限的请求才算有效吞吐。
// 挂在 slo.goodput 下（原顶层 goodput: 已合流进 slo:，schema 不保兼容）。
type GoodputCfg struct {
	TTFTMS float64 `yaml:"ttft_ms"` // 如 2000
	TPOTMS float64 `yaml:"tpot_ms"` // 如 100
}

// Sampling 请求级采样参数（2026-09-20）：所有压测请求统一携带。
// nil 字段 = 不传该参数（服务端默认）；user 模式轨迹对齐对照传 temperature: 0。
type Sampling struct {
	Temperature *float64 `yaml:"temperature"` // 0 = 贪心（重跑轨迹可对齐）；nil = 服务端默认
	TopP        *float64 `yaml:"top_p"`
}

// SLOCfg SLO 口径段（5.8 合流）：goodput 判定阈值 + 报告"体验基线评估"阈值。
// 之前 goodput 阈值在 Go 侧、基线三档判据是报告脚本内置常量——同一份 SLO 拆在两处，
// 阈值改不动、口径对不齐；现在都从配置进、随 JSON 透出，报告侧消费同一份。
type SLOCfg struct {
	Goodput  *GoodputCfg     `yaml:"goodput"`  // goodput 判定（未配置 = 不算 goodput 列）
	Baseline *SLOBaselineCfg `yaml:"baseline"` // 体验基线评估阈值（未配置 = 报告用内置默认）
}

// SLOBaselineCfg 体验基线评估（3 档制）阈值。所有数值字段可省略——省略的字段用
// 报告侧内置默认（判据与出处 docs/latency-baselines.md §7）；写了就覆盖。
// 键名与报告侧 SLO_TIERS 一致，JSON 透出后报告侧可直接 dict.update 合流。
type SLOBaselineCfg struct {
	// Enabled 默认开；false = 报告跳过"体验基线评估"节（阈值仍可配但不渲染）。
	// 指针区分"未写"（默认开）与显式 false。
	Enabled        *bool   `yaml:"enabled"`
	ShortMaxTokens int     `yaml:"short_max_tokens"` // 短输入档上界（默认 4000）
	LongMinTokens  int     `yaml:"long_min_tokens"`  // agent 大上下文档下界（默认 24000）
	ShortGoodTTFT  float64 `yaml:"short_good_ttft"`  // s，短档优线（默认 0.45）
	ShortPassTTFT  float64 `yaml:"short_pass_ttft"`  // s，短档及格线（默认 2.0）
	LongGoodTTFT   float64 `yaml:"long_good_ttft"`   // s，长档优线（默认 3.0）
	LongPassTTFT   float64 `yaml:"long_pass_ttft"`   // s，长档及格线（默认 6.0）
	GoodTPOT       float64 `yaml:"good_tpot"`        // ms，TPOT 优线（默认 40）
	PassTPOT       float64 `yaml:"pass_tpot"`        // ms，TPOT 及格线（默认 200）
	GoodTPS        float64 `yaml:"good_tps"`         // tok/s，单请求输出速度优线（默认 25）
	PassTPS        float64 `yaml:"pass_tps"`         // tok/s，及格线（默认 10）
}

// BaselineEnabled 基线评估是否渲染（未配置段或 enabled 未写 = 默认开）。
func (b *SLOBaselineCfg) BaselineEnabled() bool {
	return b == nil || b.Enabled == nil || *b.Enabled
}

// RetryCfg 连接层重试策略（默认关闭）：只重试瞬时失败（TCP/流被 reset、HTTP 5xx/429），
// 4xx 不重试。重试会记入 warnings 与 retry_count——计时窗口干净，但服务端不稳定仍可见。
type RetryCfg struct {
	MaxAttempts int `yaml:"max_attempts"` // 总尝试次数；0/1 = 不重试
	BackoffMS   int `yaml:"backoff_ms"`   // 退避基数，默认 300ms，指数退避封顶 5s
}

// Thinking 思考模式配置。
// 通用透传设计（对齐 vLLM --extra-body）：不硬编码参数名，on/off 两套 JSON 合并进请求体。
type Thinking struct {
	Mode           string         `yaml:"mode"`             // both（默认，A/B 对照）| on | off
	ExtraBodyOn    map[string]any `yaml:"extra_body_on"`    // 思考开启时合并进请求体
	ExtraBodyOff   map[string]any `yaml:"extra_body_off"`   // 思考关闭时合并进请求体
	MaxTokensFloor *int           `yaml:"max_tokens_floor"` // nil=未配置（默认 2048），0=显式关闭下限保护

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

// ProbeExtraBodies 供 probe 取「思考开启态 / 关闭态」两份 extra_body。
// levels 模式下 extra_body_on/off 通常为空（参数写在 levels 里），若直接读取会让 probe
// 整块思考探测被跳过——部署侧关思考这类问题就查不出来。这里从 levels 兜底：
// 取第一个 enabled=true 的变体当开启态、第一个 enabled=false 的当关闭态。
// 关闭态仍为空时注入 enable_thinking=false——与压测路径 Variants() 兜底同语义，
// 保证 probe 探测口径与压测一致（真机发现：部署默认 thinking=auto 时，
// max_tokens=1 的非流式探测全部进思考链 → chat_nonstream 持续误报）。
func (t Thinking) ProbeExtraBodies() (on, off map[string]any) {
	on, off = t.ExtraBodyOn, t.ExtraBodyOff
	for _, v := range t.Levels {
		if v.Enabled && on == nil {
			on = v.ExtraBody
		}
		if !v.Enabled && off == nil {
			off = v.ExtraBody
		}
	}
	if off == nil {
		off = map[string]any{
			"chat_template_kwargs": map[string]any{"enable_thinking": false},
		}
	}
	return on, off
}

// VariantNames 返回全部变体名（忽略 CLI 过滤）——CLI --thinking 校验用。
func (t Thinking) VariantNames() []string {
	all := t
	all.filter = ""
	var names []string
	for _, v := range all.Variants() {
		names = append(names, v.Name)
	}
	return names
}

// ThinkingVariant 是一个思考模式变体。
type ThinkingVariant struct {
	Name      string // "off" / "on" / 自定义档位名
	Enabled   bool
	ExtraBody map[string]any
}

// validateThinkingLevels 校验 levels 变体：名字非空、唯一、且不得为保留字
// （on/off/both，大小写不敏感，12.7）——CLI --thinking 的变体名过滤把保留字当成
// 过滤值，档位恰好叫 off 时该档位永远无法通过 CLI 选中（运行时才发现且无解，
// r4 实测踩坑）。fail-fast：配了就用不了，不如加载时报错逼着改名。
func validateThinkingLevels(th Thinking, where string) error {
	seen := map[string]bool{}
	for _, lv := range th.Levels {
		if lv.Name == "" {
			return fmt.Errorf("%s thinking.levels 变体缺少 name（报告与日志按 name 分组，必须显式命名）", where)
		}
		if seen[lv.Name] {
			return fmt.Errorf("%s thinking.levels 变体名 %q 重复——档位名必须唯一", where, lv.Name)
		}
		switch strings.ToLower(lv.Name) {
		case "on", "off", "both":
			return fmt.Errorf("%s thinking.levels 变体名 %q 是保留字（on/off/both，大小写不敏感）——"+
				"CLI --thinking 按变体名过滤时该档位将永远无法选中，请改名（如 none）", where, lv.Name)
		}
		seen[lv.Name] = true
	}
	return nil
}

// Variants 展开成变体列表：配了 levels 用 levels（保持声明顺序），否则按 mode 展开
// （both 时先 off 后 on，便于报告对照）；CLI filter 非空时只留名字匹配的变体。
func (t Thinking) Variants() []ThinkingVariant {
	var vs []ThinkingVariant
	if len(t.Levels) > 0 {
		for _, lv := range t.Levels {
			vs = append(vs, ThinkingVariant(lv))
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
	// 关闭态兜底：变体未带任何 extra_body 时，注入 enable_thinking=false。
	// 真机实测（2026-09-19，Qwen3.8 chat template）坐实：不传思考参数时部署默认
	// thinking=auto，长上下文输入下 128 token 全耗在思考链、content 为空，
	// user 多轮的 assistant 进不了 history（连续 user + 动态 cache 失效）。
	// 显式写了 extra_body/extra_body_off 的变体尊重显式值，不覆盖。
	for i, v := range vs {
		if !v.Enabled && v.ExtraBody == nil {
			vs[i].ExtraBody = map[string]any{
				"chat_template_kwargs": map[string]any{"enable_thinking": false},
			}
		}
	}
	return vs
}

// MaxTokensFloorValue 返回思考 floor（nil=未配置 → 0；0=显式关闭保护）。
func (t Thinking) MaxTokensFloorValue() int {
	if t.MaxTokensFloor == nil {
		return 0
	}
	return *t.MaxTokensFloor
}

// MaxTokens 对思考开启的变体应用 max_tokens 下限保护。
func (t Thinking) MaxTokens(maxTokens int, v ThinkingVariant) int {
	floor := t.MaxTokensFloorValue()
	if v.Enabled && floor > 0 && maxTokens < floor {
		return floor
	}
	return maxTokens
}

// MaxTokensList 对输出长度列表逐值应用思考 floor 保护（返回新切片；列表 = 输出长度扫描维度）。
// 输入列表须已升序：floor 抬高可能把多个小档位收敛成同一值，相邻去重避免重复扫同一档。
func (t Thinking) MaxTokensList(list []int, v ThinkingVariant) []int {
	out := make([]int, 0, len(list))
	for _, mt := range list {
		adj := t.MaxTokens(mt, v)
		if len(out) > 0 && out[len(out)-1] == adj {
			continue
		}
		out = append(out, adj)
	}
	return out
}

// ThinkingFor 返回某模型生效的思考配置：全局 thinking 为底，model_overrides[model].thinking
// 字段级覆盖（零值 = 未写 = 继承全局）；CLI --thinking 的变体过滤始终继承。
func (c *Config) ThinkingFor(model string) *Thinking {
	t := c.Thinking // 值拷贝（map/slice 共享底层数组但只读，安全）
	if ov := c.ModelOverrides[model]; ov != nil && ov.Thinking != nil {
		ot := ov.Thinking
		if ot.Mode != "" {
			t.Mode = ot.Mode
		}
		if ot.ExtraBodyOn != nil {
			t.ExtraBodyOn = ot.ExtraBodyOn
		}
		if ot.ExtraBodyOff != nil {
			t.ExtraBodyOff = ot.ExtraBodyOff
		}
		if ot.MaxTokensFloor != nil {
			t.MaxTokensFloor = ot.MaxTokensFloor
		}
		if len(ot.Levels) > 0 {
			t.Levels = ot.Levels
		}
	}
	// 12.8 回填：levels 模式下 extra_body_off 通常不写（参数在 levels 里），而金丝雀/预热
	// 这两个固定「关闭态」消费点直接读 ExtraBodyOff——不回填就裸发请求，默认开思考的
	// 模型把 max_tokens=16 全吃进思考链，金丝雀 0/4（r4 实测坐实）。从 levels 里第一个
	// enabled=false 的变体兜底，消费点不用改；主路径变体遍历带各变体自己的 extra_body，
	// 不受影响。用户显式写了 extra_body_off 时尊重显式值。
	if len(t.Levels) > 0 && t.ExtraBodyOff == nil {
		for _, lv := range t.Levels {
			if !lv.Enabled {
				t.ExtraBodyOff = lv.ExtraBody
				break
			}
		}
	}
	t.filter = c.Thinking.filter
	return &t
}

// ForModel 返回某模型生效的配置视图：顶层通用配置为底，model_overrides[model] 只覆盖差异项。
// 场景层在模型循环内用视图取代顶层配置（并同步换掉 env.cfg），取该模型实际生效的
// workload 形状/思考/流式开关；无覆盖时返回原配置。视图与 c 共享只读字段
// （指针、slice 底层数组），调用方不得修改视图内容。
func (c *Config) ForModel(model string) *Config {
	ov := c.ModelOverrides[model]
	if ov == nil {
		return c
	}
	v := *c
	// thinking：ThinkingFor 已按 overrides.thinking 做字段级覆盖
	v.Thinking = *c.ThinkingFor(model)
	if ov.MaxPromptTokens != nil {
		v.MaxPromptTokens = *ov.MaxPromptTokens
	}
	if ov.Stream != nil {
		v.Stream = ov.Stream
	}
	return &v
}

// ModelOverride 单个模型的差异配置：只写与通用配置不同的项，未写的键继承顶层。
// 段内覆盖语义与 ThinkingFor 一致——零值/缺省 = 继承。端点级配置（endpoint/认证/
// timeout_seconds/server_metrics）不在此覆盖——一个测试一个端点，超时由 client 统一持有。
type ModelOverride struct {
	Thinking *Thinking `yaml:"thinking"` // 覆盖全局 thinking（字段级，未写的继承）
	// MaxPromptTokens 模型上下文截止：0 = 未写（继承顶层）；指针区分"未写"与"显式写 0（解除上限）"
	MaxPromptTokens *int `yaml:"max_prompt_tokens"`
	// Enabled 本次是否测试该模型：分批重测/单模型对照时临时关掉其他模型用。
	// 指针区分"未写"（默认 true）与显式 false；禁用的模型不进场景循环，probe 也不选它
	Enabled *bool `yaml:"enabled"`
	Stream  *bool `yaml:"stream"` // 引擎能力差异：个别模型不支持流式时按模型关掉
}

// EnabledFor 模型本次是否参与测试：model_overrides[m].enabled，未写默认 true。
func (c *Config) EnabledFor(model string) bool {
	if ov := c.ModelOverrides[model]; ov != nil && ov.Enabled != nil {
		return *ov.Enabled
	}
	return true
}

// ActiveModels 返回本次参与测试的模型（enabled=false 的剔除，保持 models 声明顺序）。
// 场景循环与 probe 的模型选取都从这里取——enabled 是"本次测谁"的唯一事实来源。
func (c *Config) ActiveModels() []string {
	var out []string
	for _, m := range c.Models {
		if c.EnabledFor(m) {
			out = append(out, m)
		}
	}
	return out
}

type Config struct {
	// Raw 配置文件原文（Load 时填充）：随报告存档，保证几周后能复现"当时是什么配置跑的"。
	// 注释、键序、书写习惯都只有原文能保留——结构化字段回放不出这些信息。
	Raw            string `yaml:"-"`
	Endpoint       string `yaml:"endpoint"`
	APIKeyLiteral  string `yaml:"api_key"`     // 字面量 key，直接写配置文件（该配置文件应避免入库）；环境变量 LLM_PERF_API_KEY 优先级更高
	APIKeyEnv      string `yaml:"api_key_env"` // 从哪个环境变量读 key（留空则跳过）；字面量 api_key 与环境变量都未提供时不带认证头
	AuthScheme     string `yaml:"auth_scheme"` // bearer（默认）| raw（裸 key 无 Bearer 前缀）| none（不带认证头）
	AuthHeader     string `yaml:"auth_header"` // 自定义认证 header 名（如 X-API-Key）；空 = Authorization
	OutputDir      string `yaml:"output_dir"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	ChatPath       string `yaml:"chat_path"`    // 接口路径（默认 /chat/completions）；客户 router 路径不同时配置
	MetricsPath    string `yaml:"metrics_path"` // 服务端 metrics 路径（默认 /metrics）；如 /actuator/prometheus
	ModelsPath     string `yaml:"models_path"`  // 模型列表路径（默认 /models）；probe 用，个别网关路径不同
	IncludeUsage   *bool  `yaml:"include_usage"`
	// CorpusLang 语料语言（en/zh）：user 模式生成文本的语料选择；probe filler_fidelity 同用。
	// 长度按字符/token 换算（en≈4、zh≈1.4，见 corpus.CharsPerToken），实际以服务端 usage 为准。
	CorpusLang string `yaml:"corpus_lang"`
	// CorpusPath 自定义语料文件（.txt/.txt.gz，probe 的 filler_fidelity 校准用）；
	// "" = 使用内置 12 本公版书语料库。user 模式一律走内置语料库（一用户一书）。
	CorpusPath string `yaml:"corpus_path"`
	Stream     *bool  `yaml:"stream"` // 默认 true；false 时 TTFT/ITL/思考拆分不可测（N/A）
	Debug      bool   `yaml:"debug"`  // true: 每个请求的原始响应留存到 <output_dir>/raw/，日志同步写 run.log（排查魔改引擎用）
	// RawTimings 原始 chunk 序列落盘（nil = 默认开）：流式请求把每个含 token chunk 的时刻
	// 记入 content_times_ms（相对 sent_at 的毫秒偏移）。峰值秒桶吞吐、ITL 抖动等外部分析
	// 都依赖这份原始序列；体积随输出 token 数线性增长，超长 soak 可置 false 关闭。
	RawTimings *bool    `yaml:"raw_timings"`
	Models     []string `yaml:"models"`

	// Test 本轮测试类别（benchmark | performance | soak，留空 = performance）。
	// 只切换报告的**结论区口径**，不改测量本身：三者共用同一套数据与判据
	// ——类别是表达层的焦点声明，不是第二套管线。缺证据处如实写 NA。
	Test string `yaml:"test"`

	// MaxPromptTokens 上下文截止（tokens）：>0 时多轮会话 ctx 到顶后停止加轮。0 = 不限制。
	// CLI --max-ctx 可覆盖。建议同时参考 bench probe 报告的模型 max_model_len。
	MaxPromptTokens int `yaml:"max_prompt_tokens"`

	// ServerMetrics 服务端观测层：抓取推理服务原生 /metrics（vLLM 默认暴露），
	// 补充前缀缓存命中率、排队深度、prefill/decode 分解、MTP 接受率（不可达时自动记录并继续客户端采集）
	ServerMetrics     bool `yaml:"server_metrics"`
	MetricsIntervalMS int  `yaml:"metrics_interval_ms"` // gauge 轮询间隔，默认 500

	// SeedSalt 种子盐值：所有场景的 prompt 种子都叠加该值。服务端 prefix cache 是内存态、
	// 跨请求存活——同一配置重跑时 prompt 与上次完全相同，"冷缓存"测量会被上次测试污染。
	// 每次测试（改代码/改配置后的重测）递增盐值即可隔离；不改服务端也能拿到干净的冷缓存。
	SeedSalt int `yaml:"seed_salt"`

	// Sampling 请求级采样参数（nil 字段 = 不传，走服务端默认）。
	// user 模式的 assistant 回复由被测模型采样生成——默认采样下同配置重跑的会话轨迹
	// 会自然分叉（真机实测：同 salt 前 2 轮 prompt 一致、第 3 轮起分叉）。需要轨迹对齐
	// 的对照实验显式传 temperature: 0（贪心）。
	Sampling Sampling `yaml:"sampling"`

	// Warnings 配置诊断提示（Load 时生成，非序列化字段）：不阻止运行，
	// 但启动时打印——数量级不合理、轮次不足、覆盖关系等"合法但值得知道"的事
	Warnings []string `yaml:"-"`

	SLO   *SLOCfg   `yaml:"slo"` // SLO 口径：goodput 判定 + 基线评估阈值（原顶层 goodput 已合流）
	Retry *RetryCfg `yaml:"retry"`

	Thinking Thinking `yaml:"thinking"`

	// ModelOverrides 按模型覆盖通用配置（键=模型名，必须在 models 列表内）。
	// 组织方式：一份配置顶部写所有模型共享的通用配置，底部按模型只写差异项，
	// 未写的键继承顶层——场景层在模型循环内通过 ForModel 取该模型生效的配置视图。
	// 思考按模型覆盖写 model_overrides.<模型>.thinking（字段级覆盖）。
	ModelOverrides map[string]*ModelOverride `yaml:"model_overrides"`

	// User user 模式（生成式多轮会话，profile 驱动）——见 docs/workload-refactor-plan.md 13。
	// 形状来自外置 profile.json（trace 特征提炼），文本由经典书语料按 seed 生成，
	// assistant 使用被测模型真实回复（动态 prefix cache）。
	User User `yaml:"user"`

	// RequestSet 冻结独立请求快照的数据源（rps/concurrency 模式共用）。
	RequestSet RequestSet `yaml:"request_set"`
	// RPS rps 模式配置（开环到达，冻结请求快照）。
	RPS RPS `yaml:"rps"`
	// Concurrency concurrency 模式配置（对齐 vLLM bench serve，固定在飞上限齐射）。
	Concurrency ConcurrencyCfg `yaml:"concurrency"`

	// 运行时解析
	APIKey string `yaml:"-"`
}

// RequestSet 请求集数据源。
type RequestSet struct {
	// ShareGPTPath ShareGPT 数据集路径（.json/.json.gz）；rps/concurrency 的请求来源。
	// 口径与 vLLM bench serve --dataset-name sharegpt 对齐：prompt=截至最后一条 user 的
	// history，输出预算=其后 assistant 回复的估算 token，seed 蓄水池抽样。
	ShareGPTPath string `yaml:"sharegpt_path"`
	// NumPrompts 总请求数（默认 100；样本不足时确定性回绕）。
	NumPrompts int `yaml:"num_prompts"`
	// Seed 抽样种子（同 seed 同样本序，跨 run 可复现的前提）。
	Seed int64 `yaml:"seed"`
	// MaxOutputTokens 单请求输出预算上限（0=不设上限；ShareGPT 存在超长回复）。
	MaxOutputTokens int `yaml:"max_output_tokens"`
}

// RPS rps 模式参数。
type RPS struct {
	// Rates 到达率档位（req/s），多档 = 排队-延迟曲线扫描。
	Rates []float64 `yaml:"rates"`
	// MaxConcurrency 在飞请求上限（0=不限；防到达率超容量时无限堆积）。
	MaxConcurrency int `yaml:"max_concurrency"`
	// Burstiness 到达突发度（1=泊松；<1 更突发；>1 更均匀），默认 1。
	Burstiness float64 `yaml:"burstiness"`
}

// GetBurstiness 突发度（未配置 = 1 标准泊松）。
func (r RPS) GetBurstiness() float64 {
	if r.Burstiness <= 0 {
		return 1
	}
	return r.Burstiness
}

// ConcurrencyCfg concurrency 模式参数（对齐 vLLM bench serve）。
type ConcurrencyCfg struct {
	// Levels 在飞请求数档位（每档一轮：N 在飞齐射，跑完 num_prompts）。
	Levels []int `yaml:"levels"`
	// RequestRate 有限到达率 + 在飞上限的组合（0 = inf：尽快发起，仅 max_concurrency 控制）。
	RequestRate float64 `yaml:"request_rate"`
	// Burstiness 有限到达率下的到达分布（默认 1 泊松）。
	Burstiness float64 `yaml:"burstiness"`
}

// GetBurstiness 突发度（未配置 = 1）。
func (c ConcurrencyCfg) GetBurstiness() float64 {
	if c.Burstiness <= 0 {
		return 1
	}
	return c.Burstiness
}

// UserCfg user 模式配置。
type User struct {
	// ProfilePath profile.json 路径（scripts/profile_build.py 产出；相对路径以配置目录为基准）。
	ProfilePath string `yaml:"profile_path"`
	// Users 并行用户数（每个用户独立一条生成式多轮会话），0 = 1。
	Users int `yaml:"users"`
	// MaxTokens 输出长度（标量或列表；列表 = 输出长度扫描维度）。
	MaxTokens IntList `yaml:"max_tokens"`
	// SharedBase 基座是否跨用户共享（默认 true）：true = 全部用户同一 system 基座（同一
	// seed 取窗），测跨用户共享前缀的 cache 收益；false = 每用户独立基座。
	SharedBase *bool `yaml:"shared_base"`
	// StaggerMS 会话启动错峰（毫秒，默认 0）：users>1 时第 N 个用户延迟 N×stagger_ms
	// 启动，避免全部首轮同时 prefill 互抢（真机实测首轮 TTFT 差异达 1.7 倍、逐轮曲线双峰）。
	StaggerMS int `yaml:"stagger_ms"`
}

// GetStaggerMS 会话启动错峰毫秒数（0 = 同时启动）。
func (u User) GetStaggerMS() int { return u.StaggerMS }

// GetUsers 用户数（0/未配置 = 1）。
func (u User) GetUsers() int {
	if u.Users <= 0 {
		return 1
	}
	return u.Users
}

// GetSharedBase 基座是否跨用户共享（未配置默认 true）。
func (u User) GetSharedBase() bool { return u.SharedBase == nil || *u.SharedBase }

// 测试类别取值（Config.Test）。三者是同一个测量管线的三种**表达焦点**，
// 不改变探针/场景/指标——报告按类别切换结论区首屏，回答不同的问题：
// benchmark 回答"标准格上这台部署处于什么水平"（跨部署可比），
// performance 回答"瓶颈在哪、容量边界多远"（默认），
// soak 回答"长时间跑会不会退化/出事故"。
const (
	TestBenchmark   = "benchmark"
	TestPerformance = "performance"
	TestSoak        = "soak"
)

// TestKind 返回生效的测试类别：未配置 = performance。
// 返回值为归一化小写，与 TestBenchmark/TestPerformance/TestSoak 可直接比较。
func (c *Config) TestKind() string {
	if c.Test == "" {
		return TestPerformance
	}
	return strings.ToLower(c.Test)
}

// isBuiltinCorpus 判断 corpus_path 是否为内置语料哨兵（en/zh）。
// 哨兵不是路径，必须原样透传给 corpus.Load，不能参与相对路径拼接。
func isBuiltinCorpus(spec string) bool {
	return spec == "en" || spec == "zh"
}

// redactConfigSecrets 保留配置存档的结构和非敏感内容，但不保留认证字段值。
// 只处理精确的顶层 YAML 键，避免误伤注释或相似字段名。
func redactConfigSecrets(data []byte) string {
	lines := strings.SplitAfter(string(data), "\n")
	for i, line := range lines {
		lineEnd := ""
		content := line
		if strings.HasSuffix(content, "\n") {
			lineEnd = "\n"
			content = strings.TrimSuffix(content, "\n")
		}
		content = strings.TrimSuffix(content, "\r")
		trimmed := strings.TrimSpace(content)
		colon := strings.IndexByte(trimmed, ':')
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:colon])
		if key != "api_key" && key != "api_key_env" {
			continue
		}
		prefix := content[:len(content)-len(trimmed)]
		lines[i] = prefix + key + ": <redacted>" + lineEnd
	}
	return strings.Join(lines, "")
}

// Load 读取配置文件，应用默认值，再用环境变量覆盖。
// 环境变量优先级最高：LLM_PERF_ENDPOINT、LLM_PERF_API_KEY。
func Load(path string) (*Config, error) {
	cfg := &Config{
		OutputDir:      "output",
		TimeoutSeconds: 300,
		CorpusLang:     "en",
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
		cfg.Raw = redactConfigSecrets(data)
		// 相对路径以配置文件所在目录为基准（corpus_path / user.profile_path /
		// request_set.sharegpt_path 等），与启动时的工作目录解耦——
		// 从任何目录 `bench -f configs/xxx.yaml` 结果一致
		if cfg.CorpusPath != "" && !isBuiltinCorpus(cfg.CorpusPath) && !filepath.IsAbs(cfg.CorpusPath) {
			cfg.CorpusPath = filepath.Join(filepath.Dir(path), cfg.CorpusPath)
		}
		if cfg.User.ProfilePath != "" && !filepath.IsAbs(cfg.User.ProfilePath) {
			cfg.User.ProfilePath = filepath.Join(filepath.Dir(path), cfg.User.ProfilePath)
		}
		if cfg.RequestSet.ShareGPTPath != "" && !filepath.IsAbs(cfg.RequestSet.ShareGPTPath) {
			cfg.RequestSet.ShareGPTPath = filepath.Join(filepath.Dir(path), cfg.RequestSet.ShareGPTPath)
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
		v := os.Getenv(cfg.APIKeyEnv)
		if v == "" && cfg.APIKeyLiteral == "" && os.Getenv("LLM_PERF_API_KEY") == "" {
			return nil, fmt.Errorf("api_key_env 指向的环境变量 %q 未设置或为空", cfg.APIKeyEnv)
		}
		cfg.APIKey = v
	}
	if cfg.APIKeyLiteral != "" {
		cfg.APIKey = cfg.APIKeyLiteral
	}
	if v := os.Getenv("LLM_PERF_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	// 认证方案：枚举校验（可选值与 internal/auth 支持的方案一致；auth.Auth 对未知值按 bearer 处理，这里提前拒绝）
	switch strings.ToLower(cfg.AuthScheme) {
	case "", "bearer", "raw", "none":
	default:
		return nil, fmt.Errorf("auth_scheme 无效值 %q（可选 bearer/raw/none）", cfg.AuthScheme)
	}
	// 接口路径：默认 /chat/completions；必须以 / 开头
	if cfg.ChatPath == "" {
		cfg.ChatPath = "/chat/completions"
	}
	if !strings.HasPrefix(cfg.ChatPath, "/") {
		return nil, fmt.Errorf("chat_path %q 必须以 / 开头（是路径不是 URL）", cfg.ChatPath)
	}
	if strings.Contains(cfg.ChatPath, "://") {
		return nil, fmt.Errorf("chat_path %q 不能是完整 URL——endpoint 填到 /v1 为止，chat_path 只填接口路径", cfg.ChatPath)
	}
	if cfg.MetricsPath == "" {
		cfg.MetricsPath = "/metrics"
	}
	if cfg.ModelsPath == "" {
		cfg.ModelsPath = "/models"
	}
	if !strings.HasPrefix(cfg.MetricsPath, "/") {
		return nil, fmt.Errorf("metrics_path %q 必须以 / 开头", cfg.MetricsPath)
	}
	if !strings.HasPrefix(cfg.ModelsPath, "/") {
		return nil, fmt.Errorf("models_path %q 必须以 / 开头（是路径不是 URL）", cfg.ModelsPath)
	}
	if strings.Contains(cfg.ModelsPath, "://") {
		return nil, fmt.Errorf("models_path %q 不能是完整 URL——endpoint 填到 /v1 为止，models_path 只填接口路径", cfg.ModelsPath)
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
	// 测试类别：枚举校验 + 归一化。落盘统一小写——报告侧按 test 键直接切结论区，
	// 大小写差异不该让"同一类别"被判成两个（配置拼错时 KnownFields 抓不到值域错误，这里兜）。
	rawTest := cfg.Test
	cfg.Test = cfg.TestKind()
	switch cfg.Test {
	case TestBenchmark, TestPerformance, TestSoak:
	default:
		return nil, fmt.Errorf("test 无效值 %q（可选 benchmark/performance/soak；留空 = performance）", rawTest)
	}
	if cfg.Thinking.MaxTokensFloor == nil {
		floor := 2048
		cfg.Thinking.MaxTokensFloor = &floor
	}
	// model_overrides 覆盖校验：键必须在 models 列表内；thinking.mode 值合法
	inModels := make(map[string]bool, len(cfg.Models))
	for _, m := range cfg.Models {
		inModels[m] = true
	}
	for name, ov := range cfg.ModelOverrides {
		if ov == nil {
			continue
		}
		if !inModels[name] {
			return nil, fmt.Errorf("model_overrides 键 %q 不在 models 列表中（models: %v）", name, cfg.Models)
		}
		if ov.Thinking != nil {
			switch ov.Thinking.Mode {
			case "", "both", "on", "off":
			default:
				return nil, fmt.Errorf("model_overrides[%s].thinking.mode 无效值 %q（可选 both/on/off；留空继承全局）", name, ov.Thinking.Mode)
			}
		}
	}
	// enabled 开关联动：全禁用直接拒绝（跑了个寂寞）；部分禁用提示跳过名单
	for _, m := range cfg.Models {
		if !cfg.EnabledFor(m) {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("模型 %s enabled=false，本次跳过", m))
		}
	}
	if len(cfg.ActiveModels()) == 0 {
		return nil, fmt.Errorf("models 全部被 model_overrides enabled=false 禁用——本次没有可测试的模型（至少启用一个，或删掉 enabled 开关）")
	}
	// 语料语言：user 模式文本生成 + probe filler_fidelity 共用
	switch cfg.CorpusLang {
	case "", "en", "zh":
	default:
		return nil, fmt.Errorf("corpus_lang 无效值 %q（可选 en/zh）", cfg.CorpusLang)
	}
	// user 模式默认输出长度 + 归一化（仅在启用 user 场景时填充，避免无关场景被误警告）
	if cfg.User.ProfilePath != "" {
		if len(cfg.User.MaxTokens) == 0 {
			cfg.User.MaxTokens = IntList{256}
		}
		mt, changed, err := normalizeMaxTokens(cfg.User.MaxTokens, "user.max_tokens")
		if err != nil {
			return nil, err
		}
		cfg.User.MaxTokens = mt
		if changed {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"user.max_tokens 已排序去重 → %v（输出长度扫描按升序执行）", mt))
		}
	}
	// 请求集默认值
	if cfg.RequestSet.NumPrompts == 0 {
		cfg.RequestSet.NumPrompts = 100
	}
	if cfg.RequestSet.NumPrompts < 0 {
		return nil, fmt.Errorf("request_set.num_prompts 不能为负")
	}
	if cfg.RequestSet.MaxOutputTokens < 0 {
		return nil, fmt.Errorf("request_set.max_output_tokens 不能为负")
	}
	// 新增能力默认值与校验
	if cfg.MetricsIntervalMS <= 0 {
		cfg.MetricsIntervalMS = 500
	}

	// ── 输入合理性校验：错误在开跑前暴露，而不是跑完才发现 ──

	// thinking levels：变体名唯一；levels 生效时提示 mode/extra_body 被覆盖
	if err := validateThinkingLevels(cfg.Thinking, "全局"); err != nil {
		return nil, err
	}
	if len(cfg.Thinking.Levels) > 0 {
		cfg.Warnings = append(cfg.Warnings,
			"thinking.levels 已配置：mode 与 extra_body_on/off 不再参与思考展开（以 levels 为准）")
	}

	for name, ov := range cfg.ModelOverrides {
		if ov == nil || ov.Thinking == nil {
			continue
		}
		if err := validateThinkingLevels(*ov.Thinking, "model_overrides["+name+"].thinking"); err != nil {
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

	// 超时：缺省/非法兜底；深上下文 + 思考时给足量提示
	if cfg.TimeoutSeconds <= 0 {
		cfg.Warnings = append(cfg.Warnings, "timeout_seconds 未配置或非正，回退 300s")
		cfg.TimeoutSeconds = 300
	}
	thinkMayOn := cfg.Thinking.Mode == "on" || cfg.Thinking.Mode == "both" ||
		(len(cfg.Thinking.Levels) > 0 && anyLevelEnabled(cfg.Thinking))
	if cfg.TimeoutSeconds > 3600 {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("timeout_seconds=%d 超过 1 小时：确认不是把毫秒当秒填了", cfg.TimeoutSeconds))
	}
	if thinkMayOn && cfg.User.ProfilePath != "" && cfg.TimeoutSeconds < 300 {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
			"timeout_seconds=%d 偏小：user 模式首轮 35K+ 上下文、思考开启时单请求可达 3–8 分钟，会被误判超时", cfg.TimeoutSeconds))
	}

	// 测量守卫：输出长度是影响 decode 指标方向的一级变量——同输入下输出 64→512
	// 可使 TPOT 变化 54–77%（decode 爬坡段在短输出里占比过大）。单一短输出档
	// 测出的 TPOT/tok/s 系统性偏悲观，容量结论会整体偏保守且无任何报错。
	guardShortOutput := func(scen string, mt int) {
		if mt > 0 && mt < 128 {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"%s.max_tokens=%d 偏小：decode 爬坡段占比过大，TPOT/tok/s 系统性偏悲观"+
					"（同输入下输出 64→512 可使 TPOT 变化 54–77%%）。"+
					"冒烟跑通可忽略；出容量/性能结论请用 ≥256 档或多档输出扫描校核", scen, mt))
		}
	}
	if cfg.User.ProfilePath != "" {
		for _, mt := range cfg.User.MaxTokens {
			guardShortOutput("user", mt)
		}
	}

	// 思考 floor 联动：on 时 max_tokens 会被抬高，off/on 的 E2E 口径不同
	floor := cfg.Thinking.MaxTokensFloorValue()
	if thinkMayOn && floor > 0 && cfg.User.ProfilePath != "" {
		for _, mt := range cfg.User.MaxTokens {
			if mt < floor {
				cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
					"user.max_tokens=%d < max_tokens_floor=%d：thinking=on 的请求会被抬高到 floor",
					mt, floor))
			}
		}
	}

	// rps / concurrency 模式参数校验
	for _, r := range cfg.RPS.Rates {
		if r <= 0 {
			return nil, fmt.Errorf("rps.rates 含非正值 %v——到达率必须是正数（req/s）", r)
		}
	}
	if len(cfg.RPS.Rates) > 1 {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
			"rps.rates 扫了 %d 档：多档=排队-延迟曲线扫描，逐档串行执行（总时长 ≈ Σ num_prompts/rate）", len(cfg.RPS.Rates)))
	}
	seenLevel := map[int]bool{}
	deduped := cfg.Concurrency.Levels[:0]
	for _, lv := range cfg.Concurrency.Levels {
		if lv <= 0 {
			return nil, fmt.Errorf("concurrency.levels 含非正值 %d——在飞请求数必须是正整数", lv)
		}
		if seenLevel[lv] {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("concurrency.levels 重复档位 %d 已去重", lv))
			continue
		}
		seenLevel[lv] = true
		deduped = append(deduped, lv)
	}
	cfg.Concurrency.Levels = deduped

	// slo（goodput + 基线）/ retry / salt
	if cfg.SLO != nil {
		if g := cfg.SLO.Goodput; g != nil {
			if g.TTFTMS < 0 || g.TPOTMS < 0 {
				return nil, fmt.Errorf("slo.goodput 阈值不能为负（ttft_ms=%v tpot_ms=%v）", g.TTFTMS, g.TPOTMS)
			}
			if g.TTFTMS == 0 && g.TPOTMS == 0 {
				return nil, fmt.Errorf("slo.goodput 已启用但 ttft_ms/tpot_ms 均为 0——至少配置一项才有判定意义（不打算用请整段注释掉）")
			}
		}
		if b := cfg.SLO.Baseline; b != nil {
			neg := []struct {
				name string
				val  float64
			}{
				{"short_good_ttft", b.ShortGoodTTFT}, {"short_pass_ttft", b.ShortPassTTFT},
				{"long_good_ttft", b.LongGoodTTFT}, {"long_pass_ttft", b.LongPassTTFT},
				{"good_tpot", b.GoodTPOT}, {"pass_tpot", b.PassTPOT},
				{"good_tps", b.GoodTPS}, {"pass_tps", b.PassTPS},
			}
			for _, kv := range neg {
				if kv.val < 0 {
					return nil, fmt.Errorf("slo.baseline.%s 不能为负", kv.name)
				}
			}
			if b.ShortMaxTokens < 0 || b.LongMinTokens < 0 {
				return nil, fmt.Errorf("slo.baseline.short_max_tokens / long_min_tokens 不能为负")
			}
			if b.ShortMaxTokens > 0 && b.LongMinTokens > 0 && b.LongMinTokens <= b.ShortMaxTokens {
				return nil, fmt.Errorf(
					"slo.baseline.long_min_tokens=%d 须大于 short_max_tokens=%d（短档上界与长档下界之间为中间带）",
					b.LongMinTokens, b.ShortMaxTokens)
			}
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
	if cfg.SeedSalt < 0 {
		return nil, fmt.Errorf("seed_salt 不能为负")
	}

	// max_prompt_tokens 联动：user 模式会话到顶后提前停轮的预告
	if cfg.MaxPromptTokens > 0 && cfg.MaxPromptTokens < 1000 {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
			"max_prompt_tokens=%d 过小（<1k）：多轮会话会被截到该值，确认单位是 token 而非其它", cfg.MaxPromptTokens))
	}

	return cfg, nil
}

// normalizeMaxTokens 校验并规范化输出长度档位（max_tokens 标量或列表）：
// 未配置（空）交默认值处理；显式非正值无论标量还是列表一律报错（口径一致）；
// 排序去重。返回规范化后的档位与是否发生过修正。
func normalizeMaxTokens(l IntList, where string) (IntList, bool, error) {
	if len(l) == 0 {
		return nil, false, nil
	}
	for _, v := range l {
		if v <= 0 {
			return nil, false, fmt.Errorf("%s 含非正值 %d——输出长度必须是正整数 token 数", where, v)
		}
	}
	orig := append([]int(nil), l...)
	sort.Ints(l)
	ded := l[:0]
	for i, t := range l {
		if i == 0 || t != ded[len(ded)-1] {
			ded = append(ded, t)
		}
	}
	l = ded
	changed := len(orig) != len(ded)
	for i := 0; !changed && i < len(orig); i++ {
		changed = orig[i] != ded[i]
	}
	return l, changed, nil
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

// EffGoodput 返回生效的 goodput 判定配置（slo.goodput）；未配置返回 nil。
func (c *Config) EffGoodput() *GoodputCfg {
	if c.SLO == nil {
		return nil
	}
	return c.SLO.Goodput
}

// EffBaseline 返回生效的体验基线评估阈值段（slo.baseline）；未配置返回 nil（报告用内置默认）。
func (c *Config) EffBaseline() *SLOBaselineCfg {
	if c.SLO == nil {
		return nil
	}
	return c.SLO.Baseline
}

// StreamEnabled 返回是否使用流式请求。
func (c *Config) StreamEnabled() bool { return *c.Stream }

// RawTimingsEnabled 原始 chunk 序列是否落盘（nil = 默认开）。关掉可显著减小长 soak
// 的 JSON 体积，但分位数之外的抖动/峰值信息随之不可复原。
func (c *Config) RawTimingsEnabled() bool { return c.RawTimings == nil || *c.RawTimings }
