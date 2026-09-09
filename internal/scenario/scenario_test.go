package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/engine"
	"github.com/aleexjiang/llm-perf/internal/report"
)

// ── 种子派生语义（表驱动锁定：测试盐值隔离、fixed_seed 档内复用/档间独立、worker 互异） ──

func TestSingleSeed(t *testing.T) {
	cases := []struct {
		name              string
		fixed             bool
		tokens, run, salt int
		want              int64
	}{
		{"fixed 同档同 run 复用", true, 4096, 0, 0, 1000 + 4096},
		{"fixed 同档不同 run 复用", true, 4096, 2, 0, 1000 + 4096},
		{"fixed 不同档位独立（去嵌套）", true, 20480, 0, 0, 1000 + 20480},
		{"盐值改变种子", true, 4096, 0, 7, 1000 + 4096 + 7},
		{"非 fixed 档内 run 互异", false, 4096, 0, 0, 4096 * 100},
		{"非 fixed run1 互异", false, 4096, 1, 0, 4096*100 + 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := singleSeed(c.fixed, c.tokens, c.run, c.salt); got != c.want {
				t.Fatalf("singleSeed = %d, want %d", got, c.want)
			}
		})
	}
	// 关键不变量：fixed_seed 下不同档位种子必须互异（嵌套前缀回归测试）
	if singleSeed(true, 4096, 0, 0) == singleSeed(true, 20480, 0, 0) {
		t.Fatal("不同档位的 fixed_seed 种子相同——嵌套前缀缺陷回归")
	}
	// 关键不变量：盐值隔离测试——任意组合下 salt=0 与 salt=1 种子不同
	if singleSeed(true, 4096, 1, 0) == singleSeed(true, 4096, 1, 1) {
		t.Fatal("盐值未生效——测试隔离失效")
	}
}

func TestSessionSeed(t *testing.T) {
	if sessionSeed(0, 0) == sessionSeed(1, 0) {
		t.Fatal("不同会话种子相同")
	}
	if sessionSeed(0, 0) == sessionSeed(0, 1) {
		t.Fatal("盐值未隔离会话")
	}
}

func TestWorkerSeed(t *testing.T) {
	if workerSeed(0, 0, 0) == workerSeed(1, 0, 0) {
		t.Fatal("不同 worker 种子相同")
	}
	if workerSeed(0, 0, 0) == workerSeed(0, 1, 0) {
		t.Fatal("同 worker 不同 run 种子相同")
	}
	if openWorkerSeed(0, 0) == openWorkerSeed(1, 0) {
		t.Fatal("开环 worker 种子相同")
	}
}

// ── 纯函数：上下文截止 / 开环到达率 ──

func TestNextTurnTokens(t *testing.T) {
	cases := []struct {
		name                string
		maxPrompt, last, tt int
		want                int
	}{
		{"无截止原样返回", 0, 50000, 2000, 2000},
		{"未到上限", 20000, 5000, 2000, 2000},
		{"超限截断", 20000, 19000, 2000, 1000},
		{"剩余不足最小 turn 停轮", 20000, 19900, 2000, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{MaxPromptTokens: c.maxPrompt}
			if got := nextTurnTokens(cfg, c.tt, c.last); got != c.want {
				t.Fatalf("nextTurnTokens = %d, want %d", got, c.want)
			}
		})
	}
}

func TestOpenRates(t *testing.T) {
	if got := openRates(config.Concurrent{}); got != nil {
		t.Fatalf("无配置应返回 nil，得到 %v", got)
	}
	if got := openRates(config.Concurrent{RequestRate: 4}); len(got) != 1 || got[0] != 4 {
		t.Fatalf("request_rate 应生成单档 %v", got)
	}
	if got := openRates(config.Concurrent{RequestRate: 4, RateSweep: []float64{1, 2}}); len(got) != 2 {
		t.Fatalf("rate_sweep 应优先 %v", got)
	}
}

// ── finalizeLevel：goodput 语义 ──

func TestFinalizeLevelGoodput(t *testing.T) {
	e := &env{cfg: &config.Config{Goodput: &config.GoodputCfg{TTFTMS: 2000, TPOTMS: 200}}}
	lv := &report.ConcurrentLevel{}
	lv.Requests = []*engine.TurnMetrics{
		{TTFT: 1000, TPOTMS: 100, CompletionTokens: 100}, // 达标
		{TTFT: 5000, TPOTMS: 100, CompletionTokens: 50},  // TTFT 超标
		{TTFT: 1000, TPOTMS: 500, CompletionTokens: 50},  // TPOT 超标
	}
	finalizeLevel(e, lv, 10) // wall 10s

	if lv.SLOTotal != 3 || lv.SLOMeet != 1 {
		t.Fatalf("goodput 计数错误: meet=%d total=%d", lv.SLOMeet, lv.SLOTotal)
	}
	if lv.GoodputRPS != 0.1 {
		t.Fatalf("GoodputRPS = %v, want 0.1", lv.GoodputRPS)
	}
	if lv.GoodputTPS != 10 {
		t.Fatalf("GoodputTPS = %v, want 10", lv.GoodputTPS)
	}
	if lv.ThroughputTPS != 20 { // (100+50+50)/10
		t.Fatalf("ThroughputTPS = %v, want 20", lv.ThroughputTPS)
	}

	// 无 SLO 配置：不计 goodput
	e2 := &env{cfg: &config.Config{}}
	lv2 := &report.ConcurrentLevel{Requests: []*engine.TurnMetrics{{CompletionTokens: 10}}}
	finalizeLevel(e2, lv2, 1)
	if lv2.SLOTotal != 0 || lv2.GoodputRPS != 0 {
		t.Fatalf("无 SLO 时不应产生 goodput: %+v", lv2)
	}
}

// ── httptest 集成：SSE 桩服务 ──

type stubState struct {
	mu       sync.Mutex
	count    int
	failReq  map[int]bool // 第 N 个请求（1-based）强制断连
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
		FillerLang:     "en",
		Thinking:       config.Thinking{Mode: "off", MaxTokensFloor: 512},
		Models:         []string{"stub-model"},
	}
}

// TestMultiturnFailureResume 集成：中间一轮连接中断后，会话继续，
// 失败轮不推进 lastPrompt 基准（NewTokens 语义不被污染），keep_assistant 正常拼装 history。
func TestMultiturnFailureResume(t *testing.T) {
	state := &stubState{failReq: map[int]bool{2: true}}
	srv := sseStub(t, state)

	cfg := testCfg(t, srv.URL)
	cfg.Multiturn = config.Multiturn{Sessions: 1, Turns: 3, TurnTokens: 100, MaxTokens: config.IntList{16}, KeepAssistant: true}

	rep, err := Multiturn(context.Background(), cfg, engine.NewClient(srv.URL, "", 10*time.Second, true), "")
	if err != nil {
		t.Fatalf("Multiturn: %v", err)
	}
	turns := rep.Multiturn[0].Turns
	if len(turns) != 3 {
		t.Fatalf("应有 3 个 turn，得到 %d", len(turns))
	}
	// 失败的 turn2：无 usage，无 NewTokens
	if turns[1].PromptTokens != 0 || turns[1].Error == "" {
		t.Fatalf("turn2 应为失败轮: prompt=%d err=%q", turns[1].PromptTokens, turns[1].Error)
	}
	if turns[1].NewTokens != 0 {
		t.Fatalf("失败轮不应有 NewTokens: %d", turns[1].NewTokens)
	}
	// 修复点：turn3 的 NewTokens 相对上一成功轮（turn1），而不是清零后等于整个 ctx
	want := turns[2].PromptTokens - turns[0].PromptTokens
	if turns[2].NewTokens != want {
		t.Fatalf("turn3.NewTokens = %d, want %d（失败轮不应清零 lastPrompt 基准）",
			turns[2].NewTokens, want)
	}
	// history 拼装：turn2 请求 [u1, a1, u2]；turn3 请求 [u1, a1, u2, u3]（失败轮无 assistant）
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.bodies) != 3 {
		t.Fatalf("应发出 3 个请求，得到 %d", len(state.bodies))
	}
	if state.bodies[1] != 3 || state.bodies[2] != 4 {
		t.Fatalf("keep_assistant 拼装错误: 各请求消息数 %v", state.bodies)
	}
}

// TestConcurrentClosedBasic 集成：闭环并发基础流（level=2 × runs=1）。
func TestConcurrentClosedBasic(t *testing.T) {
	state := &stubState{}
	srv := sseStub(t, state)

	cfg := testCfg(t, srv.URL)
	cfg.Concurrent = config.Concurrent{Levels: []int{2}, RunsPerWorker: 1, PromptTokens: 100, MaxTokens: config.IntList{16}}

	rep, err := Concurrent(context.Background(), cfg, engine.NewClient(srv.URL, "", 10*time.Second, true), "")
	if err != nil {
		t.Fatalf("Concurrent: %v", err)
	}
	lv := rep.Concurrent[0]
	if len(lv.Requests) != 2 {
		t.Fatalf("应有 2 条请求，得到 %d", len(lv.Requests))
	}
	if lv.WallSeconds <= 0 || lv.ThroughputTPS <= 0 {
		t.Fatalf("wall/吞吐未计算: %+v", lv)
	}
	// 两个 worker 的 prompt 内容互异（workerSeed）
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.contents[0] == state.contents[1] {
		t.Fatal("两个 worker 的 prompt 内容相同——workerSeed 互异语义失效")
	}
}

// TestSingleCacheSemantics 集成：fixed_seed 下同档 run 复用 prompt（缓存对照有效）。
func TestSingleCacheSemantics(t *testing.T) {
	state := &stubState{}
	srv := sseStub(t, state)

	cfg := testCfg(t, srv.URL)
	cfg.Single = config.Single{Runs: 2, PromptTokens: []int{200}, MaxTokens: config.IntList{16}, FixedSeed: true}

	if _, err := Single(context.Background(), cfg, engine.NewClient(srv.URL, "", 10*time.Second, true), ""); err != nil {
		t.Fatalf("Single: %v", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.contents) != 2 {
		t.Fatalf("应发出 2 条请求，得到 %d", len(state.contents))
	}
	if state.contents[0] != state.contents[1] {
		t.Fatal("fixed_seed 下同档各 run 的 prompt 应完全相同")
	}
	if !strings.HasPrefix(state.contents[0], "") {
		t.Fatal("内容读取失败")
	}
}

// ── Scenario 注册表：新增场景实现接口 + Register 即可，main 零改动 ──

func TestScenarioRegistry(t *testing.T) {
	for _, name := range []string{"single", "multiturn", "concurrent"} {
		if _, ok := Lookup(name); !ok {
			t.Fatalf("场景 %s 应已注册", name)
		}
	}
	if _, ok := Lookup("nope"); ok {
		t.Fatal("未注册场景不应查到")
	}
}

func TestCtxLimitHit(t *testing.T) {
	cases := []struct {
		name string
		err  string
		want string
	}{
		{"vLLM 超限", `HTTP 400: {"error":"This model's maximum context length is 131072 tokens. However, you requested 200000 tokens."}`, "131072"},
		{"400 但与上下文无关", `HTTP 400: {"error":"invalid parameter"}`, ""},
		{"非 400", `HTTP 500: internal error`, ""},
		{"400 提到上下文但无数字", `HTTP 400: context length exceeded`, "未知"},
		{"无错误", "", ""},
	}
	for _, c := range cases {
		m := &engine.TurnMetrics{Error: c.err}
		if got := ctxLimitHit(m); got != c.want {
			t.Errorf("%s: ctxLimitHit=%q want %q", c.name, got, c.want)
		}
	}
}

// ── 对抗式审查回归：形状中位剔除失败请求（与报告侧口径一致） ──

func TestAggregateShapesExcludesFailed(t *testing.T) {
	mp := &mixPlan{
		shapes:  []config.MixShape{{Weight: 2, Label: "a", PromptTokens: 1000, MaxTokens: 32}},
		maxToks: []int{32},
		seq:     []int{0, 0},
	}
	reqs := []*engine.TurnMetrics{
		{TTFT: 1000, E2EMS: 2000, TokensPerSec: 50, CompletionTokens: 100},
		{TTFT: 0, E2EMS: 500, TokensPerSec: 0, CompletionTokens: 0, Error: "HTTP 504: gateway timeout"},
	}
	out := aggregateShapes(mp, reqs, []int{0, 0})
	if len(out) != 1 {
		t.Fatalf("应聚出 1 个形状, got %d", len(out))
	}
	sh := out[0]
	if sh.Count != 2 {
		t.Fatalf("Count 应反映请求总数 2, got %d", sh.Count)
	}
	if sh.TTFTS != 1.0 || sh.E2ES != 2.0 || sh.TokPS != 50 {
		t.Fatalf("中位应只用成功请求: ttft=%v e2e=%v tokps=%v", sh.TTFTS, sh.E2ES, sh.TokPS)
	}
	// 全失败形状：中位为 0（无意义），Count 仍如实
	allFailed := aggregateShapes(mp, []*engine.TurnMetrics{
		{Error: "x"}, {Error: "y"},
	}, []int{0, 0})
	if allFailed[0].Count != 2 || allFailed[0].TTFTS != 0 {
		t.Fatalf("全失败形状应 Count=2 且中位 0: %+v", allFailed[0])
	}
}
