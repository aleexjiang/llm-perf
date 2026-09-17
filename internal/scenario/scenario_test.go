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
	"github.com/aleexjiang/llm-perf/internal/smetrics"
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

// 10.3 基座共享（multiturn.shared_base，默认 true）的种子语义。
// 共享的是**基座**（system + tool defs），逐轮 user 内容必须仍然按会话独立——
// 否则会话之间逐字节相同，跨会话统计与 per-session 失败隔离都失去意义。
func TestSessionSeedsSharedBase(t *testing.T) {
	newCfg := func(shared *bool) *config.Config {
		c := &config.Config{}
		c.Multiturn.SharedBase = shared
		return c
	}
	no := false
	sharedDefault := newCfg(nil) // 未配置 = 默认共享
	if !sharedDefault.Multiturn.GetSharedBase() {
		t.Fatal("shared_base 未配置时应默认 true（2026-09-12 拍板）")
	}
	if newCfg(&no).Multiturn.GetSharedBase() {
		t.Fatal("显式 shared_base=false 未生效")
	}

	// 默认（共享）：基座种子与会话无关；逐轮种子仍随会话变化
	b0, t0 := sessionSeeds(sharedDefault, 0)
	b1, t1 := sessionSeeds(sharedDefault, 1)
	if b0 != b1 {
		t.Fatalf("共享基座下基座种子应与会话无关：session0=%d session1=%d", b0, b1)
	}
	if t0 == t1 {
		t.Fatal("共享基座不应让逐轮内容跨会话相同（会话将逐字节重复）")
	}
	// 换盐必须换基座：否则重跑命中的是上一轮的前缀缓存，冷启动测量被污染
	saltCfg := newCfg(nil)
	saltCfg.SeedSalt = 1
	if bSalt, _ := sessionSeeds(saltCfg, 0); b0 == bSalt {
		t.Fatal("盐值未生效——换盐后基座内容未变，重跑会命中上一轮缓存")
	}

	// 独立形态：基座种子随会话互异
	i0, _ := sessionSeeds(newCfg(&no), 0)
	i1, _ := sessionSeeds(newCfg(&no), 1)
	if i0 == i1 {
		t.Fatal("shared_base=false 下基座种子应随会话互异")
	}
	// 两形态的基座内容必须不同：否则对照实验里独立形态的 session0 会被共享形态的
	// 上一轮缓存提前热身
	if i0 == b0 {
		t.Fatal("共享基座种子与独立形态 session0 相同——两形态对照会被前缀缓存污染")
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
	// 回归点：stride 曾为 100，runs_per_worker ≥ 100 时相邻 worker 撞种子（相同 prompt）
	seen := map[int64]bool{}
	for w := 0; w < 8; w++ {
		for r := 0; r < 200; r++ {
			s := workerSeed(w, r, 0)
			if seen[s] {
				t.Fatalf("worker%d run%d 种子 %d 与先前组合碰撞", w, r, s)
			}
			seen[s] = true
		}
	}
}

// ── 纯函数：上下文截止 / 开环到达率 ──

// 回归点：引擎不回 usage 时 lastPrompt 恒 0，曾导致 max_prompt_tokens 截止永不触发、
// 上下文无界增长。生成侧估算（estPrompt）必须参与截止判断。
func TestUsageMissingPromptCutoff(t *testing.T) {
	cfg := &config.Config{MaxPromptTokens: 20000}
	last, est := 0, 0
	// 模拟 usage 缺失：每轮追加 10000 tokens，第 3 轮应因估算基线达到上限而停轮
	tt := nextTurnTokens(cfg, 10000, effPromptOf(last, est))
	est += tt
	if tt != 10000 {
		t.Fatalf("第 1 轮应全额 10000，实际 %d", tt)
	}
	tt = nextTurnTokens(cfg, 10000, effPromptOf(last, est))
	est += tt
	if tt != 10000 { // 剩余 15000，截断为 10000
		t.Fatalf("第 2 轮应仍为 10000，实际 %d", tt)
	}
	tt = nextTurnTokens(cfg, 10000, effPromptOf(last, est))
	if tt != 0 {
		t.Fatalf("估算基线 %d 已达上限，第 3 轮应停轮（=0），实际 %d", est, tt)
	}
	// usage 中途恢复：估算基线对齐实测（实测可能因模板开销大于生成侧估算）
	last, est = 30000, 22000
	if got := effPromptOf(last, est); got != 30000 {
		t.Fatalf("usage 恢复后应优先实测值，实际 %d", got)
	}
	if got := effPromptOf(0, 22000); got != 22000 {
		t.Fatalf("usage 缺失时应回退估算值，实际 %d", got)
	}
}

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
	e := &env{cfg: &config.Config{SLO: &config.SLOCfg{Goodput: &config.GoodputCfg{TTFTMS: 2000, TPOTMS: 200}}}}
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

func TestFinalizeLevelSeparatesFailedAndCancelled(t *testing.T) {
	e := &env{cfg: &config.Config{SLO: &config.SLOCfg{Goodput: &config.GoodputCfg{TTFTMS: 2000}}}}
	lv := &report.ConcurrentLevel{Requests: []*engine.TurnMetrics{
		{CompletionTokens: 100, TTFT: 1000},
		{CompletionTokens: 80, Error: "HTTP 500"},
		{CompletionTokens: 60, Error: "context canceled", Cancelled: true},
	}}
	finalizeLevel(e, lv, 10)
	if lv.CompletedRequests != 1 || lv.FailedRequests != 1 || lv.CancelledRequests != 1 {
		t.Fatalf("请求分类错误: %+v", lv)
	}
	if lv.ThroughputTPS != 10 || lv.SLOTotal != 2 {
		t.Fatalf("失败/取消不应污染主吞吐或 SLO 分母: %+v", lv)
	}
}

// ── httptest 集成：SSE 桩服务 ──

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

// TestConcurrentMultiturnNewTokensClamp 集成：并发多轮（collectSessionTurns）路径
// 下 tokenizer 重切分致服务端 prompt_tokens 回退（turn2 300 < turn1 1000）时，
// NewTokens 必须钳 0 且不污染后续轮基准——与单场景 Multiturn 修复同语义（scenario.go 602/709 两路径对称）。
func TestConcurrentMultiturnNewTokensClamp(t *testing.T) {
	state := &stubState{
		promptOv: map[int]int{1: 1000, 2: 300, 3: 1500}, // turn2 刻意回退：模拟 tokenizer 对累积 history 重切分
	}
	srv := sseStub(t, state)

	cfg := testCfg(t, srv.URL)
	cfg.Multiturn = config.Multiturn{Sessions: 1, Turns: 3, TurnTokens: 100, MaxTokens: config.IntList{16}, KeepAssistant: true}
	cfg.Concurrent = config.Concurrent{Levels: []int{1}, Multiturn: true, RunsPerWorker: 1, MaxTokens: config.IntList{16}}

	rep, err := Concurrent(context.Background(), cfg, engine.NewClient(srv.URL, "", 10*time.Second, true), "")
	if err != nil {
		t.Fatalf("Concurrent: %v", err)
	}
	sess := rep.Concurrent[0].Sessions
	if len(sess) != 1 || len(sess[0].Turns) != 3 {
		t.Fatalf("应有 1 会话 × 3 轮，得到 %d 会话 × %d 轮", len(sess), len(sess[0].Turns))
	}
	t1, t2, t3 := sess[0].Turns[0], sess[0].Turns[1], sess[0].Turns[2]
	if t1.NewTokens != 1000 {
		t.Fatalf("turn1.NewTokens = %d, want 1000（首轮=全量）", t1.NewTokens)
	}
	if t2.NewTokens != 0 {
		t.Fatalf("turn2.NewTokens = %d, want 0（prompt 回退 1000→300 必须钳 0，否则负值污染增量 prefill 斜率）", t2.NewTokens)
	}
	// turn3 相对 turn2 的实际值（300）计增量，而不是被回退清成整个 ctx
	if want := t3.PromptTokens - t2.PromptTokens; t3.NewTokens != want {
		t.Fatalf("turn3.NewTokens = %d, want %d（基准应推进到回退后的 300，而非清零）", t3.NewTokens, want)
	}
	if t3.NewTokens <= 0 {
		t.Fatalf("turn3 正常增长不应被误钳: %d", t3.NewTokens)
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
	for _, name := range []string{"multiturn", "concurrent"} {
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

// ── 主流口径对齐回归：goodput 只判定"已配置的"SLO 子集（vLLM 语义）──
// 修复前：goodputOf 无条件检查 TPOT>0 与 TTFT>阈值，只配 ttft_ms（或只配 tpot_ms）
// 时所有请求都被判不达标——配置校验只拦两项均为 0，单配一项是合法用法。

func TestGoodputSLOSubset(t *testing.T) {
	// 只配 TTFT：TPOT 缺失（如非流式/短输出）不应拖累判定
	eTTFT := &env{cfg: &config.Config{SLO: &config.SLOCfg{Goodput: &config.GoodputCfg{TTFTMS: 2000}}}}
	if !goodputOf(eTTFT, &engine.TurnMetrics{Stream: true, TTFT: 1500}) {
		t.Error("只配 TTFT 阈值时，TTFT 达标即应判达标（TPOT 未配置不参与）")
	}
	if goodputOf(eTTFT, &engine.TurnMetrics{Stream: true, TTFT: 3000}) {
		t.Error("TTFT 超标应不达标")
	}
	if goodputOf(eTTFT, &engine.TurnMetrics{TTFT: 0}) { // 非流式 TTFT 不可测（N/A）
		t.Error("非流式 TTFT 不可测，不应凭 0 值白拿达标")
	}

	// 只配 TPOT：TTFT 不参与判定
	eTPOT := &env{cfg: &config.Config{SLO: &config.SLOCfg{Goodput: &config.GoodputCfg{TPOTMS: 200}}}}
	if !goodputOf(eTPOT, &engine.TurnMetrics{Stream: true, TPOTMS: 150}) {
		t.Error("只配 TPOT 阈值时，TPOT 达标即应判达标")
	}
	if goodputOf(eTPOT, &engine.TurnMetrics{Stream: true, TPOTMS: 0}) {
		t.Error("TPOT 不可测（0 值）应不达标")
	}

	// 双约束：任一超标即不达标（原有语义保持）
	eBoth := &env{cfg: &config.Config{SLO: &config.SLOCfg{Goodput: &config.GoodputCfg{TTFTMS: 2000, TPOTMS: 200}}}}
	if goodputOf(eBoth, &engine.TurnMetrics{Stream: true, TTFT: 1000, TPOTMS: 500}) {
		t.Error("TPOT 超标应不达标")
	}
}

// ── 服务端观测（/metrics）语义：可选第二数据源，缺失或失败一律不得影响结论 ──

const metricsFixture = `# HELP vllm:prefix_cache_hits_total prefix cache hits
# TYPE vllm:prefix_cache_hits_total counter
vllm:prefix_cache_hits_total{engine="0"} 11708800
vllm:prefix_cache_queries_total{engine="0"} 16188982
vllm:num_preemptions_total{engine="0"} 3
vllm:num_requests_running{engine="0"} 2
`

// metricsStub 只提供 /metrics 的最小服务端（chat 侧不参与本组用例）。
func metricsStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, metricsFixture)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 观测层关闭（server_metrics=false，或端点探不到）时窗口汇总必须是 nil——
// 报告据此走「未提供 /metrics」分支，而不是渲染一个全零面板。
func TestFinishWindowNilWhenObservationOff(t *testing.T) {
	e := &env{cfg: testCfg(t, "http://127.0.0.1:1"), perReqSrv: true} // srv == nil = 观测关闭
	if got := finishWindow(e, nil, nil, time.Time{}); got != nil {
		t.Fatalf("观测层关闭时应返回 nil，实际: %+v", got)
	}
}

// 12.11：结束快照用独立 ctx——场景 ctx 被取消（SIGHUP/Ctrl+C 或场景控制停止）
// 时快照仍须抓取成功。中断场景的窗口差值是唯一的服务端数据来源；旧行为（取消传播导致
// 抓取失败）会把 queue/prefill/decode 分解与 preemptions 差值整段丢掉（r1-off S5 实测）。
func TestFinishWindowSnapshotSurvivesCancelledCtx(t *testing.T) {
	stub := metricsStub(t)
	e := &env{cfg: testCfg(t, stub.URL), perReqSrv: true}
	e.srv = smetrics.NewScraperAt(stub.URL, "/metrics")
	e.cfg.MetricsPath = "/metrics"

	live, cancel := context.WithCancel(context.Background())
	defer cancel()
	before, err := e.srv.Scrape(live)
	if err != nil || before == nil {
		t.Fatalf("前置快照应抓取成功: %v", err)
	}
	e.provider = smetrics.DetectProvider(before)
	winStart := time.Now()

	pctx, pcancel := context.WithCancel(context.Background())
	poller := smetrics.StartGaugePoller(pctx, e.srv, 5*time.Millisecond, e.provider)
	pcancel()
	cancel() // 场景被取消（中断）：结束快照不得受其波及

	sum := finishWindow(e, before, poller, winStart)
	if sum == nil {
		t.Fatal("应返回汇总而不是 nil")
	}
	if !sum.Available {
		t.Fatalf("12.11：场景 ctx 已取消时结束快照仍应抓取成功（note=%q）", sum.Note)
	}
	if sum.Note != "" {
		t.Errorf("快照成功不应带失败 note，实际 %q", sum.Note)
	}
}

// 结束快照真失败（服务端不可达）时：Available 必须为 false 且保留原因。
// 回归背景：旧实现写 available=true + 全零计数器，报告渲染出一句「服务端观测（single）：。」。
// 窗口内已轮询到的 gauges 独立于结束快照，应照常挂回。
func TestFinishWindowNoWindowDeltaOnFailedSnapshot(t *testing.T) {
	stub := metricsStub(t)
	e := &env{cfg: testCfg(t, stub.URL), perReqSrv: true}
	e.srv = smetrics.NewScraperAt(stub.URL, "/metrics")
	e.cfg.MetricsPath = "/metrics"

	before, err := e.srv.Scrape(context.Background())
	if err != nil || before == nil {
		t.Fatalf("前置快照应抓取成功: %v", err)
	}
	e.provider = smetrics.DetectProvider(before)
	winStart := time.Now()

	pctx, pcancel := context.WithCancel(context.Background())
	poller := smetrics.StartGaugePoller(pctx, e.srv, 5*time.Millisecond, e.provider)
	deadline := time.Now().Add(3 * time.Second)
	for poller.Health().Samples == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	gotSamples := poller.Health().Samples > 0
	pcancel()
	stub.Close() // 服务端下线 → 结束快照必然失败

	sum := finishWindow(e, before, poller, winStart)
	if sum == nil {
		t.Fatal("应返回带失败原因的汇总而不是 nil——nil 会被报告误判成「未启用 /metrics」")
	}
	if sum.Available {
		t.Error("结束快照失败时 Available 必须为 false；否则报告会把全零计数器当成已采集数据")
	}
	if sum.Note == "" {
		t.Error("必须保留失败原因（note），供报告如实说明「已启用但未取到窗口差值」")
	}
	if gotSamples && len(sum.Gauges) == 0 {
		t.Error("窗口内已轮询到的 gauges 与结束快照无关，应照常挂回")
	}
}

// 12.3：名义末轮深度 = (system + tool_defs + turns×turn_tokens) × 1.07 模板开销。
// trace 模式无名义口径（0）；参数非法（turns/turn_tokens 非正）也返回 0。
func TestNominalLastPrompt(t *testing.T) {
	mt := config.Multiturn{SystemTokens: 1000, ToolDefsTokens: 500, Turns: 8, TurnTokens: 4000}
	// (1000+500+8×4000)×1.07 = 33500×1.07 = 35845
	if got := nominalLastPrompt(false, mt); got != 35845 {
		t.Fatalf("名义末轮应为 35845，got %d", got)
	}
	if got := nominalLastPrompt(true, mt); got != 0 {
		t.Fatalf("trace 模式应为 0，got %d", got)
	}
	if got := nominalLastPrompt(false, config.Multiturn{Turns: 0, TurnTokens: 4000}); got != 0 {
		t.Fatalf("turns=0 应为 0，got %d", got)
	}
	if got := nominalLastPrompt(false, config.Multiturn{Turns: 8}); got != 0 {
		t.Fatalf("turn_tokens=0 应为 0，got %d", got)
	}
	// 5.11 混合档：名义末轮按会话所属档位（轮数/增量）计算
	mtP := config.Multiturn{SystemTokens: 1000, ToolDefsTokens: 500, Turns: 8, TurnTokens: 4000,
		Profiles: []config.MixProfile{{Weight: 1, TurnTokens: 1000, Turns: 2}}}
	// (1000+500+2×1000)×1.07 = 3745
	if got := nominalLastPromptAt(false, mtP, 0); got != 3745 {
		t.Fatalf("混合档名义末轮应为 3745，got %d", got)
	}
}

// FirstMatchedModel：与场景侧 filterModels 同一 -m 子串语义（恢复探针取首个匹配）。
// 回归背景：恢复探针曾用精确相等 m == filter——`-m mock` 命中 mock-model-a 时探针恒空，
// 熔断冷却后误判「服务端未恢复」并停掉剩余场景。
func TestFirstMatchedModel(t *testing.T) {
	models := []string{"mock-model-a", "mock-model-b"}
	if got := FirstMatchedModel(models, ""); got != "mock-model-a" {
		t.Fatalf("空 filter 应取首个: %q", got)
	}
	if got := FirstMatchedModel(models, "model-b"); got != "mock-model-b" {
		t.Fatalf("子串匹配应取命中: %q", got)
	}
	if got := FirstMatchedModel(models, "mock"); got != "mock-model-a" {
		t.Fatalf("宽子串应取首个命中: %q", got)
	}
	if got := FirstMatchedModel(models, "nope"); got != "" {
		t.Fatalf("无匹配应返回空串: %q", got)
	}
	if got := FirstMatchedModel(nil, ""); got != "" {
		t.Fatalf("空列表应返回空串: %q", got)
	}
}

// ── 5.11 混合档（multiturn.profiles）：SWRR 分配 / 分档求和 / 并发分档执行 ──

// profileAt 平滑加权轮转（同 nginx smooth）：3:1 按周期 [A,A,B,A] 循环，
// 一周期内精确覆盖权重比——确定性分配（同配置重跑一致，不引入随机）。
func TestProfileAt(t *testing.T) {
	mt := config.Multiturn{Profiles: []config.MixProfile{
		{Weight: 3, Name: "real", TurnTokens: 5000, Turns: 32},
		{Weight: 1, Name: "heavy", TurnTokens: 27500, Turns: 8},
	}}
	want := []string{"real", "real", "heavy", "real", "real", "real", "heavy", "real"}
	for i, w := range want {
		p, ok := profileAt(mt, i)
		if !ok || p.Name != w {
			t.Fatalf("idx %d = %q(ok=%v), want %q", i, p.Name, ok, w)
		}
	}
	cnt := map[string]int{}
	for i := 0; i < 4; i++ { // 一周期内精确 3:1
		p, _ := profileAt(mt, i)
		cnt[p.Name]++
	}
	if cnt["real"] != 3 || cnt["heavy"] != 1 {
		t.Fatalf("一周期分布应 3:1，got %v", cnt)
	}
	if _, ok := profileAt(config.Multiturn{}, 0); ok {
		t.Fatal("无 profiles 应 ok=false")
	}
}

// effectiveTurnsSum：混合档逐会话求和（前 n 个会话与发车序号一一对应；均匀档等价 n×turns）。
func TestEffectiveTurnsSum(t *testing.T) {
	mc := &config.Config{Multiturn: config.Multiturn{Profiles: []config.MixProfile{
		{Weight: 2, Name: "a", TurnTokens: 5000, Turns: 4},
		{Weight: 1, Name: "b", TurnTokens: 5000, Turns: 2},
	}}}
	// 权重 2:1 前 3 会话 = a,b,a → 4+2+4 = 10
	if got := effectiveTurnsSum(mc, 3); got != 10 {
		t.Fatalf("混合档求和 = %d, want 10", got)
	}
	plain := &config.Config{Multiturn: config.Multiturn{Turns: 3, TurnTokens: 5000}}
	if got := effectiveTurnsSum(plain, 5); got != 15 {
		t.Fatalf("均匀档求和 = %d, want 15", got)
	}
}

// TestConcurrentMultiturnProfiles 集成：并发多轮混合档——会话按所属档位决定轮数，
// MultiturnRun 带档位标签（报告侧按档分组切体验的数据源）。
func TestConcurrentMultiturnProfiles(t *testing.T) {
	state := &stubState{}
	srv := sseStub(t, state)

	cfg := testCfg(t, srv.URL)
	cfg.Multiturn = config.Multiturn{
		Sessions: 2, Turns: 5, TurnTokens: 5000, MaxTokens: config.IntList{16}, KeepAssistant: true,
		Profiles: []config.MixProfile{
			{Weight: 1, Name: "light", TurnTokens: 100, Turns: 3},
			{Weight: 1, Name: "heavy", TurnTokens: 200, Turns: 1},
		},
	}
	cfg.Concurrent = config.Concurrent{Levels: []int{2}, Multiturn: true, RunsPerWorker: 1, MaxTokens: config.IntList{16}}

	rep, err := Concurrent(context.Background(), cfg, engine.NewClient(srv.URL, "", 10*time.Second, true), "")
	if err != nil {
		t.Fatalf("Concurrent: %v", err)
	}
	sess := rep.Concurrent[0].Sessions
	if len(sess) != 2 {
		t.Fatalf("应有 2 会话（level×1 runs），得到 %d", len(sess))
	}
	byIdx := map[int]report.MultiturnRun{}
	for _, s := range sess {
		byIdx[s.Session] = s
	}
	// 权重 1:1 → 会话序号 0/1 分属 light/heavy（SWRR 交替）
	light, heavy := byIdx[1], byIdx[2]
	if light.Profile != "light" || heavy.Profile != "heavy" {
		t.Fatalf("档位标签：%q / %q, want light/heavy", light.Profile, heavy.Profile)
	}
	if len(light.Turns) != 3 || len(heavy.Turns) != 1 {
		t.Fatalf("逐档轮数：light=%d heavy=%d, want 3/1", len(light.Turns), len(heavy.Turns))
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.count != 4 { // 3 + 1 轮：档位轮数直接决定发车量
		t.Fatalf("总请求 = %d, want 4", state.count)
	}
}
