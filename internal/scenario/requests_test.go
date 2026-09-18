package scenario

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/engine"
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
