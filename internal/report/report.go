// Package report 定义各场景的 JSON 输出结构。
// 本工具只负责产出原始 JSON；HTML/图表等报告呈现由外部工具（如 WorkBuddy）基于 JSON 二次加工。
package report

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aleexjiang/llm-perf/internal/engine"
	"github.com/aleexjiang/llm-perf/internal/smetrics"
)

// MultiturnRun：一个模型的一次多轮会话（每 turn 均含思考时长）。
// user 模式复用本结构：Session = 用户序号；Profile = 会话档位（light/medium/heavy）。
type MultiturnRun struct {
	Model     string `json:"model"`
	Thinking  string `json:"thinking"` // "on" / "off"
	Session   int    `json:"session"`
	MaxTokens int    `json:"max_tokens"` // 输出长度（max_tokens 扫描维度；thinking=on 时已含 floor 抬高）

	// 会话档位标签：user 模式来自 profile 的 light/medium/heavy；空 = 未分档。
	// 报告/分析按此分组切各档位体验（TTFT、单流速度等）。
	Profile string `json:"profile,omitempty"`

	Turns []*engine.TurnMetrics `json:"turns"`

	// soak 时会话启动时刻相对档位开始的偏移（秒），供外部分析首末时段漂移。
	StartOffsetS float64 `json:"start_offset_s,omitempty"`

	// 12.3 multiturn 深度实测校验：
	// LastPromptTokens 末轮（最后一个 usage.prompt_tokens>0 的成功轮）实测 prompt_tokens——
	//   名义外推（turn_tokens×turns）系统性偏乐观 10–15%（filler 语料抽样去重/边界不足额），
	//   落盘实测值供报告侧画像区对照，偏差 >10% 告警。
	// NominalLastPrompt 名义末轮上下文（filler 口径：基座+轮数×每轮增量，×1.07 模板开销）；
	//   0 = trace 模式（轮次来自回放会话，名义值无意义，实测深度即原会话深度）。
	LastPromptTokens  int `json:"last_prompt_tokens,omitempty"`
	NominalLastPrompt int `json:"nominal_last_prompt,omitempty"`

	// TokenBudget user 场景的单请求总上下文预算（prompt + output）：
	// `max_prompt_tokens` 语义修正后即该预算，剩余空间不足以容纳下一轮 prompt + max_tokens
	// 时提前止损，不再发出必然 400 的请求。0 = 未启用预算控制。
	TokenBudget int `json:"token_budget,omitempty"`
}

// FillLastPromptTokens 12.3：从轮次数据回填末轮实测 prompt_tokens（倒序找第一个 >0 的成功轮，
// 中途失败/截断的会话也能拿到最后一个可用深度）。
func (r *MultiturnRun) FillLastPromptTokens() {
	for i := len(r.Turns) - 1; i >= 0; i-- {
		if r.Turns[i].PromptTokens > 0 {
			r.LastPromptTokens = r.Turns[i].PromptTokens
			return
		}
	}
}

// ConcurrentLevel：一个模型在一个并发档位 × 思考模式下的结果。
// Multiturn=false 时 Requests 为各虚拟用户的单轮请求；true 时 Sessions 为各虚拟用户的
// 完整多轮会话（filler=合成模拟对话，trace 数据源为真实会话重放），逐 turn 计量。
// RequestRate>0 为开环到达率模式（Level=0，rate 为实际到达率）。
type ConcurrentLevel struct {
	Model             string                `json:"model"`
	Thinking          string                `json:"thinking"`   // "on" / "off"
	MaxTokens         int                   `json:"max_tokens"` // 输出长度（max_tokens 扫描维度；thinking=on 时已含 floor 抬高）
	Level             int                   `json:"level"`
	RequestRate       float64               `json:"request_rate,omitempty"` // 开环模式的到达率（req/s）
	Requests          []*engine.TurnMetrics `json:"requests,omitempty"`
	Sessions          []MultiturnRun        `json:"sessions,omitempty"`
	WallSeconds       float64               `json:"wall_seconds"`
	ThroughputTPS     float64               `json:"throughput_tps"`              // 完整成功请求的 completion tokens/s
	ActiveDecodeTPS   float64               `json:"active_decode_tps,omitempty"` // Σ completion / decode 区间并集
	CompletedRequests int                   `json:"completed_requests"`
	FailedRequests    int                   `json:"failed_requests"`
	CancelledRequests int                   `json:"cancelled_requests"`

	// goodput（SLO 约束吞吐，配置了 goodput 时填充）：SLOMeet/SLOTotal 为达标/总请求数（多轮按 turn 计）。
	// 刻意不带 omitempty（12.2）：0 是有意义的结果——slo_total>0 时 slo_meet=0 =「整档 0 达标」；
	// slo_total=0 = 未配置 slo.goodput（未统计）。加 omitempty 后两种情况键都消失，
	// 消费方无法区分「0 达标」与「没测」。对齐 SourceCheck.Deviation 的先例。
	SLOMeet    int     `json:"slo_meet"`
	SLOTotal   int     `json:"slo_total"`
	GoodputRPS float64 `json:"goodput_rps"` // 达标请求 / 墙钟
	GoodputTPS float64 `json:"goodput_tps"` // 达标请求的 completion tokens / 墙钟

	// WaitingMax 本档位观测到的 waiting 排队深度峰值（服务端 /metrics gauge；0 = 观测层
	// 不可用或未采样）。与饱和止损是否启用无关，常开记录——它是 saturation_guard.max_waiting
	// 的标定数据源（建议阈值 = 峰值 × 3–5，报告侧会自动给出建议值）。
	WaitingMax float64 `json:"waiting_max,omitempty"`

	// RunningMax 本档位观测到的 running 并发执行数峰值（服务端 /metrics gauge；0 = 观测层
	// 不可用或未采样）。与 WaitingMax 成对的观测对偶（12.5）：服务端参数 API 取不到
	// max_num_seqs，靠人记录不可靠（实测踩坑：120 误沿用实为 8 造成方向性误读）——
	// 「加并发吞吐不涨」时对照本值即可一眼看出服务端槽位天花板。
	RunningMax float64 `json:"running_max,omitempty"`

	// Aborted 非空 = 本档位被提前终止（saturation_guard 的饱和/墙钟 drain；
	// 运行中断（SIGHUP/Ctrl+C）），
	// 值为终止原因；场景层据此停止后续档位。
	// drain 语义：已发出的请求全部保留完整数据，仅样本量少于配置值。
	Aborted string `json:"aborted,omitempty"`

	// 10.5 时长制 soak 标记（闭环）：DurationSeconds>0 = 时长制档位（runs_per_worker 忽略，
	// 各 worker 跑满墙钟）；Renew = 会话滚完换新重开（多轮）。报告侧据此标稳态窗口
	// （首批会话滚完前的暂态剔除）与首末时段漂移分析。
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	Renew           bool    `json:"renew,omitempty"`
}

// SLO 记录本次评测的 goodput 约束（报告侧据此计算达标口径）。
type SLO struct {
	TTFTMS float64 `json:"ttft_ms"`
	TPOTMS float64 `json:"tpot_ms"`
}

// SLOBaseline 记录本次评测的体验基线评估阈值（slo.baseline，5.8）：键名与报告脚本
// 内置默认（SLO_TIERS）一致，报告侧直接合流覆盖。数值字段 0/缺省 = 未配置（用内置默认）。
type SLOBaseline struct {
	Enabled        bool    `json:"enabled"` // false = 报告跳过基线评估节
	ShortMaxTokens int     `json:"short_max_tokens,omitempty"`
	LongMinTokens  int     `json:"long_min_tokens,omitempty"`
	ShortGoodTTFT  float64 `json:"short_good_ttft,omitempty"`
	ShortPassTTFT  float64 `json:"short_pass_ttft,omitempty"`
	LongGoodTTFT   float64 `json:"long_good_ttft,omitempty"`
	LongPassTTFT   float64 `json:"long_pass_ttft,omitempty"`
	GoodTPOT       float64 `json:"good_tpot,omitempty"`
	PassTPOT       float64 `json:"pass_tpot,omitempty"`
	GoodTPS        float64 `json:"good_tps,omitempty"`
	PassTPS        float64 `json:"pass_tps,omitempty"`
}

// Plan 测试画像（5.10）：配置校验通过后、发首个请求前估算的"这次要跑什么形状"总览。
// 估算口径 = 执行口径（复用 ClampLadder / MaxTokensList / Variants / ForModel），
// 开跑前打印 + 随每份场景 JSON 落盘（与 Environment/ConfigRaw 合并消费，回溯"当时的计划"）。
type Plan struct {
	Endpoint      string      `json:"endpoint"`
	Auth          string      `json:"auth,omitempty"` // 认证方案描述（bearer 为默认时不省略，落盘完整）
	TimeoutS      int         `json:"timeout_seconds"`
	NumModels     int         `json:"num_models"`
	Warmup        int         `json:"warmup_requests"`
	Correctness   int         `json:"correctness_samples"`
	Models        []PlanModel `json:"models"`
	TotalRequests int         `json:"total_requests"` // 所有模型所有场景之和；不含预热与金丝雀
}

// PlanModel 单模型的测试计划（model_overrides 覆盖后各模型请求量可能不同）。
type PlanModel struct {
	Model     string         `json:"model"`
	Scenarios []PlanScenario `json:"scenarios"`
	Requests  int            `json:"requests"`
}

// PlanScenario 单场景估算：Detail 为人读明细行，Requests 为该模型该场景的请求估算。
// DurationS > 0 = 10.5 时长制档位（跑满墙钟）：请求数由吞吐决定、无法预先估算——
// Requests 记 0 且渲染层以「—」呈现（不得当「0 个请求」读）。
type PlanScenario struct {
	Name      string `json:"name"` // multiturn | concurrent | concurrent-multi（公共入口）
	Detail    string `json:"detail"`
	Requests  int    `json:"requests"`
	DurationS int    `json:"duration_s,omitempty"`
}

// Render 输出人读总览行（bench 启动时逐行打印；HTML 报告另按结构渲染）。
func (p *Plan) Render() []string {
	lines := []string{fmt.Sprintf(
		"测试画像：端点 %s ｜ 模型 %d 个 ｜ 认证 %s ｜ 超时 %ds ｜ 预热 %d/场景 ｜ 金丝雀 %d/模型",
		p.Endpoint, p.NumModels, p.Auth, p.TimeoutS, p.Warmup, p.Correctness)}
	hasDur := false
	for _, m := range p.Models {
		for _, s := range m.Scenarios {
			if s.DurationS > 0 {
				hasDur = true
				lines = append(lines, fmt.Sprintf("  [%s] %s", s.Name, s.Detail))
				continue
			}
			lines = append(lines, fmt.Sprintf("  [%s] %s ≈ %d 请求", s.Name, s.Detail, s.Requests))
		}
	}
	if hasDur {
		lines = append(lines, fmt.Sprintf(
			"总请求估算（不含预热与金丝雀；不含时长制档位——其请求数取决于吞吐）≈ %d", p.TotalRequests))
	} else {
		lines = append(lines, fmt.Sprintf("总请求估算（不含预热与金丝雀）≈ %d", p.TotalRequests))
	}
	return lines
}

// CorrectnessRow 一条正确性金丝雀请求的判定结果；完整计时在 Report.AuxiliaryRequests 中保留。
type CorrectnessRow struct {
	Model  string  `json:"model"`  // 该金丝雀请求发往的模型（按模型分区落盘时据此归属）
	Number string  `json:"number"` // 要求转写的目标数字
	Reply  string  `json:"reply"`
	Match  bool    `json:"match"`
	E2EMS  float64 `json:"e2e_ms"`
	Error  string  `json:"error,omitempty"`
}

// AuxiliaryRequest 是不进入主 benchmark KPI、但必须完整留存的请求。
// Phase 当前为 warmup/correctness；主压测请求继续保留在 single/multiturn/concurrent 中，
// 避免重复复制大体量 TurnMetrics。
type AuxiliaryRequest struct {
	Phase   string              `json:"phase"`
	Index   int                 `json:"index"`
	Metrics *engine.TurnMetrics `json:"metrics"`
}

// GaugeSummary / HistSummary / ServerMetricsSummary：服务端 /metrics 观测汇总。
// 挂在每个场景 Report 上，覆盖该场景执行窗口。
//
// 【定位】服务端 /metrics 是**可选的第二数据源**：有就多采一份做交叉验证与归因，
// 没有就用客户端实测——后者是本工具唯一的标准口径。任何消费方都不得因为本字段缺失
// 而少给结论、漏掉档位或改变判定。
//
// Available 的语义严格限定为「**窗口差值**（counter/hist）是否取到」：
// 结束快照失败时为 false（Note 说明原因），此时 Gauges 若存在仍为有效数据（轮询独立于快照）。
type ServerMetricsSummary struct {
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`

	// counter 窗口差值（并发窗口内为混合贡献；vLLM 命中率 = hit/query）。
	// SGLang 不暴露 hit/query counter，其 cache_hit_rate 作为 gauge 落在 Gauges。
	CacheHitTokens   float64 `json:"cache_hit_tokens,omitempty"`
	CacheQueryTokens float64 `json:"cache_query_tokens,omitempty"`
	// 刻意不加 omitempty：0 表示「窗口内没有发生抢占」这一**有意义的好结果**。
	// 加上 omitempty 后该键会整个消失，读数据的人会把「实测 0」误读成「没采集到」。
	// 是否可采信看同级的 Available（窗口差值没取到时它才是 false）。
	Preemptions        float64 `json:"preemptions"`
	SpecDrafts         float64 `json:"spec_drafts,omitempty"`
	SpecAcceptedTokens float64 `json:"spec_accepted_tokens,omitempty"`

	// PromptTokens / GenerationTokens：窗口内服务端自报的 prefill / 生成 token 数
	// （counter 差值；0 = 引擎未暴露该指标；SGLang 两者均提供）。
	PromptTokens float64 `json:"prompt_tokens,omitempty"`
	// GenerationTokens 服务端自报的生成 token 数用于两源一致性交叉校验，不参与评测指标。
	// 服务端观测面的原始事实，报告侧据此做两源一致性交叉校验；不参与任何评测指标。
	GenerationTokens float64 `json:"generation_tokens,omitempty"`

	// WindowSeconds 本场景观测窗口时长（开始快照 → 结束快照，秒）。给两源一致性换算 tok/s 用。
	WindowSeconds float64 `json:"window_seconds,omitempty"`

	// gauge 轮询聚合（running/waiting 排队深度、kv_usage KV 池占用率）
	Gauges map[string]GaugeSummary `json:"gauges,omitempty"`

	// histogram 窗口差值分位估计（服务端口径的延迟分解；key 为引擎指标名）
	Hists map[string]smetrics.HistDelta `json:"histograms,omitempty"`

	// 观测健康度：gauge 轮询从未成功或连续失败达到阈值时置位（报告应醒目标注）
	ObservationDegraded bool   `json:"observation_degraded,omitempty"`
	ObservationNote     string `json:"observation_note,omitempty"`
}

// GaugeSummary 复用 smetrics 的轮询聚合类型。
type GaugeSummary = smetrics.GaugeSummary

// SourceCheck 两源一致性（10.1）：客户端实测聚合吞吐 vs 服务端自报生成吞吐的交叉校验。
//
// 【定位】诊断层守卫，不产生新的评测指标（指标层仍是四数）。要防的危险形态是「数百并发流 +
// 高 chunk 率下客户端自身成瓶颈」——此时客户端读数系统性偏低，而服务端产出其实正常，
// 单看客户端数字会得出「服务变慢了」的错误归因。
//
// 【口径】两边用同一分母（本场景各档位墙钟之和），因此比较等价于 token 量比较；
// 服务端分子是 counter 窗口差值，含窗口内其他流量属已知近似。
// 【降级】观测层缺失/不可比时只填 Note 记 NA，绝不因此改变任何结论——与 server_metrics
// 的降级原则一致（客户端实测是唯一基线）。
type SourceCheck struct {
	ClientTPS     float64 `json:"client_tps,omitempty"`     // 客户端实测聚合吞吐（tok/s）
	ServerTPS     float64 `json:"server_tps,omitempty"`     // 服务端生成吞吐（tok/s）
	ClientTokens  float64 `json:"client_tokens,omitempty"`  // 客户端累计 completion tokens
	ServerTokens  float64 `json:"server_tokens,omitempty"`  // 服务端 counter 窗口差值
	WindowSeconds float64 `json:"window_seconds,omitempty"` // 共同分母（各档位墙钟之和）
	// 刻意不加 omitempty：0 = 两源完全一致，是**最有意义的好结果**。加了之后该键会消失，
	// 报告会把它渲染成「NA（缺可比口径）」——把最好的情况报成测不出来。
	Deviation float64 `json:"deviation"`      // (client-server)/server；负 = 客户端偏低
	Note      string  `json:"note,omitempty"` // 不可比原因（NA 口径）
}

// ThroughputSummary 是场景级总吞吐与单流速度的稳定入口。
// 不把 user 的会话展开成并发档位：user 的墙钟由场景层记录，rps/concurrency
// 可由各档位汇总；分层单流速度避免把 stop 正常完成轮与 length 截断轮混成一个中位数。
type ThroughputSummary struct {
	WallSeconds       float64 `json:"wall_seconds"`
	CompletedRequests int     `json:"completed_requests"`
	FailedRequests    int     `json:"failed_requests"`
	CancelledRequests int     `json:"cancelled_requests"`
	CompletionTokens  int     `json:"completion_tokens"`
	ThroughputTPS     float64 `json:"throughput_tps"` // completion_tokens / wall_seconds

	// ActiveDecode 是时间对齐后的“活跃 decode 聚合”：把每条成功流式请求的
	// [TTFT, E2E] 区间投影到同一时间轴，只在至少一个请求处于 decode 时统计。
	// ActiveDecodeTPS = ActiveDecodeTokens / ActiveDecodeSeconds。
	ActiveDecodeTokens  int     `json:"active_decode_tokens,omitempty"`
	ActiveDecodeSeconds float64 `json:"active_decode_seconds,omitempty"`
	ActiveDecodeTPS     float64 `json:"active_decode_tps,omitempty"`

	// 流式单流速度按完成类型分层；加权值 = 分层总 completion / 分层总 decode 时间。
	// P50/P95 只作分布参照，不能替代加权值。
	Streaming struct {
		All    ThroughputClass `json:"all"`
		Stop   ThroughputClass `json:"stop"`
		Length ThroughputClass `json:"length"`
	} `json:"streaming"`
}

// ThroughputClass 一层请求/轮次的单流统计。
type ThroughputClass struct {
	Count               int     `json:"count"`
	CompletionTokens    int     `json:"completion_tokens"`
	DecodeSeconds       float64 `json:"decode_seconds"`
	WeightedTPS         float64 `json:"weighted_tps"` // completion_tokens / decode_seconds
	ActiveDecodeTokens  int     `json:"active_decode_tokens,omitempty"`
	ActiveDecodeSeconds float64 `json:"active_decode_seconds,omitempty"`
	ActiveDecodeTPS     float64 `json:"active_decode_tps,omitempty"`
	P50TPS              float64 `json:"p50_tps,omitempty"`
	P95TPS              float64 `json:"p95_tps,omitempty"`
	P99TPS              float64 `json:"p99_tps,omitempty"`
	P50TTFTMS           float64 `json:"p50_ttft_ms,omitempty"`
	P95TTFTMS           float64 `json:"p95_ttft_ms,omitempty"`
	P50TPOTMS           float64 `json:"p50_tpot_ms,omitempty"`
	P95TPOTMS           float64 `json:"p95_tpot_ms,omitempty"`
}

// BuildThroughputSummary 汇总场景内所有 TurnMetrics。
// wallSeconds 由调用方提供：user 用场景起止时间，rps/concurrency 用各档位墙钟之和。
func BuildThroughputSummary(ms []*engine.TurnMetrics, wallSeconds float64) *ThroughputSummary {
	out := &ThroughputSummary{WallSeconds: wallSeconds}
	type bucket struct {
		count        int
		tokens       int
		decodeS      float64
		activeTokens int
		intervals    [][2]time.Time
		tps          []float64
		ttft         []float64
		tpot         []float64
	}
	var all, stop, length bucket

	add := func(m *engine.TurnMetrics, b *bucket) {
		b.count++
		b.tokens += m.CompletionTokens
		if ttft := m.TTFT; m.Stream && ttft > 0 {
			b.ttft = append(b.ttft, ttft)
		}
		if tpot := m.TPOTMS; m.Stream && tpot > 0 {
			b.tpot = append(b.tpot, tpot)
		}
		if !m.Stream || m.TTFT <= 0 || m.E2EMS <= m.TTFT || m.CompletionTokens <= 0 {
			return
		}
		decode := (m.E2EMS - m.TTFT) / 1000
		b.decodeS += decode
		b.tps = append(b.tps, float64(m.CompletionTokens)/decode)
		if !m.SentAt.IsZero() && !m.EndAt.IsZero() {
			start := m.SentAt.Add(time.Duration(m.TTFT * float64(time.Millisecond)))
			if m.EndAt.After(start) {
				b.intervals = append(b.intervals, [2]time.Time{start, m.EndAt})
				b.activeTokens += m.CompletionTokens
			}
		}
	}

	for _, m := range ms {
		if m == nil {
			continue
		}
		switch {
		case m.Cancelled:
			out.CancelledRequests++
			continue
		case m.Error != "":
			out.FailedRequests++
			continue
		default:
			out.CompletedRequests++
			out.CompletionTokens += m.CompletionTokens
			add(m, &all)
			switch m.FinishReason {
			case "stop":
				add(m, &stop)
			case "length":
				add(m, &length)
			}
		}
	}
	if wallSeconds > 0 {
		out.ThroughputTPS = float64(out.CompletionTokens) / wallSeconds
	}
	finalize := func(b bucket) ThroughputClass {
		c := ThroughputClass{Count: b.count, CompletionTokens: b.tokens, DecodeSeconds: b.decodeS}
		if b.decodeS > 0 {
			c.WeightedTPS = float64(b.tokens) / b.decodeS
		}
		c.ActiveDecodeTokens = b.activeTokens
		c.ActiveDecodeSeconds = UnionSeconds(b.intervals)
		if c.ActiveDecodeSeconds > 0 {
			c.ActiveDecodeTPS = float64(c.ActiveDecodeTokens) / c.ActiveDecodeSeconds
		}
		c.P50TPS = percentileOrZero(b.tps, 0.50)
		c.P95TPS = percentileOrZero(b.tps, 0.95)
		c.P99TPS = percentileOrZero(b.tps, 0.99)
		c.P50TTFTMS = percentileOrZero(b.ttft, 0.50)
		c.P95TTFTMS = percentileOrZero(b.ttft, 0.95)
		c.P50TPOTMS = percentileOrZero(b.tpot, 0.50)
		c.P95TPOTMS = percentileOrZero(b.tpot, 0.95)
		return c
	}
	out.Streaming.All = finalize(all)
	out.Streaming.Stop = finalize(stop)
	out.Streaming.Length = finalize(length)
	out.ActiveDecodeTokens = out.Streaming.All.ActiveDecodeTokens
	out.ActiveDecodeSeconds = out.Streaming.All.ActiveDecodeSeconds
	out.ActiveDecodeTPS = out.Streaming.All.ActiveDecodeTPS
	return out
}

// UnionSeconds 返回时间区间并集总秒数（供场景层与报告层复用）。
func UnionSeconds(intervals [][2]time.Time) float64 {
	if len(intervals) == 0 {
		return 0
	}
	sorted := append([][2]time.Time(nil), intervals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i][0].Before(sorted[j][0]) })
	start, end := sorted[0][0], sorted[0][1]
	total := 0.0
	for _, iv := range sorted[1:] {
		if !iv[0].After(end) {
			if iv[1].After(end) {
				end = iv[1]
			}
			continue
		}
		total += end.Sub(start).Seconds()
		start, end = iv[0], iv[1]
	}
	return total + end.Sub(start).Seconds()
}

func percentileOrZero(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	idx := p * float64(len(s)-1)
	lo, hi := int(math.Floor(idx)), int(math.Ceil(idx))
	if lo == hi {
		return s[lo]
	}
	return s[lo] + (s[hi]-s[lo])*(idx-float64(lo))
}

// Version 是工具版本，随每个 JSON 输出落盘（报告追溯用）。
// 默认 dev；Makefile 构建时用 -ldflags 注入 git describe 版本号。
var Version = "llm-perf/dev"

// SchemaVersionCurrent 数据契约版本：JSON 结构变更时递增，外部消费方据此做兼容判断。
// 契约唯一权威文档 docs/data-contract.md，与本值同步维护（2026-09-17 报告层剥离后，
// 这份 JSON 契约就是工具的对外接口）。
const SchemaVersionCurrent = 7

// Report 是一次场景执行的完整数据，整体落盘为单个 JSON 文件。
type Report struct {
	Tool string `json:"tool"`
	// SchemaVersion 数据契约版本（落盘时由 SaveJSON 兜底填充，见 SchemaVersionCurrent）
	SchemaVersion int       `json:"schema_version"`
	Scenario      string    `json:"scenario"`
	GeneratedAt   time.Time `json:"generated_at"`
	// Test 测试类别（benchmark | performance | soak）：报告据其切换结论区口径。
	// 恒有值且**不带 omitempty**——JSON 自描述，读者不必猜"缺键 = 默认还是旧产物"。
	Test              string                `json:"test"`
	Endpoint          string                `json:"endpoint"`
	Note              string                `json:"note,omitempty"`
	SLO               *SLO                  `json:"slo,omitempty"`
	SLOBaseline       *SLOBaseline          `json:"slo_baseline,omitempty"`
	Plan              *Plan                 `json:"plan,omitempty"`
	Multiturn         []MultiturnRun        `json:"multiturn,omitempty"`
	Concurrent        []ConcurrentLevel     `json:"concurrent,omitempty"`
	Correctness       []CorrectnessRow      `json:"correctness,omitempty"`
	AuxiliaryRequests []AuxiliaryRequest    `json:"auxiliary_requests,omitempty"`
	Server            *ServerMetricsSummary `json:"server_metrics,omitempty"`
	// KVCapacity 服务端 KV 容量画像（vllm:cache_config_info；场景开始时快照一次）。
	// 用途单一：外部分析并列「实测负载行为 vs KV 上界」，先回答「是不是 KV 内存先满」。
	// 同为可选第二数据源——缺失（引擎未暴露/观测层关闭）不影响客户端采集。
	KVCapacity *smetrics.KVCapacity `json:"kv_capacity,omitempty"`
	// SourceCheck 两源一致性（10.1，仅并发场景计算）：客户端 vs 服务端生成吞吐。
	SourceCheck *SourceCheck `json:"source_check,omitempty"`
	// Throughput 场景级总吞吐与分层单流速度；user 也统一从这里看总吞吐。
	Throughput *ThroughputSummary `json:"throughput,omitempty"`

	// 环境存档：几周后回看数据时"当时是什么引擎/什么配置跑的"必须有据可查。
	Environment *engine.ProbeResult `json:"environment,omitempty"`
	ConfigRaw   string              `json:"config_raw,omitempty"`

	// PartitionModel 按模型分区时该分区归属的模型名（不落盘）：main 据此拼 <output_dir>/<模型>/ 子目录
	PartitionModel string `json:"-"`
}

// SaveJSON 将报告写入单个 JSON 文件（自动创建父目录）。
func (r *Report) SaveJSON(path string) error {
	if r.SchemaVersion == 0 {
		r.SchemaVersion = SchemaVersionCurrent // 兜底填充：场景层构造处无需逐点赋值
	}
	return SaveJSONAny(r, path)
}

// SaveJSONAny 任意结构落盘为 JSON（probe 等非 Report 结构用）。
func SaveJSONAny(v any, path string) error {
	rj, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(rj); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// DefaultName 生成默认输出文件名：<scenario>-<timestamp>.json
func DefaultName(scenario string) string {
	return fmt.Sprintf("%s-%s.json", scenario, time.Now().Format("20060102-150405"))
}

// PartitionByModel 按模型把报告拆成每模型一份（数据落盘以模型为单位：output/<model>/）。
// Single/Multiturn/Concurrent/Correctness 按各行 Model 字段分桶；Endpoint 级的
// Note/SLO/Server 原样带入每个分区。分区顺序按模型首次出现的顺序。
func (r *Report) PartitionByModel() []*Report {
	order := []string{}
	buckets := map[string]*Report{}
	get := func(model string) *Report {
		if p, ok := buckets[model]; ok {
			return p
		}
		p := &Report{
			Tool:           r.Tool,
			Scenario:       r.Scenario,
			GeneratedAt:    r.GeneratedAt,
			Test:           r.Test,
			Endpoint:       r.Endpoint,
			Note:           r.Note,
			SLO:            r.SLO,
			SLOBaseline:    r.SLOBaseline,
			Plan:           r.Plan,
			Server:         r.Server,
			KVCapacity:     r.KVCapacity,
			SourceCheck:    r.SourceCheck,
			Throughput:     r.Throughput,
			Environment:    r.Environment,
			ConfigRaw:      r.ConfigRaw,
			PartitionModel: model,
		}
		buckets[model] = p
		order = append(order, model)
		return p
	}
	for i := range r.Multiturn {
		p := get(r.Multiturn[i].Model)
		p.Multiturn = append(p.Multiturn, r.Multiturn[i])
	}
	for i := range r.Concurrent {
		p := get(r.Concurrent[i].Model)
		p.Concurrent = append(p.Concurrent, r.Concurrent[i])
	}
	for _, row := range r.Correctness {
		get(row.Model).Correctness = append(buckets[row.Model].Correctness, row)
	}
	for _, row := range r.AuxiliaryRequests {
		if row.Metrics == nil {
			continue
		}
		model := row.Metrics.Model
		get(model).AuxiliaryRequests = append(buckets[model].AuxiliaryRequests, row)
	}
	parts := make([]*Report, 0, len(order))
	for _, m := range order {
		parts = append(parts, buckets[m])
	}
	return parts
}
