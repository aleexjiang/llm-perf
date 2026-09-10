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

	"github.com/aleexjiang/llm-perf/internal/auth"
	"github.com/aleexjiang/llm-perf/internal/smetrics"
)

// ProbeCheck 一项探测结果。
//
// Tier 决定这条结论能不能当「配置基线」用，是本探针的一等原则：
//   - TierCore：证据来自标准 OpenAI 兼容面（/chat/completions）。任何号称兼容的
//     推理服务都必须具备这个面，测出的问题就是真问题，配置以它为准。
//   - TierExt：证据来自引擎扩展面（vLLM/SGLang 的 /metrics、/models 的 max_model_len、
//     Server 头……）。大量推理服务、尤其经网关代理之后并不提供这些端点，
//     或者网关会把扩展字段剥掉——缺失属正常形态而非缺陷。
//     探到了就用它优化采集与报告，探不到记 NA，不判失败、不计入通过率。
//
// NA 表示「该探测面在本服务上不存在」，与 OK=false（存在但结论不达标）严格区分。
type ProbeCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	Tier   string `json:"tier,omitempty"` // core(默认,省略) | ext
	NA     bool   `json:"na,omitempty"`   // 仅 ext：服务端未提供该扩展面
}

// 证据分级取值。
const (
	TierCore = "core" // 标准 OpenAI 兼容面——配置基线的唯一来源
	TierExt  = "ext"  // 引擎扩展面——可选增强，缺失不算失败
)

// Tally 检查项分级统计。
type Tally struct {
	CorePass int // 标准面通过
	CoreFail int // 标准面失败（真问题，必须处理）
	ExtPass  int // 扩展面可用（白捡的增强）
	ExtFail  int // 扩展面存在但异常
	ExtNA    int // 扩展面服务端未提供（非标准端点，属预期）
}

// Tally 按证据分级统计全部检查项。
func (r *ProbeResult) Tally() Tally {
	var t Tally
	for _, c := range r.Checks {
		if c.Tier == TierExt {
			switch {
			case c.NA:
				t.ExtNA++
			case c.OK:
				t.ExtPass++
			default:
				t.ExtFail++
			}
			continue
		}
		if c.OK {
			t.CorePass++
		} else {
			t.CoreFail++
		}
	}
	return t
}

// Summarize 一句话结论。标准面是基线，扩展面只报「可用/未提供」，
// 措辞上刻意不让扩展面的缺失听起来像故障。
func (r *ProbeResult) Summarize() string {
	t := r.Tally()
	s := fmt.Sprintf("标准面 %d/%d 通过", t.CorePass, t.CorePass+t.CoreFail)
	ext := fmt.Sprintf("扩展面 %d 项可用", t.ExtPass)
	if t.ExtNA > 0 {
		ext += fmt.Sprintf("、%d 项服务端未提供（非标准端点，不影响结论）", t.ExtNA)
	}
	if t.ExtFail > 0 {
		ext += fmt.Sprintf("、%d 项异常", t.ExtFail)
	}
	return s + "；" + ext
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
	Summary       string       `json:"summary,omitempty"`          // 分级统计的一句话结论
	Suggested     string       `json:"suggested_config,omitempty"` // 可直接粘回配置文件的 YAML 片段
	CrossChecks   []CrossCheck `json:"cross_checks,omitempty"`     // 交叉验证建议（引擎→原生 perf 工具）
	ServerMetrics string       `json:"server_metrics,omitempty"`   // /metrics 可用性（观测层前置条件，扩展面）

	// ThinkingLevel 探测到的思考等级控制参数（空 = 未探测到可控参数）
	ThinkingLevelParam string `json:"thinking_level_param,omitempty"`
	ThinkingLevelNote  string `json:"thinking_level_note,omitempty"`

	// CacheProbe 前缀缓存定性探针结果（probe --cache 开启时非空）
	CacheProbe *CacheProbeResult `json:"cache_probe,omitempty"`
}

// ProbeOptions 探测参数。
type ProbeOptions struct {
	Endpoint     string
	APIKey       string
	Auth         auth.Auth      // 认证方案（默认 bearer + Authorization）
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

	// CacheCheck 前缀缓存定性探针（--cache 开启，默认关闭：默认 4 次长上下文请求，成本不低）
	CacheCheck bool

	// CacheSizeTokens 缓存探针的上下文大小（tokens），默认 40000（agent 真实档位）
	CacheSizeTokens int

	// FillerLang 缓存探针的填充语料语言（继承配置 filler_lang）
	FillerLang string
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
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	res := &ProbeResult{GeneratedAt: time.Now(), Endpoint: o.Endpoint}
	// 三种登记口径：标准面 / 扩展面 / 扩展面未提供。分级不是装饰——它决定这条结论
	// 能不能当配置基线用，见 ProbeCheck 注释。
	check := func(name string, ok bool, detail string) {
		res.Checks = append(res.Checks, ProbeCheck{Name: name, OK: ok, Detail: detail, Tier: TierCore})
	}
	checkExt := func(name string, ok bool, detail string) {
		res.Checks = append(res.Checks, ProbeCheck{Name: name, OK: ok, Detail: detail, Tier: TierExt})
	}
	checkExtNA := func(name, detail string) {
		res.Checks = append(res.Checks, ProbeCheck{Name: name, OK: true, Detail: detail, Tier: TierExt, NA: true})
	}

	// URL 统一按「origin + 绝对路径」拼装：endpoint 自带的前缀（如 /v1）折叠进路径，
	// 候选挂载点因此可以直接整体替换，不必猜「挂前缀 / 挂根」两种拼法。
	origin, basePath := splitOrigin(o.Endpoint)
	chatPath := resolvePath(basePath, o.ChatPath, "/chat/completions")

	// effAuth：后续请求实际使用的认证方案。认证自举命中备选后替换它，
	// 避免一个认证配置错误把整份报告的结论带偏。
	effAuth := o.Auth

	httpGet := func(u string) (int, []byte, http.Header, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return 0, nil, nil, err
		}
		effAuth.Apply(req, o.APIKey)
		resp, err := (&http.Client{Timeout: timeout}).Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
		return resp.StatusCode, b, resp.Header, nil
	}

	// ── 1. 模型列表 + Server 头（扩展面）──
	// /models 在 OpenAI 契约里有，但相当多网关只转发 /chat/completions，
	// 所以这里探不到只记 NA —— 连通性与能力基线一律以 chat 面为准。
	modelsPath := resolvePath(basePath, o.ModelsPath, "/models")
	mStatus, mBody, mHdr, mErr := httpGet(origin + modelsPath)
	if mErr == nil && (mStatus == http.StatusNotFound || mStatus == http.StatusMethodNotAllowed) {
		for _, p := range pathCandidates(modelsPath, modelsPathCandidates) {
			if st, b, h, e := httpGet(origin + p); e == nil && st == http.StatusOK {
				modelsPath, mStatus, mBody, mHdr, mErr = p, st, b, h, e
				res.Verdicts = append(res.Verdicts, "模型列表实际挂在 "+p+"（models_path 建议改成该值）")
				break
			}
		}
	}
	res.Server = mHdr.Get("Server")
	if v := mHdr.Get("X-Request-Id"); v != "" {
		res.Server += " | x-request-id: " + v
	}
	var ml probeModelsResp
	modelsOK := mErr == nil && mStatus == http.StatusOK && json.Unmarshal(mBody, &ml) == nil && len(ml.Data) > 0
	switch {
	case modelsOK:
		for _, m := range ml.Data {
			res.Models = append(res.Models, m.ID)
		}
		checkExt("models_list", true, fmt.Sprintf("%s HTTP 200，%d 个模型", modelsPath, len(ml.Data)))
	case mErr != nil:
		checkExtNA("models_list", "未提供（"+modelsPath+" 请求失败: "+mErr.Error()+"）——该端点非必需，连通性以 chat 为准")
	case mStatus == http.StatusUnauthorized || mStatus == http.StatusForbidden:
		checkExtNA("models_list", fmt.Sprintf("HTTP %d（认证被拒，见下方 auth_scheme）", mStatus))
	case mStatus == http.StatusNotFound || mStatus == http.StatusMethodNotAllowed:
		checkExtNA("models_list", fmt.Sprintf("未提供（%s 及 %d 个常见挂载点均 404/405）——网关常见形态，不影响压测，只要配置里显式写 model",
			modelsPath, len(modelsPathCandidates)))
	default:
		checkExt("models_list", false, fmt.Sprintf("%s HTTP %d（可达但不可用）%.160s", modelsPath, mStatus, strings.TrimSpace(string(mBody))))
	}
	model := o.Model
	if model == "" {
		if len(res.Models) == 0 {
			res.Verdicts = append(res.Verdicts, "拿不到模型列表，且配置里未显式指定 model——无法继续，在配置里写 model 后重试")
			res.Summary = res.Summarize()
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
	// max_model_len 是引擎扩展字段（标准 OpenAI /models 只返回 id/object/created/owned_by），
	// 网关剥掉它很常见——探不到记 NA，别让它看起来像端点故障。
	if res.ModelMaxLen > 0 {
		checkExt("context_limit", true, fmt.Sprintf("模型 %s max_model_len=%d（引擎扩展字段）", model, res.ModelMaxLen))
		if o.MaxContext > res.ModelMaxLen {
			res.Verdicts = append(res.Verdicts, fmt.Sprintf("⚠️ 计划压测的最大上下文 %dtk 超过模型上限 %dtk——超限请求会被服务端拒绝，请用 --max-ctx 或 max_prompt_tokens 截止到 %d 以内", o.MaxContext, res.ModelMaxLen, res.ModelMaxLen))
		} else if o.MaxContext > 0 {
			res.Verdicts = append(res.Verdicts, fmt.Sprintf("上下文规划 OK：计划最大 %dtk ≤ 模型上限 %dtk（注意预留 max_tokens 输出空间）", o.MaxContext, res.ModelMaxLen))
		}
	} else {
		checkExtNA("context_limit", "服务端未报告 max_model_len（标准 /models 无此字段，网关也常剥掉）——长上下文压测前先用小档位试探")
	}
	res.EngineGuess = guessEngine(res.Server, mBody)

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
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, origin+chatPath, bytes.NewReader(pj))
		req.Header.Set("Content-Type", "application/json")
		effAuth.Apply(req, o.APIKey)
		resp, err := (&http.Client{Timeout: timeout}).Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer resp.Body.Close()
		raw, _ = io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
		return resp.StatusCode, raw, resp.Header, nil
	}

	// ── 1.5 chat 面自举：连通性 → 挂载路径 → 认证方案 ──
	//
	// 这三件事决定后面每个探测项能否成立，值得先用一个 max_tokens=1 的最小请求串起来定夺。
	// 之所以不依赖 /models 来判断，是因为网关常常压根没开这个端点；
	// chat 面才是所有 OpenAI 兼容服务都必须提供的标准面。
	//
	// 认证只在「已被拒」时才试备选：当前方案可用就不额外发送 key，不把密钥撒到更多 header 上。
	minChat := func() (int, []byte) {
		st, raw, _, err := doChat(nil, 1, false)
		if err != nil {
			return 0, []byte(err.Error())
		}
		return st, raw
	}
	chatSt, chatRaw := minChat()
	if chatSt == http.StatusNotFound || chatSt == http.StatusMethodNotAllowed {
		for _, p := range pathCandidates(chatPath, chatPathCandidates) {
			chatPath = p
			if st, raw, _, err := doChat(nil, 1, false); err == nil && st != http.StatusNotFound && st != http.StatusMethodNotAllowed {
				chatSt, chatRaw = st, raw
				res.Verdicts = append(res.Verdicts, "chat 实际挂在 "+p+"——把配置的 chat_path 改成该值")
				break
			}
		}
	}
	switch {
	case chatSt == 0:
		check("chat_endpoint", false, "请求失败: "+strings.TrimSpace(string(chatRaw)))
		res.Verdicts = append(res.Verdicts, "chat 端点不可达——先解决网络（VPN/代理/端口/证书），其余探测项无从谈起")
		res.Summary = res.Summarize()
		return res
	case chatSt == http.StatusNotFound || chatSt == http.StatusMethodNotAllowed:
		check("chat_path", false, fmt.Sprintf("%s 及 %d 个常见挂载点全落空（404/405）——确认服务端路由前缀", chatPath, len(chatPathCandidates)))
		res.Verdicts = append(res.Verdicts, "chat 路径全部落空——把正确的 chat_path 写进配置后重跑")
		res.Summary = res.Summarize()
		return res
	case chatSt == http.StatusOK:
		check("chat_endpoint", true, fmt.Sprintf("%s HTTP 200——chat 标准面可达，能力基线成立", chatPath))
	default:
		check("chat_endpoint", true, fmt.Sprintf("%s HTTP %d（可达，非 200；具体判定见下方各项）", chatPath, chatSt))
	}
	if chatSt == http.StatusUnauthorized || chatSt == http.StatusForbidden {
		// 认证自举：候选含当前配置（/models 的 401 未必代表业务面也拒），命中即替换 effAuth
		cands := []auth.Auth{
			o.Auth,
			{Scheme: "raw", Header: "Authorization"}, // Authorization: <key>（网关裸 key）
			{Scheme: "raw", Header: "X-API-Key"},
			{Scheme: "bearer", Header: "X-API-Key"},
		}
		if o.APIKey == "" {
			check("auth_scheme", false, fmt.Sprintf("HTTP %d 但配置里没有 API key——补 api_key，或服务端本就免认证时设 auth_scheme: none", chatSt))
		} else {
			var tried []string
			hit := false
			for _, c := range cands {
				effAuth = c
				st, raw := minChat()
				switch {
				case st == 0:
					tried = append(tried, c.Describe()+" → 请求失败: "+strings.TrimSpace(string(raw)))
				case st == http.StatusUnauthorized || st == http.StatusForbidden:
					tried = append(tried, fmt.Sprintf("%s → HTTP %d", c.Describe(), st))
				default:
					hit = true
					check("auth_scheme", true, fmt.Sprintf("自举命中：%s（HTTP %d）——按此改配置即可，不必再猜", c.Describe(), st))
					res.Verdicts = append(res.Verdicts, "认证方案已自举出可用值——写进配置的 auth_scheme / auth_header")
				}
				if hit {
					break
				}
			}
			if !hit {
				effAuth = o.Auth
				check("auth_scheme", false, "4 种认证方案均被拒："+strings.Join(tried, "；"))
				res.Verdicts = append(res.Verdicts, "认证自举失败——确认 key 是否正确/过期，或向服务方索取正确的 header 名")
			}
		}
	} else {
		check("auth_scheme", true, "当前方案可用："+o.Auth.Describe())
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
		if err != nil {
			check("thinking_"+label, false, "请求失败: "+err.Error())
			return "none", 0
		}
		if st != 200 {
			// HTTP 400 时 err 为 nil，只打 "HTTP 400 err=<nil>" 等于没有证据——带上服务端错误体
			check("thinking_"+label, false, fmt.Sprintf("HTTP %d %.200s", st, strings.TrimSpace(string(raw))))
			return "none", 0
		}
		pm := streamAnalyze(raw)
		return pm.reasoningField, pm.ReasoningChars
	}

	// tryThinking 与 testThinking 同源，但不产出 check 行：等级候选需要逐个试（含枚举值试探），
	// 每个失败都写一行会让同名 check 重复，也无法区分"参数被拒"与"思考量真的是 0"。
	tryThinking := func(extra map[string]any) (reasoningLen int, ok bool, note string) {
		st, raw, _, err := doChat(extra, budget, true)
		if err != nil {
			return 0, false, "请求失败: " + err.Error()
		}
		if st != 200 {
			return 0, false, fmt.Sprintf("HTTP %d %.160s", st, strings.TrimSpace(string(raw)))
		}
		return streamAnalyze(raw).ReasoningChars, true, ""
	}

	// ── 4. 部署侧默认思考状态（无思考参数基线）──
	// 独立于配置，必须显式探测：有些部署在服务端就把思考关掉了（模板硬编码
	// enable_thinking=false、没挂 --reasoning-parser、或启动参数指定了非思考模式），
	// 此时"本次压测跑的是思考态"是错误前提，整份性能结论会整体错位。
	// 无参数请求反映的正是客户默认拿到的行为，是判断这件事的唯一直接证据。
	nDef, okDef, noteDef := tryThinking(nil)
	switch {
	case !okDef:
		check("thinking_default", false, "无参数基线请求失败: "+noteDef)
	case nDef > 0:
		check("thinking_default", true,
			fmt.Sprintf("无参数基线：思考增量 %d 字符 → 部署默认【开】思考（不带参数发的请求就是思考态）", nDef))
	default:
		check("thinking_default", true,
			"无参数基线：思考增量 0 字符 → 部署默认【关】思考（客户默认拿到的是非思考响应）")
		res.Verdicts = append(res.Verdicts,
			"⚠️ 服务端默认不思考：不带思考参数的压测衡量的是非思考性能。若本次要测思考态，"+
				"必须显式传开启参数，并以 thinking_on 的结果为准（参数无效时下面会报 ❌）")
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
			msg := "思考开启参数未生效：确认参数名（chat_template_kwargs/enable_thinking/thinking...）与模型模板"
			if okDef && nDef == 0 {
				msg = "思考开启参数未生效，且无参数基线同样没有思考增量——优先怀疑部署侧关掉了思考" +
					"（模板硬编码 enable_thinking=false / 未挂 --reasoning-parser / 启动参数就是非思考模式），而非参数名写错"
			}
			res.Verdicts = append(res.Verdicts, "⚠️ "+msg)
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
			type levelHigh struct {
				val  string // 枚举值/参数值的人读标签，写进结论——避免把兜底成功的值冒充成首次尝试的值
				body map[string]any
			}
			type levelCand struct {
				name  string
				low   map[string]any
				highs []levelHigh // 拉高方向候选：枚举值跨引擎不同（vLLM 认 xhigh/medium/low，OpenAI 认 high），逐个试到第一个被接受
			}
			cands := []levelCand{
				{"reasoning_effort(顶层,OpenAI口径)",
					map[string]any{"reasoning_effort": "low"},
					[]levelHigh{
						{"high", map[string]any{"reasoning_effort": "high"}},
						{"xhigh", map[string]any{"reasoning_effort": "xhigh"}}}},
				{"reasoning_effort(chat_template_kwargs)",
					map[string]any{"chat_template_kwargs": map[string]any{"reasoning_effort": "low"}},
					[]levelHigh{
						{"high", map[string]any{"chat_template_kwargs": map[string]any{"reasoning_effort": "high"}}},
						{"xhigh", map[string]any{"chat_template_kwargs": map[string]any{"reasoning_effort": "xhigh"}}}}},
				{"thinking_budget(chat_template_kwargs,Qwen系)",
					map[string]any{"chat_template_kwargs": map[string]any{"thinking_budget": 16}},
					[]levelHigh{{fmt.Sprintf("budget=%d", budget), map[string]any{"chat_template_kwargs": map[string]any{"thinking_budget": budget}}}}},
				{"thinking_budget(顶层)",
					map[string]any{"thinking_budget": 16},
					[]levelHigh{{fmt.Sprintf("budget=%d", budget), map[string]any{"thinking_budget": budget}}}},
				{"thinking.type+budget(GLM系)",
					map[string]any{"thinking": map[string]any{"type": "disabled"}},
					[]levelHigh{{"enabled", map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": budget}}}}},
			}
			var rejected []string
			for _, c := range cands {
				nLow, okLow, noteLow := tryThinking(c.low)
				if !okLow {
					rejected = append(rejected, fmt.Sprintf("%s 压低请求被拒（%s）", c.name, noteLow))
					continue
				}
				if nLow == 0 || nLow*2 >= nBase {
					continue // 压低不显著，参数不可控
				}
				// 拉高方向：确认双向可控。枚举值不被接受时如实标注，不把 400 当成"思考量为 0"。
				// 被拒的候选必须留痕——客户最需要知道的恰是「这个部署没有 high 档」。
				// 早先的写法一遇到成功候选就 break，把此前被拒的候选全丢了：只在 detail 里
				// 留下成功的那个档位，读者无从得知其余档位是被拒还是压根没试。
				var highRejected []string
				highNote := ""
				for _, h := range c.highs {
					n, ok, note := tryThinking(h.body)
					if ok {
						highNote = fmt.Sprintf("拉高=%s（%d字）", h.val, n)
						break
					}
					if r := []rune(note); len(r) > 90 {
						note = string(r[:90]) + "…"
					}
					highRejected = append(highRejected, fmt.Sprintf("%s 被拒（%s）", h.val, note))
				}
				if len(highRejected) > 0 {
					highNote += "；被拒候选: " + strings.Join(highRejected, " / ")
				}
				if highNote == "" {
					highNote = "拉高方向未确认"
				}
				working = c.name
				check("thinking_levels", true,
					fmt.Sprintf("参数[%s]可控：low=%d字（基线=%d字），%s", c.name, nLow, nBase, highNote))
				break
			}
			if working == "" {
				msg := fmt.Sprintf("5 组候选等级参数均未能显著压低思考量（基线=%d字）——按开/关两态压测，或查部署框架文档补充参数", nBase)
				if len(rejected) > 0 {
					msg += "；被服务端拒绝的候选取证: " + strings.Join(rejected, " / ")
				}
				check("thinking_levels", false, msg)
			}
		}
		res.ThinkingLevelParam = working
		res.ThinkingLevelNote = fmt.Sprintf("基线思考 %d 字（探测预算 %dtk）", nBase, budget)
		if working != "" {
			res.Verdicts = append(res.Verdicts,
				"思考等级可控（参数="+working+"）——可在配置 thinking.levels 定义多档变体（name/extra_body），报告按档位名分组对比")
		}
	}

	// ── 6. /metrics 可用性（扩展面）──
	// 最容易踩「过度依赖」的一处：很多推理服务、尤其经网关代理之后根本不转发 /metrics，
	// 或者直接 404。缺失只记 NA——压测侧本来就会自动降级为纯客户端计时。
	// 注意：/metrics 按 Prometheus 惯例挂在 origin 根上，不继承 endpoint 的 /v1 前缀
	// （smetrics.NewScraperAt 自身会剥掉尾部的 /v1，与此一致）。
	metricsPath := strings.TrimRight(o.MetricsPath, "/")
	if metricsPath == "" {
		metricsPath = "/metrics"
	}
	metricsOK := false
	{
		s := smetrics.NewScraperAt(origin, metricsPath)
		s.Auth = effAuth // 与业务接口同一套认证（网关保护 metrics 端点时不至误报不可用）
		s.APIKey = o.APIKey
		ok, detail := s.Available(ctx)
		if !ok {
			for _, p := range pathCandidates(metricsPath, metricsPathCandidates) {
				cand := smetrics.NewScraperAt(origin, p)
				cand.Auth = effAuth
				cand.APIKey = o.APIKey
				if ok2, d2 := cand.Available(ctx); ok2 {
					ok, detail, metricsPath = true, d2, p
					res.Verdicts = append(res.Verdicts, "/metrics 实际挂在 "+p+"——该端点非标准面，只用于增强采集")
					break
				}
			}
		}
		metricsOK = ok
		if ok {
			res.ServerMetrics = "available: " + detail
			checkExt("server_metrics", true, metricsPath+" 可用（"+detail+"）→ 配置 server_metrics: true 可开启观测层（缓存命中率/排队/prefill-decode 分解）")
		} else {
			res.ServerMetrics = "unavailable: " + detail
			checkExtNA("server_metrics", fmt.Sprintf("未提供（%s 及 %d 个常见挂载点均不可用：%s）——非标准端点，压测自动降级为纯客户端计时，结论不受影响",
				metricsPath, len(metricsPathCandidates), detail))
		}
	}

	// ── 7. tool-call 健康检查（默认开；--no-toolcall 关闭）──
	if o.ToolCall {
		runToolCallCheck(ctx, res, check, o, origin, chatPath, timeout, model, streamAnalyze)
	}

	// ── 7.5 前缀缓存定性探针（--cache 开启，默认关）──
	// 上下文大小对齐模型上限：超出会被服务端拒绝，自动收到 max_model_len-1024 以内
	if o.CacheCheck {
		size := o.CacheSizeTokens
		if size <= 0 {
			size = 40000
		}
		if res.ModelMaxLen > 0 && size > res.ModelMaxLen-1024 {
			size = res.ModelMaxLen - 1024
			res.Verdicts = append(res.Verdicts, fmt.Sprintf("缓存探针上下文已收到模型上限以内: %dtk（原配置 %dtk + 输出预算会超 max_model_len=%d）", size, o.CacheSizeTokens, res.ModelMaxLen))
		}
		pc := NewClient(origin, o.APIKey, timeout, o.IncludeUsage)
		pc.Auth = effAuth
		pc.ChatPath = chatPath
		cacheProbeInto(ctx, res, pc, CacheProbeOptions{
			Model:       model,
			SizeTokens:  size,
			ThinkingOff: o.ThinkingOff,
			FillerLang:  o.FillerLang,
		})
	}

	// ── 8. 交叉验证建议：引擎 → 原生 perf 工具 ──
	res.CrossChecks = crossChecks(o, res.EngineGuess, model)
	if _, ok := nativeTools[res.EngineGuess]; !ok {
		res.Verdicts = append(res.Verdicts,
			"无法识别引擎（Server 头不吐特征）——交叉验证命令不可用，自研网关/魔改版属预期；其余探测项不受影响")
	}

	// ── 9. 配置片段 + 分级汇总 ──
	res.Suggested = buildSuggestedConfig(res, o, effAuth, origin, chatPath, modelsPath, metricsPath, modelsOK, metricsOK, model)
	res.Summary = res.Summarize()

	return res
}

// buildSuggestedConfig 把探测结论收敛成可直接粘回配置文件的 YAML 片段。
//
// 证据分级在这里同样是硬约束：只有标准面确认过的值才写成「生效项」；
// 由引擎扩展面（/models 的 max_model_len、/metrics）推导出来的一律注释掉并标明来源——
// 那些不是所有服务都有，当成配置基线用会在换个端点后立刻失效。
func buildSuggestedConfig(res *ProbeResult, o ProbeOptions, effAuth auth.Auth, origin, chatPath, modelsPath, metricsPath string,
	modelsOK, metricsOK bool, model string) string {

	var sb strings.Builder
	fmt.Fprintf(&sb, "# 由 bench probe 生成（%s），可直接粘回配置文件\n", res.GeneratedAt.Format("2006-01-02 15:04"))
	sb.WriteString("# 生效项 = 标准面已确认；被注释的项 = 引擎扩展面推导，仅供增强，换端点后需重新探测\n")
	fmt.Fprintf(&sb, "endpoint: %s\n", origin)
	fmt.Fprintf(&sb, "chat_path: %s          # 标准面已确认\n", chatPath)
	if modelsOK {
		fmt.Fprintf(&sb, "models_path: %s\n", modelsPath)
	} else {
		fmt.Fprintf(&sb, "# models_path: %s        # 未探测到，该端点非必需\n", modelsPath)
	}
	na := effAuth.Normalize()
	authNote := "沿用配置"
	if na != o.Auth.Normalize() {
		authNote = "认证自举命中"
	}
	fmt.Fprintf(&sb, "auth_scheme: %s        # %s\n", na.Scheme, authNote)
	if na.Header != "" && na.Header != "Authorization" {
		fmt.Fprintf(&sb, "auth_header: %s\n", na.Header)
	}
	if model != "" {
		fmt.Fprintf(&sb, "model: %s\n", model)
	}
	if res.ModelMaxLen > 2048 {
		fmt.Fprintf(&sb, "# max_prompt_tokens: %d   # 由 /models 的 max_model_len=%d 减输出预算推得（引擎扩展字段）\n",
			res.ModelMaxLen-2048, res.ModelMaxLen)
	} else {
		sb.WriteString("# max_prompt_tokens: 32000   # 未探测到模型上限（/models 未提供 max_model_len），先用保守值试探\n")
	}
	if metricsOK {
		// 与 max_prompt_tokens 一致：凡由扩展面推导来的项一律注释掉，让人显式确认后再启用。
		// 此前这里写成生效态，与 README「扩展面推导项一律注释」的自述直接矛盾——
		// 而它开的不过是「多采一份服务端观测」，与任何结论都无关，更没理由默认打开。
		fmt.Fprintf(&sb, "# server_metrics: true   # %s 可达——扩展面推导项，按约定注释；需要增强观测时取消注释（不开不影响任何结论）\n", metricsPath)
	} else {
		sb.WriteString("# server_metrics: false   # 未探测到 /metrics（非标准端点，属常见形态）\n")
	}
	if res.ThinkingLevelParam != "" {
		fmt.Fprintf(&sb, "# thinking:  # 思考等级可控（参数=%s），按需定义 levels 变体做多档对比\n", res.ThinkingLevelParam)
	} else {
		sb.WriteString("# thinking:  # 未探测到可控的思考等级参数，建议按 thinking.on/off 两态压测\n")
	}
	return sb.String()
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

// splitOrigin 把 endpoint 拆成 origin 与路径前缀（如 http://h:8849/v1 → http://h:8849 + /v1）。
// 统一按「origin + 绝对路径」拼装 URL 之后，候选挂载点可以直接整体替换，
// 不必再猜「挂在前缀之后 / 挂在根上」两种拼法。
func splitOrigin(endpoint string) (origin, basePath string) {
	rest := endpoint
	scheme := ""
	if i := strings.Index(rest, "://"); i >= 0 {
		scheme, rest = rest[:i+3], rest[i+3:]
	}
	if i := strings.Index(rest, "/"); i >= 0 {
		return scheme + rest[:i], strings.TrimRight(rest[i:], "/")
	}
	return scheme + rest, ""
}

// resolvePath 把配置里的路径解析为相对 origin 的绝对路径。
// 配置可能写全前缀（/v1/chat/completions），也可能只写 /chat/completions，两种都要能算对。
func resolvePath(basePath, p, def string) string {
	if p == "" {
		p = def
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if basePath == "" || p == basePath || strings.HasPrefix(p, basePath+"/") {
		return p
	}
	return basePath + p
}

// pathCandidates 返回候选挂载点（剔除与当前值重复的项，保持声明顺序）。
// 只在默认路径明确落空（404/405）时才用——这些是各框架/网关的既有惯例，不是瞎猜。
func pathCandidates(cur string, cands []string) []string {
	out := make([]string, 0, len(cands))
	for _, p := range cands {
		if p != cur {
			out = append(out, p)
		}
	}
	return out
}

// 常见挂载点候选。命中即写进结论，让用户改配置而不是靠猜。
var (
	chatPathCandidates    = []string{"/v1/chat/completions", "/chat/completions", "/openai/v1/chat/completions", "/api/v1/chat/completions"}
	modelsPathCandidates  = []string{"/v1/models", "/models", "/api/v1/models"}
	metricsPathCandidates = []string{"/metrics", "/actuator/prometheus", "/api/v1/metrics", "/v1/metrics"}
)

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
