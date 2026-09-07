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

	"github.com/aleexjiang/llm-perf/internal/smetrics"
)

// ProbeCheck 一项探测结果。
type ProbeCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// CrossCheck 交叉验证建议：识别出引擎后给出对应的原生 perf 工具与等价命令。
// 工具结果存疑时，用户可用引擎原生工具独立复核（对数量级与分位趋势，非逐数对齐）。
type CrossCheck struct {
	Tool    string `json:"tool"`              // 原生工具（含运行方式）
	Command string `json:"command,omitempty"` // 按当前配置映射的等价命令（能给出来的都给出）
	Note    string `json:"note,omitempty"`
}

// ProbeResult 一次兼容性探测的完整报告（落盘为 probe-<时间戳>.json）。
type ProbeResult struct {
	GeneratedAt   time.Time    `json:"generated_at"`
	Endpoint      string       `json:"endpoint"`
	Server        string       `json:"server_header,omitempty"`
	EngineGuess   string       `json:"engine_guess,omitempty"`
	Models        []string     `json:"models,omitempty"`
	ModelMaxLen   int          `json:"model_max_len,omitempty"` // 服务端报告的模型上下文上限（vLLM 等提供）
	Checks        []ProbeCheck `json:"checks"`
	Verdicts      []string     `json:"verdicts,omitempty"`
	CrossChecks   []CrossCheck `json:"cross_checks,omitempty"`   // 交叉验证建议（引擎→原生 perf 工具）
	ServerMetrics string       `json:"server_metrics,omitempty"` // /metrics 可用性（观测层前置条件）

	// ThinkingLevel 探测到的思考等级控制参数（空 = 未探测到可控参数）
	ThinkingLevelParam string `json:"thinking_level_param,omitempty"`
	ThinkingLevelNote  string `json:"thinking_level_note,omitempty"`
}

// ProbeOptions 探测参数。
type ProbeOptions struct {
	Endpoint     string
	APIKey       string
	Auth         Auth           // 认证方案（默认 bearer + Authorization）
	ChatPath     string         // 接口路径，默认 /chat/completions
	MetricsPath  string         // 服务端 metrics 路径，默认 /metrics
	ModelsPath   string         // 模型列表路径，默认 /models
	Model        string         // 为空则取 /models 列表第一个
	ThinkingOn   map[string]any // 思考开启的 extra_body（可空）
	ThinkingOff  map[string]any // 思考关闭的 extra_body（可空）
	IncludeUsage bool
	MaxContext   int // 计划压测的最大上下文（config.LargestPromptTokens()），与服务端上限对比

	// Timeout 单请求超时（chat 与 models 共用；0 = 默认 120s）。
	// 之前硬编码 120s，配置的 timeout_seconds 完全不作用于 probe——慢网关误判超时
	Timeout time.Duration

	// ToolCall 是否执行 tool-call 健康检查（默认开；--no-toolcall 关闭）。
	// 只做前置门禁：检出引擎能否正常调工具 + 可行动结论；不测性能、不进主压测路径
	ToolCall bool

	// CaptureDir 非空时把 tool-call 检查的原始响应落盘到该目录（给厂商排障证据 + 判据回归 fixture）。
	// 必须显式指定才生效（不默认落盘）；内容含业务数据，外发前请按需脱敏
	CaptureDir string

	// 交叉验证命令映射用的计划压测参数
	XVPromptTokens int
	XVMaxTokens    int

	// ThinkingBudget 思考探测的 max_tokens 上限（≤0 或 >1024 时取 1024）：
	// 探测 prompt 极短，但思考长的模型（如 Qwen3 对填充文本思考 1500+ token）
	// 在 512 预算下会吃光预算导致误报"开关未生效"
	ThinkingBudget int
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
	chatPath := o.ChatPath
	if chatPath == "" {
		chatPath = "/chat/completions"
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	res := &ProbeResult{GeneratedAt: time.Now(), Endpoint: o.Endpoint}
	check := func(name string, ok bool, detail string) {
		res.Checks = append(res.Checks, ProbeCheck{Name: name, OK: ok, Detail: detail})
	}

	// ── 1. 模型列表 + Server 头 ──
	modelsPath := o.ModelsPath
	if modelsPath == "" {
		modelsPath = "/models"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+modelsPath, nil)
	o.Auth.Apply(req, o.APIKey)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
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
	// 服务端上下文上限：取目标模型（找不到精确匹配就取列表最大值）
	for _, m := range ml.Data {
		if m.ID == model && m.MaxModelLen > 0 {
			res.ModelMaxLen = m.MaxModelLen
		}
	}
	if res.ModelMaxLen == 0 {
		for _, m := range ml.Data {
			if m.MaxModelLen > res.ModelMaxLen {
				res.ModelMaxLen = m.MaxModelLen
			}
		}
	}
	if res.ModelMaxLen > 0 {
		check("context_limit", true, fmt.Sprintf("模型 %s max_model_len=%d", model, res.ModelMaxLen))
		if o.MaxContext > res.ModelMaxLen {
			res.Verdicts = append(res.Verdicts, fmt.Sprintf("⚠️ 计划压测的最大上下文 %dtk 超过模型上限 %dtk——超限请求会被服务端拒绝，请用 --max-ctx 或 max_prompt_tokens 截止到 %d 以内", o.MaxContext, res.ModelMaxLen, res.ModelMaxLen))
		} else if o.MaxContext > 0 {
			res.Verdicts = append(res.Verdicts, fmt.Sprintf("上下文规划 OK：计划最大 %dtk ≤ 模型上限 %dtk（注意预留 max_tokens 输出空间）", o.MaxContext, res.ModelMaxLen))
		}
	} else {
		check("context_limit", false, "服务端未报告 max_model_len（网关可能剥离）——长上下文压测前先用小档位试探")
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
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+chatPath, bytes.NewReader(pj))
		req.Header.Set("Content-Type", "application/json")
		o.Auth.Apply(req, o.APIKey)
		resp, err := (&http.Client{Timeout: timeout}).Do(req)
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
		pm := &TurnMetrics{}
		pm.applyWholeBody(raw)
		check("chat_nonstream", st == 200 && pm.Error == "" && pm.ReplyText != "",
			fmt.Sprintf("HTTP %d finish=%s content=%.30q", st, pm.FinishReason, pm.ReplyText))
		if st != 200 {
			res.Verdicts = append(res.Verdicts, "非流式基础对话失败（HTTP "+fmt.Sprint(st)+"）——检查认证/模型名")
		}
	}

	// ── 3. 流式基础对话：字段名清单 + usage + [DONE]（走共享解析器，与计时同一份代码）──
	streamAnalyze := func(raw []byte) *TurnMetrics {
		pm := &TurnMetrics{Stream: true}
		_ = ingestSSEBody(pm, bytes.NewReader(raw), time.Now, o.IncludeUsage)
		pm.closeOutWarnings(o.IncludeUsage)
		return pm
	}
	pm, serr := func() (*TurnMetrics, error) {
		st, raw, _, err := doChat(baseExtra, 16, true)
		if err != nil {
			return nil, err
		}
		if st != 200 {
			return nil, fmt.Errorf("HTTP %d: %.200s", st, raw)
		}
		return streamAnalyze(raw), nil
	}()
	if serr != nil {
		check("chat_stream", false, serr.Error())
	} else {
		keys := make([]string, 0, len(pm.seenDeltaKeys))
		for k := range pm.seenDeltaKeys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		check("chat_stream", true, fmt.Sprintf("delta 字段: [%s] usage=%v done=%v finish=%s",
			strings.Join(keys, ","), pm.usageSeen, pm.doneSeen, pm.FinishReason))
		for _, w := range pm.Warnings {
			if w == "usage_missing" {
				res.Verdicts = append(res.Verdicts, "⚠️ 流式响应不带 usage → token 数全 0，性能结论不可信；改配置 include_usage: false 并改用本地估算，或修服务端")
			} else if w == "stream_ended_without_done" {
				res.Verdicts = append(res.Verdicts, "⚠️ 流没有 [DONE] 终止符——魔改迹象，记录到 issue")
			} else if strings.HasPrefix(w, "unknown_delta_fields:") {
				res.Verdicts = append(res.Verdicts, "发现非标 delta 字段 "+strings.TrimPrefix(w, "unknown_delta_fields: ")+"——可能魔改，把 raw 转储发给工具维护者适配")
			}
		}
	}

	// ── 4/5. 思考开关有效性 ──
	budget := o.ThinkingBudget
	if budget <= 0 || budget > 1024 {
		budget = 1024
	}
	testThinking := func(extra map[string]any, label string) (reasoningField string, reasoningLen int) {
		st, raw, _, err := doChat(extra, budget, true)
		if err != nil || st != 200 {
			check("thinking_"+label, false, fmt.Sprintf("HTTP %d err=%v", st, err))
			return "none", 0
		}
		pm := streamAnalyze(raw)
		return pm.reasoningField, pm.ReasoningChars
	}

	if len(o.ThinkingOn) > 0 {
		fOn, nOn := testThinking(o.ThinkingOn, "on")
		if nOn > 0 {
			check("thinking_on", true, fmt.Sprintf("思考增量字段=%s，%d 字符（探测预算 %dtk）", fOn, nOn, budget))
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

	// ── 5.5 思考等级（effort/budget）控制探测 ──
	// 基线 = 思考开启状态下的默认思考长度；候选参数逐个试"压低"方向，
	// 显著低于基线（<50%）判为可控制；命中后试对应"拉高"方向确认双向可控。
	// 不同框架参数名不同，全走 extra_body 顶层合并（网关对 chat_template_kwargs 的处理也能顺带验证）。
	if len(o.ThinkingOn) > 0 {
		_, nBase := testThinking(o.ThinkingOn, "levels_base")
		working := ""
		if nBase == 0 {
			check("thinking_levels", false, "思考开启参数下思考增量=0，等级探测无意义（先解决思考开启）")
		} else {
			type levelCand struct {
				name string
				low  map[string]any
				high map[string]any
			}
			cands := []levelCand{
				{"reasoning_effort(顶层,OpenAI口径)",
					map[string]any{"reasoning_effort": "low"},
					map[string]any{"reasoning_effort": "high"}},
				{"reasoning_effort(chat_template_kwargs)",
					map[string]any{"chat_template_kwargs": map[string]any{"reasoning_effort": "low"}},
					map[string]any{"chat_template_kwargs": map[string]any{"reasoning_effort": "high"}}},
				{"thinking_budget(chat_template_kwargs,Qwen系)",
					map[string]any{"chat_template_kwargs": map[string]any{"thinking_budget": 16}},
					map[string]any{"chat_template_kwargs": map[string]any{"thinking_budget": budget}}},
				{"thinking_budget(顶层)",
					map[string]any{"thinking_budget": 16},
					map[string]any{"thinking_budget": budget}},
				{"thinking.type+budget(GLM系)",
					map[string]any{"thinking": map[string]any{"type": "disabled"}},
					map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": budget}}},
			}
			for _, c := range cands {
				_, nLow := testThinking(c.low, "levels_low")
				if nLow == 0 || nLow*2 >= nBase {
					continue // 压低不显著，参数不可控
				}
				_, nHigh := testThinking(c.high, "levels_high")
				working = c.name
				check("thinking_levels", true,
					fmt.Sprintf("参数[%s]可控：low=%d字（基线=%d字），high=%d字", c.name, nLow, nBase, nHigh))
				break
			}
			if working == "" {
				check("thinking_levels", false,
					fmt.Sprintf("5 组候选等级参数均未能显著压低思考量（基线=%d字）——按开/关两态压测，或查部署框架文档补充参数", nBase))
			}
		}
		res.ThinkingLevelParam = working
		res.ThinkingLevelNote = fmt.Sprintf("基线思考 %d 字（探测预算 %dtk）", nBase, budget)
		if working != "" {
			res.Verdicts = append(res.Verdicts,
				"思考等级可控（参数="+working+"）——可在配置 thinking.levels 定义多档变体（name/extra_body），报告按档位名分组对比")
		}
	}

	// ── 6. /metrics 可用性（服务端观测层的前置条件；vLLM 默认暴露） ──
	metricsPath := o.MetricsPath
	if metricsPath == "" {
		metricsPath = "/metrics"
	}
	s := smetrics.NewScraperAt(o.Endpoint, metricsPath)
	s.AuthScheme = o.Auth.Scheme // /metrics 与业务接口同一套认证（网关保护 metrics 端点时探测不至误报不可用）
	s.AuthHeader = o.Auth.Header
	s.APIKey = o.APIKey
	if ok, detail := s.Available(ctx); ok {
		res.ServerMetrics = "available: " + detail
		check("server_metrics", true, metricsPath+" 可用（"+detail+"）→ 配置 server_metrics: true 可开启观测层（缓存命中率/排队/prefill-decode 分解）")
	} else {
		res.ServerMetrics = "unavailable: " + detail
		check("server_metrics", false, metricsPath+" 不可达（"+detail+"）→ 观测层不可用，压测时自动降级为纯客户端计时")
	}

	// ── 7. tool-call 健康检查（默认开；--no-toolcall 关闭）──
	if o.ToolCall {
		runToolCallCheck(ctx, res, check, o, base, chatPath, timeout, model, streamAnalyze)
	}

	// ── 8. 交叉验证建议：引擎 → 原生 perf 工具 ──
	res.CrossChecks = crossChecks(o, res.EngineGuess, model)
	if _, ok := nativeTools[res.EngineGuess]; !ok {
		res.Verdicts = append(res.Verdicts,
			"无法识别引擎（Server 头不吐特征）——交叉验证命令不可用，自研网关/魔改版属预期；其余探测项不受影响")
	}

	return res
}

// nativeTools 引擎 → 原生 perf 工具对照（命令映射只对 OpenAI 兼容客户端型工具能给全；其余给指引）。
// 这些工具同样走 OpenAI 兼容端点的可从本机直接跑；引擎内置离线基准需到服务端跑。
var nativeTools = map[string]struct{ Tool, Note string }{
	"vllm": {"vllm bench serve（vLLM CLI 内置，原 benchmark_serving.py）",
		"可在本机跑（OpenAI 兼容客户端）；数据集/计时口径与本工具不同，用于交叉校验数量级与分位趋势"},
	"sglang": {"python3 -m sglang.bench_serving（SGLang 内置）",
		"--backend openai 可打任意 OpenAI 兼容端点，含 vLLM；本机可跑"},
	"mindie(华为)": {"MindIE-Service 自带 benchmark 工具（版本间差异大，以部署版本文档为准）",
		"通常需在服务端环境跑；本工具的 warnings 层对 MindIE 魔改字段已做兼容探测"},
	"tensorrt-llm/triton": {"trtllm-bench --mode throughput|latency（TensorRT-LLM 自带）",
		"以 TRT-LLM 版本文档为准；Triton 用 GenAI-Perf / AIPerf"},
	"tgi": {"text-generation-benchmark（TGI 自带）",
		"以 TGI 版本文档为准"},
	"llama.cpp": {"llama-bench（离线引擎基准，非服务级）",
		"服务级压测可直接用本工具 + llama-server 的 OpenAI 兼容端点"},
	"ollama": {"无官方服务级 perf 工具",
		"本工具即主要观测手段；注意 ollama 默认并发=1，需 OLLAMA_NUM_PARALLEL"},
}

// crossChecks 构建交叉验证建议列表。
func crossChecks(o ProbeOptions, engineGuess, model string) []CrossCheck {
	entry, ok := nativeTools[engineGuess]
	if !ok {
		return []CrossCheck{{
			Tool: "无法识别引擎（Server 头: " + engineGuess + "）",
			Note: "请查看服务端部署框架文档确认原生 perf 工具；或把 probe JSON 反馈给工具维护者补充对照表",
		}}
	}
	cc := CrossCheck{Tool: entry.Tool, Note: entry.Note}
	// vLLM / SGLang 的工具是 OpenAI 兼容客户端：能按当前配置直接映射等价命令
	switch engineGuess {
	case "vllm", "sglang":
		if u := strings.TrimRight(o.Endpoint, "/"); strings.HasSuffix(u, "/v1") {
			base := strings.TrimSuffix(u, "/v1")
			host, port := splitHostPort(base)
			pt := o.XVPromptTokens
			if pt <= 0 {
				pt = 1024
			}
			mt := o.XVMaxTokens
			if mt <= 0 {
				mt = 256
			}
			if engineGuess == "vllm" {
				cc.Command = fmt.Sprintf("vllm bench serve --backend openai-chat --base-url %s --model %s "+
					"--dataset-name random --random-input-len %d --random-output-len %d --num-prompts 64 --request-rate 4",
					u, model, pt, mt)
			} else {
				cc.Command = fmt.Sprintf("python3 -m sglang.bench_serving --backend openai --host %s --port %s --model %s "+
					"--dataset-name random --random-input-len %d --random-output-len %d --num-prompts 64 --request-rate 4",
					host, port, model, pt, mt)
			}
		}
	}
	return []CrossCheck{cc}
}

// splitHostPort 从 http://host:port 拆出主机与端口（给 sglang bench 的 --host/--port）。
func splitHostPort(base string) (host, port string) {
	s := strings.TrimPrefix(strings.TrimPrefix(base, "http://"), "https://")
	if i := strings.LastIndex(s, ":"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, "80"
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
	case strings.Contains(s, "text-generation-inference") || strings.Contains(s, "tgi"):
		return "tgi"
	case strings.Contains(s, "ollama"):
		return "ollama"
	case server == "":
		return "未知（Server 头为空，常见于网关/魔改版）"
	}
	return server
}
