package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// percentile 线性插值分位（偶数样本的 P50 自动等于两中值平均）。
// 与外部分析的中位数口径一致——
// 之前的 floor 取整口径在偶数样本时系统性偏低半步。
func percentile(xs []float64, p float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return percentileSorted(s, p)
}

// ── 重试策略：只对连接层瞬时失败重试，计时窗口干净，重试本身留痕 ──

type flakyState struct {
	mu    sync.Mutex
	count int
	codes []int // 每次请求返回的状态码；0 = 劫持断连
}

func flakyStub(t *testing.T, codes []int) (*httptest.Server, *flakyState) {
	t.Helper()
	st := &flakyState{codes: codes}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		idx := st.count
		st.count++
		code := 0
		if idx < len(st.codes) {
			code = st.codes[idx]
		}
		st.mu.Unlock()
		if code == 0 { // 劫持断连（connection reset 语义）
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		if code != 200 {
			w.WriteHeader(code)
			fmt.Fprint(w, `{"error":{"message":"stub boom"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

func retryOpts(stream bool) ChatOptions {
	return ChatOptions{Model: "m", Stream: stream, MaxTokens: 8,
		Messages: []Message{{Role: "user", Content: "hi"}}}
}

func TestRetryStreamResetThenSuccess(t *testing.T) {
	srv, st := flakyStub(t, []int{0, 200, 200})
	c := NewClient(srv.URL, "", 5*time.Second, false)
	c.Retry = &RetryPolicy{MaxAttempts: 3, Backoff: time.Millisecond}

	m, err := c.Chat(context.Background(), retryOpts(true))
	if err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if m.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1", m.RetryCount)
	}
	if !hasWarningPrefix(m.Warnings, "retried") {
		t.Fatalf("重试应留痕 warnings: %v", m.Warnings)
	}
	if m.StreamBroken {
		t.Fatal("最终成功的尝试不应标记 StreamBroken")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.count != 2 {
		t.Fatalf("应发出 2 次请求，得到 %d", st.count)
	}
	// 计时窗口干净：成功的尝试耗时远小于重试退避+两次请求的语义由测量保证，这里只验证 m 归属最后一次
	if m.CompletionTokens != 1 {
		t.Fatalf("应使用最后一次成功尝试的 usage: %d", m.CompletionTokens)
	}
}

func TestRetryHTTP500ThenSuccess(t *testing.T) {
	srv, st := flakyStub(t, []int{500, 200})
	c := NewClient(srv.URL, "", 5*time.Second, false)
	c.Retry = &RetryPolicy{MaxAttempts: 2, Backoff: time.Millisecond}

	m, err := c.Chat(context.Background(), retryOpts(false))
	if err != nil {
		t.Fatalf("5xx 重试后应成功: %v", err)
	}
	if m.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1", m.RetryCount)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.count != 2 {
		t.Fatalf("应发出 2 次请求，得到 %d", st.count)
	}
}

func TestRetryHTTP4xxNoRetry(t *testing.T) {
	srv, st := flakyStub(t, []int{400, 400, 200})
	c := NewClient(srv.URL, "", 5*time.Second, false)
	c.Retry = &RetryPolicy{MaxAttempts: 3, Backoff: time.Millisecond}

	m, err := c.Chat(context.Background(), retryOpts(false))
	if err == nil || !strings.Contains(m.Error, "400") {
		t.Fatalf("4xx 应确定性失败: err=%v m.Error=%q", err, m.Error)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.count != 1 {
		t.Fatalf("4xx 不应重试，应只发 1 次请求，得到 %d", st.count)
	}
}

func TestNoRetryDefault(t *testing.T) {
	srv, st := flakyStub(t, []int{0, 200})
	c := NewClient(srv.URL, "", 5*time.Second, false) // 无 Retry 策略

	// 劫持断连：可能表现为 Do 层错误（服务端未回任何响应）或 StreamBroken（响应头后中断），两者皆可
	m, err := c.Chat(context.Background(), retryOpts(true))
	broken := (err != nil) || (m != nil && m.StreamBroken)
	if !broken {
		t.Fatal("断连应表现为 err 或 StreamBroken")
	}
	if m != nil && m.RetryCount != 0 {
		t.Fatalf("默认不应重试: RetryCount=%d", m.RetryCount)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.count != 1 {
		t.Fatalf("默认只发 1 次请求，得到 %d", st.count)
	}
}

func hasWarningPrefix(ws []string, prefix string) bool {
	for _, w := range ws {
		if strings.HasPrefix(w, prefix) {
			return true
		}
	}
	return false
}

func TestRawCaptureRequiresExplicitDebugDir(t *testing.T) {
	srv, _ := flakyStub(t, []int{500})
	c := NewClient(srv.URL, "", 5*time.Second, false)
	m, err := c.Chat(context.Background(), retryOpts(false))
	if err == nil || m == nil {
		t.Fatalf("请求应失败且保留 metrics: m=%v err=%v", m, err)
	}
	if len(m.rawResp) != 0 {
		t.Fatalf("未开启 DebugDir 时不应保留 raw，长度=%d", len(m.rawResp))
	}
}

func TestRawCaptureUsesPrivateFile(t *testing.T) {
	dir := t.TempDir()
	srv, _ := flakyStub(t, []int{500})
	c := NewClient(srv.URL, "", 5*time.Second, false)
	c.DebugDir = filepath.Join(dir, "raw")
	m, err := c.Chat(context.Background(), retryOpts(false))
	if err == nil || m == nil {
		t.Fatalf("请求应失败且保留 metrics: m=%v err=%v", m, err)
	}
	entries, err := os.ReadDir(c.DebugDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("显式 DebugDir 应生成一个 capture，得到 %d", len(entries))
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("raw 文件权限 = %o，want 600", got)
	}
}

func TestPreviewHeadTail(t *testing.T) {
	if got := PreviewHeadTail("短文本"); got != "短文本" {
		t.Fatalf("短文本应原样: %q", got)
	}
	long := strings.Repeat("甲", 120) + strings.Repeat("乙", 100) + strings.Repeat("丙", 120)
	got := PreviewHeadTail(long)
	wantHead := strings.Repeat("甲", 120)
	wantTail := strings.Repeat("丙", 120)
	if !strings.HasPrefix(got, wantHead) || !strings.HasSuffix(got, wantTail) {
		t.Fatalf("长文本应掐头120掐尾120: head=%v tail=%v", strings.HasPrefix(got, wantHead), strings.HasSuffix(got, wantTail))
	}
	if !strings.Contains(got, "中略 100 字") {
		t.Fatalf("应含省略提示: %q", got)
	}
}

// ── 断流必须进 Error：scenario 全链路只认 Error 区分成败，只标 StreamBroken 会让
// 半截响应（部分 TTFT/usage）混进成功统计与 goodput ──

type errMidStream struct{ n int }

func (e *errMidStream) Read(p []byte) (int, error) {
	if e.n == 0 {
		e.n++
		s := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
		copy(p, s)
		return len(s), nil
	}
	return 0, fmt.Errorf("connection reset by peer")
}

type brokenBodyRT struct{}

func (brokenBodyRT) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(&errMidStream{}),
	}, nil
}

func TestStreamBrokenSetsError(t *testing.T) {
	c := NewClient("http://stub", "", 5*time.Second, false)
	c.HTTP = &http.Client{Transport: brokenBodyRT{}}

	m, err := c.Chat(context.Background(), retryOpts(true))
	if err != nil {
		t.Fatalf("无重试策略时断流应返回 (m, nil): %v", err)
	}
	if !m.StreamBroken {
		t.Fatal("应标记 StreamBroken")
	}
	if m.Error == "" {
		t.Fatal("断流必须写 Error——否则 scenario 层会把半截响应当成功数据统计")
	}
	if m.TTFT <= 0 {
		t.Fatalf("断流前已收到 chunk，TTFT 应部分可用: %v", m.TTFT)
	}
}

func TestStreamEOFWithoutDoneIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"ok\\\"}}]}\\n\\n")
	}))
	defer srv.Close()
	m, err := NewClient(srv.URL, "", 5*time.Second, false).Chat(context.Background(), retryOpts(true))
	if err != nil {
		t.Fatalf("缺 [DONE] 的响应仍应返回指标对象供排障: %v", err)
	}
	if !m.StreamBroken || m.Error == "" {
		t.Fatalf("缺 [DONE] 必须标记不完整失败: %+v", m)
	}
}

// ── ThinkMS 负值钳 0：reasoning 块晚于 content 首包（魔改引擎时序异常）不污染中位数 ──

func TestThinkMSNegativeClamped(t *testing.T) {
	base := time.Unix(1700000000, 0)
	m := &TurnMetrics{
		Stream:           true,
		SentAt:           base,
		FirstChunkAt:     ptrTime(base.Add(100 * time.Millisecond)),
		FirstContentAt:   ptrTime(base.Add(200 * time.Millisecond)),
		FirstReasoningAt: ptrTime(base.Add(300 * time.Millisecond)), // 异常：reasoning 晚于 content
		EndAt:            base.Add(1000 * time.Millisecond),
		CompletionTokens: 10,
	}
	m.Finalize()
	if m.ThinkMS != 0 {
		t.Fatalf("负 ThinkMS 应钳 0, got %v", m.ThinkMS)
	}
	// TTFT 必须取较早的 content 首包（200ms）而非 reasoning（300ms）——
	// 回归点：Finalize 曾固定"reasoning 优先"，content 先到时 TTFT 偏大
	if got := m.TTFT; got != 200 {
		t.Fatalf("TTFT 应取较早的 content 首包=200ms，实际 %.0f", got)
	}
	if !hasWarningPrefix(m.Warnings, "think_ms_negative") {
		t.Fatalf("应有 think_ms_negative 告警: %v", m.Warnings)
	}
	if m.DecodeMS <= 0 || m.E2EMS <= 0 {
		t.Fatalf("正常指标不应受影响: decode=%v e2e=%v", m.DecodeMS, m.E2EMS)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// 回归点：percentile 曾用 floor 索引，偶数样本 P50 系统性偏低半步，
// 与外部分析的中位数口径不一致
func TestPercentileMedianParity(t *testing.T) {
	if got := percentile([]float64{1, 2}, 50); got != 1.5 {
		t.Fatalf("偶数样本 P50 应取两中值平均=1.5，实际 %v", got)
	}
	if got := percentile([]float64{3, 1, 2}, 50); got != 2 {
		t.Fatalf("奇数样本 P50 应为中位=2，实际 %v", got)
	}
	if got := percentile([]float64{10, 20, 30, 40}, 50); got != 25 {
		t.Fatalf("四样本 P50 应=25，实际 %v", got)
	}
	if got := percentile([]float64{1, 2, 3, 4}, 100); got != 4 {
		t.Fatalf("P100 应取最大=4，实际 %v", got)
	}
}

// TestSamplingParamsInBody 请求级采样参数（真机发现：user 模式轨迹对齐需要 temperature=0）：
// 显式字段进请求体；nil 不传（服务端默认）；ExtraBody 不得静默覆盖显式采样参数。
func TestSamplingParamsInBody(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "", 5*time.Second, true)
	temp, topP := 0.0, 0.9
	_, err := c.Chat(context.Background(), ChatOptions{
		Model: "m", Stream: true, MaxTokens: 8,
		Messages:    []Message{{Role: "user", Content: "hi"}},
		Temperature: &temp, TopP: &topP,
		ExtraBody: map[string]any{"temperature": 1.5}, // 应被显式字段保护
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["temperature"] != 0.0 || got["top_p"] != 0.9 {
		t.Fatalf("采样参数注入错误: temperature=%v top_p=%v", got["temperature"], got["top_p"])
	}

	got = nil
	if _, err := c.Chat(context.Background(), retryOpts(true)); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["temperature"]; ok {
		t.Fatalf("nil 采样参数不应进请求体: %v", got)
	}
}
