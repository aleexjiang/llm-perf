// toolprobe.go：tool-call 健康检查（probe 的门禁项，默认开启，--no-toolcall 关闭）。
//
// 定位：只做前置门禁——告诉客户"这个引擎能不能正常调工具、不能的话该改哪里"，
// 不测性能、不进主压测路径（压测核心场景不带 tools）。
//
// 检查项：
//   T1 基线连通 = 复用现有 chat_nonstream（失败即跳过）
//   T2 非流式结构化（tools + tool_choice: auto）
//   T3 tool_choice 探针（required，与 T2 联合判定）
//   T4 流式聚合（按 index 分桶，与非流式对照：函数名丢失 / 调用数变少是流式丢调用的签名）
//   T5 标记泄漏（DSML / tool▁calls / <tool_call> 出现在 content = 内容污染）
//
// 判据分级：硬特征（结构性错误，正常回答不可能出现）→ FAIL；
// 软特征（可能是配置/网关问题而非引擎能力）→ WARN；
// 检查自身失败（超时/网关错/解析异常）→"检查未完成"，不计入 ❌，
// 避免客户拿我们自己的超时当引擎缺陷去提工单。
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// toolCallSelfCheckNotice 自检提示（每次 tool-call 检查后输出，全绿也不省略——
// 全绿恰恰是最需要提醒的场景）。本项的检出能力（sensitivity）尚未在真实故障引擎上
// 验证过；首次在真实坏引擎上跑通后删除本常量即可摘除。
const toolCallSelfCheckNotice = "ℹ️ tool-call 检查自检提示：本项的\"检出能力\"尚未在真实故障引擎上验证（目前只验证过\"正常引擎不误报\"）。" +
	"建议在确定不支持 function calling 的端点（如 base 非 chat 模型）上跑一次 bench probe 确认能报出 ❌，" +
	"并用 --probe-capture <目录> 落盘原始响应回传——这是本项唯一的验证来源。"

// builtinTools 内置探测工具（最小 one-of：单函数单必填参数）。
var builtinTools = []map[string]any{
	{
		"type": "function",
		"function": map[string]any{
			"name":        "get_weather",
			"description": "查询指定城市的当前天气",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string", "description": "城市名，如 北京"}},
				"required":   []string{"city"},
			},
		},
	},
}

const toolCallPrompt = "北京今天的天气怎么样？请调用工具查询，不要自己编造。"

// leakRe 工具标记泄漏特征：U+2581（▁）私有标记 / DSML / XML 标记——正常回答几乎不可能出现。
var leakRe = regexp.MustCompile(`DSML|tool▁calls|▁tool|<tool_call>`)

// toolCallVerdict 单项判据结论。
type toolCallVerdict struct {
	Level  string // "PASS" / "FAIL"（硬特征）/ "WARN"（软特征）/ "INCOMPLETE"（检查自身失败）
	Detail string // 判据依据（给使用者看）
	Fix    string // 可行动结论（可直接贴给客户/厂商）
}

// judgeToolCall T2/T3 判据（纯函数，fixture 单测覆盖六种失败形态）。
//   status=0 表示请求没发出去（网络错）；wantName 为请求里声明的工具名。
func judgeToolCall(status int, apiErr string, pm *TurnMetrics, wantName string) toolCallVerdict {
	if status == 0 {
		return toolCallVerdict{Level: "INCOMPLETE", Detail: "请求未送达: " + apiErr,
			Fix: "与引擎能力无关（网络/网关层），请检查连通性后重试"}
	}
	if status != 200 {
		v := toolCallVerdict{Level: "WARN", Detail: fmt.Sprintf("HTTP %d", status)}
		if apiErr != "" {
			v.Detail += " error=" + truncate(apiErr, 200)
		}
		v.Fix = "带 tools 的请求被拒——常见于网关未透传 tools 字段或路由到不支持工具的模型，查 router 配置与模型能力声明"
		return v
	}
	calls := pm.ToolCalls
	content := pm.ReplyText

	// 硬特征 1：内容标记泄漏（parser 未启用时最典型形态）
	if leakRe.MatchString(content) {
		return toolCallVerdict{Level: "FAIL",
			Detail: "content 含工具调用标记泄漏（DSML/tool▁calls/<tool_call>），模型输出未被序列化",
			Fix:    "未启用或不支持该模型的 tool-call parser——查推理引擎启动参数 --tool-call-parser 与 --enable-auto-tool-choice"}
	}
	// 硬特征 2：content 是裸 JSON 数组（parser 缺失、required 强制路径生效的典型形态）
	if isPureJSONArray(content) {
		return toolCallVerdict{Level: "FAIL",
			Detail: "content 是裸 JSON 数组而非结构化 tool_calls",
			Fix:    "tool-call parser 缺失——查推理引擎启动参数 --tool-call-parser 与 --enable-auto-tool-choice"}
	}
	// 有结构化调用：校验名称、参数、finish_reason
	if len(calls) > 0 {
		for _, c := range calls {
			if wantName != "" && c.Name != wantName {
				return toolCallVerdict{Level: "FAIL",
					Detail: fmt.Sprintf("tool_calls 函数名 %q 不在请求声明内（期望 %q）", c.Name, wantName),
					Fix:    "流式/解析层函数名丢失或错位——升级推理引擎"}
			}
			if !isJSONObjectString(c.Arguments) {
				return toolCallVerdict{Level: "FAIL",
					Detail: fmt.Sprintf("tool_calls[%s].arguments 非法 JSON: %.80q", c.Name, c.Arguments),
					Fix:    "参数被截断或增量拼接有 bug——查 max_tokens 是否吃光与引擎增量拼接实现"}
			}
		}
		if pm.FinishReason != "tool_calls" {
			return toolCallVerdict{Level: "WARN",
				Detail: fmt.Sprintf("有 tool_calls 但 finish_reason=%s（期望 tool_calls）", pm.FinishReason),
				Fix:    "parser 映射不完整（个别引擎确实如此实现）——升级引擎或反馈厂商"}
		}
		return toolCallVerdict{Level: "PASS",
			Detail: fmt.Sprintf("%d 个结构化调用（finish=%s, arguments 合法）", len(calls), pm.FinishReason)}
	}
	// 无 tool_calls：content 是自然语言"我来调用 xx"（软特征——正常闲聊也可能这么写）
	if strings.Contains(content, "调用") || strings.Contains(strings.ToLower(content), "tool") || strings.Contains(strings.ToLower(content), "function") {
		return toolCallVerdict{Level: "WARN",
			Detail: "无 tool_calls，content 疑似自然语言描述工具调用",
			Fix:    "模型未走序列化路径——查 chat template 是否含工具段，或模型不支持 function calling"}
	}
	return toolCallVerdict{Level: "FAIL",
		Detail: fmt.Sprintf("无 tool_calls，content=%.60q（finish=%s）", content, pm.FinishReason),
		Fix:    "引擎未返回结构化工具调用——tool-call parser 缺失或该模型不支持 function calling，升级引擎"}
}

// judgeRequired T3 联合判定（A.2）。
func judgeRequired(t2, t3 toolCallVerdict) toolCallVerdict {
	if t3.Level == "INCOMPLETE" {
		return t3
	}
	switch {
	case t2.Level == "PASS" && t3.Level == "PASS":
		return toolCallVerdict{Level: "PASS", Detail: "required 与 auto 均正常"}
	case t2.Level != "PASS" && t3.Level == "PASS":
		return toolCallVerdict{Level: "FAIL",
			Detail: "auto 失败但 required 成功——parser 在，auto 分支未触发",
			Fix:    "配置问题：检查请求侧 tools/tool_choice 组装，或引擎对 auto 的分支处理"}
	case t3.Detail != "" && strings.HasPrefix(t3.Detail, "HTTP 4"):
		return toolCallVerdict{Level: "WARN",
			Detail: "tool_choice: required 被拒（" + truncate(t3.Detail, 120) + "）",
			Fix:    "引擎未实现 required——可能是网关不透传，未必是引擎缺陷；auto 可用时影响有限"}
	default:
		return toolCallVerdict{Level: "FAIL",
			Detail: "auto 与 required 均未返回结构化调用",
			Fix:    "tool-call parser 缺失 / 不支持该模型——升级引擎或启用 parser"}
	}
}

// judgeStreamAgg T4 与非流式对照：函数名丢失 / 调用数变少是流式丢调用的签名。
func judgeStreamAgg(nonStream, stream []ToolCall) toolCallVerdict {
	if len(nonStream) == 0 {
		// 非流式本身没产出调用，流式无从对照；只要求不比非流式差
		if len(stream) > 0 {
			return toolCallVerdict{Level: "PASS", Detail: fmt.Sprintf("流式 %d 个调用（非流式基线为 0，流式可用）", len(stream))}
		}
		return toolCallVerdict{Level: "PASS", Detail: "流式与非流式一致（均无结构化调用）"}
	}
	if len(stream) == 0 {
		return toolCallVerdict{Level: "FAIL", Detail: "流式聚合后 0 个调用（非流式有）——流式丢调用的典型签名",
			Fix: "查引擎流式 tool_calls 增量实现（delta.tool_calls 分片）"}
	}
	for _, sc := range stream {
		if sc.Name == "" {
			return toolCallVerdict{Level: "FAIL", Detail: "流式聚合后函数名为空——首片 name 丢失",
				Fix: "查引擎流式增量实现：name 应在首片携带"}
		}
		if !isJSONObjectString(sc.Arguments) {
			return toolCallVerdict{Level: "FAIL", Detail: fmt.Sprintf("流式聚合后 arguments 非法 JSON: %.80q", sc.Arguments),
				Fix: "arguments 增量拼接有 bug（未按 index 分桶或漏片）"}
		}
	}
	if len(stream) < len(nonStream) {
		return toolCallVerdict{Level: "WARN",
			Detail: fmt.Sprintf("流式 %d 个调用少于非流式 %d 个", len(stream), len(nonStream)),
			Fix:    "流式聚合不完整——升级引擎"}
	}
	return toolCallVerdict{Level: "PASS", Detail: fmt.Sprintf("流式 %d 个调用，与非流式一致，arguments 合法", len(stream))}
}

// isPureJSONArray content 是可解析的 JSON 数组（硬特征：正常对话不会吐裸 JSON 数组）。
func isPureJSONArray(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") {
		return false
	}
	var arr []any
	return json.Unmarshal([]byte(s), &arr) == nil
}

// isJSONObjectString arguments 字符串是合法 JSON 对象（空串不算——OpenAI 口径 arguments 必有内容）。
func isJSONObjectString(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	var obj map[string]any
	return json.Unmarshal([]byte(s), &obj) == nil
}

// runToolCallCheck 执行 T2-T5 并把结果写进 res（check 与 verdicts）。
// streamAnalyze 复用 probe 主流程的流式解析闭包（同一份解析代码 = 与计时同口径）。
func runToolCallCheck(ctx context.Context, res *ProbeResult, check func(string, bool, string),
	o ProbeOptions, base, chatPath string, timeout time.Duration, model string,
	streamAnalyze func([]byte) *TurnMetrics) {

	wantName := builtinTools[0]["function"].(map[string]any)["name"].(string)
	send := func(payload map[string]any) (int, []byte, error) {
		pj, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+chatPath, bytes.NewReader(pj))
		req.Header.Set("Content-Type", "application/json")
		o.Auth.Apply(req, o.APIKey)
		resp, err := (&http.Client{Timeout: timeout}).Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
		return resp.StatusCode, raw, nil
	}
	capture := func(tag string, status int, raw []byte) {
		if o.CaptureDir == "" {
			return
		}
		if err := os.MkdirAll(o.CaptureDir, 0o755); err != nil {
			return
		}
		name := fmt.Sprintf("toolcall-%s-%d-%s.log", tag, status, time.Now().Format("150405"))
		var b bytes.Buffer
		fmt.Fprintf(&b, "endpoint=%s chat=%s model=%s status=%d captured_at=%s\n--- RAW ---\n",
			o.Endpoint, chatPath, model, status, time.Now().Format(time.RFC3339))
		b.Write(raw)
		_ = os.WriteFile(filepath.Join(o.CaptureDir, name), b.Bytes(), 0o644)
	}
	apply := func(tag string, v toolCallVerdict, checkName string, detail string) {
		switch v.Level {
		case "PASS":
			check(checkName, true, detail)
		case "FAIL":
			check(checkName, false, detail+" → "+v.Fix)
		case "WARN":
			check(checkName, true, "⚠️ "+detail+" → "+v.Fix)
		default: // INCOMPLETE
			// 检查自身失败与"检查未通过"必须区分——不计 ❌，只给提示
			res.Verdicts = append(res.Verdicts, "tool-call 检查未完成（"+checkName+": "+v.Detail+"）——与引擎能力无关，不计入结论")
			check(checkName, true, "⏭️ 检查未完成（"+v.Detail+"）")
			return
		}
		res.Verdicts = append(res.Verdicts, fmt.Sprintf("tool-call %s: %s → %s", tag, v.Detail, v.Fix))
	}

	// T2 非流式结构化（auto）
	base2 := map[string]any{
		"model": model, "messages": []Message{{Role: "user", Content: toolCallPrompt}},
		"max_tokens": 256, "stream": false, "tools": builtinTools, "tool_choice": "auto",
	}
	st2, raw2, err2 := send(base2)
	capture("T2-auto", st2, raw2)
	pm2 := &TurnMetrics{}
	if err2 == nil && st2 == 200 {
		pm2.applyWholeBody(raw2)
	}
	v2 := judgeToolCall(st2, errMsgOr(err2, pm2), pm2, wantName)
	apply("非流式(auto)", v2, "toolcall_nonstream",
		fmt.Sprintf("HTTP %d finish=%s content=%.40q", st2, pm2.FinishReason, pm2.ReplyText))
	leak2 := leakRe.MatchString(pm2.ReplyText)

	// T3 required（与 T2 联合判定；4xx 是软特征——可能网关不透传）
	p3 := map[string]any{
		"model": model, "messages": []Message{{Role: "user", Content: toolCallPrompt}},
		"max_tokens": 256, "stream": false, "tools": builtinTools, "tool_choice": "required",
	}
	st3, raw3, err3 := send(p3)
	capture("T3-required", st3, raw3)
	pm3 := &TurnMetrics{}
	if err3 == nil && st3 == 200 {
		pm3.applyWholeBody(raw3)
	}
	v3raw := judgeToolCall(st3, errMsgOr(err3, pm3), pm3, wantName)
	detail3 := fmt.Sprintf("HTTP %d finish=%s calls=%d", st3, pm3.FinishReason, len(pm3.ToolCalls))
	v3 := judgeRequired(v2, v3raw)
	if st3 == 0 || (err3 != nil && st3 == 0) {
		v3 = toolCallVerdict{Level: "INCOMPLETE", Detail: "请求未送达: " + errMsgOr(err3, nil)}
	}
	apply("required", v3, "toolcall_required", detail3)

	// T4 流式聚合（auto + stream），与非流式对照
	p4 := map[string]any{
		"model": model, "messages": []Message{{Role: "user", Content: toolCallPrompt}},
		"max_tokens": 256, "stream": true, "tools": builtinTools, "tool_choice": "auto",
	}
	st4, raw4, err4 := send(p4)
	capture("T4-stream", st4, raw4)
	leak4 := false
	if err4 != nil || st4 != 200 {
		// 检查自身失败与"检查未通过"必须区分——不计 ❌，只给提示
		res.Verdicts = append(res.Verdicts,
			fmt.Sprintf("tool-call 检查未完成（流式: HTTP %d err=%v）——与引擎能力无关，不计入结论", st4, err4))
		check("toolcall_stream", true, fmt.Sprintf("⏭️ 检查未完成（HTTP %d err=%v）", st4, err4))
	} else {
		pm4 := streamAnalyze(raw4)
		v4 := judgeStreamAgg(pm2.ToolCalls, pm4.ToolCalls)
		apply("流式", v4, "toolcall_stream",
			fmt.Sprintf("HTTP %d 流式聚合 calls=%d finish=%s", st4, len(pm4.ToolCalls), pm4.FinishReason))
		leak4 = leakRe.MatchString(pm4.ReplyText)
	}

	// T5 标记泄漏（对 T2/T4 的 content；硬特征）
	if leak2 || leak4 {
		check("toolcall_leak", false, "content 命中工具标记（DSML/tool▁calls/<tool_call>）→ parser 未启用，查 --tool-call-parser")
	} else {
		check("toolcall_leak", true, "T2/T4 content 无工具标记泄漏")
	}

	// 总结论 + 自检提示 + capture 提示
	failed := v2.Level == "FAIL" || v3.Level == "FAIL" || leak2 || leak4
	switch {
	case failed:
		res.Verdicts = append(res.Verdicts,
			"❌ tool-call 检查未通过（上方 ❌ 项附可行动结论）——不影响本次压测（压测不带 tools），但 agent 场景会受影响，建议先修复再验 agent 功能")
	default:
		res.Verdicts = append(res.Verdicts,
			"✅ tool-call 检查通过：引擎能返回结构化工具调用；本次压测不带 tools，不影响性能结论")
	}
	if o.CaptureDir != "" {
		res.Verdicts = append(res.Verdicts,
			"tool-call 原始响应已落盘 "+o.CaptureDir+"（含业务数据，外发给厂商前请按需脱敏）")
	}
	res.Verdicts = append(res.Verdicts, toolCallSelfCheckNotice)
}

// errMsgOr 返回网络错误或 API 错误体（判据函数的统一入参）。
func errMsgOr(err error, pm *TurnMetrics) string {
	if err != nil {
		return err.Error()
	}
	if pm != nil && pm.Error != "" {
		return pm.Error
	}
	return ""
}
