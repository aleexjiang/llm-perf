// Package engine 实现 OpenAI 兼容 API 的流式客户端与逐 chunk 计时。
//
// 核心设计：对每个流式请求分别记录
//   - 首个任意 chunk          -> first_chunk_at（原始证据，空 chunk 也算）
//   - 首个含 token 的 chunk   -> TTFT（主流口径，GenAI-Perf/LLMPerf：忽略空首响应）
//   - 首个 reasoning chunk    -> prefill 完成时刻（思考模型）
//   - 首个 content chunk      -> 可见输出开始（= prefill + 思考）
//   - 最后一个 chunk          -> 请求结束
//
// 由此拆出：TTFT、思考时长、decode 时长、ITL 分位数，token 数取自响应 usage 字段。
// 非流式请求（stream=false）只能测端到端延迟与 usage，TTFT/思考拆分不可测（N/A）。
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aleexjiang/llm-perf/internal/auth"
	"github.com/aleexjiang/llm-perf/internal/smetrics"
)

// Message 是一条对话消息。ToolCallID 仅 role=tool 时使用（OpenAI 协议必填，
// full 回放时从 trace 透传；缺失会被服务端 400）。
type Message struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// RetryPolicy 可选的连接层重试策略。压测语义下默认关闭（重试会掩盖服务端的不稳定），
// 开启后只对瞬时失败重试：TCP/流被 reset、HTTP 5xx/429。重试本身会记入 warnings
// （retried ×N）与 RetryCount——测量的计时窗口是干净的，但服务端的不稳定不会从数据里消失。
type RetryPolicy struct {
	MaxAttempts int           // 总尝试次数（1 = 不重试）
	Backoff     time.Duration // 退避基数（默认 300ms，指数退避，封顶 5s）
}

// Client 是 OpenAI 兼容客户端（流式/非流式）。
type Client struct {
	BaseURL      string       // 如 http://host:30082/router/v1
	APIKey       string       // 为空则不带 Authorization
	Auth         auth.Auth    // 认证方案（默认 bearer + Authorization；客户网关裸 key / 自定义 header 时配置）
	ChatPath     string       // 接口路径，默认 /chat/completions（客户 router 路径不同时配置）
	IncludeUsage bool         // 请求 stream_options.include_usage
	HTTP         *http.Client //
	DebugDir     string       // 非空时留存每个请求的原始响应到该目录（排查魔改引擎）
	Retry        *RetryPolicy // nil = 不重试（压测默认）

	// RawTimings 原始 chunk 序列落盘：流式请求把每个含 token chunk 的时刻记进
	// content_times_ms（相对 sent_at 的毫秒偏移）。峰值秒桶吞吐、ITL 抖动等外部分析
	// 都依赖这份原始序列；体积随输出 token 数线性增长，超长 soak 可关。
	RawTimings bool

	seq atomic.Int64 // 原始流量转储文件序号
}

// ChatURL 返回完整接口地址。
func (c *Client) ChatURL() string {
	p := c.ChatPath
	if p == "" {
		p = "/chat/completions"
	}
	return strings.TrimRight(c.BaseURL, "/") + p
}

// NewClient 创建客户端。timeout 作用于整个请求（含流式读取）。
func NewClient(baseURL, apiKey string, timeout time.Duration, includeUsage bool) *Client {
	// 自定义连接池：默认 Transport 的 MaxIdleConnsPerHost=2，
	// 并发压测时会反复建连（TIME_WAIT 堆积 + 建连耗时混进 TTFT 污染数据）
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   512, // 显式放宽：避免高并发流式请求因默认上限 2 反复建连
		MaxConnsPerHost:       0,   // 不限：并发度由场景层控制
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true, // 压缩会让 chunk 攒批，破坏 ITL 计时精度
	}
	return &Client{
		BaseURL:      strings.TrimRight(baseURL, "/"),
		APIKey:       apiKey,
		IncludeUsage: includeUsage,
		HTTP:         &http.Client{Timeout: timeout, Transport: transport},
	}
}

// ToolCall 一次结构化工具调用（probe tool-call 检查用）。
// Arguments 是 OpenAI 口径的 JSON 对象字符串（流式时由增量拼接而成）。
type ToolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// TurnMetrics 记录一次请求的完整计时与 token 统计。时间字段为毫秒。
type TurnMetrics struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Thinking bool   `json:"thinking"` // 本请求是否处于思考开启变体
	// Phase 请求用途：benchmark（主压测）、warmup（预热）、correctness（正确性金丝雀）。
	// 空值兼容直接由 engine.Client 调用的 probe/cache 诊断请求；场景层请求恒显式标记。
	Phase string `json:"phase,omitempty"`

	// 原始时间戳（仅流式填充）
	SentAt           time.Time  `json:"sent_at"`
	FirstChunkAt     *time.Time `json:"first_chunk_at,omitempty"`
	FirstReasoningAt *time.Time `json:"first_reasoning_at,omitempty"`
	FirstContentAt   *time.Time `json:"first_content_at,omitempty"`
	EndAt            time.Time  `json:"end_at"`

	// chunk 统计（仅流式）
	Chunks          int    `json:"chunks"`
	ReasoningChunks int    `json:"reasoning_chunks"`
	ContentChunks   int    `json:"content_chunks"`
	ReasoningChars  int    `json:"reasoning_chars"`
	ContentChars    int    `json:"content_chars"`
	ReplyText       string `json:"reply_text,omitempty"`
	// 预览字段（Finalize 填充）：长文本掐头 120 + 掐尾 120，报告与排错用，全量不入 JSON
	ContentPreview   string `json:"content_preview,omitempty"`
	ReasoningPreview string `json:"reasoning_preview,omitempty"`
	Error            string `json:"error,omitempty"`

	// usage（服务端精确值）
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	ReasoningTokens  int    `json:"reasoning_tokens,omitempty"`
	TotalTokens      int    `json:"total_tokens"`
	CachedTokens     int    `json:"cached_tokens,omitempty"`   // usage.prompt_tokens_details.cached_tokens（引擎不回传时缺省）
	FinishReason     string `json:"finish_reason,omitempty"`   // stop / length / ...（思考吃光预算时为 length 且无 content）
	ReasoningField   string `json:"reasoning_field,omitempty"` // 思考增量字段名：reasoning / reasoning_content（引擎口径证据）

	// NewTokens 多轮场景专用：本轮相对上一轮新增的 prompt tokens（scenario 层在响应返回后填）。
	// 与增量 prefill 速率（TTFT/新增 tokens）配合，量化"上下文越滚越贵"。
	NewTokens int `json:"new_tokens,omitempty"`

	// 派生指标（Finalize 后填充），单位 ms；非流式时 TTFT/思考/ITL 为 0（N/A）
	E2EMS         float64 `json:"e2e_ms"`                      // 请求发出 -> 结束（两种模式都有）
	TTFT          float64 `json:"ttft_ms,omitempty"`           // 首个含 token 的 chunk（主流口径，空首 chunk 不算；原始首 chunk 在 first_chunk_at）
	TTFTReasoning float64 `json:"ttft_reasoning_ms,omitempty"` // 首个 reasoning chunk ≈ prefill 完成
	TTFTContent   float64 `json:"ttft_content_ms,omitempty"`   // 首个 content chunk = prefill + 思考
	ThinkMS       float64 `json:"think_ms,omitempty"`          // reasoning 首包 -> content 首包
	DecodeMS      float64 `json:"decode_ms,omitempty"`         // content 首包 -> 结束；无 content（思考吃光预算）时清 0，键消失 = 不可测
	ITLAvg        float64 `json:"itl_avg_ms,omitempty"`
	ITLP50        float64 `json:"itl_p50_ms,omitempty"`
	ITLP90        float64 `json:"itl_p90_ms,omitempty"`
	ITLP95        float64 `json:"itl_p95_ms,omitempty"`
	ITLP99        float64 `json:"itl_p99_ms,omitempty"`
	ITLMax        float64 `json:"itl_max_ms,omitempty"`

	// ContentTimesMS 每个 content chunk 相对 sent_at 的毫秒偏移（原始序列，raw_timings
	// 开启时落盘）：外部分析据此重建 chunk 到达时刻、间隔抖动和 chunk 级峰值。
	// 一个 chunk 可能含多个 token，不能据此无损重建逐 token 时刻；非流式/关闭时缺键。
	ContentTimesMS []float64 `json:"content_times_ms,omitempty"`
	// TPOT 每 output token 时间（GenAI-Perf 口径：(E2E−TTFT)/(completion−1)，含思考 token），
	// 横评常用；与 ITL（仅 content chunk 间隔）互补
	TPOTMS       float64 `json:"tpot_ms,omitempty"`
	TokensPerSec float64 `json:"tokens_per_sec"` // 流式 = completion/(E2E−TTFT)，与 TPOT 同窗（含思考段）；非流式 = completion/E2E

	// SrvDelta 服务端 /metrics counter 增量（前缀缓存命中、preemptions、MTP 接受率）；
	// server_metrics 开启时由 scenario 层在请求前后抓取差值填入
	SrvDelta *smetrics.CounterDelta `json:"server_counter_delta,omitempty"`

	// 思考吃光输出预算标记：Thinking + 流式 + 全程无 content + finish_reason=length。
	// 此时 ThinkMS/DecodeMS/ITL 均不可测，分析时应剔除或调大 max_tokens 重跑。
	ThinkingNoContent bool `json:"thinking_no_content,omitempty"`

	// StreamBroken 流式读取中断（连接 reset/EOF 等）：响应不完整，TTFT/usage 可能部分可用
	// 但整体不可信。重试策略（RetryPolicy）以此判定可重试。
	StreamBroken bool `json:"stream_broken,omitempty"`
	// Cancelled 客户端主动取消（SIGHUP/Ctrl+C/场景控制）；原始请求仍保留，
	// 但不进入已完成请求、SLO 分母或主吞吐统计。
	Cancelled bool `json:"cancelled,omitempty"`

	// RetryCount 经历过几次重试（RetryPolicy 开启时；计时只含最后一次成功尝试）
	RetryCount int `json:"retry_count,omitempty"`

	// 引擎兼容性告警（usage 缺失、未知增量字段、流未正常终止等）——排查魔改引擎的关键线索
	Warnings []string `json:"warnings,omitempty"`

	// ToolCalls 结构化工具调用（probe tool-call 检查读取；流式在 closeOutWarnings 聚合成型）。
	// 不进压测数据契约（json:"-"）——主压测不带 tools，该字段只有 probe 检查项消费
	ToolCalls       []ToolCall        `json:"-"`
	toolCallBuckets map[int]*ToolCall // 流式按 index 分桶的聚合状态

	contentTimes []time.Time
	// rawTimings 原始 chunk 序列开关（Client.RawTimings 透传）：Finalize 据此把
	// contentTimes 换算进 ContentTimesMS
	rawTimings bool
	// 解析状态（sse.go 的 ingest 逻辑使用；probe 也读它们做兼容性判定）
	seenDeltaKeys  map[string]bool // 流中出现过的全部 delta 键名
	unknownKeys    map[string]bool // 非标 delta 键名
	usageSeen      bool
	doneSeen       bool
	reasoningField string
	rawResp        []byte // 原始响应头部片段（用于失败/调试转储）
	discardRaw     bool   // 未显式开启 DebugDir 时不在内存中保留原始请求/响应

	reasoningBuf string // 思考增量累积（日志预览用；json:"-" 不入库，上限 64KB）

}

// ReasoningText 返回思考增量的累积文本（日志预览用；流式与非流式都填）。
func (m *TurnMetrics) ReasoningText() string { return m.reasoningBuf }

// PreviewHeadTail 长文本掐头 120 + 掐尾 120（按 rune 计，中文友好）。
// ≤280 rune 原样返回；否则首 120 + 中略提示 + 尾 120。
func PreviewHeadTail(s string) string {
	r := []rune(s)
	if len(r) <= 280 {
		return s
	}
	return string(r[:120]) + fmt.Sprintf("……[中略 %d 字]……", len(r)-240) + string(r[len(r)-120:])
}

// TruncateRunes 按 rune 截断到 n 个字符（中文安全）。n<=0 返回原文。
// scenario 层的 assistant history 截断用——之前按字节切（reply[:2000]），中文会切出半个 UTF-8 字符
func TruncateRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func (m *TurnMetrics) warn(format string, args ...any) {
	m.Warnings = append(m.Warnings, fmt.Sprintf(format, args...))
}

// Finalize 根据 raw 时间戳计算派生指标。必须在请求结束后调用。
func (m *TurnMetrics) Finalize() {
	m.ContentPreview = PreviewHeadTail(m.ReplyText)
	m.ReasoningPreview = PreviewHeadTail(m.reasoningBuf)
	m.E2EMS = ms(m.SentAt, m.EndAt)
	if !m.Stream {
		// 非流式：只有端到端延迟可测
		if m.CompletionTokens > 0 && m.E2EMS > 0 {
			m.TokensPerSec = float64(m.CompletionTokens) / (m.E2EMS / 1000)
		}
		return
	}
	// TTFT 只锚定含 reasoning/content token 的首包；FirstChunkAt 仅保留协议首帧证据，
	// role-only/usage/空 delta 流没有 token 增量时不得伪造 TTFT。
	var first *time.Time
	if m.FirstReasoningAt != nil {
		first = m.FirstReasoningAt
	}
	if m.FirstContentAt != nil && (first == nil || m.FirstContentAt.Before(*first)) {
		first = m.FirstContentAt
	}
	if first != nil {
		m.TTFT = ms(m.SentAt, *first)
	}
	if m.FirstReasoningAt != nil {
		m.TTFTReasoning = ms(m.SentAt, *m.FirstReasoningAt)
	}
	if m.FirstContentAt != nil {
		m.TTFTContent = ms(m.SentAt, *m.FirstContentAt)
		if m.FirstReasoningAt != nil {
			m.ThinkMS = ms(*m.FirstReasoningAt, *m.FirstContentAt)
			if m.ThinkMS < 0 {
				// reasoning 块出现在 content 之后（魔改引擎时序异常）——负值会污染下游
				// 中位数统计，钳 0 并告警；原始时序证据保留在 first_*_at 时间戳里
				neg := m.ThinkMS
				m.ThinkMS = 0
				m.warn("think_ms_negative: reasoning 首包晚于 content 首包 %.0fms，已钳 0", -neg)
			}
		}
		m.DecodeMS = ms(*m.FirstContentAt, m.EndAt)
	}
	if m.Stream && m.Thinking && m.FirstContentAt == nil && m.FinishReason == "length" {
		m.ThinkingNoContent = true
		// 与 ThinkMS 同处理：全程无 content 时 DecodeMS 会被填成 first_chunk→end，
		// 那是整段 reasoning 的生成时长，不是 content 解码时长。留在 JSON 里报告 decode 列
		// 会照常出数（消费方只会看见一个像样的数字），而日志侧已按「不可测」打「—」——
		// 同一份数据两处口径相反。清 0 让键消失，「测不出」与「真的是这个数」才可区分。
		m.DecodeMS = 0
	}

	// ITL：相邻 content chunk 间隔（GenAI-Perf 口径，不含 TTFT）
	var itl []float64
	for i := 1; i < len(m.contentTimes); i++ {
		itl = append(itl, ms(m.contentTimes[i-1], m.contentTimes[i]))
	}
	if len(itl) > 0 {
		sort.Float64s(itl) // 先排序，Max 取末位
		m.ITLAvg = avg(itl)
		m.ITLP50 = percentileSorted(itl, 50)
		m.ITLP90 = percentileSorted(itl, 90)
		m.ITLP95 = percentileSorted(itl, 95)
		m.ITLP99 = percentileSorted(itl, 99)
		m.ITLMax = itl[len(itl)-1]
	}

	// 原始 chunk 序列（raw_timings 开启时落盘）：ITL 分位数之外的抖动/峰值信息
	// 只在这里有。在 ITL 排序之前换算不受影响（用原始 contentTimes）。
	if m.rawTimings && len(m.contentTimes) > 0 {
		m.ContentTimesMS = make([]float64, len(m.contentTimes))
		for i, t := range m.contentTimes {
			m.ContentTimesMS[i] = ms(m.SentAt, t)
		}
	}

	// 极短输出轮（真机发现 2.5）：completion < 8 时只有 1~2 个 decode chunk，
	// ITL 分位/TPOT/tok/s 全部退化为噪声（真机实测近空轮 tok/s=1704、itl_p50=0）。
	// 置空让「测不出」与「真的是这个数」可区分，并留痕供会话聚合侧排除。
	if m.CompletionTokens > 0 && m.CompletionTokens < 8 && m.FinishReason == "stop" && !m.Cancelled {
		m.warn("short_output: completion=%d<8——decode 指标（ITL/TPOT/tok/s）不可测，已置空", m.CompletionTokens)
		m.ITLAvg, m.ITLP50, m.ITLP90, m.ITLP95, m.ITLP99, m.ITLMax = 0, 0, 0, 0, 0, 0
		m.TPOTMS, m.TokensPerSec = 0, 0
		return
	}

	// TPOT（GenAI-Perf 口径）：含思考 token 在内的每个 output token 平均耗时
	if m.CompletionTokens > 1 && m.TTFT > 0 {
		m.TPOTMS = (m.E2EMS - m.TTFT) / float64(m.CompletionTokens-1)
	}

	// 吞吐（per 请求）：分母 = E2E−TTFT（首 token 后的全部生成时段，含思考段），
	// 与 TPOT 同窗互逆（≈ 1000/TPOT）。不能用 DecodeMS：思考模型的 completion_tokens
	// 含 reasoning token，而 DecodeMS 只覆盖 content 时段——思考 token 计入分子、
	// 思考耗时不在分母，tok/s 会被显著虚高。非思考模型 E2E−TTFT == DecodeMS，数值不变。
	if m.CompletionTokens > 0 && m.TTFT > 0 && m.E2EMS > m.TTFT {
		m.TokensPerSec = float64(m.CompletionTokens) / ((m.E2EMS - m.TTFT) / 1000)
	}
}

func ms(from, to time.Time) float64 { return float64(to.Sub(from)) / 1e6 }

func avg(xs []float64) float64 {
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// percentile 线性插值分位（偶数样本的 P50 自动等于两中值平均）。
// 与外部分析的中位数口径一致——
// 之前的 floor 取整口径在偶数样本时系统性偏低半步。
func percentile(xs []float64, p float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return percentileSorted(s, p)
}

func percentileSorted(s []float64, p float64) float64 {
	n := len(s)
	if n == 0 {
		return 0
	}
	idx := p / 100 * float64(n-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return s[lo]
	}
	return s[lo] + (s[hi]-s[lo])*(idx-float64(lo))
}

// ChatOptions 一次请求的全部参数。
type ChatOptions struct {
	Model     string
	Messages  []Message
	MaxTokens int
	Stream    bool
	Thinking  bool           // 仅记录进指标，标记本请求是否思考开启
	ExtraBody map[string]any // 合并进请求体（思考开关等透传；不可覆盖 model/messages）
	// Temperature/TopP 请求级采样参数（nil = 不传，服务端默认）。user 模式重跑对照
	// 需要轨迹对齐时显式传 temperature=0（真机发现：默认采样下 assistant 长度随机，
	// 会话第 3 轮起自然分叉——设计使然，但对照实验需要可控）。
	Temperature *float64
	TopP        *float64
}

// Chat 发起一次 chat completion（流式或非流式），返回计时指标。
// 配置了 RetryPolicy 时对"连接层瞬时失败"重试（见 RetryPolicy），计时只记最终成功的那次尝试。
func (c *Client) Chat(ctx context.Context, o ChatOptions) (*TurnMetrics, error) {
	attempts := 1
	if c.Retry != nil && c.Retry.MaxAttempts > 1 {
		attempts = c.Retry.MaxAttempts
	}
	var last *TurnMetrics
	var lastErr error
	retried := 0
	firstErr := ""
	for i := 0; i < attempts; i++ {
		if i > 0 {
			backoff := c.Retry.Backoff
			if backoff <= 0 {
				backoff = 300 * time.Millisecond
			}
			shift := i - 1
			if shift < 30 && backoff <= 5*time.Second/time.Duration(1<<shift) {
				backoff <<= shift // 指数退避，封顶 5s
			} else {
				backoff = 5 * time.Second
			}
			select {
			case <-ctx.Done():
				return last, lastErr
			case <-time.After(backoff):
			}
		}
		m, err, retryable := c.attempt(ctx, o)
		last, lastErr = m, err
		if !retryable || i == attempts-1 {
			if retried > 0 && m != nil {
				m.RetryCount = retried
				m.warn("retried ×%d: %s", retried, firstErr)
			}
			return m, err
		}
		retried++
		if firstErr == "" {
			if err != nil {
				firstErr = err.Error()
			} else if m != nil {
				firstErr = m.Error
			}
		}
	}
	return last, lastErr // 不可达（循环内已返回）
}

// attempt 执行一次完整请求尝试。retryable 表示该失败属于"连接层瞬时失败"，
// 值得重试：传输错误（非 ctx 取消/超时）、HTTP 5xx/429、流式读取中断。
// HTTP 4xx 是服务端确定性行为（记录进数据），不重试。
func (c *Client) attempt(ctx context.Context, o ChatOptions) (m *TurnMetrics, err error, retryable bool) {
	body := map[string]any{
		"model":      o.Model,
		"messages":   o.Messages,
		"stream":     o.Stream,
		"max_tokens": o.MaxTokens,
	}
	if o.Temperature != nil {
		body["temperature"] = *o.Temperature
	}
	if o.TopP != nil {
		body["top_p"] = *o.TopP
	}
	if o.Stream && c.IncludeUsage {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	for k, v := range o.ExtraBody {
		switch k {
		case "model", "messages", "stream", "max_tokens", "stream_options", "temperature", "top_p":
			continue // 测量核心字段与显式采样参数不允许被供应商扩展静默覆盖
		default:
			body[k] = v
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err, false
	}

	m = &TurnMetrics{Model: o.Model, Stream: o.Stream, Thinking: o.Thinking, rawTimings: c.RawTimings, discardRaw: c.DebugDir == ""}
	m.appendRaw(string(payload) + "\n--- RESPONSE ---\n")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ChatURL(), bytes.NewReader(payload))
	if err != nil {
		return nil, err, false
	}
	req.Header.Set("Content-Type", "application/json")
	c.Auth.Apply(req, c.APIKey)

	m.SentAt = time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		m.Error = err.Error()
		m.EndAt = time.Now()
		m.Finalize()
		c.dumpIfNeeded(m, payload, 0, nil, o.Stream)
		// ctx 取消/超时是调用方或整请求超时的确定性行为，重试只会重复等待
		transient := !(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
		return m, fmt.Errorf("request failed: %w", err), transient
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		// 保留短错误摘要，同时有限 drain 剩余响应，尽可能让 HTTP/1.1 连接回到连接池。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		m.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(buf))
		m.EndAt = time.Now()
		m.Finalize()
		m.appendRaw(fmt.Sprintf("HTTP %d\n", resp.StatusCode) + string(buf))
		c.dumpIfNeeded(m, payload, resp.StatusCode, resp.Header, o.Stream)
		retryable = resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		return m, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(buf), 500)), retryable
	}

	var bodyErr error
	if o.Stream {
		c.readStream(resp, m)
	} else {
		bodyErr = c.readWhole(resp, m)
	}
	m.EndAt = time.Now()
	m.Finalize()
	c.dumpIfNeeded(m, payload, resp.StatusCode, resp.Header, o.Stream)
	if bodyErr != nil {
		retryable = !(errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded))
		return m, fmt.Errorf("response body read failed: %w", bodyErr), retryable
	}
	return m, nil, m.StreamBroken
}

// readStream 读 SSE 流：解析/告警逻辑在 sse.go（与 probe 共用），这里只接网络与真实时钟。
func (c *Client) readStream(resp *http.Response, m *TurnMetrics) {
	if err := ingestSSEBody(m, resp.Body, time.Now, c.IncludeUsage); err != nil {
		m.StreamBroken = true // 读取中断：连接层瞬时失败，可重试
		// 断流必须同时进 Error：scenario 层全链路只认 Error 区分成败——只标 StreamBroken
		// 会让半截响应（部分 TTFT/usage）以 Error=="" 混进成功统计与 goodput
		m.Error = fmt.Sprintf("stream broken: %v", err)
		return
	}
	m.closeOutWarnings(c.IncludeUsage)
	if !m.doneSeen {
		m.StreamBroken = true
		if m.Error == "" {
			m.Error = "stream ended without [DONE]"
		}
	}
}

const maxRawKeep = 256 * 1024

// appendRaw 保留原始流片段（头尾各留一半），用于 debug 转储与失败排查。
func (m *TurnMetrics) appendRaw(line string) {
	if m.discardRaw || len(m.rawResp) >= maxRawKeep {
		return
	}
	remaining := maxRawKeep - len(m.rawResp)
	if len(line) > remaining {
		line = line[:remaining]
	}
	m.rawResp = append(m.rawResp, line...)
}

// dumpIfNeeded 把请求上下文 + 原始响应写进显式配置的 DebugDir。
func (c *Client) dumpIfNeeded(m *TurnMetrics, reqBody []byte, status int, respHeader http.Header, stream bool) {
	if c.DebugDir == "" {
		return
	}
	dir := c.DebugDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	name := fmt.Sprintf("%s-%s-%d-%s.log", time.Now().Format("150405"), sanitize(m.Model), c.seq.Add(1), map[bool]string{true: "stream", false: "whole"}[stream])
	var b bytes.Buffer
	fmt.Fprintf(&b, "time=%s endpoint=%s model=%s stream=%v thinking=%v status=%d\n", time.Now().Format(time.RFC3339), c.BaseURL, m.Model, stream, m.Thinking, status)
	fmt.Fprintf(&b, "request: %s\n", truncate(string(maskJSON(reqBody)), 4096))
	if respHeader != nil {
		fmt.Fprintf(&b, "response_headers: %s\n", truncate(respHeader.Get("Server")+" | "+respHeader.Get("Content-Type")+" | fingerprint header? "+respHeader.Get("X-Request-Id"), 300))
	}
	b.Write(m.rawResp)
	path := filepath.Join(dir, name)
	_ = os.WriteFile(path, b.Bytes(), 0o600)
	_ = os.Chmod(path, 0o600)
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_' {
			return r
		}
		return '-'
	}, s)
}

// maskJSON 把请求体里的长消息内容截断（保留结构，方便人工查看）。
func maskJSON(raw []byte) []byte {
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return raw
	}
	out := map[string]any{"model": body.Model, "message_count": len(body.Messages), "messages_preview": []map[string]string{}}
	prev := []map[string]string{}
	for i, msg := range body.Messages {
		if i >= 4 {
			prev = append(prev, map[string]string{"role": "...", "content": fmt.Sprintf("（共 %d 条消息省略）", len(body.Messages)-4)})
			break
		}
		prev = append(prev, map[string]string{"role": msg.Role, "content": truncate(msg.Content, 160)})
	}
	out["messages_preview"] = prev
	rj, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return rj
}

// readWhole 读非流式响应体：解析逻辑在 sse.go（applyWholeBody）。
func (c *Client) readWhole(resp *http.Response, m *TurnMetrics) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		m.Error = err.Error()
		return err
	}
	m.appendRaw(string(data))
	m.applyWholeBody(data)
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return TruncateRunes(s, n) + "..."
}
