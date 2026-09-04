// probe.go：引擎兼容性探针。对新接的推理服务（尤其是魔改版 vLLM / MindIE / 网关）
// 先跑一遍 bench probe，摸清服务端的协议实现细节，再决定压测配置怎么写。
//
// 探测项：模型列表 / Server 头 / 非流式基础对话 / 流式格式与 usage / [DONE] 终止符 /
// delta 字段名清单（reasoning vs reasoning_content vs 未知魔改字段）/ 思考开关是否真的生效。
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ProbeCheck 一项探测结果。
type ProbeCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// ProbeResult 一次兼容性探测的完整报告（落盘为 probe-<时间戳>.json）。
type ProbeResult struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Endpoint    string       `json:"endpoint"`
	Server      string       `json:"server_header,omitempty"`
	EngineGuess string       `json:"engine_guess,omitempty"`
	Models      []string     `json:"models,omitempty"`
	Checks      []ProbeCheck `json:"checks"`
	Verdicts    []string     `json:"verdicts,omitempty"`
}

// ProbeOptions 探测参数。
type ProbeOptions struct {
	Endpoint     string
	APIKey       string
	Model        string        // 为空则取 /models 列表第一个
	ThinkingOn   map[string]any // 思考开启的 extra_body（可空）
	ThinkingOff  map[string]any // 思考关闭的 extra_body（可空）
	IncludeUsage bool
}

type probeModelsResp struct {
	Data []struct {
		ID          string `json:"id"`
		Root        string `json:"root"`
		MaxModelLen int    `json:"max_model_len"`
	} `json:"data"`
}

// Probe 对目标端点执行全套兼容性探测。
func Probe(ctx context.Context, o ProbeOptions) *ProbeResult {
	base := strings.TrimRight(o.Endpoint, "/")
	res := &ProbeResult{GeneratedAt: time.Now(), Endpoint: o.Endpoint}
	check := func(name string, ok bool, detail string) {
		res.Checks = append(res.Checks, ProbeCheck{Name: name, OK: ok, Detail: detail})
	}

	// ── 1. 模型列表 + Server 头 ──
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		res.Checks = append(res.Checks, ProbeCheck{Name: "models_list", OK: false, Detail: err.Error()})
		res.Verdicts = append(res.Verdicts, "端点不可达，先解决网络/认证再继续")
		return res
	}
	defer resp.Body.Close()
	res.Server = resp.Header.Get("Server")
	if v := resp.Header.Get("X-Request-Id"); v != "" {
		res.Server += " | x-request-id: " + v
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	var ml probeModelsResp
	modelsOK := resp.StatusCode == 200 && json.Unmarshal(body, &ml) == nil && len(ml.Data) > 0
	check("models_list", modelsOK, fmt.Sprintf("HTTP %d, %d 个模型", resp.StatusCode, len(ml.Data)))
	if modelsOK {
		for _, m := range ml.Data {
			res.Models = append(res.Models, m.ID)
		}
	}
	model := o.Model
	if model == "" {
		if len(res.Models) == 0 {
			res.Verdicts = append(res.Verdicts, "拿不到模型列表，请在配置里显式指定 model 后重试")
			return res
		}
		model = res.Models[0]
	}
	res.EngineGuess = guessEngine(res.Server, body)

	doChat := func(extra map[string]any, maxTokens int, stream bool) (status int, raw []byte, hdr http.Header, err error) {
		payload := map[string]any{
			"model":      model,
			"messages":   []Message{{Role: "user", Content: "请原样回复：OK"}},
			"max_tokens": maxTokens,
			"stream":     stream,
		}
		for k, v := range extra {
			payload[k] = v
		}
		if stream && o.IncludeUsage {
			payload["stream_options"] = map[string]any{"include_usage": true}
		}
		pj, _ := json.Marshal(payload)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(pj))
		req.Header.Set("Content-Type", "application/json")
		if o.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+o.APIKey)
		}
		resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer resp.Body.Close()
		raw, _ = io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
		return resp.StatusCode, raw, resp.Header, nil
	}

	// ── 2. 非流式基础对话（带思考关闭参数，防思考吃光 max_tokens 导致误报）──
	baseExtra := map[string]any{}
	for k, v := range o.ThinkingOff {
		baseExtra[k] = v
	}
	st, raw, _, err := doChat(baseExtra, 16, false)
	if err != nil {
		check("chat_nonstream", false, err.Error())
	} else {
		var out chunkChoiceList
		jerr := json.Unmarshal(raw, &out)
		content := ""
		if jerr == nil && len(out.Choices) > 0 && out.Choices[0].Message != nil {
			content = out.Choices[0].Message.Content
		}
		check("chat_nonstream", st == 200 && jerr == nil && content != "",
			fmt.Sprintf("HTTP %d finish=%s content=%.30q", st, out.Choices[0].FinishReason, content))
		if st != 200 {
			res.Verdicts = append(res.Verdicts, "非流式基础对话失败（HTTP "+fmt.Sprint(st)+"）——检查认证/模型名")
		}
	}

	// ── 3. 流式基础对话：字段名清单 + usage + [DONE] ──
	fields := map[string]bool{}
	usageSeen, doneSeen, finish := false, false, ""
	serr := func() error {
		st, raw, _, err := doChat(baseExtra, 16, true)
		if err != nil {
			return err
		}
		if st != 200 {
			return fmt.Errorf("HTTP %d: %.200s", st, raw)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				doneSeen = true
				continue
			}
			var ch chunkChoiceList
			if json.Unmarshal([]byte(data), &ch) != nil {
				continue
			}
			if ch.Usage != nil {
				usageSeen = true
			}
			if len(ch.Choices) == 0 {
				continue
			}
			if ch.Choices[0].FinishReason != "" {
				finish = ch.Choices[0].FinishReason
			}
			if ch.Choices[0].Delta != nil {
				var probeCh struct {
					Choices []struct {
						Delta map[string]any `json:"delta"`
					} `json:"choices"`
				}
				_ = json.Unmarshal([]byte(data), &probeCh)
				if len(probeCh.Choices) > 0 {
					for k := range probeCh.Choices[0].Delta {
						fields[k] = true
					}
				}
			}
		}
		return nil
	}()
	if serr != nil {
		check("chat_stream", false, serr.Error())
	} else {
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		check("chat_stream", true, fmt.Sprintf("delta 字段: [%s] usage=%v done=%v finish=%s", strings.Join(keys, ","), usageSeen, doneSeen, finish))
		if o.IncludeUsage && !usageSeen {
			res.Verdicts = append(res.Verdicts, "⚠️ 流式响应不带 usage → token 数全 0，性能结论不可信；改配置 include_usage: false 并改用本地估算，或修服务端")
		}
		if !doneSeen {
			res.Verdicts = append(res.Verdicts, "⚠️ 流没有 [DONE] 终止符——魔改迹象，记录到 issue")
		}
		unknown := []string{}
		for k := range fields {
			if !knownDeltaKeys[k] {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			res.Verdicts = append(res.Verdicts, "发现非标 delta 字段 "+strings.Join(unknown, ",")+"——可能魔改，把 raw 转储发给工具维护者适配")
		}
	}

	// ── 4/5. 思考开关有效性 ──
	testThinking := func(extra map[string]any, label string) (reasoningField string, reasoningLen int) {
		st, raw, _, err := doChat(extra, 512, true)
		if err != nil || st != 200 {
			check("thinking_"+label, false, fmt.Sprintf("HTTP %d err=%v", st, err))
			return "none", 0
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") || strings.TrimSpace(strings.TrimPrefix(line, "data:")) == "[DONE]" {
				continue
			}
			var ch chunkChoiceList
			if json.Unmarshal([]byte(line[5:]), &ch) != nil || len(ch.Choices) == 0 || ch.Choices[0].Delta == nil {
				continue
			}
			d := ch.Choices[0].Delta
			if d.ReasoningContent != "" {
				reasoningField = "reasoning_content"
				reasoningLen += len(d.ReasoningContent)
			} else if d.Reasoning != "" {
				reasoningField = "reasoning"
				reasoningLen += len(d.Reasoning)
			}
		}
		return reasoningField, reasoningLen
	}

	if len(o.ThinkingOn) > 0 {
		fOn, nOn := testThinking(o.ThinkingOn, "on")
		if nOn > 0 {
			check("thinking_on", true, fmt.Sprintf("思考增量字段=%s，%d 字符", fOn, nOn))
			if fOn == "reasoning_content" {
				res.Verdicts = append(res.Verdicts, "思考字段为旧版 reasoning_content（引擎已兼容）")
			} else {
				res.Verdicts = append(res.Verdicts, "思考字段为新版 reasoning（vLLM v0.27+ 口径，引擎已兼容）")
			}
		} else {
			check("thinking_on", false, "extra_body 透传后没有产生任何思考增量——参数名可能不对，或该模型/模板不支持")
			res.Verdicts = append(res.Verdicts, "⚠️ 思考开启参数未生效：确认参数名（chat_template_kwargs/enable_thinking/thinking...）与模型模板")
		}
	}
	if len(o.ThinkingOff) > 0 {
		fOff, nOff := testThinking(o.ThinkingOff, "off")
		check("thinking_off", nOff == 0, fmt.Sprintf("关闭后思考增量=%d 字符（字段=%s）", nOff, fOff))
		if nOff > 0 {
			res.Verdicts = append(res.Verdicts, "⚠️ 思考关闭参数未生效：off 变体仍在思考，A/B 对照会失真")
		}
	}

	return res
}

// guessEngine 从 Server 头与响应体特征猜引擎类型。
func guessEngine(server string, modelsBody []byte) string {
	s := strings.ToLower(server)
	switch {
	case strings.Contains(s, "vllm") || bytes.Contains(bytes.ToLower(modelsBody), []byte("vllm")):
		return "vllm"
	case strings.Contains(s, "sglang") || strings.Contains(s, "sgl"):
		return "sglang"
	case strings.Contains(s, "mindie") || strings.Contains(s, "ascend"):
		return "mindie(华为)"
	case strings.Contains(s, "tensorrt") || strings.Contains(s, "triton"):
		return "tensorrt-llm/triton"
	case strings.Contains(s, "llama.cpp") || strings.Contains(s, "llamacpp"):
		return "llama.cpp"
	case strings.Contains(s, "ollama"):
		return "ollama"
	case server == "":
		return "未知（Server 头为空，常见于网关/魔改版）"
	}
	return server
}
