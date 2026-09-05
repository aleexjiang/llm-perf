// sse.go：SSE / 响应体解析核心，client（计时采集）与 probe（兼容性探测）共用。
//
// 设计要点：
//   - deltaPayload 自定义 UnmarshalJSON：一次解析同时拿到字段值与键名清单，
//     消除"同一行 JSON 解析两遍"的开销，键名清单供未知字段（魔改）探测
//   - ingestSSEBody 把"逐行读流 → 解析 → 喂给 metrics"收敛为唯一入口，
//     时钟由调用方注入（生产传 time.Now，测试传合成时钟），计时逻辑可单测
package engine

import (
	"bufio"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"
)

// knownDeltaKeys 是流式 delta 的已知字段；出现其他字段说明引擎魔改或协议变体，记录进 warnings。
var knownDeltaKeys = map[string]bool{
	"role": true, "content": true, "reasoning": true, "reasoning_content": true,
	"tool_calls": true, "function_call": true,
}

type deltaPayload struct {
	Content string `json:"content"`
	// 思考增量字段两代命名并存：
	//   reasoning_content — 旧版 vLLM / DeepSeek / OpenRouter 等
	//   reasoning         — 新版 vLLM(v0.27+) 官方口径
	Reasoning        string `json:"reasoning"`
	ReasoningContent string `json:"reasoning_content"`

	// Keys 是 delta 里出现过的全部键名（已排序，probe 用）；UnknownKeys 是其中的非标字段（告警用）
	Keys        []string `json:"-"`
	UnknownKeys []string `json:"-"`
}

// reasoningText 返回思考增量内容（兼容两种字段命名）。
func (d deltaPayload) reasoningText() string {
	if d.ReasoningContent != "" {
		return d.ReasoningContent
	}
	return d.Reasoning
}

// UnmarshalJSON 解析字段值的同时收集键名清单。
func (d *deltaPayload) UnmarshalJSON(b []byte) error {
	type plain deltaPayload
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*d = deltaPayload(p)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for k := range raw {
		d.Keys = append(d.Keys, k)
		if !knownDeltaKeys[k] {
			d.UnknownKeys = append(d.UnknownKeys, k)
		}
	}
	sort.Strings(d.Keys)
	sort.Strings(d.UnknownKeys)
	return nil
}

type chunkChoice struct {
	Delta        *deltaPayload `json:"delta,omitempty"`
	Message      *deltaPayload `json:"message,omitempty"`
	FinishReason string        `json:"finish_reason"`
}

type usageInfo struct {
	PromptTokens            int `json:"prompt_tokens"`
	CompletionTokens        int `json:"completion_tokens"`
	TotalTokens             int `json:"total_tokens"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"` // 前缀缓存命中 token 数（OpenAI 口径；部分引擎不填，置信 /metrics 观测层）
	} `json:"prompt_tokens_details"`
}

type chunkChoiceList struct {
	Choices []chunkChoice `json:"choices"`
	Usage   *usageInfo    `json:"usage"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// SSEEvent 是一行 data: JSON（或非流式响应体）解析后的统一视图。
type SSEEvent struct {
	Choices []chunkChoice
	Usage   *usageInfo
	APIErr  string // 服务端返回的 {"error":{"message":...}}
}

// parseSSEData 解析一行 data: 后的 JSON。
func parseSSEData(data []byte) (*SSEEvent, error) {
	var ch chunkChoiceList
	if err := json.Unmarshal(data, &ch); err != nil {
		return nil, err
	}
	ev := &SSEEvent{Choices: ch.Choices, Usage: ch.Usage}
	if ch.Error != nil {
		ev.APIErr = ch.Error.Message
	}
	return ev, nil
}

// splitSSEData 判断一行是否为 SSE data 行；返回去掉前缀后的负载。
func splitSSEData(line string) (data string, ok bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true
}

// applyUsage 把服务端 usage 写进 metrics（后到覆盖先到）。
func (m *TurnMetrics) applyUsage(u *usageInfo) {
	if u == nil {
		return
	}
	m.PromptTokens = u.PromptTokens
	m.CompletionTokens = u.CompletionTokens
	m.TotalTokens = u.TotalTokens
	if u.CompletionTokensDetails != nil {
		m.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	if u.PromptTokensDetails != nil {
		m.CachedTokens = u.PromptTokensDetails.CachedTokens
	}
}

// ingestEvent 把一个解析好的事件喂给 metrics（含计时；时钟由 ingestSSEBody 注入）。
func (m *TurnMetrics) ingestEvent(ev *SSEEvent, now time.Time) {
	if ev.APIErr != "" {
		m.Error = ev.APIErr
		return
	}
	if m.FirstChunkAt == nil {
		t := now
		m.FirstChunkAt = &t
	}
	m.Chunks++
	if ev.Usage != nil {
		m.usageSeen = true
	}
	m.applyUsage(ev.Usage)
	if len(ev.Choices) == 0 {
		return
	}
	if fr := ev.Choices[0].FinishReason; fr != "" {
		m.FinishReason = fr
	}
	d := ev.Choices[0].Delta
	if d == nil {
		return
	}
	if m.seenDeltaKeys == nil {
		m.seenDeltaKeys = map[string]bool{}
	}
	if m.unknownKeys == nil {
		m.unknownKeys = map[string]bool{}
	}
	for _, k := range d.Keys {
		m.seenDeltaKeys[k] = true
	}
	for _, k := range d.UnknownKeys {
		m.unknownKeys[k] = true
	}
	if rt := d.reasoningText(); rt != "" {
		m.ReasoningChunks++
		m.ReasoningChars += len(rt)
		if m.reasoningField == "" {
			m.reasoningField = d.reasoningFieldName()
		}
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

// reasoningFieldName 返回思考增量实际使用的字段名（"reasoning" / "reasoning_content" / ""）。
func (d deltaPayload) reasoningFieldName() string {
	if d.ReasoningContent != "" {
		return "reasoning_content"
	}
	return "reasoning"
}

// ingestSSEBody 从 body 逐行读 SSE 流并喂给 metrics。
// clock 注入便于测试（合成时间）；includeUsage 决定 usage_missing 告警是否适用。
// 返回流读取错误（如有）。
func ingestSSEBody(m *TurnMetrics, body io.Reader, clock func() time.Time, includeUsage bool) error {
	if m.seenDeltaKeys == nil {
		m.seenDeltaKeys = map[string]bool{}
	}
	if m.unknownKeys == nil {
		m.unknownKeys = map[string]bool{}
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	badLines := 0
	var lastBad string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		m.appendRaw(line + "\n")
		data, ok := splitSSEData(line)
		if !ok {
			continue // 非 SSE data 行（注释、BOM 等魔改迹象留存在 raw 转储里）
		}
		if data == "[DONE]" {
			m.doneSeen = true
			break
		}
		ev, err := parseSSEData([]byte(data))
		if err != nil {
			// 聚合告警：魔改流可能几十行都解析失败，逐条记会刷屏
			badLines++
			lastBad = data
			continue
		}
		m.ingestEvent(ev, clock())
	}
	if badLines > 0 {
		m.warn("unparseable_stream_line ×%d, last=%.80s", badLines, lastBad)
	}
	if err := scanner.Err(); err != nil {
		m.warn("stream_read_error: %v", err)
		return err
	}
	return nil
}

// closeOutWarnings 流结束后的兼容性收尾告警。
func (m *TurnMetrics) closeOutWarnings(includeUsage bool) {
	if len(m.unknownKeys) > 0 {
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
	if includeUsage && !m.usageSeen {
		m.warn("usage_missing") // 服务端未回 usage → token 数全 0，性能数据不可信
	}
}

// applyWholeBody 解析非流式 JSON 响应体并喂给 metrics。
func (m *TurnMetrics) applyWholeBody(data []byte) {
	var out chunkChoiceList
	if err := json.Unmarshal(data, &out); err != nil {
		m.Error = "invalid json response: " + truncate(string(data), 200)
		return
	}
	if out.Error != nil {
		m.Error = out.Error.Message
		return
	}
	m.applyUsage(out.Usage)
	if len(out.Choices) == 0 {
		return
	}
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
			m.reasoningField = msg.reasoningFieldName()
		}
	}
}
