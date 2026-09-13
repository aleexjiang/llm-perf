package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// probe_test 的这组用例锁的是 probe 的证据分级原则：
// 标准 OpenAI 兼容面（/chat/completions）上的结论才算配置基线；
// 引擎扩展面（/models 的 max_model_len、/metrics）探不到只记 NA，不判失败。
// 大量推理服务、尤其经网关代理之后不提供这些端点——把缺失当故障会让 probe 到处误报。

// mockChat 按请求体里的 stream 字段返回非流式 JSON 或 SSE 流。
func mockChat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"OK"}}]}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w,
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"想一想\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: [DONE]\n\n")
}

// newProbeServer 起一个可控的 mock：handle 返回 true 表示请求已被处理，false 则回 404。
func newProbeServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request) bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handle != nil && handle(w, r) {
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// chatOnly 只在指定路径上提供 chat，其余一律 404。
func chatOnly(path string) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != path {
			return false
		}
		mockChat(w, r)
		return true
	}
}

func findCheck(res *ProbeResult, name string) *ProbeCheck {
	for i := range res.Checks {
		if res.Checks[i].Name == name {
			return &res.Checks[i]
		}
	}
	return nil
}

// 只提供 chat 标准面的网关：/models 与 /metrics 一律 404。
// 期望：标准面全过，扩展面全部记 NA，通过率不受影响。
func TestProbe_GatewayWithoutExtSurfaces(t *testing.T) {
	srv := newProbeServer(t, chatOnly("/v1/chat/completions"))

	res := Probe(context.Background(), ProbeOptions{
		Endpoint: srv.URL + "/v1",
		Model:    "qwen3.8-27b",
		Timeout:  5 * time.Second,
	})

	if tal := res.Tally(); tal.CoreFail != 0 {
		t.Errorf("标准面不应有失败项，实际 CoreFail=%d，checks=%s", tal.CoreFail, dumpChecks(res))
	}
	for _, name := range []string{"models_list", "context_limit", "server_metrics"} {
		c := findCheck(res, name)
		if c == nil {
			t.Fatalf("缺少检查项 %s", name)
		}
		if !c.NA {
			t.Errorf("%s 应为 NA（服务端未提供该扩展面），实际 NA=false detail=%s", name, c.Detail)
		}
		if c.Tier != TierExt {
			t.Errorf("%s 应归为扩展面（TierExt），实际 %q", name, c.Tier)
		}
	}
	if strings.Contains(res.Summary, "失败") {
		t.Errorf("扩展面缺失不该出现在失败措辞里：%s", res.Summary)
	}
	if !strings.Contains(res.Summary, "未提供") {
		t.Errorf("汇总应如实说明扩展面未提供：%s", res.Summary)
	}
	// 汇总里的标准面分母不该被 NA 撑大
	if !strings.Contains(res.Summary, "标准面 5/5 通过") {
		t.Errorf("标准面计数不符，实际 summary=%s checks=%s", res.Summary, dumpChecks(res))
	}
}

// 填充保真度（自举校准）：服务端 tokenizer 与构造近似（corpus.CharsPerToken=4.0）
// 系统性偏离时，probe 必须实测出来并告警——否则"标称 4k 档"实际发 10k，档位语义失真。
// 这里造一个 usage 只有字符数一半的端点（等价于 2 chars/token 的紧凑 tokenizer）。
func TestProbe_FillerFidelityWarnsOnDeviation(t *testing.T) {
	UnloadCorpus("en") // 走合成词表路径，确保测的是 filler.go 的构造口径
	t.Cleanup(func() { UnloadCorpus("en") })

	srv := newProbeServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/chat/completions" {
			return false
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		chars := 0
		for _, m := range req.Messages {
			chars += len([]rune(m.Content))
		}
		usage := chars / 2 // 2 chars/token：构造侧按 4.0 换算 → 实测 token 数是标称的 2 倍
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"content":"OK"}}],`+
				`"usage":{"prompt_tokens":%d,"completion_tokens":1,"total_tokens":%d}}`, usage, usage+1)
			return true
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"想一想\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: {\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":1,\"total_tokens\":%d}}\n\n", usage, usage+1)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return true
	})

	res := Probe(context.Background(), ProbeOptions{
		Endpoint: srv.URL + "/v1", Model: "compact-tok", Timeout: 5 * time.Second, FillerLang: "en",
	})

	c := findCheck(res, "filler_fidelity")
	if c == nil {
		t.Fatal("缺少 filler_fidelity 检查项")
	}
	if c.Tier != TierExt {
		t.Errorf("filler_fidelity 应归扩展面（构造近似不是服务端问题），实际 %q", c.Tier)
	}
	if c.OK {
		t.Errorf("2 chars/token 的端点应判偏离（构造按 4.0 换算），实际 OK: %s", c.Detail)
	}
	if res.FillerCPT < 1.9 || res.FillerCPT > 2.1 {
		t.Errorf("实测 chars/token 应 ≈2.0，实际 %.2f（detail=%s）", res.FillerCPT, c.Detail)
	}
	// 告警必须给出可行动路径，否则用户只能看着偏差无从下手
	if !strings.Contains(c.Detail, "语料") {
		t.Errorf("告警应给出可行动建议（改用语料），实际 detail=%s", c.Detail)
	}
}

// 无 usage 的网关：填充保真度必须记 NA，而不是告警或失败。
// 真实世界里有服务（尤其经网关代理后）压根不返回 usage——此时"档位标称偏多少"根本无从得知，
// 把未知渲染成 WARN 会让每个这样的客户现场都挂一条假警。NA = 该面不存在，与"存在但不达标"严格区分。
func TestProbe_FillerFidelityNAWithoutUsage(t *testing.T) {
	UnloadCorpus("en")
	t.Cleanup(func() { UnloadCorpus("en") })

	// chatOnly 的非流式响应刻意不带 usage（见 mockChat）——正是无 usage 网关的形态。
	srv := newProbeServer(t, chatOnly("/v1/chat/completions"))

	res := Probe(context.Background(), ProbeOptions{
		Endpoint: srv.URL + "/v1", Model: "no-usage-gw", Timeout: 5 * time.Second, FillerLang: "en",
	})

	c := findCheck(res, "filler_fidelity")
	if c == nil {
		t.Fatal("缺少 filler_fidelity 检查项")
	}
	if !c.NA {
		t.Errorf("无 usage 时应记 NA（未知≠不达标），实际 NA=false ok=%v detail=%s", c.OK, c.Detail)
	}
	if c.Tier != TierExt {
		t.Errorf("filler_fidelity 应归扩展面，实际 %q", c.Tier)
	}
	if res.FillerCPT != 0 {
		t.Errorf("未测出时 FillerCPT 应为 0（omitempty 落盘即缺键），实际 %.2f", res.FillerCPT)
	}
	// NA 不该进入失败措辞，也不该撑大标准面分母
	if tal := res.Tally(); tal.CoreFail != 0 {
		t.Errorf("标准面不应有失败项，实际 CoreFail=%d，checks=%s", tal.CoreFail, dumpChecks(res))
	}
	if strings.Contains(res.Summary, "失败") {
		t.Errorf("无 usage 不该出现在失败措辞里：%s", res.Summary)
	}
}

// 认证自举：服务端只认 X-API-Key，配置里写的是默认 Bearer。
// 期望：自举出可用方案并把它写进配置片段，而不是直接判死。
func TestProbe_AuthBootstrap(t *testing.T) {
	// 路径对但认证不对时返回 401 —— 这才是「认证问题」该有的形态（404 是路径问题）
	srv := newProbeServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/chat/completions" {
			return false
		}
		if r.Header.Get("X-API-Key") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"unauthorized"}}`)
			return true
		}
		mockChat(w, r)
		return true
	})

	res := Probe(context.Background(), ProbeOptions{
		Endpoint: srv.URL + "/v1",
		APIKey:   "secret",
		Model:    "qwen3.8-27b",
		Timeout:  5 * time.Second,
	})

	c := findCheck(res, "auth_scheme")
	if c == nil || !c.OK {
		t.Fatalf("认证自举应命中 X-API-Key，实际 %+v", c)
	}
	if !strings.Contains(c.Detail, "自举命中") || !strings.Contains(c.Detail, "X-API-Key") {
		t.Errorf("结论未说明命中的方案：%s", c.Detail)
	}
	if !strings.Contains(res.Suggested, "auth_header: X-API-Key") {
		t.Errorf("配置片段应给出可用的 header，实际:\n%s", res.Suggested)
	}
}

// 挂载点扫描：chat 实际挂在 /openai/v1/chat/completions，配置默认路径落空。
func TestProbe_ChatPathScan(t *testing.T) {
	srv := newProbeServer(t, chatOnly("/openai/v1/chat/completions"))

	res := Probe(context.Background(), ProbeOptions{
		Endpoint: srv.URL,
		Model:    "qwen3.8-27b",
		Timeout:  5 * time.Second,
	})

	if c := findCheck(res, "chat_endpoint"); c == nil || !c.OK {
		t.Fatalf("扫到挂载点后 chat_endpoint 应通过，实际 %+v，checks=%s", c, dumpChecks(res))
	}
	hit := false
	for _, v := range res.Verdicts {
		if strings.Contains(v, "/openai/v1/chat/completions") {
			hit = true
		}
	}
	if !hit {
		t.Errorf("应提示实际挂载点，verdicts=%v", res.Verdicts)
	}
	if !strings.Contains(res.Suggested, "chat_path: /openai/v1/chat/completions") {
		t.Errorf("配置片段应给出扫到的 chat_path，实际:\n%s", res.Suggested)
	}
}

// 中止路径也要给配置片段：端点全 404 时 chat 路径扫描全落空，probe 提前返回。
// 此时片段只能列已确证的事实——绝不能复用正常路径的生成器，那会写出
// 「未探测到 /metrics」这类断言，而 /metrics 根本没探过。
func TestProbe_AbortPathStillSuggestsConfig(t *testing.T) {
	srv := newProbeServer(t, nil) // 一切 404

	res := Probe(context.Background(), ProbeOptions{
		Endpoint: srv.URL,
		Model:    "qwen3.8-27b",
		Timeout:  5 * time.Second,
	})

	if c := findCheck(res, "chat_path"); c == nil || c.OK {
		t.Fatalf("chat 路径全落空时应有 chat_path 失败项，实际 %+v，checks=%s", c, dumpChecks(res))
	}
	if res.Suggested == "" {
		t.Fatalf("中止路径也必须给出配置片段（最需要照抄就能改的恰是这些场景）")
	}
	if !strings.Contains(res.Suggested, "endpoint: "+srv.URL) {
		t.Errorf("片段应含已确证的 endpoint，实际:\n%s", res.Suggested)
	}
	if !strings.Contains(res.Suggested, "model: qwen3.8-27b") {
		t.Errorf("已指定的 model 应写成生效项，实际:\n%s", res.Suggested)
	}
	// 未探测的扩展面一项都不许出现（中止时它们压根没被探过）
	for _, bad := range []string{"server_metrics", "max_prompt_tokens", "models_path"} {
		if strings.Contains(res.Suggested, bad) {
			t.Errorf("中止片段不得断言未探测项 %s：\n%s", bad, res.Suggested)
		}
	}
}

// URL 拼装：endpoint 带 /v1 前缀与不带前缀两种写法都要算对。
func TestSplitOriginResolvePath(t *testing.T) {
	cases := []struct {
		endpoint, wantOrigin, wantBase string
	}{
		{"http://h:8849/v1", "http://h:8849", "/v1"},
		{"http://h:8849/v1/", "http://h:8849", "/v1"},
		{"http://h:8849", "http://h:8849", ""},
		{"https://gw.example.com/openai/v1", "https://gw.example.com", "/openai/v1"},
	}
	for _, c := range cases {
		origin, base := splitOrigin(c.endpoint)
		if origin != c.wantOrigin || base != c.wantBase {
			t.Errorf("splitOrigin(%q) = (%q,%q)，期望 (%q,%q)", c.endpoint, origin, base, c.wantOrigin, c.wantBase)
		}
	}
	// 配置写全前缀时不能再叠一层
	if got := resolvePath("/v1", "/v1/chat/completions", "/chat/completions"); got != "/v1/chat/completions" {
		t.Errorf("resolvePath 不应重复叠加前缀，实际 %q", got)
	}
	if got := resolvePath("/v1", "", "/chat/completions"); got != "/v1/chat/completions" {
		t.Errorf("默认路径应挂在 endpoint 前缀之后，实际 %q", got)
	}
	if got := resolvePath("", "", "/chat/completions"); got != "/chat/completions" {
		t.Errorf("无前缀 endpoint 的默认路径应挂在根上，实际 %q", got)
	}
	if got := resolvePath("/v1", "chat/completions", "/x"); got != "/v1/chat/completions" {
		t.Errorf("缺前导斜杠的路径应被纠正，实际 %q", got)
	}
}

func dumpChecks(res *ProbeResult) string {
	var sb strings.Builder
	for _, c := range res.Checks {
		sb.WriteString("\n  ")
		sb.WriteString(c.Name)
		sb.WriteString(" tier=")
		sb.WriteString(c.Tier)
		if c.NA {
			sb.WriteString(" NA")
		}
		if !c.OK {
			sb.WriteString(" FAIL")
		}
		sb.WriteString(" | ")
		sb.WriteString(c.Detail)
	}
	return sb.String()
}
