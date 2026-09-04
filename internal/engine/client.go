// Package engine 实现 OpenAI 兼容 API 的流式客户端与逐 chunk 计时。
//
// 核心设计：对每个流式请求分别记录
//   - 首个任意 chunk          -> TTFT（含排队 + prefill）
//   - 首个 reasoning chunk    -> prefill 完成时刻（思考模型）
//   - 首个 content chunk      -> 可见输出开始（= prefill + 思考）
//   - 最后一个 chunk          -> 请求结束
//
// 由此拆出：TTFT、思考时长、decode 时长、ITL 分位数，token 数取自响应 usage 字段。
// 非流式请求（stream=false）只能测端到端延迟与 usage，TTFT/思考拆分不可测（N/A）。
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Message 是一条对话消息。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client 是 OpenAI 兼容客户端（流式/非流式）。
type Client struct {
	BaseURL      string       // 如 http://host:30082/router/v1
	APIKey       string       // 为空则不带 Authorization
	IncludeUsage bool         // 请求 stream_options.include_usage
	HTTP         *http.Client //
	DebugDir     string       // 非空时留存每个请求的原始响应到该目录（排查魔改引擎）；请求失败时即使为空也会留存

	seq atomic.Int64 // 原始流量转储文件序号
}

// NewClient 创建客户端。timeout 作用于整个请求（含流式读取）。
func NewClient(baseURL, apiKey string, timeout time.Duration, includeUsage bool) *Client {
	return &Client{
		BaseURL:      strings.TrimRight(baseURL, "/"),
		APIKey:       apiKey,
		IncludeUsage: includeUsage,
		HTTP:         &http.Client{Timeout: timeout},
	}
}

type deltaPayload struct {
	Content string `json:"content"`
	// 思考增量字段两代命名并存：
	//   reasoning_content — 旧版 vLLM / DeepSeek / OpenRouter 等
	//   reasoning         — 新版 vLLM(v0.27+) 官方口径
	Reasoning        string `json:"reasoning"`
	ReasoningContent string `json:"reasoning_content"`
}

// reasoningText 返回思考增量内容（兼容两种字段命名）。
func (d deltaPayload) reasoningText() string {
	if d.ReasoningContent != "" {
		return d.ReasoningContent
	}
	return d.Reasoning
}

type chunkChoice struct {
	Delta        *deltaPayload `json:"delta,omitempty"`
	Message      *deltaPayload `json:"message,omitempty"`
	FinishReason string        `json:"finish_reason"`
}

type usageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type chunkChoiceList struct {
	Choices []chunkChoice `json:"choices"`
	Usage   *usageInfo    `json:"usage"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// TurnMetrics 记录一次请求的完整计时与 token 统计。时间字段为毫秒。
type TurnMetrics struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Thinking bool   `json:"thinking"` // 本请求是否处于思考开启变体

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
	Error           string `json:"error,omitempty"`

	// usage（服务端精确值）
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens"`
	FinishReason     string `json:"finish_reason,omitempty"` // stop / length / ...（思考吃光预算时为 length 且无 content）

	// 派生指标（Finalize 后填充），单位 ms；非流式时 TTFT/思考/ITL 为 0（N/A）
	E2EMS   float64 `json:"e2e_ms"`                // 请求发出 -> 结束（两种模式都有）
	TTFT    float64 `json:"ttft_ms,omitempty"`     // 首个任意 chunk（含排队 + prefill）
	TTFTReasoning float64 `json:"ttft_reasoning_ms,omitempty"` // 首个 reasoning chunk ≈ prefill 完成
	TTFTContent   float64 `json:"ttft_content_ms,omitempty"`   // 首个 content chunk = prefill + 思考
	ThinkMS  float64  `json:"think_ms,omitempty"`  // reasoning 首包 -> content 首包
	DecodeMS float64  `json:"decode_ms,omitempty"` // content 首包 -> 结束
	ITLAvg   float64  `json:"itl_avg_ms,omitempty"`
	ITLP50   float64  `json:"itl_p50_ms,omitempty"`
	ITLP95   float64  `json:"itl_p95_ms,omitempty"`
	TokensPerSec float64 `json:"tokens_per_sec"`

	// 思考吃光输出预算标记：Thinking + 流式 + 全程无 content + finish_reason=length。
	// 此时 ThinkMS/DecodeMS/ITL 均不可测，分析时应剔除或调大 max_tokens 重跑。
	ThinkingNoContent bool `json:"thinking_no_content,omitempty"`

	// 引擎兼容性告警（usage 缺失、未知增量字段、流未正常终止等）——排查魔改引擎的关键线索
	Warnings []string `json:"warnings,omitempty"`

	contentTimes []time.Time
	unknownKeys  map[string]bool // 流式 delta 中出现的非标字段名
	usageSeen    bool
	doneSeen     bool
	rawResp      []byte // 原始响应头部片段（用于失败/调试转储）
}

func (m *TurnMetrics) warn(format string, args ...any) {
	m.Warnings = append(m.Warnings, fmt.Sprintf(format, args...))
}

// Finalize 根据 raw 时间戳计算派生指标。必须在请求结束后调用。
func (m *TurnMetrics) Finalize() {
	m.E2EMS = ms(m.SentAt, m.EndAt)
	if !m.Stream {
		// 非流式：只有端到端延迟可测
		if m.CompletionTokens > 0 && m.E2EMS > 0 {
			m.TokensPerSec = float64(m.CompletionTokens) / (m.E2EMS / 1000)
		}
		return
	}
	if m.FirstChunkAt != nil {
		m.TTFT = ms(m.SentAt, *m.FirstChunkAt)
	}
	if m.FirstReasoningAt != nil {
		m.TTFTReasoning = ms(m.SentAt, *m.FirstReasoningAt)
	}
	if m.FirstContentAt != nil {
		m.TTFTContent = ms(m.SentAt, *m.FirstContentAt)
		if m.FirstReasoningAt != nil {
			m.ThinkMS = ms(*m.FirstReasoningAt, *m.FirstContentAt)
		}
		m.DecodeMS = ms(*m.FirstContentAt, m.EndAt)
	} else if m.FirstChunkAt != nil {
		m.DecodeMS = ms(*m.FirstChunkAt, m.EndAt)
	}
	if m.Stream && m.Thinking && m.FirstContentAt == nil && m.FinishReason == "length" {
		m.ThinkingNoContent = true
	}

	// ITL：相邻 content chunk 间隔（GenAI-Perf 口径，不含 TTFT）
	var itl []float64
	for i := 1; i < len(m.contentTimes); i++ {
		itl = append(itl, ms(m.contentTimes[i-1], m.contentTimes[i]))
	}
	if len(itl) > 0 {
		m.ITLAvg = avg(itl)
		m.ITLP50 = percentile(itl, 50)
		m.ITLP95 = percentile(itl, 95)
	}

	if m.CompletionTokens > 0 && m.DecodeMS > 0 {
		m.TokensPerSec = float64(m.CompletionTokens) / (m.DecodeMS / 1000)
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

func percentile(xs []float64, p float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	idx := int(p / 100 * float64(len(s)-1))
	return s[idx]
}

// ChatOptions 一次请求的全部参数。
type ChatOptions struct {
	Model     string
	Messages  []Message
	MaxTokens int
	Stream    bool
	Thinking  bool            // 仅记录进指标，标记本请求是否思考开启
	ExtraBody map[string]any  // 合并进请求体（思考开关等透传；不可覆盖 model/messages）
}

// Chat 发起一次 chat completion（流式或非流式），返回计时指标。
func (c *Client) Chat(ctx context.Context, o ChatOptions) (*TurnMetrics, error) {
	body := map[string]any{
		"model":      o.Model,
		"messages":   o.Messages,
		"stream":     o.Stream,
		"max_tokens": o.MaxTokens,
	}
	if o.Stream && c.IncludeUsage {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	for k, v := range o.ExtraBody {
		if k == "model" || k == "messages" {
			continue // 核心字段不允许被透传覆盖
		}
		body[k] = v
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	m := &TurnMetrics{Model: o.Model, Stream: o.Stream, Thinking: o.Thinking}
	m.appendRaw(string(payload) + "\n--- RESPONSE ---\n")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	m.SentAt = time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		m.Error = err.Error()
		m.EndAt = time.Now()
		m.Finalize()
		c.dumpIfNeeded(m, payload, 0, nil, o.Stream)
		return m, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		m.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(buf))
		m.EndAt = time.Now()
		m.Finalize()
		m.appendRaw(fmt.Sprintf("HTTP %d\n", resp.StatusCode) + string(buf))
		c.dumpIfNeeded(m, payload, resp.StatusCode, resp.Header, o.Stream)
		return m, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(buf), 500))
	}

	if o.Stream {
		c.readStream(resp, m)
	} else {
		c.readWhole(resp, m)
	}
	m.EndAt = time.Now()
	m.Finalize()
	c.dumpIfNeeded(m, payload, resp.StatusCode, resp.Header, o.Stream)
	return m, nil
}

func (c *Client) applyUsage(m *TurnMetrics, u *usageInfo) {
	if u == nil {
		return
	}
	m.PromptTokens = u.PromptTokens
	m.CompletionTokens = u.CompletionTokens
	m.TotalTokens = u.TotalTokens
	if u.CompletionTokensDetails != nil {
		m.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
}

// knownDeltaKeys 是流式 delta 的已知字段；出现其他字段说明引擎魔改或协议变体，记录进 warnings。
var knownDeltaKeys = map[string]bool{
	"role": true, "content": true, "reasoning": true, "reasoning_content": true,
	"tool_calls": true, "function_call": true,
}

// readStream 逐行解析 SSE，记录逐 chunk 时间戳。
func (c *Client) readStream(resp *http.Response, m *TurnMetrics) {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		m.appendRaw(line + "\n")
		if !strings.HasPrefix(line, "data:") {
			continue // 非 SSE data 行（注释放宽、BOM 等魔改迹象也留存在 raw 转储里）
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			m.doneSeen = true
			break
		}
		var ch chunkChoiceList
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			m.warn("unparseable_stream_line: %.120s", data)
			continue // 跳过无法解析的行
		}
		if ch.Error != nil {
			m.Error = ch.Error.Message
			continue
		}
		now := time.Now()
		if m.FirstChunkAt == nil {
			t := now
			m.FirstChunkAt = &t
		}
		m.Chunks++
		if ch.Usage != nil {
			m.usageSeen = true
		}
		c.applyUsage(m, ch.Usage)
		if len(ch.Choices) == 0 {
			continue
		}
		if fr := ch.Choices[0].FinishReason; fr != "" {
			m.FinishReason = fr
		}
		if d := ch.Choices[0].Delta; d != nil {
			// 未知字段探测：二次解析 delta 取键名（在时间戳捕获之后做，不影响计时）
			var probeCh struct {
				Choices []struct {
					Delta map[string]any `json:"delta"`
				} `json:"choices"`
			}
			_ = json.Unmarshal([]byte(data), &probeCh)
			if len(probeCh.Choices) > 0 {
				for k := range probeCh.Choices[0].Delta {
					if !knownDeltaKeys[k] {
						if m.unknownKeys == nil {
							m.unknownKeys = map[string]bool{}
						}
						m.unknownKeys[k] = true
					}
				}
			}
			if rt := d.reasoningText(); rt != "" {
				m.ReasoningChunks++
				m.ReasoningChars += len(rt)
				if m.FirstReasoningAt == nil {
					t := now
					m.FirstReasoningAt = &t
				}
			}
			if d.Content != "" {
				m.ContentChunks++
				m.ContentChars += len(d.Content)
				if len(m.ReplyText) < 16*1024 {
					m.ReplyText += d.Content
				}
				m.contentTimes = append(m.contentTimes, now)
				if m.FirstContentAt == nil {
					t := now
					m.FirstContentAt = &t
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		m.warn("stream_read_error: %v", err)
	}
	// 收尾告警：兼容性问题在这几条里暴露
	if m.unknownKeys != nil {
		keys := make([]string, 0, len(m.unknownKeys))
		for k := range m.unknownKeys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		m.warn("unknown_delta_fields: %s", strings.Join(keys, ","))
	}
	if m.Chunks > 0 && !m.doneSeen {
		m.warn("stream_ended_without_done")
	}
	if c.IncludeUsage && !m.usageSeen {
		m.warn("usage_missing") // 服务端未回 usage → token 数全 0，性能数据不可信
	}
}

const maxRawKeep = 256 * 1024

// appendRaw 保留原始流片段（头尾各留一半），用于 debug 转储与失败排查。
func (m *TurnMetrics) appendRaw(line string) {
	if len(m.rawResp) >= maxRawKeep {
		return
	}
	m.rawResp = append(m.rawResp, line...)
}

// dumpIfNeeded 把请求上下文 + 原始响应写进 DebugDir；请求失败时无条件留存。
func (c *Client) dumpIfNeeded(m *TurnMetrics, reqBody []byte, status int, respHeader http.Header, stream bool) {
	dumpOnError := m.Error != ""
	if c.DebugDir == "" && !dumpOnError {
		return
	}
	dir := c.DebugDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
	_ = os.WriteFile(filepath.Join(dir, name), b.Bytes(), 0o644)
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

// readWhole 解析非流式 JSON 响应。只有端到端延迟与 usage 可测。
func (c *Client) readWhole(resp *http.Response, m *TurnMetrics) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		m.Error = err.Error()
		return
	}
	m.appendRaw(string(data))
	var out chunkChoiceList
	if err := json.Unmarshal(data, &out); err != nil {
		m.Error = "invalid json response: " + truncate(string(data), 200)
		return
	}
	if out.Error != nil {
		m.Error = out.Error.Message
		return
	}
	c.applyUsage(m, out.Usage)
	if len(out.Choices) > 0 {
		if fr := out.Choices[0].FinishReason; fr != "" {
			m.FinishReason = fr
		}
		if msg := out.Choices[0].Message; msg != nil {
			if msg.Content != "" {
				m.ContentChars = len(msg.Content)
				m.ReplyText = msg.Content
			}
			if rt := msg.reasoningText(); rt != "" {
				m.ReasoningChars = len(rt)
			}
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
