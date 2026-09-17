// toolprobe_test.go：tool-call 判据的 fixture 回放单测。
// 覆盖 A.3 六种失败形态 + 正常形态 + 流式聚合对照。
// fixture 是手写的 SSE / JSON 响应——判据函数是纯确定性逻辑，fixture 验证比真机更严格。
package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func metricsWith(status int, finish, content string, calls []ToolCall) *TurnMetrics {
	return &TurnMetrics{
		FinishReason: finish,
		ReplyText:    content,
		ToolCalls:    calls,
	}
}

// ── 正常形态 ──

func TestJudgeToolCallPass(t *testing.T) {
	pm := metricsWith(200, "tool_calls", "", []ToolCall{{ID: "c1", Name: "get_weather", Arguments: `{"city":"北京"}`}})
	v := judgeToolCall(200, "", pm, "get_weather")
	if v.Level != "PASS" {
		t.Fatalf("结构化调用应 PASS: %+v", v)
	}
}

// ── A.3 六种失败形态 ──

// 1. content 含 DSML / tool▁calls 标记 → FAIL（parser 未启用）
func TestJudgeLeak(t *testing.T) {
	for _, content := range []string{
		`好的<tool_call>{"name":"get_weather"}</tool_call>`,
		"DSML片段 tool▁calls 残留",
	} {
		v := judgeToolCall(200, "", metricsWith(200, "stop", content, nil), "get_weather")
		if v.Level != "FAIL" {
			t.Errorf("标记泄漏 %q 应 FAIL: %+v", content, v)
		}
		if !strings.Contains(v.Fix, "tool-call-parser") {
			t.Errorf("泄漏结论应指向 parser: %s", v.Fix)
		}
	}
}

// 2. content 是纯 JSON 数组 → FAIL（parser 缺失，required 强制路径生效）
func TestJudgePureJSONArray(t *testing.T) {
	v := judgeToolCall(200, "", metricsWith(200, "stop", `[{"name":"get_weather","arguments":{"city":"北京"}}]`, nil), "get_weather")
	if v.Level != "FAIL" {
		t.Fatalf("裸 JSON 数组应 FAIL: %+v", v)
	}
}

// 3. content 是自然语言"我来调用 xx" → WARN（软特征：正常闲聊也可能这么写）
func TestJudgeNaturalLanguage(t *testing.T) {
	v := judgeToolCall(200, "", metricsWith(200, "stop", "好的，我来调用 get_weather 工具帮您查询。", nil), "get_weather")
	if v.Level != "WARN" {
		t.Fatalf("自然语言应 WARN: %+v", v)
	}
}

// 4. 有 tool_calls 但 finish_reason=stop → WARN（parser 映射不完整）
func TestJudgeFinishReasonStop(t *testing.T) {
	pm := metricsWith(200, "stop", "", []ToolCall{{Name: "get_weather", Arguments: `{"city":"北京"}`}})
	v := judgeToolCall(200, "", pm, "get_weather")
	if v.Level != "WARN" {
		t.Fatalf("finish_reason=stop 应 WARN: %+v", v)
	}
}

// 5. arguments 非法 JSON → FAIL（截断或增量拼接 bug）
func TestJudgeInvalidArguments(t *testing.T) {
	pm := metricsWith(200, "tool_calls", "", []ToolCall{{Name: "get_weather", Arguments: `{"city":北`}})
	v := judgeToolCall(200, "", pm, "get_weather")
	if v.Level != "FAIL" {
		t.Fatalf("非法 arguments 应 FAIL: %+v", v)
	}
}

// 6. 请求 4xx → WARN（网关未透传 tools，未必是引擎问题）
func TestJudge4xx(t *testing.T) {
	v := judgeToolCall(400, "invalid tools", metricsWith(0, "", "", nil), "get_weather")
	if v.Level != "WARN" {
		t.Fatalf("4xx 应 WARN: %+v", v)
	}
	if !strings.Contains(v.Fix, "router") {
		t.Errorf("4xx 结论应指向网关/router: %s", v.Fix)
	}
}

// 检查自身失败 → INCOMPLETE（不计 ❌）
func TestJudgeIncomplete(t *testing.T) {
	v := judgeToolCall(0, "context deadline exceeded", metricsWith(0, "", "", nil), "get_weather")
	if v.Level != "INCOMPLETE" {
		t.Fatalf("网络超时应 INCOMPLETE: %+v", v)
	}
}

// 函数名不在请求声明内 → FAIL（流式/解析层丢失或错位）
func TestJudgeNameMismatch(t *testing.T) {
	pm := metricsWith(200, "tool_calls", "", []ToolCall{{Name: "hallucinated_fn", Arguments: `{}`}})
	v := judgeToolCall(200, "", pm, "get_weather")
	if v.Level != "FAIL" {
		t.Fatalf("函数名错位应 FAIL: %+v", v)
	}
}

// ── T2 × T3 联合判定（A.2）──

func TestJudgeRequiredJoint(t *testing.T) {
	fail := toolCallVerdict{Level: "FAIL", Detail: "x"}
	pass := toolCallVerdict{Level: "PASS", Detail: "ok"}
	warn4xx := toolCallVerdict{Level: "WARN", Detail: "HTTP 400: required not supported"}

	if v := judgeRequired(pass, pass); v.Level != "PASS" {
		t.Errorf("双双通过应 PASS: %+v", v)
	}
	if v := judgeRequired(fail, pass); v.Level != "FAIL" || !strings.Contains(v.Detail, "auto 分支未触发") {
		t.Errorf("auto 败 required 成 = 配置问题: %+v", v)
	}
	if v := judgeRequired(fail, fail); v.Level != "FAIL" || !strings.Contains(v.Fix, "升级引擎") {
		t.Errorf("双双失败 = parser 缺失: %+v", v)
	}
	if v := judgeRequired(pass, warn4xx); v.Level != "WARN" {
		t.Errorf("required 4xx 应 WARN（软特征）: %+v", v)
	}
}

// ── T4 流式聚合对照 ──

func TestJudgeStreamAgg(t *testing.T) {
	ns := []ToolCall{{Name: "get_weather", Arguments: `{"city":"北京"}`}}

	// 一致 → PASS
	if v := judgeStreamAgg(ns, ns); v.Level != "PASS" {
		t.Errorf("流式与非流式一致应 PASS: %+v", v)
	}
	// 流式 0 调用 → FAIL（丢调用签名）
	if v := judgeStreamAgg(ns, nil); v.Level != "FAIL" {
		t.Errorf("流式丢调用应 FAIL: %+v", v)
	}
	// name 首片丢失 → FAIL
	lost := []ToolCall{{Name: "", Arguments: `{"city":"北京"}`}}
	if v := judgeStreamAgg(ns, lost); v.Level != "FAIL" {
		t.Errorf("函数名丢失应 FAIL: %+v", v)
	}
	// arguments 拼接错 → FAIL
	broken := []ToolCall{{Name: "get_weather", Arguments: `{"city":`}}
	if v := judgeStreamAgg(ns, broken); v.Level != "FAIL" {
		t.Errorf("arguments 拼接错应 FAIL: %+v", v)
	}
	// 调用数变少 → WARN
	fewer := ns[:0]
	fewer = append(fewer, ns[0], ToolCall{})
	_ = fewer
	if v := judgeStreamAgg([]ToolCall{ns[0], ns[0]}, ns); v.Level != "WARN" {
		t.Errorf("调用数变少应 WARN: %+v", v)
	}
}

// ── 流式 tool_calls 增量分桶聚合（sse.go ingestEvent）──

func TestStreamToolCallAggregation(t *testing.T) {
	// 手写 SSE：首片带 name，后续片增量拼 arguments（OpenAI 流式协议形态）
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ci"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"上海\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n")
	m := &TurnMetrics{Stream: true}
	if err := ingestSSEBody(m, strings.NewReader(sse), syntheticClock(time.Now(), time.Millisecond), true); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	m.closeOutWarnings(true)
	if len(m.ToolCalls) != 1 {
		t.Fatalf("应聚合出 1 个调用: %+v", m.ToolCalls)
	}
	c := m.ToolCalls[0]
	if c.ID != "c1" || c.Name != "get_weather" {
		t.Fatalf("ID/name 错误: %+v", c)
	}
	if c.Arguments != `{"city":"上海"}` {
		t.Fatalf("arguments 增量拼接错误: %q", c.Arguments)
	}
	if m.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason 错误: %s", m.FinishReason)
	}
}

// 两个并发 index 分桶不串
func TestStreamToolCallMultiIndex(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"name":"fn_b","arguments":"{"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"fn_a","arguments":"{"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"}"}}]}}]}`,
		`data: [DONE]`,
	}, "\n")
	m := &TurnMetrics{Stream: true}
	_ = ingestSSEBody(m, strings.NewReader(sse), syntheticClock(time.Now(), time.Millisecond), true)
	m.closeOutWarnings(true)
	if len(m.ToolCalls) != 2 {
		t.Fatalf("应聚合出 2 个调用: %+v", m.ToolCalls)
	}
	if m.ToolCalls[0].Name != "fn_a" || m.ToolCalls[0].Arguments != "{}" {
		t.Fatalf("index 0 聚合错误: %+v", m.ToolCalls[0])
	}
	if m.ToolCalls[1].Name != "fn_b" || m.ToolCalls[1].Arguments != "{}" {
		t.Fatalf("index 1 聚合错误: %+v", m.ToolCalls[1])
	}
}

// 非流式 message.tool_calls 解析（applyWholeBody）
func TestWholeBodyToolCalls(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	m := &TurnMetrics{}
	m.applyWholeBody([]byte(body))
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].Name != "get_weather" || m.ToolCalls[0].Arguments != `{"city":"北京"}` {
		t.Fatalf("非流式 tool_calls 解析错误: %+v", m.ToolCalls)
	}
	if m.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason 错误: %s", m.FinishReason)
	}
}

// tool_calls 键不再压制 unknown_delta_fields 白名单语义（deltaPayload 解析 ToolCalls 值）
func TestDeltaPayloadToolCallsParsed(t *testing.T) {
	var d deltaPayload
	if err := json.Unmarshal([]byte(`{"tool_calls":[{"index":0,"function":{"name":"x","arguments":"{}"}}]}`), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.ToolCalls) != 1 || d.ToolCalls[0].Function.Name != "x" {
		t.Fatalf("tool_calls 值应被解析（此前被丢弃）: %+v", d.ToolCalls)
	}
}

// ── 判据辅助函数 ──

func TestIsPureJSONArray(t *testing.T) {
	if !isPureJSONArray(` [1,2] `) {
		t.Error("JSON 数组应命中")
	}
	if isPureJSONArray(`{"a":1}`) || isPureJSONArray(`普通文本`) || isPureJSONArray(`[未闭合`) {
		t.Error("非 JSON 数组不应命中")
	}
}

func TestIsJSONObjectString(t *testing.T) {
	if !isJSONObjectString(`{"city":"x"}`) {
		t.Error("合法对象应通过")
	}
	if isJSONObjectString("") || isJSONObjectString(`[1]`) || isJSONObjectString(`{bad`) {
		t.Error("空串/数组/坏 JSON 不应通过")
	}
}
