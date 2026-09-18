package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/engine"
)

// ── httptest 集成：SSE 桩服务（user/rps/concurrency 集成测试共用） ──

type stubState struct {
	mu       sync.Mutex
	count    int
	failReq  map[int]bool // 第 N 个请求（1-based）强制断连
	promptOv map[int]int  // 第 N 个请求（1-based）强制返回指定 prompt_tokens（模拟 tokenizer 重切分回退）
	bodies   []int        // 每个请求的 messages 数量
	contents []string     // 每个请求的最后一条 user 消息前 80 字符
}

// sseStub 返回 OpenAI 兼容流式桩：usage.prompt_tokens = 消息字符总量/4。
// failReq 中的请求直接劫持并关闭连接（模拟 connection reset）。
func sseStub(t *testing.T, state *stubState) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		state.mu.Lock()
		state.count++
		idx := state.count
		fail := state.failReq[idx]
		last := ""
		if len(body.Messages) > 0 {
			last = body.Messages[len(body.Messages)-1].Content
			if len(last) > 80 {
				last = last[:80]
			}
		}
		state.bodies = append(state.bodies, len(body.Messages))
		state.contents = append(state.contents, last)
		state.mu.Unlock()

		if fail {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", 500)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			conn.Close() // 不给任何响应，模拟 connection reset
			return
		}

		total := 0
		for _, m := range body.Messages {
			total += len(m.Content)
		}
		prompt := total / 4
		if prompt < 1 {
			prompt = 1
		}
		state.mu.Lock()
		if ov, ok := state.promptOv[idx]; ok {
			prompt = ov
		}
		state.mu.Unlock()
		usage := map[string]any{
			"prompt_tokens": prompt, "completion_tokens": 4, "total_tokens": prompt + 4,
		}
		if !body.Stream {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "hi hi "}, "finish_reason": "stop"}},
				"usage":   usage,
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		emit := func(v map[string]any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
			f.Flush()
		}
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"role": "assistant"}}}})
		for i := 0; i < 2; i++ {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "hi "}}}})
		}
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usage})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testCfg(t *testing.T, endpoint string) *config.Config {
	t.Helper()
	on := true
	return &config.Config{
		Endpoint:       endpoint,
		OutputDir:      t.TempDir(),
		TimeoutSeconds: 10,
		IncludeUsage:   &on,
		Stream:         &on,
		CorpusLang:     "en",
		Thinking:       config.Thinking{Mode: "off", MaxTokensFloor: intPtr(512)},
		Models:         []string{"stub-model"},
	}
}

func intPtr(v int) *int { return &v }

// ── Scenario 注册表：新增场景实现接口 + Register 即可，main 零改动 ──

func TestScenarioRegistry(t *testing.T) {
	for _, name := range []string{"user", "rps", "concurrency"} {
		if _, ok := Lookup(name); !ok {
			t.Fatalf("场景 %s 应已注册", name)
		}
	}
	if _, ok := Lookup("nope"); ok {
		t.Fatal("未注册场景不应查到")
	}
}

// TestCtxLimitHit 上下文上限类 400 的识别（user 多轮提前停轮依赖它）。
func TestCtxLimitHit(t *testing.T) {
	cases := []struct {
		err  string
		want string
	}{
		{"HTTP 400: This model's maximum context length is 32768 tokens", "32768"},
		{"HTTP 400: other error", ""},
		{"HTTP 500: maximum context length is 4096", ""}, // 非 400 不算
		{"", ""},
	}
	for _, c := range cases {
		m := &engine.TurnMetrics{}
		if c.err != "" {
			m.Error = c.err
		}
		if got := ctxLimitHit(m); got != c.want {
			t.Fatalf("ctxLimitHit(%q) = %q, want %q", c.err, got, c.want)
		}
	}
	if got := ctxLimitHit(nil); got != "" {
		t.Fatalf("nil metrics 应返回空: %q", got)
	}
}

// TestRunOneNilMetrics 集成：请求构造失败（nil metrics）必须收口为失败指标，不 panic。
func TestRunOneNilMetrics(t *testing.T) {
	// chat URL 非法 → NewRequest 失败 → Chat 返回 (nil, err)
	e := &env{cfg: testCfg(t, "http://127.0.0.1:1"), client: engine.NewClient("http://127.0.0.1:1", "", time.Second, false), perReqSrv: false}
	e.cfg.TimeoutSeconds = 1
	m := runOne(context.Background(), e, "m", []engine.Message{{Role: "user", Content: "x"}}, 8, config.ThinkingVariant{})
	if m == nil {
		t.Fatal("runOne 必须返回非 nil metrics（失败也是数据）")
	}
	if m.Error == "" {
		t.Fatalf("应带 Error: %+v", m)
	}
}
