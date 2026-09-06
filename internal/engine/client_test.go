package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

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
