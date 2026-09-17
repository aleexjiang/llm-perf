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

// vLLM v0.27.1 真实流样例（<实例IP>:8849 现场抓取，思考字段为 reasoning）
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
	// 计时：首 chunk（空 role-only）t0+100ms，reasoning 首包 t0+110ms，content 首包 t0+130ms
	// TTFT 主流口径：空首 chunk 不算 token，取首个含 token 的 chunk（此处 reasoning 首包）
	if m.TTFT != 110 {
		t.Errorf("TTFT = %v, want 110（首个含 token chunk 口径）", m.TTFT)
	}
	if m.FirstChunkAt == nil || ms(m.SentAt, *m.FirstChunkAt) != 100 {
		t.Errorf("first_chunk_at 应保留原始首 chunk 时刻（+100ms）: %v", m.FirstChunkAt)
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
	if m.ContentChars != 1 { // "答" 1 字（rune 口径）
		t.Errorf("ContentChars = %d, want 1", m.ContentChars)
	}
	if m.ReasoningChars != 2 { // "思考" 2 字
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
		t.Fatal("ThinkingNoContent should be true")
	}
	// 全程无 content 时 DecodeMS 会被填成 first_chunk→end（那其实是整段 reasoning 的
	// 生成时长）。留在 JSON 里报告 decode 列照常出数，与「思考段不可界定」的结论相反，
	// 也和日志侧打的「—」矛盾——必须与 ThinkMS 一起清 0，让键消失。
	if m.ThinkMS != 0 || m.DecodeMS != 0 || m.TTFTContent != 0 {
		t.Errorf("ThinkMS/DecodeMS/TTFTContent should be 0, got %v/%v/%v",
			m.ThinkMS, m.DecodeMS, m.TTFTContent)
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
		if m.ReasoningChars != 3 || m.reasoningField != "reasoning_content" { // "想了想" 3 字
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

// 原始 chunk 序列（raw_timings）：开启时落盘 content_times_ms（相对 sent_at 的毫秒偏移，
// feed 合成时钟 100ms 起、10ms/chunk）；关闭时缺键。ITL 分位数之外的抖动/峰值分析
// 都依赖这份原始序列。
func TestFinalize_ContentTimesMS(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"c\"},\"finish_reason\":\"stop\"}]}\n" +
		"data: [DONE]\n"
	m := &TurnMetrics{Stream: true, rawTimings: true}
	feed(t, m, raw, true)
	want := []float64{100, 110, 120}
	if len(m.ContentTimesMS) != len(want) {
		t.Fatalf("ContentTimesMS 长度 = %d, want %d", len(m.ContentTimesMS), len(want))
	}
	for i, w := range want {
		if m.ContentTimesMS[i] != w {
			t.Errorf("ContentTimesMS[%d] = %v, want %v", i, m.ContentTimesMS[i], w)
		}
	}
	m2 := &TurnMetrics{Stream: true} // 开关关闭：键不落盘
	feed(t, m2, raw, true)
	if m2.ContentTimesMS != nil {
		t.Errorf("rawTimings 关闭时不应落盘原始序列: %v", m2.ContentTimesMS)
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

// ── 主流口径对齐回归：TTFT 忽略空首 chunk（GenAI-Perf/LLMPerf disregard empty
// initial responses），首个含 token 的 chunk 才是 TTFT ──

func TestTTFTSkipsEmptyFirstChunk(t *testing.T) {
	// 非思考模型：首 chunk 为 role-only 空 content，第二个 chunk 才有正文
	raw := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"你好\"},\"finish_reason\":null}]}\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n" +
		"data: [DONE]\n"
	m := &TurnMetrics{Stream: true}
	feed(t, m, raw, true)

	// 空首 chunk t0+100ms，正文首包 t0+110ms → TTFT=110 而非 100
	if m.TTFT != 110 {
		t.Errorf("TTFT = %v, want 110（空首 chunk 不算 token）", m.TTFT)
	}
	if m.TTFTContent != 110 {
		t.Errorf("TTFTContent = %v, want 110", m.TTFTContent)
	}
	// TPOT 用同一口径：(E2E−TTFT)/(completion−1)
	if m.TPOTMS <= 0 {
		t.Errorf("TPOT 应可测: %v", m.TPOTMS)
	}
}

func TestTTFTContentOnlyFallback(t *testing.T) {
	// 全程无 reasoning：TTFT 取首个 content chunk
	raw := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"答\"},\"finish_reason\":\"stop\"}]}\n" +
		"data: [DONE]\n"
	m := &TurnMetrics{Stream: true}
	feed(t, m, raw, true)
	if m.TTFT != m.TTFTContent {
		t.Errorf("无 reasoning 时 TTFT 应等于 TTFTContent: %v vs %v", m.TTFT, m.TTFTContent)
	}
}

// ── DeepSeek review 回归：思考模型 tok/s 分母必须含思考段（与 TPOT 同窗）──
// 修复前：TokensPerSec = completion_tokens/DecodeMS，而 completion 含 reasoning
// token、DecodeMS 只覆盖 content 时段——思考 token 计入分子、思考耗时不在分母，
// DeepSeek-V4-Flash 这类长思考模型 tok/s 被显著虚高。

func TestTokensPerSecThinkingWindow(t *testing.T) {
	m := &TurnMetrics{Stream: true, Thinking: true}
	feed(t, m, sseVLLM027, true)

	want := float64(m.CompletionTokens) / ((m.E2EMS - m.TTFT) / 1000)
	if m.TokensPerSec != want {
		t.Errorf("TokensPerSec = %v, want %v（completion/(E2E−TTFT) 同窗口径）", m.TokensPerSec, want)
	}
	if old := float64(m.CompletionTokens) / (m.DecodeMS / 1000); m.TokensPerSec >= old {
		t.Errorf("思考流新口径应低于旧口径（新=%v 旧=%v，旧口径虚高）", m.TokensPerSec, old)
	}
	// 与 TPOT 同窗互逆：tps×tpot_ms/1000 == n/(n−1)（同一时间窗，差一个 −1）
	if m.TPOTMS > 0 {
		want := float64(m.CompletionTokens) / float64(m.CompletionTokens-1)
		if r := m.TokensPerSec * m.TPOTMS / 1000; r < want*0.999 || r > want*1.001 {
			t.Errorf("tok/s 与 TPOT 应同窗互逆: r=%v want≈%v", r, want)
		}
	}
}

func TestTokensPerSecNonThinkingUnchanged(t *testing.T) {
	// 非思考流：TTFT=content 首包 → E2E−TTFT == DecodeMS，数值与旧口径完全一致
	raw := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"你好世界\"},\"finish_reason\":null}]}\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":8,\"total_tokens\":18}}\n" +
		"data: [DONE]\n"
	m := &TurnMetrics{Stream: true}
	feed(t, m, raw, true)
	want := float64(m.CompletionTokens) / (m.DecodeMS / 1000)
	if m.TokensPerSec != want {
		t.Errorf("非思考流 tok/s 应与旧口径一致: %v vs %v", m.TokensPerSec, want)
	}
}
