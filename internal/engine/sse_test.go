package engine

import (
	"strings"
	"testing"
	"time"
)

// syntheticClock 生成递增的合成时间：每次调用前进 step。
func syntheticClock(base time.Time, step time.Duration) func() time.Time {
	cur := base
	return func() time.Time {
		t := cur
		cur = cur.Add(step)
		return t
	}
}

// feed 把原始 SSE 文本喂给 metrics（测试用：合成时钟，10ms/chunk）。
func feed(t *testing.T, m *TurnMetrics, raw string, includeUsage bool) {
	t.Helper()
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.Local)
	m.SentAt = base
	clock := syntheticClock(base.Add(100*time.Millisecond), 10*time.Millisecond)
	if err := ingestSSEBody(m, strings.NewReader(raw), clock, includeUsage); err != nil {
		t.Fatalf("ingestSSEBody: %v", err)
	}
	m.closeOutWarnings(includeUsage)
	m.EndAt = clock()
	m.Finalize()
}

func hasWarning(m *TurnMetrics, prefix string) bool {
	for _, w := range m.Warnings {
		if strings.HasPrefix(w, prefix) {
			return true
		}
	}
	return false
}

// vLLM v0.27.1 真实流样例（172.17.0.3:8849 现场抓取，思考字段为 reasoning）
const sseVLLM027 = `data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning":"User"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning":" asks"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"content":"\n\n2"},"finish_reason":"stop"}]}

data: {"id":"c1","choices":[],"usage":{"prompt_tokens":62,"total_tokens":106,"completion_tokens":44}}

data: [DONE]
`

// 旧版 reasoning_content 风格（DeepSeek / 旧 vLLM / OpenRouter）
const sseLegacy = `data: {"id":"c2","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"c2","choices":[{"index":0,"delta":{"reasoning_content":"让我想想"},"finish_reason":null}]}

data: {"id":"c2","choices":[{"index":0,"delta":{"content":"答案是 2"},"finish_reason":"stop"}]}

data: {"id":"c2","choices":[],"usage":{"prompt_tokens":10,"total_tokens":30,"completion_tokens":20,"completion_tokens_details":{"reasoning_tokens":9}}}

data: [DONE]
`

// 魔改引擎样例：非标字段 thinking_state、无 [DONE]、缺 usage、夹带注释行与垃圾行
const sseMutated = `: keep-alive comment

data: {"id":"c3","choices":[{"index":0,"delta":{"role":"assistant","content":"","thinking_state":"start"},"finish_reason":null}]}

data: {"id":"c3","choices":[{"index":0,"delta":{"reasoning_content":"思考"},"finish_reason":null}]}

data: {not-json-at-all}

data: {"id":"c3","choices":[{"index":0,"delta":{"content":"答"},"finish_reason":"stop"}]}
`

func TestIngestSSE_VLLM027_ReasoningField(t *testing.T) {
	m := &TurnMetrics{Stream: true, Thinking: true}
	feed(t, m, sseVLLM027, true)

	if m.reasoningField != "reasoning" {
		t.Errorf("reasoningField = %q, want reasoning", m.reasoningField)
	}
	if m.ReasoningChars != len("User")+len(" asks") {
		t.Errorf("ReasoningChars = %d", m.ReasoningChars)
	}
	if m.ReasoningChunks != 2 {
		t.Errorf("ReasoningChunks = %d, want 2", m.ReasoningChunks)
	}
	if m.ContentChars != len("\n\n2") || m.ContentChunks != 1 {
		t.Errorf("content: chars=%d chunks=%d", m.ContentChars, m.ContentChunks)
	}
	if m.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", m.FinishReason)
	}
	if m.CompletionTokens != 44 || m.PromptTokens != 62 {
		t.Errorf("usage: prompt=%d completion=%d", m.PromptTokens, m.CompletionTokens)
	}
	if m.ThinkingNoContent {
		t.Error("ThinkingNoContent should be false")
	}
	if len(m.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", m.Warnings)
	}
	// 计时：首 chunk 在 t0+100ms，reasoning 首包 t0+110ms，content 首包 t0+130ms
	if m.TTFT != 100 {
		t.Errorf("TTFT = %v, want 100", m.TTFT)
	}
	if m.TTFTReasoning != 110 {
		t.Errorf("TTFTReasoning = %v, want 110", m.TTFTReasoning)
	}
	if m.TTFTContent != 130 {
		t.Errorf("TTFTContent = %v, want 130", m.TTFTContent)
	}
	if m.ThinkMS != 20 {
		t.Errorf("ThinkMS = %v, want 20", m.ThinkMS)
	}
}

func TestIngestSSE_LegacyReasoningContent(t *testing.T) {
	m := &TurnMetrics{Stream: true, Thinking: true}
	feed(t, m, sseLegacy, true)

	if m.reasoningField != "reasoning_content" {
		t.Errorf("reasoningField = %q, want reasoning_content", m.reasoningField)
	}
	if m.ReasoningTokens != 9 {
		t.Errorf("ReasoningTokens = %d, want 9", m.ReasoningTokens)
	}
	if m.TTFTReasoning != 110 || m.TTFTContent != 120 {
		t.Errorf("timing: ttft_reasoning=%v ttft_content=%v", m.TTFTReasoning, m.TTFTContent)
	}
	if len(m.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", m.Warnings)
	}
}

func TestIngestSSE_MutatedEngine_Warnings(t *testing.T) {
	m := &TurnMetrics{Stream: true}
	feed(t, m, sseMutated, true)

	// 非标字段被识别
	if !hasWarning(m, "unknown_delta_fields: thinking_state") {
		t.Errorf("want unknown_delta_fields warning, got %v", m.Warnings)
	}
	// 无 [DONE]
	if !hasWarning(m, "stream_ended_without_done") {
		t.Errorf("want stream_ended_without_done warning, got %v", m.Warnings)
	}
	// 缺 usage
	if !hasWarning(m, "usage_missing") {
		t.Errorf("want usage_missing warning, got %v", m.Warnings)
	}
	// 垃圾行被跳过且不中断流（后续 content 仍被采集）
	if m.ContentChars != len("答") {
		t.Errorf("ContentChars = %d, want 1", m.ContentChars)
	}
	if m.ReasoningChars != len("思考") {
		t.Errorf("ReasoningChars = %d", m.ReasoningChars)
	}
	// 垃圾行留下证据
	if !hasWarning(m, "unparseable_stream_line ×1") {
		t.Errorf("want unparseable_stream_line warning, got %v", m.Warnings)
	}
	// keep-alive 注释行进 raw 转储
	if !strings.Contains(string(m.rawResp), ": keep-alive comment") {
		t.Error("raw dump should keep non-data lines")
	}
}

func TestIngestSSE_ThinkingNoContent(t *testing.T) {
	// 思考吃光 max_tokens：finish=length、全程无 content
	raw := "data: {\"choices\":[{\"delta\":{\"reasoning\":\"想\"},\"finish_reason\":null}]}\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n" +
		"data: [DONE]\n"
	m := &TurnMetrics{Stream: true, Thinking: true}
	feed(t, m, raw, true)

	if !m.ThinkingNoContent {
		t.Error("ThinkingNoContent should be true")
	}
	if m.ThinkMS != 0 || m.TTFTContent != 0 {
		t.Errorf("ThinkMS/TTFTContent should be 0, got %v/%v", m.ThinkMS, m.TTFTContent)
	}
}

func TestIngestSSE_ErrorChunk(t *testing.T) {
	raw := "data: {\"error\":{\"message\":\"model overloaded\"}}\ndata: [DONE]\n"
	m := &TurnMetrics{Stream: true}
	feed(t, m, raw, true)
	if m.Error != "model overloaded" {
		t.Errorf("Error = %q", m.Error)
	}
	if m.Chunks != 0 {
		t.Errorf("error chunk should not count, Chunks = %d", m.Chunks)
	}
}

func TestApplyWholeBody(t *testing.T) {
	t.Run("openai_style", func(t *testing.T) {
		m := &TurnMetrics{}
		m.applyWholeBody([]byte(`{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`))
		if m.ReplyText != "OK" || m.FinishReason != "stop" || m.PromptTokens != 5 {
			t.Errorf("got reply=%q finish=%q prompt=%d", m.ReplyText, m.FinishReason, m.PromptTokens)
		}
	})
	t.Run("legacy_reasoning_content", func(t *testing.T) {
		m := &TurnMetrics{}
		m.applyWholeBody([]byte(`{"choices":[{"message":{"content":"2","reasoning_content":"想了想"},"finish_reason":"stop"}]}`))
		if m.ReasoningChars != len("想了想") || m.reasoningField != "reasoning_content" {
			t.Errorf("reasoning: chars=%d field=%q", m.ReasoningChars, m.reasoningField)
		}
	})
	t.Run("api_error", func(t *testing.T) {
		m := &TurnMetrics{}
		m.applyWholeBody([]byte(`{"error":{"message":"quota exceeded"}}`))
		if m.Error != "quota exceeded" {
			t.Errorf("Error = %q", m.Error)
		}
	})
	t.Run("invalid_json", func(t *testing.T) {
		m := &TurnMetrics{}
		m.applyWholeBody([]byte(`<html>502</html>`))
		if m.Error == "" {
			t.Error("invalid body should set Error")
		}
	})
}

func TestFinalize_ITLPercentiles(t *testing.T) {
	// content chunk 均匀 10ms 间隔：ITL avg/p50/p95 都是 10ms
	m := &TurnMetrics{Stream: true}
	feed(t, m, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\ndata: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\ndata: {\"choices\":[{\"delta\":{\"content\":\"c\"},\"finish_reason\":\"stop\"}]}\ndata: [DONE]\n", true)
	if m.ITLAvg != 10 || m.ITLP50 != 10 || m.ITLP95 != 10 {
		t.Errorf("ITL avg/p50/p95 = %v/%v/%v, want 10/10/10", m.ITLAvg, m.ITLP50, m.ITLP95)
	}
}

func BenchmarkParseSSEData(b *testing.B) {
	line := []byte(`{"id":"chatcmpl-83baf14829319a30","object":"chat.completion.chunk","created":1788541311,"model":"qwen3.8-27b","choices":[{"index":0,"delta":{"reasoning":"User asks something"},"logprobs":null,"finish_reason":null,"token_ids":null}]}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parseSSEData(line); err != nil {
			b.Fatal(err)
		}
	}
}
