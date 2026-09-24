package scenario

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/engine"
	"github.com/aleexjiang/llm-perf/internal/report"
)

const requestSetFixture = `[
  {"conversations":[
    {"from":"human","value":"q1"},
    {"from":"gpt","value":"answer one"},
    {"from":"human","value":"q2"}
  ]},
  {"conversations":[
    {"from":"human","value":"hello"},
    {"from":"gpt","value":"hi there"}
  ]},
  {"conversations":[
    {"from":"human","value":"third"},
    {"from":"gpt","value":"reply three"}
  ]}
]`

func rpsTestCfg(t *testing.T, endpoint string) *config.Config {
	t.Helper()
	cfg := testCfg(t, endpoint)
	p := filepath.Join(t.TempDir(), "sharegpt.json")
	if err := os.WriteFile(p, []byte(requestSetFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.RequestSet = config.RequestSet{ShareGPTPath: p, NumPrompts: 6, Seed: 42}
	cfg.Thinking = config.Thinking{Mode: "off"}
	return cfg
}

// TestRPSScenario 集成：开环到达 + 冻结请求快照。
// 高到达率（60/s）使 6 条请求近乎立即全部发射；断言完成数、报告结构与吞吐汇总。
func TestRPSScenario(t *testing.T) {
	srv := sseStub(t, &stubState{})
	cfg := rpsTestCfg(t, srv.URL)
	cfg.RPS = config.RPS{Rates: []float64{60}, MaxConcurrency: 0}

	rep, err := RPSScenario(context.Background(), cfg, engine.NewClient(srv.URL, "", 10*time.Second, true), "")
	if err != nil {
		t.Fatalf("RPSScenario: %v", err)
	}
	if rep.Scenario != "rps" {
		t.Fatalf("scenario = %q", rep.Scenario)
	}
	if len(rep.Concurrent) != 1 {
		t.Fatalf("应有 1 个到达率档位: %d", len(rep.Concurrent))
	}
	lv := rep.Concurrent[0]
	if lv.RequestRate != 60 || len(lv.Requests) != 6 {
		t.Fatalf("档位数据错误: rate=%v requests=%d", lv.RequestRate, len(lv.Requests))
	}
	for _, m := range lv.Requests {
		if m.Error != "" {
			t.Fatalf("请求不应失败: %q", m.Error)
		}
		if m.PromptTokens <= 0 {
			t.Fatalf("usage 缺失（stub 应回 prompt_tokens）")
		}
	}
	if lv.CompletedRequests != 6 || lv.ThroughputTPS <= 0 {
		t.Fatalf("汇总错误: completed=%d tps=%.1f", lv.CompletedRequests, lv.ThroughputTPS)
	}
	if lv.SLOTotal != 0 || lv.SLOMeet != 0 || lv.GoodputRPS != 0 || lv.GoodputTPS != 0 {
		t.Fatalf("未配置 SLO 时 goodput 应保持零值: %+v", lv)
	}
}

// TestConcurrencyScenario 集成：固定在飞齐射（request_rate=inf）+ 多档位。
func TestConcurrencyScenario(t *testing.T) {
	srv := sseStub(t, &stubState{})
	cfg := rpsTestCfg(t, srv.URL)
	cfg.Concurrency = config.ConcurrencyCfg{Levels: []int{1, 2}}

	rep, err := ConcurrencyScenario(context.Background(), cfg, engine.NewClient(srv.URL, "", 10*time.Second, true), "")
	if err != nil {
		t.Fatalf("ConcurrencyScenario: %v", err)
	}
	if rep.Scenario != "concurrency" {
		t.Fatalf("scenario = %q", rep.Scenario)
	}
	if len(rep.Concurrent) != 2 {
		t.Fatalf("应有 2 个并发档位: %d", len(rep.Concurrent))
	}
	for i, lv := range rep.Concurrent {
		if lv.Level == 0 || len(lv.Requests) != 6 {
			t.Fatalf("档位 %d 数据错误: level=%d requests=%d", i, lv.Level, len(lv.Requests))
		}
	}
}

// TestRequestScenariosSourceCheck 回归：数据契约承诺 rps/concurrency 输出 source_check
// （真机发现：applySourceCheck 已实现但场景未接线，字段在真实 vLLM JSON 中恒缺失）。
func TestRequestScenariosSourceCheck(t *testing.T) {
	for name, run := range map[string]func(*config.Config, *engine.Client) (*report.Report, error){
		"rps": func(cfg *config.Config, c *engine.Client) (*report.Report, error) {
			cfg.RPS = config.RPS{Rates: []float64{60}, MaxConcurrency: 0}
			return RPSScenario(context.Background(), cfg, c, "")
		},
		"concurrency": func(cfg *config.Config, c *engine.Client) (*report.Report, error) {
			cfg.Concurrency = config.ConcurrencyCfg{Levels: []int{1}}
			return ConcurrencyScenario(context.Background(), cfg, c, "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := sseStub(t, &stubState{})
			cfg := rpsTestCfg(t, srv.URL)
			cfg.ServerMetrics = true
			rep, err := run(cfg, engine.NewClient(srv.URL, "", 10*time.Second, true))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if rep.SourceCheck == nil {
				t.Fatalf("%s 应输出 source_check", name)
			}
			if rep.SourceCheck.ClientTokens <= 0 || rep.SourceCheck.ServerTokens <= 0 {
				t.Fatalf("%s source_check 数据不完整: %+v", name, rep.SourceCheck)
			}
		})
	}
}

// TestFinishLevelGoodput 回归：数据契约承诺的档位级 goodput 字段必须接线。
// 已配置 SLO 时失败请求计入分母且不达标，主动取消不计入分母；
// goodput_rps 是达标请求数/墙钟，goodput_tps 只累计达标请求的 completion tokens。
func TestFinishLevelGoodput(t *testing.T) {
	e := &env{cfg: &config.Config{
		SLO: &config.SLOCfg{Goodput: &config.GoodputCfg{TTFTMS: 100, TPOTMS: 50}},
	}}
	lv := report.ConcurrentLevel{
		WallSeconds: 2,
		Requests: []*engine.TurnMetrics{
			{TTFT: 80, TPOTMS: 40, CompletionTokens: 100},
			{TTFT: 120, TPOTMS: 40, CompletionTokens: 200},
			{Error: "HTTP 500", CompletionTokens: 300},
			{Cancelled: true, CompletionTokens: 400},
		},
	}
	finishLevel(e, &lv)
	if lv.SLOMeet != 1 || lv.SLOTotal != 3 {
		t.Fatalf("SLO 计数错误: meet=%d total=%d", lv.SLOMeet, lv.SLOTotal)
	}
	if lv.GoodputRPS != 0.5 || lv.GoodputTPS != 50 {
		t.Fatalf("goodput 汇总错误: rps=%v tps=%v", lv.GoodputRPS, lv.GoodputTPS)
	}
	if lv.CompletedRequests != 2 || lv.FailedRequests != 1 || lv.CancelledRequests != 1 {
		t.Fatalf("成败计数错误: completed=%d failed=%d cancelled=%d",
			lv.CompletedRequests, lv.FailedRequests, lv.CancelledRequests)
	}
}

// rps/concurrency 的档位级活跃 decode 聚合必须由场景层落盘，
// 不能只在报告脚本里临时重算。
func TestFinishLevelActiveDecodeTPS(t *testing.T) {
	e := &env{cfg: &config.Config{}}
	base := time.Now()
	lv := report.ConcurrentLevel{
		WallSeconds: 3,
		Requests: []*engine.TurnMetrics{
			{Stream: true, SentAt: base, TTFT: 100, E2EMS: 2100, EndAt: base.Add(2100 * time.Millisecond), CompletionTokens: 100},
			{Stream: true, SentAt: base.Add(500 * time.Millisecond), TTFT: 100, E2EMS: 1600, EndAt: base.Add(2100 * time.Millisecond), CompletionTokens: 100},
		},
	}
	finishLevel(e, &lv)
	if lv.ActiveDecodeTPS <= 0 {
		t.Fatalf("active_decode_tps 未写入: %+v", lv)
	}
	if lv.ActiveDecodeTPS <= lv.ThroughputTPS {
		t.Fatalf("重叠 decode 的活跃聚合应高于 batch 平均: active=%.1f batch=%.1f", lv.ActiveDecodeTPS, lv.ThroughputTPS)
	}
}

// TestBarrierOpenLoopRateIndependentOfLatency 开环到达率与处理耗时解耦（review H3 回归）：
// 旧实现 worker 处理完上一请求才按间隔 sleep——处理慢于到达间隔时实际发射率被拉长，
// 变成"带节奏的闭环"，测不到过载排队。新实现由独立发射时钟按泊松过程注入。
// 构造：4 请求、rate=100/s（间隔 10ms）、每请求处理 300ms——
// 解耦后 wall ≈ 首请求处理 300ms + 少量发射窗口；耦合实现 wall ≈ 4×300ms = 1.2s。
func TestBarrierOpenLoopRateIndependentOfLatency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\n")
	}))
	defer srv.Close()

	cfg := testCfg(t, srv.URL)
	cfg.RequestSet = config.RequestSet{NumPrompts: 4, Seed: 42}
	cfg.Thinking = config.Thinking{Mode: "off"}
	e := &env{cfg: cfg, client: engine.NewClient(srv.URL, "", 10*time.Second, true), perReqSrv: false}
	em, _ := forModel(e, cfg, "stub-model")

	samples := make([]engine.RequestSample, 4)
	for i := range samples {
		samples[i] = engine.RequestSample{OutputTokens: 8}
	}
	start := time.Now()
	lv := runRequestBarrier(context.Background(), em, "stub-model",
		config.ThinkingVariant{Name: "off"}, samples, 4, 100, 1)
	wall := time.Since(start)

	if len(lv.Requests) != 4 {
		t.Fatalf("应完成 4 个请求，实际 %d", len(lv.Requests))
	}
	if wall > 800*time.Millisecond {
		t.Fatalf("开环 wall=%v > 800ms——发射时钟仍与处理耗时耦合（旧实现 ≈1.2s）", wall)
	}
}
