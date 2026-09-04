// Package engine 实现 OpenAI 兼容 API 的流式客户端与逐 chunk 计时。
//
// 核心设计：对每个请求分别记录
//   - 首个任意 chunk          -> TTFT（含排队 + prefill）
//   - 首个 reasoning chunk    -> prefill 完成时刻（思考模型）
//   - 首个 content chunk      -> 可见输出开始（= prefill + 思考）
//   - 最后一个 chunk          -> 请求结束
//
// 由此拆出：TTFT、思考时长、decode 时长、ITL 分位数，token 数取自响应 usage 字段。
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Message 是一条对话消息。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client 是 OpenAI 兼容流式客户端。
type Client struct {
	BaseURL      string        // 如 http://host:30082/router/v1
	APIKey       string        // 为空则不带 Authorization
	IncludeUsage bool          // 请求 stream_options.include_usage
	HTTP         *http.Client  //
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

type chatRequest struct {
	Model         string          `json:"model"`
	Messages      []Message       `json:"messages"`
	Stream        bool            `json:"stream"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	StreamOptions *streamOptions  `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type deltaPayload struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
}

type chunkChoice struct {
	Delta deltaPayload `json:"delta"`
}

type usageInfo struct {
	PromptTokens            int `json:"prompt_tokens"`
	CompletionTokens        int `json:"completion_tokens"`
	TotalTokens             int `json:"total_tokens"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type streamChunk struct {
	Choices []chunkChoice `json:"choices"`
	Usage   *usageInfo    `json:"usage"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// TurnMetrics 记录一次流式请求的完整计时与 token 统计。时间字段为毫秒。
type TurnMetrics struct {
	Model string `json:"model"`

	// 原始时间戳
	SentAt           time.Time  `json:"sent_at"`
	FirstChunkAt     *time.Time `json:"first_chunk_at,omitempty"`
	FirstReasoningAt *time.Time `json:"first_reasoning_at,omitempty"`
	FirstContentAt   *time.Time `json:"first_content_at,omitempty"`
	EndAt            time.Time  `json:"end_at"`

	// chunk 统计
	Chunks          int     `json:"chunks"`
	ReasoningChunks int     `json:"reasoning_chunks"`
	ContentChunks   int     `json:"content_chunks"`
	ReasoningChars  int     `json:"reasoning_chars"`
	ContentChars    int     `json:"content_chars"`
	ReplyText       string  `json:"reply_text,omitempty"`
	Error           string  `json:"error,omitempty"`

	// usage（服务端精确值）
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens"`

	// 派生指标（Finalize 后填充），单位 ms
	TTFT     float64   `json:"ttft_ms"`            // 首个任意 chunk（含思考模型则≈TTFT_reasoning 更贴近 prefill）
	TTFTReasoning float64 `json:"ttft_reasoning_ms,omitempty"` // 首个 reasoning chunk
	TTFTContent   float64 `json:"ttft_content_ms,omitempty"`   // 首个 content chunk
	ThinkMS  float64   `json:"think_ms,omitempty"`  // reasoning 首包 -> content 首包
	DecodeMS float64   `json:"decode_ms"`           // content 首包 -> 结束
	ITLAvg   float64   `json:"itl_avg_ms"`
	ITLP50   float64   `json:"itl_p50_ms"`
	ITLP95   float64   `json:"itl_p95_ms"`
	TokensPerSec float64 `json:"tokens_per_sec"`

	contentTimes []time.Time
}

// Finalize 根据 raw 时间戳计算派生指标。必须在流结束后调用。
func (m *TurnMetrics) Finalize() {
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
		start := *m.FirstContentAt
		if m.FirstContentAt == nil && m.FirstChunkAt != nil {
			start = *m.FirstChunkAt
		}
		m.DecodeMS = ms(start, m.EndAt)
	} else if m.FirstChunkAt != nil {
		m.DecodeMS = ms(*m.FirstChunkAt, m.EndAt)
	}

	// ITL：相邻 content chunk 间隔
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

// Stream 发起一次流式 chat completion，逐 chunk 记录时间戳。
func (c *Client) Stream(ctx context.Context, model string, msgs []Message, maxTokens int) (*TurnMetrics, error) {
	reqBody := chatRequest{
		Model:     model,
		Messages:  msgs,
		Stream:    true,
		MaxTokens: maxTokens,
	}
	if c.IncludeUsage {
		reqBody.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	m := &TurnMetrics{Model: model}
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
		return m, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		buf := make([]byte, 2048)
		n, _ := resp.Body.Read(buf)
		m.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(buf[:n]))
		m.EndAt = time.Now()
		m.Finalize()
		return m, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(buf[:n]), 500))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ch streamChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
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
			m.PromptTokens = ch.Usage.PromptTokens
			m.CompletionTokens = ch.Usage.CompletionTokens
			m.TotalTokens = ch.Usage.TotalTokens
			if ch.Usage.CompletionTokensDetails != nil {
				m.ReasoningTokens = ch.Usage.CompletionTokensDetails.ReasoningTokens
			}
		}
		if len(ch.Choices) == 0 {
			continue
		}
		d := ch.Choices[0].Delta
		if d.ReasoningContent != "" {
			m.ReasoningChunks++
			m.ReasoningChars += len(d.ReasoningContent)
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
	m.EndAt = time.Now()
	m.Finalize()
	return m, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
