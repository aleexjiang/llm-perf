// Package config 负责加载 yaml 配置并用环境变量覆盖。
// 配置按"通用 + 模型差异"组织：一份文件顶部写所有模型共享的通用配置，
// 底部用 model_overrides 按模型只写差异项（缺省继承通用配置），见 ForModel。
package config

import (
	"bytes"
	"fmt"
	"math"
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

type Single struct {
	Runs         int     `yaml:"runs"`
	PromptTokens []int   `yaml:"prompt_tokens"`
	MaxTokens    IntList `yaml:"max_tokens"` // 标量或列表（列表 = 输出长度扫描）
	FixedSeed    bool    `yaml:"fixed_seed"`
}

type Multiturn struct {
	Sessions       int     `yaml:"sessions"`
	Turns          int     `yaml:"turns"`
	SystemTokens   int     `yaml:"system_tokens"`
	ToolDefsTokens int     `yaml:"tool_defs_tokens"`
	TurnTokens     int     `yaml:"turn_tokens"`
	MaxTokens      IntList `yaml:"max_tokens"` // 标量或列表（列表 = 输出长度扫描）
	KeepAssistant  bool    `yaml:"keep_assistant"`
	// MaxReplyChars assistant 回复保留进 history 的截断长度（按 rune 计，中文安全），默认 2000。
	// 之前按字节切（reply[:2000]），中文会切出半个 UTF-8 字符发给服务端
	MaxReplyChars int `yaml:"max_reply_chars"`
	// SharedBase 基座（system + tool defs）跨会话共享：true = 全部会话用同一套前缀，
	// 贴近"一套部署一套提示词"的真实形态，测的是**跨用户共享前缀值多少 TTFT**；
	// false = 每会话独立基座（互异内容，缓存不可跨会话复用），基线形态。
	// 用指针是刻意的：nil = 未配置 → 默认 true（2026-09-12 拍板），显式 false 才关。
	// 逐轮 user 内容**始终**按会话独立（与基座共享与否无关），否则会话之间会变成
	// 逐字节相同，跨会话对比与 per-session 统计都失去意义。
	SharedBase *bool `yaml:"shared_base"`

	// Profiles 混合档（5.11）：非空时并发多轮的会话按权重分属不同增量档（跨会话混合）——
	// 测"重度会话与轻量会话同场竞技"时的容量与相互干扰（重度抢 KV 对轻量体验的影响）。
	// 会话增量/轮数由所属档位决定，标量 TurnTokens 在并发多轮下忽略（并存时告警）；
	// 分配用平滑加权轮转（与 5.6 concurrent.mix 同源，确定性可复现）。
	// 仅并发多轮生效（单发多轮忽略并提示）；filler 专属——dataset.mode=trace 时加载报错。
	Profiles []MixProfile `yaml:"profiles"`
}

// MixProfile 混合档的会话档位（5.11）：并发多轮时按权重把会话分配到不同增量档。
// 典型用法：真实增量档（+5k/轮）占多数 + 重度档（+27.5k/轮）占少数。
type MixProfile struct {
	Weight     int    `yaml:"weight"`      // 相对权重（正整数）
	Name       string `yaml:"name"`        // 档位名（报告分组用，留空自动 profile1/profile2…不可重复）
	TurnTokens int    `yaml:"turn_tokens"` // 该档每轮增量
	Turns      int    `yaml:"turns"`       // 该档会话轮数（0 = 继承 multiturn.turns）
}

// GetSharedBase 基座是否跨会话共享（未配置默认 true）。
func (m Multiturn) GetSharedBase() bool { return m.SharedBase == nil || *m.SharedBase }

// SetSharedBaseFalse 供测试与模型覆盖使用。
func (m *Multiturn) SetSharedBaseFalse() { f := false; m.SharedBase = &f }

// MixShape 混合负载的请求形状（5.6）：并发场景按 weight 确定性混跑长短请求。
// 线上流量从不均匀——均匀负载测出的吞吐/p99 系统性偏乐观，混跑才能测出容量折扣与真实尾延迟。
type MixShape struct {
	Weight       int    `yaml:"weight"`        // 相对权重（正整数），按平滑加权轮转展开成确定性序列
	Label        string `yaml:"label"`         // 形状名（报告分组用，留空自动 shape1/shape2…不可重复）
	PromptTokens int    `yaml:"prompt_tokens"` // 该形状输入长度
	MaxTokens    int    `yaml:"max_tokens"`    // 该形状输出上限（标量；与输出长度扫描正交，不做列表）
}

type Concurrent struct {
	Levels        []int      `yaml:"levels"`
	RunsPerWorker int        `yaml:"runs_per_worker"`
	PromptTokens  int        `yaml:"prompt_tokens"`
	MaxTokens     IntList    `yaml:"max_tokens"` // 标量或列表（列表 = 输出长度扫描）
	Multiturn     bool       `yaml:"multiturn"`  // true=每个虚拟用户各自跑完整多轮会话（filler=模拟对话，trace=真实会话重放）
	Mix           []MixShape `yaml:"mix"`        // 混合负载：非空时按权重混跑各形状（与 multiturn 互斥，
	// prompt_tokens/max_tokens 单值与 max_tokens 扫描失效）

	// 开环到达率模式（对齐 vLLM bench serve / inference-perf）：request_rate>0 或 rate_sweep
	// 非空时替代 levels 闭环——请求按 Poisson 过程到达，能测出排队-延迟曲线
	RequestRate    float64   `yaml:"request_rate"`    // 到达率（req/s），>0 启用开环模式
	RateSweep      []float64 `yaml:"rate_sweep"`      // 多档到达率扫描（饱和点寻找），每档跑一轮开环
	NumPrompts     int       `yaml:"num_prompts"`     // 开环模式总请求数（multiturn 时为总会话数）
	MaxConcurrency int       `yaml:"max_concurrency"` // 开环模式并发上限（0=不限）

	// Burstiness 开环到达的突发度（gamma 采样 shape，对齐 vLLM bench serve 的 burstiness）：
	// 1 = 标准泊松（指数间隔，默认）；<1 比泊松更突发；>1 趋向恒定间隔（均匀到达）。
	// 采样后按理论总量 (n-1)/rate 整体重整——跨 seed 到达总量严格一致，吞吐跨 run 可比。
	Burstiness float64 `yaml:"burstiness"`

	// 5.7 闭环错峰发车（默认开，仅闭环 levels 生效）：首批发 1 个会话/worker，等该批
	// 全部完成首轮后放下一批 min(上批×RampFactor, 剩余)——批次节奏由服务端首轮实际
	// 耗时决定（自适应，无需按端点调参），替代 barrier 齐射对服务端的瞬间满额冲击。
	// Ramp 显式 false 退回齐射；RampFactor 默认 2（1 = 逐个串行发车，无爬坡意义，报错）。
	Ramp       *bool `yaml:"ramp"`
	RampFactor int   `yaml:"ramp_factor"`

	// 10.5 时长制 soak（闭环 levels 专属；与开环互斥）：
	// DurationSeconds > 0 → 各档位跑满该时长为止（runs_per_worker 被忽略）——单轮 worker
	// 循环发到时长满；多轮要求 Renew: true，会话滚完 turns 轮后换新 seed 重开。
	// Renew 语义：会话滚完换新内容重开（上下文清零重涨），暂态后在途会话年龄铺满
	// 0~turns 区间 = 稳态；报告侧据此标稳态窗口、做首末时段漂移分析。
	DurationSeconds int  `yaml:"duration_seconds"`
	Renew           bool `yaml:"renew"`
}

// RampEnabled 闭环错峰发车是否启用（nil = 默认开）。
func (c Concurrent) RampEnabled() bool { return c.Ramp == nil || *c.Ramp }

// EffBurstiness 生效的开环到达突发度（<=0 回落 1 = 标准泊松）。
func (c Concurrent) EffBurstiness() float64 {
	if c.Burstiness <= 0 {
		return 1
	}
	return c.Burstiness
}

// EffRampFactor 生效的批次放大系数（<2 回落 2）。
func (c Concurrent) EffRampFactor() int {
	if c.RampFactor < 2 {
		return 2
	}
	return c.RampFactor
}

// DatasetCfg 数据源：filler（默认，token 精确的合成/语料填充，用于变量控制实验）
// 或 trace（真实会话回放，贴近客户实际流量分布）。
type DatasetCfg struct {
	Mode        string `yaml:"mode"`         // filler | trace
	Path        string `yaml:"path"`         // trace 文件路径（.json / .json.gz）
	Format      string `yaml:"format"`       // sharegpt | sessions（空=自动识别）
	MinTurns    int    `yaml:"min_turns"`    // 会话最少 user 轮数（sharegpt 过滤），默认 2
	MaxSessions int    `yaml:"max_sessions"` // 最多加载多少会话，0=不限
	// ReplayMode 回放保真度：full（默认，按原序注入全部 role——assistant/tool 消息进上下文，
	// 测真实 history 深度）| user_only（显式配置，只回放 user 轮——ShareGPT 问答类数据集
	// 或对比历史口径时用）。
	// user_only 的回放上下文系统性偏小（真实 agent 会话里工具结果往往占大头），full 才是忠实回放
	ReplayMode string `yaml:"replay_mode"`
}

// GoodputCfg SLO 约束（goodput 口径）：同时满足 TTFT 与 TPOT 上限的请求才算有效吞吐。
// 挂在 slo.goodput 下（原顶层 goodput: 已合流进 slo:，schema 不保兼容）。
type GoodputCfg struct {
	TTFTMS float64 `yaml:"ttft_ms"` // 如 2000
	TPOTMS float64 `yaml:"tpot_ms"` // 如 100
}

// SLOCfg SLO 口径段（5.8 合流）：goodput 判定阈值 + 报告"体验基线评估"阈值。
// 之前 goodput 阈值在 Go 侧、基线三档判据是报告脚本内置常量——同一份 SLO 拆在两处，
// 阈值改不动、口径对不齐；现在都从配置进、随 JSON 透出，报告侧消费同一份。
type SLOCfg struct {
	Goodput  *GoodputCfg     `yaml:"goodput"`  // goodput 判定（未配置 = 不算 goodput 列）
	Baseline *SLOBaselineCfg `yaml:"baseline"` // 体验基线评估阈值（未配置 = 报告用内置默认）
}

// SLOBaselineCfg 体验基线评估（3 档制）阈值。所有数值字段可省略——省略的字段用
// 报告脚本内置默认（SLO_TIERS，判据与出处 docs/latency-baselines.md §7）；写了就覆盖。
// 键名与报告脚本 SLO_TIERS 一致，JSON 透出后报告侧可直接 dict.update 合流。
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

// SaturationGuardCfg 饱和止损（默认关闭）：负载已饱和时停止加压，别把时间烧在
// 注定全错的深饱和区（stall_guard 管「服务端变慢」，本段管「负载积压」——互补）。
//
// 两个独立判据，任一触发即**停止向当前档位发新请求**（drain 语义：在飞请求自然跑完，
// 已发出的每个请求都保留完整数据——被截断的档位是干净的前缀样本而非残缺数据；
// 收尾最多多等一个 timeout_seconds），报告 aborted 留痕，场景层据此停止后续档位
// （饱和之后更高档只会更糟，继续加压是纯浪费）：
//   - max_waiting：服务端 waiting 排队深度持续 ≥ 阈值达 window_seconds（需 server_metrics）；
//   - max_wall_seconds：本档位发射窗口超过上限（GuideLLM max_duration 语义，不依赖观测层）。
type SaturationGuardCfg struct {
	Enabled *bool `yaml:"enabled"` // 默认 true（写了该段即生效）；false = 保留配置但不启用

	MaxWaiting     int     `yaml:"max_waiting"`      // waiting 排队深度阈值（0 = 不启用该判据）
	WindowSeconds  int     `yaml:"window_seconds"`   // 持续超阈多久触发，默认 120
	SampleSeconds  float64 `yaml:"sample_seconds"`   // 探测周期（秒），默认 5
	MaxWallSeconds int     `yaml:"max_wall_seconds"` // 每档位墙钟上限（0 = 不启用该判据）
}

// SatEnabled 该配置段是否生效（未配置或 enabled:false 都返回 false）。
func (s *SaturationGuardCfg) SatEnabled() bool {
	return s != nil && (s.Enabled == nil || *s.Enabled)
}

// GetMaxWaiting / GetWindowSeconds nil 安全读取：观测器在 guard 未配置段时也要能
// 构造判定核心（armed=false 时这些值不会被消费，但引用必须不 panic）。
func (s *SaturationGuardCfg) GetMaxWaiting() int {
	if s == nil {
		return 0
	}
	return s.MaxWaiting
}

func (s *SaturationGuardCfg) GetWindowSeconds() int {
	if s == nil {
		return 0
	}
	return s.WindowSeconds
}

// RetryCfg 连接层重试策略（默认关闭）：只重试瞬时失败（TCP/流被 reset、HTTP 5xx/429），
// 4xx 不重试。重试会记入 warnings 与 retry_count——计时窗口干净，但服务端不稳定仍可见。
type RetryCfg struct {
	MaxAttempts int `yaml:"max_attempts"` // 总尝试次数；0/1 = 不重试
	BackoffMS   int `yaml:"backoff_ms"`   // 退避基数，默认 300ms，指数退避封顶 5s
}

// StallGuardCfg 降速熔断（2026-09-11 新增，默认关闭；2026-09-16 判据改单流）：
// 按「单流 decode 速度中位」判定服务端是否已退化到不值得继续跑——长窗口下低产出
// 会白白烧掉几小时。
//
// 口径：各在飞流窗口内输出增量 / 窗口时长，取中位数（抗单流偶发抖动、反映普遍劣化；
// r1-off S5 实测聚合 ~113 tok/s 掩盖了单流 6–16 tok/s 的劣化，故弃用聚合判据）。
// 判定：中位速度连续低于 min_tps 达 window_seconds 即触发；任一次采样回升到阈值
// 以上即重置计时。空闲与纯 prefill（未出首 token）不参与判定，避免误触发
// （细节见 internal/engine/stall.go）。
//
// 触发后只中止**当前场景**（不是整轮），冷却 cooldown_seconds 后继续下一个场景；
// 已完成的数据照常落盘，报告 note 与 run.log 里标注熔断原因与现场速度。
type StallGuardCfg struct {
	Enabled         *bool   `yaml:"enabled"`          // 默认 true（写了该段即生效）；false = 保留配置但不启用
	MinTPS          float64 `yaml:"min_tps"`          // 阈值（tok/s，单流 decode 速度中位），默认 10——熔断兜底（防接近死机白烧长跑），不是 UX 评级；评级线见 docs/latency-baselines.md §8
	WindowSeconds   int     `yaml:"window_seconds"`   // 连续低于阈值多久触发，默认 600（10 分钟）
	CooldownSeconds int     `yaml:"cooldown_seconds"` // 触发后到下一个场景的冷却，默认 300（5 分钟）；0 = 不等
	SampleSeconds   float64 `yaml:"sample_seconds"`   // 采样周期（秒），默认 2；支持亚秒（熔断回归用 0.5）

	// 8.2 冷却+探针：冷却只给恢复留时间窗，续跑与否由探针实测决定——冷却结束后发短探针
	// （4k prompt / 256 输出 ×3 取中位），实测 tok/s ≥ ProbeFactor×min_tps 才继续下一个
	// 场景，否则停止整轮。默认 2；显式 0 = 关闭探针、退回纯计时冷却（旧行为）。
	// 指针类型是为了区分「未配置」（默认 2）与「显式 0」（关闭）。
	ProbeFactor *float64 `yaml:"probe_factor"`
}

// EffProbeFactor 生效的探针倍数（nil = 默认 2；负数视作 0 = 关闭）。
func (sg *StallGuardCfg) EffProbeFactor() float64 {
	if sg == nil || sg.ProbeFactor == nil {
		return 2
	}
	if *sg.ProbeFactor < 0 {
		return 0
	}
	return *sg.ProbeFactor
}

// StallEnabled 该配置段是否生效（未配置或 enabled:false 都返回 false）。
func (s *StallGuardCfg) StallEnabled() bool {
	return s != nil && (s.Enabled == nil || *s.Enabled)
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

// ProbeExtraBodies 供 probe 取「思考开启态 / 关闭态」两份 extra_body。
// levels 模式下 extra_body_on/off 通常为空（参数写在 levels 里），若直接读取会让 probe
// 整块思考探测被跳过——部署侧关思考这类问题就查不出来。这里从 levels 兜底：
// 取第一个 enabled=true 的变体当开启态、第一个 enabled=false 的当关闭态。
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
	return on, off
}

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
		if ot.MaxTokensFloor > 0 {
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
	if s := ov.Single; s != nil {
		m := v.Single
		if s.Runs > 0 {
			m.Runs = s.Runs
		}
		if len(s.PromptTokens) > 0 {
			m.PromptTokens = s.PromptTokens
		}
		if len(s.MaxTokens) > 0 {
			m.MaxTokens = s.MaxTokens
		}
		if s.FixedSeed {
			m.FixedSeed = true
		}
		v.Single = m
	}
	if s := ov.Multiturn; s != nil {
		m := v.Multiturn
		if s.Sessions > 0 {
			m.Sessions = s.Sessions
		}
		if s.Turns > 0 {
			m.Turns = s.Turns
		}
		if s.SystemTokens > 0 {
			m.SystemTokens = s.SystemTokens
		}
		if s.ToolDefsTokens > 0 {
			m.ToolDefsTokens = s.ToolDefsTokens
		}
		if s.TurnTokens > 0 {
			m.TurnTokens = s.TurnTokens
		}
		if len(s.MaxTokens) > 0 {
			m.MaxTokens = s.MaxTokens
		}
		if s.KeepAssistant {
			m.KeepAssistant = true
		}
		if s.MaxReplyChars > 0 {
			m.MaxReplyChars = s.MaxReplyChars
		}
		if len(s.Profiles) > 0 {
			m.Profiles = s.Profiles // 档位切片运行期只读，浅拷贝即可（同 Concurrent.Mix）
		}
		v.Multiturn = m
	}
	if s := ov.Concurrent; s != nil {
		m := v.Concurrent
		if len(s.Levels) > 0 {
			m.Levels = s.Levels
		}
		if s.RunsPerWorker > 0 {
			m.RunsPerWorker = s.RunsPerWorker
		}
		if s.PromptTokens > 0 {
			m.PromptTokens = s.PromptTokens
		}
		if len(s.MaxTokens) > 0 {
			m.MaxTokens = s.MaxTokens
		}
		if len(s.Mix) > 0 {
			m.Mix = s.Mix // 形状切片运行期只读，浅拷贝即可
		}
		if s.Multiturn {
			m.Multiturn = true
		}
		if s.RequestRate > 0 {
			m.RequestRate = s.RequestRate
		}
		if len(s.RateSweep) > 0 {
			m.RateSweep = s.RateSweep
		}
		if s.NumPrompts > 0 {
			m.NumPrompts = s.NumPrompts
		}
		if s.MaxConcurrency > 0 {
			m.MaxConcurrency = s.MaxConcurrency
		}
		if s.Burstiness > 0 {
			m.Burstiness = s.Burstiness
		}
		v.Concurrent = m
	}
	if ov.MaxPromptTokens != nil {
		v.MaxPromptTokens = *ov.MaxPromptTokens
	}
	if ov.Stream != nil {
		v.Stream = ov.Stream
	}
	// 10.1 per-model 熔断标定：单模型标定值套全模型会误杀更慢的模型（同一份配置逐模型跑
	// 就已踩此口径）——此处按字段级覆盖，未写的键继承顶层（顶层已做过默认值归一）。
	if sg := ov.StallGuard; sg != nil {
		m := StallGuardCfg{}
		if c.StallGuard != nil {
			m = *c.StallGuard
		}
		if sg.Enabled != nil {
			m.Enabled = sg.Enabled
		}
		if sg.MinTPS > 0 {
			m.MinTPS = sg.MinTPS
		}
		if sg.WindowSeconds > 0 {
			m.WindowSeconds = sg.WindowSeconds
		}
		if sg.CooldownSeconds > 0 {
			m.CooldownSeconds = sg.CooldownSeconds
		}
		if sg.SampleSeconds > 0 {
			m.SampleSeconds = sg.SampleSeconds
		}
		if sg.ProbeFactor != nil {
			m.ProbeFactor = sg.ProbeFactor
		}
		v.StallGuard = &m
	}
	return &v
}

// ModelOverride 单个模型的差异配置：只写与通用配置不同的项，未写的键继承顶层。
// 段内覆盖语义与 ThinkingFor 一致——零值/缺省 = 继承；因此布尔与"合法零值"字段
// （fixed_seed、keep_assistant、concurrent.multiturn 等）无法在模型层显式改回 false，
// 这类全局形状请保持各模型一致或拆分配置。端点级配置（endpoint/认证/timeout_seconds/
// server_metrics）不在此覆盖——一个测试一个端点，超时由 client 统一持有。
// 例外：stall_guard.enabled 是指针，故可在模型层显式关掉该模型的熔断。
type ModelOverride struct {
	Thinking   *Thinking   `yaml:"thinking"` // 覆盖全局 thinking（字段级，未写的继承）
	Single     *Single     `yaml:"single"`
	Multiturn  *Multiturn  `yaml:"multiturn"`
	Concurrent *Concurrent `yaml:"concurrent"`
	// StallGuard 按模型覆盖降速熔断阈值（字段级）：各模型 decode 速度不同——用同一个 min_tps
	// 会把更慢的模型误熔断（或对更快的模型形同虚设）。建议取该模型 probe 实测速度的 10–20%。
	StallGuard *StallGuardCfg `yaml:"stall_guard"`
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
	FillerLang     string `yaml:"filler_lang"`
	FillerCorpus   string `yaml:"filler_corpus"` // "":合成词表 | "en"/"zh":内置公版书语料 | 文件路径(.txt/.txt.gz):自定义语料
	Stream         *bool  `yaml:"stream"`        // 默认 true；false 时 TTFT/ITL/思考拆分不可测（N/A）
	Debug          bool   `yaml:"debug"`         // true: 每个请求的原始响应留存到 <output_dir>/raw/，日志同步写 run.log（排查魔改引擎用）
	// RawTimings 原始 chunk 序列落盘（nil = 默认开）：流式请求把每个含 token chunk 的时刻
	// 记入 content_times_ms（相对 sent_at 的毫秒偏移）。峰值秒桶吞吐、ITL 抖动等外部分析
	// 都依赖这份原始序列；体积随输出 token 数线性增长，超长 soak 可置 false 关闭。
	RawTimings *bool    `yaml:"raw_timings"`
	Models     []string `yaml:"models"`

	// Test 本轮测试类别（benchmark | performance | soak，留空 = performance）。
	// 只切换报告的**结论区口径**，不改测量本身：三者共用同一套数据与判据
	// （指标层已冻结为四个数，见 docs/testing-architecture.md）——类别是表达层的焦点声明，
	// 不是第二套管线。soak 的时长制原语见 docs/scenario-guide.md，采集层只保留原始证据，
	// 现有可得证据（事故/提前终止/canary），缺证据处如实写 NA。
	Test string `yaml:"test"`

	// MaxPromptTokens 上下文截止（tokens）：>0 时所有多轮请求的 prompt 规模都不超过该值；
	// 多轮会话 ctx 到顶后停止加轮。0 = 不限制。
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
	// 跨请求存活——同一配置重跑时 prompt 与上次完全相同，"冷缓存"测量会被上次测试污染。
	// 每次测试（改代码/改配置后的重测）递增盐值即可隔离；不改服务端也能拿到干净的冷缓存。
	SeedSalt int `yaml:"seed_salt"`

	// Warnings 配置诊断提示（Load 时生成，非序列化字段）：不阻止运行，
	// 但启动时打印——数量级不合理、轮次不足、覆盖关系等"合法但值得知道"的事
	Warnings []string `yaml:"-"`

	Dataset         DatasetCfg          `yaml:"dataset"`
	SLO             *SLOCfg             `yaml:"slo"` // SLO 口径：goodput 判定 + 基线评估阈值（原顶层 goodput 已合流）
	Retry           *RetryCfg           `yaml:"retry"`
	StallGuard      *StallGuardCfg      `yaml:"stall_guard"`      // 降速熔断（默认关闭）
	SaturationGuard *SaturationGuardCfg `yaml:"saturation_guard"` // 饱和止损（默认关闭）
	Correctness     *CorrectnessCfg     `yaml:"correctness"`

	Thinking Thinking `yaml:"thinking"`

	// ModelOverrides 按模型覆盖通用配置（键=模型名，必须在 models 列表内）。
	// 组织方式：一份配置顶部写所有模型共享的通用配置，底部按模型只写差异项，
	// 未写的键继承顶层——场景层在模型循环内通过 ForModel 取该模型生效的配置视图。
	// 思考按模型覆盖写 model_overrides.<模型>.thinking（字段级覆盖）。
	ModelOverrides map[string]*ModelOverride `yaml:"model_overrides"`

	Single     Single     `yaml:"single"`
	Multiturn  Multiturn  `yaml:"multiturn"`
	Concurrent Concurrent `yaml:"concurrent"`

	// 运行时解析
	APIKey string `yaml:"-"`
}

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

// isBuiltinCorpus 判断 filler_corpus 是否为内置语料哨兵（en/zh）。
// 哨兵不是路径，必须原样透传给 corpus.Load，不能参与相对路径拼接。
// 取值集合与 internal/corpus.Load 的 switch 保持一致，新增内置语料时两处同步。
func isBuiltinCorpus(spec string) bool {
	return spec == "en" || spec == "zh"
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
		cfg.Raw = string(data)
		// 相对路径以配置文件所在目录为基准（dataset.path / filler_corpus 等），
		// 与启动时的工作目录解耦——从任何目录 `bench -f configs/xxx.yaml` 结果一致
		if isRel := !filepath.IsAbs(cfg.Dataset.Path) && cfg.Dataset.Path != ""; isRel {
			cfg.Dataset.Path = filepath.Join(filepath.Dir(path), cfg.Dataset.Path)
		}
		// 与 dataset.path 同口径：无条件 join（不判断目标文件是否存在）——
		// 按存在与否分叉会导致写错路径时报错信息按 cwd 拼接，误导排查。
		// 例外：en/zh 是内置语料哨兵（见 isBuiltinCorpus），不是路径
		if cfg.FillerCorpus != "" && !isBuiltinCorpus(cfg.FillerCorpus) && !filepath.IsAbs(cfg.FillerCorpus) {
			cfg.FillerCorpus = filepath.Join(filepath.Dir(path), cfg.FillerCorpus)
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
	if cfg.Thinking.MaxTokensFloor <= 0 {
		cfg.Thinking.MaxTokensFloor = 2048
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
		if ov.Single != nil && len(ov.Single.PromptTokens) > 0 {
			normalized, _, err := normalizeLadder(ov.Single.PromptTokens,
				fmt.Sprintf("model_overrides[%s].single.prompt_tokens", name))
			if err != nil {
				return nil, err
			}
			ov.Single.PromptTokens = normalized
		}
		if ov.StallGuard != nil {
			sg := ov.StallGuard
			if sg.MinTPS < 0 || sg.WindowSeconds < 0 || sg.CooldownSeconds < 0 || sg.SampleSeconds < 0 {
				return nil, fmt.Errorf("model_overrides[%s].stall_guard 的 min_tps/window_seconds/cooldown_seconds/sample_seconds 不能为负", name)
			}
			if sg.ProbeFactor != nil && *sg.ProbeFactor < 0 {
				return nil, fmt.Errorf("model_overrides[%s].stall_guard.probe_factor=%.3g 不能为负（0 = 关闭探针）", name, *sg.ProbeFactor)
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
	// 各场景默认值
	if cfg.Single.Runs <= 0 {
		cfg.Single.Runs = 3
	}
	if len(cfg.Single.PromptTokens) == 0 {
		cfg.Single.PromptTokens = []int{4000, 10000, 20000, 40000}
	}
	if mt, changed, err := normalizeMaxTokens(cfg.Single.MaxTokens, "single.max_tokens"); err != nil {
		return nil, err
	} else if len(mt) > 0 {
		cfg.Single.MaxTokens = mt
		if changed {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"single.max_tokens 已排序去重 → %v（输出长度扫描按升序执行）", mt))
		}
	}
	if len(cfg.Single.MaxTokens) == 0 {
		cfg.Single.MaxTokens = IntList{512}
	}
	rawMultiturnTurnTokens := cfg.Multiturn.TurnTokens // 供 profiles 并存告警判别标量是否被显式填写
	if cfg.Multiturn.Sessions <= 0 {
		cfg.Multiturn.Sessions = 2
	}
	if cfg.Multiturn.Turns <= 0 {
		cfg.Multiturn.Turns = 8
	}
	if cfg.Multiturn.TurnTokens <= 0 {
		cfg.Multiturn.TurnTokens = 2000
	}
	if mt, changed, err := normalizeMaxTokens(cfg.Multiturn.MaxTokens, "multiturn.max_tokens"); err != nil {
		return nil, err
	} else if len(mt) > 0 {
		cfg.Multiturn.MaxTokens = mt
		if changed {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"multiturn.max_tokens 已排序去重 → %v（输出长度扫描按升序执行）", mt))
		}
	}
	if len(cfg.Multiturn.MaxTokens) == 0 {
		cfg.Multiturn.MaxTokens = IntList{256}
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
	if mt, changed, err := normalizeMaxTokens(cfg.Concurrent.MaxTokens, "concurrent.max_tokens"); err != nil {
		return nil, err
	} else if len(mt) > 0 {
		cfg.Concurrent.MaxTokens = mt
		if changed {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"concurrent.max_tokens 已排序去重 → %v（输出长度扫描按升序执行）", mt))
		}
	}
	if len(cfg.Concurrent.MaxTokens) == 0 {
		cfg.Concurrent.MaxTokens = IntList{256}
	}
	// 混合负载（5.6）：形状校验 + 与单值/扫描维度的冲突告警
	if len(cfg.Concurrent.Mix) > 0 {
		if cfg.Concurrent.Multiturn {
			return nil, fmt.Errorf("concurrent.mix 与 multiturn: true 互斥：多轮会话的形状由 multiturn 配置（或 trace 数据）决定，无法按权重混跑；跨会话混合请用 multiturn.profiles")
		}
		seen := map[string]bool{}
		for i := range cfg.Concurrent.Mix {
			s := &cfg.Concurrent.Mix[i]
			if s.Weight <= 0 {
				return nil, fmt.Errorf("concurrent.mix[%d].weight 必须为正整数（相对权重）", i)
			}
			if s.PromptTokens <= 0 {
				return nil, fmt.Errorf("concurrent.mix[%d].prompt_tokens 必须为正（该形状输入长度）", i)
			}
			if s.MaxTokens <= 0 {
				return nil, fmt.Errorf("concurrent.mix[%d].max_tokens 必须为正（该形状输出上限，标量）", i)
			}
			if s.Label == "" {
				s.Label = fmt.Sprintf("shape%d", i+1)
			}
			if seen[s.Label] {
				return nil, fmt.Errorf("concurrent.mix label %q 重复（报告按 label 分组）", s.Label)
			}
			seen[s.Label] = true
		}
		cfg.Warnings = append(cfg.Warnings,
			"concurrent.mix 已配置：该场景下请求形状由 mix 各项决定，prompt_tokens/max_tokens 单值与 max_tokens 输出扫描失效")
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
	// 回放保真度：枚举校验 + 数据源联动。默认 full（完整 role 序列——agent 会话的 token
	// 大头在 assistant/tool 回灌，user_only 会系统性低估上下文；需要旧口径时显式配置）。
	rawReplayMode := cfg.Dataset.ReplayMode
	switch cfg.Dataset.ReplayMode {
	case "":
		cfg.Dataset.ReplayMode = "full"
	case "user_only", "full":
	default:
		return nil, fmt.Errorf("dataset.replay_mode 无效值 %q（可选 user_only/full）", cfg.Dataset.ReplayMode)
	}
	// 仅在用户显式配置了 full + 非 trace 时提醒（默认值触达 filler 场景不打扰）
	if rawReplayMode == "full" && cfg.Dataset.Mode != "trace" {
		cfg.Warnings = append(cfg.Warnings,
			"dataset.replay_mode=full 仅在 dataset.mode=trace 下生效（filler 模式没有原始会话可回放，忽略）")
	}
	// 多轮回复截断：rune 口径，默认 2000
	if cfg.Multiturn.MaxReplyChars <= 0 {
		cfg.Multiturn.MaxReplyChars = 2000
	}
	if cfg.Multiturn.MaxReplyChars < 100 {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
			"multiturn.max_reply_chars=%d 过小：assistant 回复几乎全被截掉，history 深度失真（默认 2000）",
			cfg.Multiturn.MaxReplyChars))
	}
	if cfg.Concurrent.RequestRate < 0 {
		return nil, fmt.Errorf("concurrent.request_rate 不能为负")
	}
	if cfg.Concurrent.Burstiness < 0 || math.IsNaN(cfg.Concurrent.Burstiness) || math.IsInf(cfg.Concurrent.Burstiness, 0) {
		return nil, fmt.Errorf("concurrent.burstiness 必须是有限非负数（1=标准泊松；<1 更突发；>1 趋向均匀）")
	}
	if cfg.Concurrent.RampFactor < 0 {
		return nil, fmt.Errorf("concurrent.ramp_factor 不能为负（默认 2；1 = 逐个串行发车，无爬坡意义）")
	}
	if cfg.Concurrent.RampFactor == 1 {
		cfg.Warnings = append(cfg.Warnings,
			"concurrent.ramp_factor=1 相当于逐个串行发车：爬坡期被拉到整个场景长度，通常不是想要的效果")
	}

	// ── 输入合理性校验：错误在开跑前暴露，而不是跑完才发现 ──

	// single 结构暂保留用于读取旧配置/旧内部测试，但公共 bench 入口不再执行；
	// 显式配置时提醒用户迁移到 multiturn/concurrent，避免以为单发单轮仍会运行。
	if len(cfg.Single.PromptTokens) > 0 || cfg.Single.Runs > 0 || len(cfg.Single.MaxTokens) > 0 {
		cfg.Warnings = append(cfg.Warnings, "single 配置已废弃且不会由公共 bench 执行，请迁移到 multiturn 或 concurrent")
	}

	// 单发档位：拒绝非正值；排序去重（被修正时提示）；相邻增量 <10% 拒绝
	if len(cfg.Single.PromptTokens) > 0 {
		normalized, changed, err := normalizeLadder(cfg.Single.PromptTokens, "single.prompt_tokens")
		if err != nil {
			return nil, err
		}
		cfg.Single.PromptTokens = normalized
		if changed {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"single.prompt_tokens 已排序去重 → %v（原顺序/重复档位不影响结果，但图表与日志按修正后顺序展示）", normalized))
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
	if cfg.Multiturn.TurnTokens > 0 && len(cfg.Multiturn.Profiles) == 0 {
		base := cfg.Multiturn.SystemTokens + cfg.Multiturn.ToolDefsTokens
		reach := base + cfg.Multiturn.Turns*cfg.Multiturn.TurnTokens
		w := fmt.Sprintf("多轮可达深度：base %d + %d 轮 × %d ≈ 末轮 %d token",
			base, cfg.Multiturn.Turns, cfg.Multiturn.TurnTokens, reach)
		if cfg.MaxPromptTokens > 0 && reach > cfg.MaxPromptTokens {
			w += fmt.Sprintf("（超过 max_prompt_tokens=%d，到顶后提前停轮）", cfg.MaxPromptTokens)
		}
		cfg.Warnings = append(cfg.Warnings, w)
	}

	// 混合档（5.11）：会话按权重分属不同增量档——校验 + 逐档位可达深度（标量深度行让位于此）
	if len(cfg.Multiturn.Profiles) > 0 {
		seenProf := map[string]bool{}
		base := cfg.Multiturn.SystemTokens + cfg.Multiturn.ToolDefsTokens
		for i := range cfg.Multiturn.Profiles {
			p := &cfg.Multiturn.Profiles[i]
			if p.Weight <= 0 {
				return nil, fmt.Errorf("multiturn.profiles[%d].weight 必须为正整数（相对权重）", i)
			}
			if p.TurnTokens <= 0 {
				return nil, fmt.Errorf("multiturn.profiles[%d].turn_tokens 必须为正（该档每轮增量）", i)
			}
			if p.TurnTokens > 200000 {
				return nil, fmt.Errorf("multiturn.profiles[%d].turn_tokens=%d 过大：单条 user 消息大概率超过模型上下文上限（同 multiturn.turn_tokens 上限 200000）",
					i, p.TurnTokens)
			}
			if p.Turns < 0 {
				return nil, fmt.Errorf("multiturn.profiles[%d].turns 不能为负（0 = 继承 multiturn.turns）", i)
			}
			if p.Name == "" {
				p.Name = fmt.Sprintf("profile%d", i+1)
			}
			if seenProf[p.Name] {
				return nil, fmt.Errorf("multiturn.profiles name %q 重复（报告按 name 分组）", p.Name)
			}
			seenProf[p.Name] = true
			effTurns := p.Turns
			if effTurns <= 0 {
				effTurns = cfg.Multiturn.Turns
			}
			reach := base + effTurns*p.TurnTokens
			w := fmt.Sprintf("混合档 %s（权重 %d）：base %d + %d 轮 × %d ≈ 末轮 %d token",
				p.Name, p.Weight, base, effTurns, p.TurnTokens, reach)
			if cfg.MaxPromptTokens > 0 && reach > cfg.MaxPromptTokens {
				w += fmt.Sprintf("（超过 max_prompt_tokens=%d，到顶后提前停轮）", cfg.MaxPromptTokens)
			}
			cfg.Warnings = append(cfg.Warnings, w)
		}
		if rawMultiturnTurnTokens > 0 {
			cfg.Warnings = append(cfg.Warnings,
				"multiturn.profiles 已配置：并发多轮的会话增量/轮数由各档位决定，标量 turn_tokens 不再参与（单发多轮不受影响）")
		}
		if cfg.Dataset.Mode == "trace" {
			return nil, fmt.Errorf(
				"multiturn.profiles 与 dataset.mode=trace 互斥：重放的会话形状（轮数/增量）来自 trace 数据，无法按权重分档；请去掉 profiles——trace 的跨会话差异天然来自真实会话")
		}
	}

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
	if cfg.MultiturnMaxDepth() >= 100000 {
		deepCtx = true
	}
	if cfg.TimeoutSeconds > 3600 {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("timeout_seconds=%d 超过 1 小时：确认不是把毫秒当秒填了", cfg.TimeoutSeconds))
	}
	if thinkMayOn && deepCtx && cfg.TimeoutSeconds < 300 {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
			"timeout_seconds=%d 偏小：思考开启 + 100k 级上下文时单请求可达 3–8 分钟，会被误判超时", cfg.TimeoutSeconds))
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
	for _, mt := range cfg.Single.MaxTokens {
		guardShortOutput("single", mt)
	}
	for _, mt := range cfg.Multiturn.MaxTokens {
		guardShortOutput("multiturn", mt)
	}
	// mix 模式下 concurrent 的输出上限由各形状自带，单值守卫不适用
	if len(cfg.Concurrent.Mix) == 0 {
		for _, mt := range cfg.Concurrent.MaxTokens {
			guardShortOutput("concurrent", mt)
		}
	}
	// 输入扫了多档但输出只有一档：prefill/decode 效应混在一起，斜率结论归因不清
	if len(cfg.Single.PromptTokens) > 1 && len(cfg.Single.MaxTokens) == 1 {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
			"single.prompt_tokens 扫了 %d 档但 max_tokens 只有单档 %d：输入/输出两个一级变量只扫了一个，"+
				"TTFT 斜率里混着 decode 效应；容量规划建议输出长度也做多档对照",
			len(cfg.Single.PromptTokens), cfg.Single.MaxTokens[0]))
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
		for _, mt := range cfg.Single.MaxTokens {
			if mt < cfg.Thinking.MaxTokensFloor {
				cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
					"single.max_tokens=%d < max_tokens_floor=%d：thinking=on 的请求会被抬高到 floor，off/on 的 E2E 不可直接横向比（off 受输出档钳制、on 受 floor 抬高）",
					mt, cfg.Thinking.MaxTokensFloor))
			}
		}
		for _, mt := range cfg.Multiturn.MaxTokens {
			if mt < cfg.Thinking.MaxTokensFloor {
				cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
					"multiturn.max_tokens=%d < max_tokens_floor=%d：thinking=on 的请求会被抬高到 floor",
					mt, cfg.Thinking.MaxTokensFloor))
			}
		}
	}

	// 多轮深度与单发档位的衔接（混合档取最深档位口径）
	if reach := cfg.MultiturnMaxDepth(); reach > 0 && len(cfg.Single.PromptTokens) > 0 {
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
	} else if b := cfg.Concurrent.Burstiness; b > 0 && b != 1 {
		cfg.Warnings = append(cfg.Warnings,
			"concurrent.burstiness 仅在开环模式（request_rate/rate_sweep）下生效，闭环 levels 模式会忽略")
	}
	// 10.5 时长制 soak（duration_seconds + renew；闭环专属）。约束取严格口径（fail-fast）：
	// 开环时长由 num_prompts × 到达率决定、不适用；多轮必须显式 renew 才能跑满时长。
	if d := cfg.Concurrent.DurationSeconds; d > 0 {
		if openLoop {
			return nil, fmt.Errorf(
				"duration_seconds 仅闭环 levels 模式：开环的时长由 num_prompts × 到达率决定（请去掉 request_rate/rate_sweep 或 duration_seconds）")
		}
		if cfg.Dataset.Mode == "trace" {
			return nil, fmt.Errorf(
				"duration_seconds 暂不支持 trace 回放（会话续跑需要 filler 形状）；请改用 filler 数据源")
		}
		if cfg.Concurrent.Multiturn && !cfg.Concurrent.Renew {
			return nil, fmt.Errorf(
				"concurrent 多轮 + duration_seconds=%d 需 renew: true——会话滚完换新内容重开才能跑满时长（否则单会话跑完即结束，时长语义不成立）", d)
		}
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
			"duration_seconds=%d 时长制生效：各档位按墙钟跑满（runs_per_worker 被忽略；在飞请求跑完保留）", d))
	} else if cfg.Concurrent.Renew {
		return nil, fmt.Errorf(
			"concurrent.renew 需配合 duration_seconds>0——续跑要有时长边界，否则会话滚完换新重开将无限循环")
	}
	if cfg.Concurrent.Renew && !cfg.Concurrent.Multiturn {
		return nil, fmt.Errorf(
			"concurrent.renew 仅多轮会话（multiturn: true）有效——单轮请求天然每次都是新会话")
	}
	if !cfg.Concurrent.Multiturn && cfg.Concurrent.PromptTokens > 200000 {
		return nil, fmt.Errorf(
			"concurrent.prompt_tokens=%d 过大：单条消息大概率超过模型上下文上限（阈值 200k，与 single 档位同一约束）",
			cfg.Concurrent.PromptTokens)
	}

	// slo（goodput + 基线）/ retry / correctness / warmup / salt
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
	if cfg.SaturationGuard != nil {
		sg := cfg.SaturationGuard
		if sg.MaxWaiting < 0 || sg.WindowSeconds < 0 || sg.SampleSeconds < 0 || sg.MaxWallSeconds < 0 {
			return nil, fmt.Errorf("saturation_guard 的 max_waiting/window_seconds/sample_seconds/max_wall_seconds 不能为负")
		}
		if sg.SatEnabled() {
			if sg.MaxWaiting <= 0 && sg.MaxWallSeconds <= 0 {
				cfg.Warnings = append(cfg.Warnings,
					"saturation_guard 已启用但 max_waiting/max_wall_seconds 均未配置——没有判定阈值，本段不生效（0 = 关闭对应判据）")
			}
			if sg.MaxWaiting > 0 {
				if sg.WindowSeconds == 0 {
					sg.WindowSeconds = 120 // 默认持续 2 分钟
				}
				if sg.SampleSeconds == 0 {
					sg.SampleSeconds = 5
				}
				if sg.SampleSeconds > float64(sg.WindowSeconds) {
					return nil, fmt.Errorf("saturation_guard.sample_seconds=%.3g 大于 window_seconds=%d：探测间隔比判定窗口还长，永远判不出持续积压",
						sg.SampleSeconds, sg.WindowSeconds)
				}
				if !cfg.ServerMetrics {
					cfg.Warnings = append(cfg.Warnings,
						"saturation_guard.max_waiting 依赖服务端 /metrics 的 waiting 排队深度（server_metrics: true），当前未启用——waiting 判定不生效，墙钟上限仍有效")
				}
				cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
					"饱和止损已启用：排队深度 waiting≥%d 持续 %ds 即截断当前档位并停止后续档位（已完成数据照常落盘）",
					sg.MaxWaiting, sg.WindowSeconds))
			}
			if sg.MaxWallSeconds > 0 {
				cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
					"饱和止损已启用：单个档位/到达率墙钟超过 %ds 即截断并停止后续档位", sg.MaxWallSeconds))
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
	if cfg.Correctness != nil && cfg.Correctness.Samples < 0 {
		return nil, fmt.Errorf("correctness.samples 不能为负")
	}
	if cfg.StallGuard != nil {
		sg := cfg.StallGuard
		if sg.MinTPS < 0 || sg.WindowSeconds < 0 || sg.CooldownSeconds < 0 || sg.SampleSeconds < 0 {
			return nil, fmt.Errorf("stall_guard 的 min_tps/window_seconds/cooldown_seconds/sample_seconds 不能为负")
		}
		if sg.ProbeFactor != nil && *sg.ProbeFactor < 0 {
			return nil, fmt.Errorf("stall_guard.probe_factor=%.3g 不能为负（0 = 关闭探针，退回纯计时冷却）", *sg.ProbeFactor)
		}
		if sg.StallEnabled() {
			if sg.MinTPS == 0 {
				sg.MinTPS = 10 // 默认阈值 10 tok/s（跨机器安全下限；部署级建议值由 bench probe 实测输出）
			}
			if sg.WindowSeconds == 0 {
				sg.WindowSeconds = 600 // 默认持续 10 分钟
			}
			if sg.SampleSeconds == 0 {
				sg.SampleSeconds = 2
			}
			if sg.SampleSeconds > float64(sg.WindowSeconds) {
				return nil, fmt.Errorf("stall_guard.sample_seconds=%.3g 大于 window_seconds=%d：采样间隔比判定窗口还长，永远判不出持续降速",
					sg.SampleSeconds, sg.WindowSeconds)
			}
			// 冷却默认 5 分钟：由 main 在场景之间执行；0 表示触发后立即继续下一个场景。
			if sg.CooldownSeconds == 0 {
				sg.CooldownSeconds = 300
			}
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"降速熔断已启用：单流 decode 速度中位持续低于 %.0f tok/s 达 %ds 即中止当前场景，冷却 %ds 后继续下一个场景",
				sg.MinTPS, sg.WindowSeconds, sg.CooldownSeconds))
			// 10.1 多模型标定提醒：一个阈值套所有模型会误杀更慢的模型（同一份配置逐模型
			// 跑就已踩此口径）。提示按模型覆盖，但不阻止运行。
			if active := cfg.ActiveModels(); len(active) > 1 {
				var missing []string
				for _, m := range active {
					if ov := cfg.ModelOverrides[m]; ov == nil || ov.StallGuard == nil {
						missing = append(missing, m)
					}
				}
				if len(missing) > 0 {
					cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
						"多模型共用同一 min_tps=%.0f：各模型 decode 速度差异大时会把更慢的模型误熔断——"+
							"建议按模型标定 model_overrides.<模型>.stall_guard.min_tps（取该模型 probe 实测速度的 10–20%%）；未单独标定: %s",
						sg.MinTPS, strings.Join(missing, "、")))
				}
			}
		}
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

// normalizeLadder 校验并规范化单发 token 档位：非正值报错、排序去重、相邻增量 <10% 报错。
// where 用于错误信息定位（顶层或某个模型覆盖段）。返回规范化后的档位与是否发生过修正。
// 档位必须升序：场景按档位顺序执行，命中上下文上限时靠升序跳过更大档位。
func normalizeLadder(tokens []int, where string) ([]int, bool, error) {
	for _, t := range tokens {
		if t <= 0 {
			return nil, false, fmt.Errorf("%s 含非正值 %d——档位必须是正整数 token 数", where, t)
		}
	}
	orig := append([]int(nil), tokens...)
	sort.Ints(tokens)
	ded := tokens[:0]
	for i, t := range tokens {
		if i == 0 || t != ded[len(ded)-1] {
			ded = append(ded, t)
		}
	}
	tokens = ded
	changed := len(orig) != len(ded)
	for i := 0; !changed && i < len(orig); i++ {
		changed = orig[i] != ded[i]
	}
	for i := 1; i < len(tokens); i++ {
		prev, cur := tokens[i-1], tokens[i]
		if cur-prev < prev/10 {
			return nil, false, fmt.Errorf(
				"%s 档位 %d 与前一档 %d 增量仅 %.1f%%（<10%%）——同量级档位的 TTFT 差异会淹没在请求间抖动里，测了也测不出结论；请拉开差距或删除多余档位（如 40000 之后想探更深，用 60000/80000 而不是 41000）",
				where, cur, prev, float64(cur-prev)/float64(prev)*100)
		}
	}
	return tokens, changed, nil
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

// MultiturnMaxDepth 多轮场景可达的最深上下文（混合档取最深档位；未配置返回 0）。
// 口径与执行一致：档位 turns 覆盖时按该档位（base + turns×turn_tokens）计算。
func (c *Config) MultiturnMaxDepth() int {
	mt := c.Multiturn
	base := mt.SystemTokens + mt.ToolDefsTokens
	if len(mt.Profiles) > 0 {
		mx := 0
		for _, p := range mt.Profiles {
			effTurns := p.Turns
			if effTurns <= 0 {
				effTurns = mt.Turns
			}
			if r := base + effTurns*p.TurnTokens; r > mx {
				mx = r
			}
		}
		return mx
	}
	if mt.TurnTokens > 0 {
		return base + mt.Turns*mt.TurnTokens
	}
	return 0
}

// LargestPromptTokens 返回多轮/并发配置中最大的 prompt 规模，用于超时提示与 probe 对比。
func (c *Config) LargestPromptTokens() int {
	mx := 0
	if est := c.MultiturnMaxDepth(); est > mx {
		mx = est
	}
	if c.Concurrent.PromptTokens > mx {
		mx = c.Concurrent.PromptTokens
	}
	return c.ClampOne(mx)
}
