package smetrics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aleexjiang/llm-perf/internal/auth"
)

const sampleText = `# HELP vllm:prefix_cache_hits_total prefix cache hits
# TYPE vllm:prefix_cache_hits_total counter
vllm:prefix_cache_hits_total{engine="0",model_name="qwen3.8-27b"} 11708800
vllm:prefix_cache_queries_total{engine="0",model_name="qwen3.8-27b"} 16188982
vllm:num_preemptions_total{engine="0"} 3
vllm:spec_decode_num_drafts_total{engine="0"} 100
vllm:spec_decode_num_accepted_tokens_total{engine="0"} 180
vllm:num_requests_running{engine="0"} 2
vllm:num_requests_waiting{engine="0"} 5
vllm:gpu_cache_usage_perc{engine="0"} 0.42
vllm:request_queue_time_seconds_bucket{le="0.01"} 10
vllm:request_queue_time_seconds_bucket{le="0.1"} 90
vllm:request_queue_time_seconds_bucket{le="+Inf"} 100
vllm:request_queue_time_seconds_sum 50.0
vllm:request_queue_time_seconds_count 100
vllm:some_other_metric 1.5
`

func TestParsePrometheusTimestamp(t *testing.T) {
	s := Parse(`vllm:num_requests_running 7 1712000000000
vllm:num_requests_waiting{engine="0"} 3 1712000000000
`)
	if s.Gauges["vllm:num_requests_running"] != 7 || s.Gauges["vllm:num_requests_waiting"] != 3 {
		t.Fatalf("带 timestamp 的 gauge 解析错误: %+v", s.Gauges)
	}
}

func TestParse(t *testing.T) {
	s := Parse(sampleText)
	if got := s.Counters["vllm:prefix_cache_hits"]; got != 11708800 {
		t.Fatalf("prefix_cache_hits = %v, want 11708800（_total 应剥离）", got)
	}
	if got := s.Gauges["vllm:num_requests_running"]; got != 2 {
		t.Fatalf("running gauge = %v, want 2", got)
	}
	h := s.Hists["vllm:request_queue_time_seconds"]
	if h == nil || h.Count != 100 || h.Sum != 50 {
		t.Fatalf("hist 解析错误: %+v", h)
	}
	if len(h.Buckets) != 3 || !isInf(h.Buckets[2].LE) {
		t.Fatalf("hist 桶错误: %+v", h.Buckets)
	}
}

func TestParseLabeledBucketsMerged(t *testing.T) {
	// 同一 family 同一 le 出现两个 label 系列（vLLM 真实形态）——
	// 回归点：Parse 曾逐行 append 不合并，histQuantile 的 map 覆盖导致分位估算失真
	text := `vllm:time_to_first_token_seconds_bucket{model_name="a",le="0.1"} 3
vllm:time_to_first_token_seconds_bucket{model_name="b",le="0.1"} 5
vllm:time_to_first_token_seconds_bucket{model_name="a",le="1.0"} 9
vllm:time_to_first_token_seconds_bucket{model_name="b",le="1.0"} 10
vllm:time_to_first_token_seconds_bucket{model_name="a",le="+Inf"} 10
vllm:time_to_first_token_seconds_bucket{model_name="b",le="+Inf"} 10
vllm:time_to_first_token_seconds_count 20
`
	s := Parse(text)
	h := s.Hists["vllm:time_to_first_token_seconds"]
	if h == nil {
		t.Fatal("直方图未解析")
	}
	if len(h.Buckets) != 3 {
		t.Fatalf("同 le 桶应合并为一条，实际 %d 条: %+v", len(h.Buckets), h.Buckets)
	}
	if h.Buckets[0].LE != 0.1 || h.Buckets[0].Count != 8 {
		t.Fatalf("le=0.1 桶应累加 3+5=8，实际 %+v", h.Buckets[0])
	}
	if h.Buckets[1].LE != 1.0 || h.Buckets[1].Count != 19 {
		t.Fatalf("le=1.0 桶应累加 9+10=19，实际 %+v", h.Buckets[1])
	}
	// 分位可估算：total=20（+Inf 差），累计到 10 落在 le=1.0 桶
	if got := histQuantile(nil, h, 0.50); got != 1.0 {
		t.Fatalf("P50 应落在 le=1.0 桶，实际 %v", got)
	}
}

func TestParseInfoLabels(t *testing.T) {
	// 12.12：info 型指标（值恒 1、配置在 label）单开 label 快照——vllm:cache_config_info
	text := `noise_total 5
vllm:cache_config_info{block_size="1600",cache_dtype="fp8",gpu_memory_utilization="0.9",kv_cache_max_concurrency="5.74",kv_cache_size_tokens="1505497"} 1
`
	s := Parse(text)
	got := s.Info["vllm:cache_config_info"]
	if got == nil {
		t.Fatal("cache_config_info label 快照未提取")
	}
	if got["kv_cache_size_tokens"] != "1505497" || got["block_size"] != "1600" {
		t.Fatalf("label 值错误: %+v", got)
	}
	// 常规面行为不变：info 值照常进 gauges（无消费方，仅保持解析口径一致）
	if s.Gauges["vllm:cache_config_info"] != 1 {
		t.Fatalf("info 值应照常进 gauges，实际 %v", s.Gauges["vllm:cache_config_info"])
	}
}

func TestParseInfoLabelsKeepsFirstSeries(t *testing.T) {
	// 多系列（如多 engine label）取首个：引擎级配置各系列相同
	text := `vllm:cache_config_info{engine="0",kv_cache_size_tokens="100"} 1
vllm:cache_config_info{engine="1",kv_cache_size_tokens="200"} 1
`
	s := Parse(text)
	if got := s.Info["vllm:cache_config_info"]["kv_cache_size_tokens"]; got != "100" {
		t.Fatalf("多系列应取首个，实际 %v", got)
	}
}

func TestExtractKVCapacity(t *testing.T) {
	text := `vllm:cache_config_info{block_size="1600",cache_dtype="fp8",gpu_memory_utilization="0.9",kv_cache_max_concurrency="5.74",kv_cache_size_tokens="1505497"} 1
`
	c := ExtractKVCapacity(Parse(text))
	if c == nil {
		t.Fatal("画像未提取")
	}
	if c.SizeTokens != 1505497 || c.MaxConcurrency != 5.74 || c.BlockSize != 1600 ||
		c.CacheDtype != "fp8" || c.GPUUtil != 0.9 {
		t.Fatalf("画像字段错误: %+v", c)
	}
	if c.Describe() == "" {
		t.Fatal("Describe 不应为空")
	}

	// 引擎未暴露该指标（非 vLLM / 旧版本）→ nil 宽容
	if got := ExtractKVCapacity(Parse("vllm:num_requests_running 1\n")); got != nil {
		t.Fatalf("未暴露时应返回 nil，实际 %+v", got)
	}
	// label 命名不符（未来版本改名）→ nil（避免空壳画像）
	if got := ExtractKVCapacity(Parse(`vllm:cache_config_info{something_else="1"} 1`)); got != nil {
		t.Fatalf("命名不符时应返回 nil，实际 %+v", got)
	}
	// nil sample 安全（观测层关闭路径）
	if got := ExtractKVCapacity(nil); got != nil {
		t.Fatalf("nil sample 应返回 nil")
	}
}

func TestCounterDelta(t *testing.T) {
	before := Parse(sampleText)
	afterText := strings.NewReplacer(
		"16188982", "16189982", // 查询 +1000
		`num_preemptions_total{engine="0"} 3`, `num_preemptions_total{engine="0"} 5`,
		"spec_decode_num_drafts_total{engine=\"0\"} 100", "spec_decode_num_drafts_total{engine=\"0\"} 160",
		"spec_decode_num_accepted_tokens_total{engine=\"0\"} 180", "spec_decode_num_accepted_tokens_total{engine=\"0\"} 288",
	).Replace(sampleText)
	after := Parse(afterText)
	d := DiffCounters(before, after, VLLM())
	if d.PrefixCacheHitTokens != 0 || d.PrefixCacheQueryTokens != 1000 {
		t.Fatalf("cache delta 错误: hit=%v query=%v", d.PrefixCacheHitTokens, d.PrefixCacheQueryTokens)
	}
	if d.Preemptions != 2 || d.SpecDrafts != 60 || d.SpecAcceptedTokens != 108 {
		t.Fatalf("counter delta 错误: %+v", d)
	}
	if got := d.CacheHitRate(); got != 0 {
		t.Fatalf("无命中时命中率应为 0，得到 %v", got)
	}
}

func TestCounterDeltaHitRate(t *testing.T) {
	b := &Sample{Counters: map[string]float64{}}
	a := &Sample{Counters: map[string]float64{
		"vllm:prefix_cache_hits":    800,
		"vllm:prefix_cache_queries": 1000,
	}}
	d := DiffCounters(b, a, VLLM())
	if d.CacheHitRate() != 0.8 {
		t.Fatalf("命中率 = %v, want 0.8", d.CacheHitRate())
	}
}

func TestHistDeltaQuantile(t *testing.T) {
	before := Parse(sampleText)
	after := Parse(sampleText +
		"vllm:time_to_first_token_seconds_bucket{le=\"0.05\"} 4\n" +
		"vllm:time_to_first_token_seconds_bucket{le=\"0.1\"} 8\n" +
		"vllm:time_to_first_token_seconds_bucket{le=\"1.0\"} 10\n" +
		"vllm:time_to_first_token_seconds_bucket{le=\"+Inf\"} 10\n" +
		"vllm:time_to_first_token_seconds_sum 2.0\n" +
		"vllm:time_to_first_token_seconds_count 10\n")
	ds := HistDeltas(before, after, VLLM())
	ttft, ok := ds["vllm:time_to_first_token_seconds"]
	if !ok || ttft.Count != 10 {
		t.Fatalf("TTFT 直方图 delta 错误: %+v", ttft)
	}
	if ttft.P50 != 0.1 || ttft.P99 != 1.0 {
		t.Fatalf("分位估计错误: p50=%v p99=%v（期望 0.1 / 1.0）", ttft.P50, ttft.P99)
	}
	if ttft.Mean != 0.2 {
		t.Fatalf("均值 = %v, want 0.2", ttft.Mean)
	}
	// queue 直方图无增量（before==after）→ 不应出现
	if q, ok := ds["vllm:request_queue_time_seconds"]; ok {
		t.Fatalf("无增量直方图不应出现: %+v", q)
	}
}

// ── MetricsProvider：前缀识别 / SGLang 命名 / 降级判定 ──

func TestDetectProvider(t *testing.T) {
	if got := DetectProvider(nil); got.Name() != "vllm" {
		t.Fatalf("nil sample 应回落 vllm，得到 %s", got.Name())
	}
	if got := DetectProvider(Parse(sampleText)); got.Name() != "vllm" {
		t.Fatalf("vllm 命名应识别为 vllm，得到 %s", got.Name())
	}
	sg := Parse("sglang:num_running_reqs 2\nsglang:token_usage 0.3\n")
	if got := DetectProvider(sg); got.Name() != "sglang" {
		t.Fatalf("sglang 命名应识别为 sglang，得到 %s", got.Name())
	}
}

func TestSGLangGaugeNames(t *testing.T) {
	sg := SGLang()
	if _, ok := sg.CounterNames()["generation_tokens"]; !ok {
		t.Fatal("sglang 应采集 generation_tokens 用于两源一致性")
	}
	cands := sg.GaugeNames()["running"]
	if len(cands) == 0 || cands[0] != "sglang:num_running_reqs" {
		t.Fatalf("sglang running 候选名错误: %v", cands)
	}
	if cands := sg.GaugeNames()["cache_hit_rate"]; len(cands) == 0 || cands[0] != "sglang:cache_hit_rate" {
		t.Fatalf("sglang cache_hit_rate 候选名错误: %v", cands)
	}
	hists := sg.HistNames()
	if len(hists) != 3 || hists[0] != "sglang:time_to_first_token_seconds" {
		t.Fatalf("sglang histogram 候选名错误: %v", hists)
	}
}

func TestPollerSGLangNaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "sglang:num_running_reqs 3\nsglang:num_queue_reqs 1\n"+
			"sglang:token_usage 0.5\nsglang:num_used_tokens 123859\nsglang:cache_hit_rate 0.42\n")
	}))
	defer srv.Close()

	ctx := context.Background()
	g := StartGaugePoller(ctx, NewScraperAt(srv.URL, "/metrics"), 10*time.Millisecond, SGLang())
	defer g.Stop()
	time.Sleep(80 * time.Millisecond)
	h := g.Health()
	if h.Samples == 0 {
		t.Fatalf("sglang 命名轮询应有成功样本: %+v", h)
	}
	sum := g.Summary()
	if sum["running"].Max != 3 || sum["waiting"].Max != 1 || sum["kv_usage"].Max != 0.5 {
		t.Fatalf("sglang gauge 取值错误: %+v", sum)
	}
	if sum["kv_used_tokens"].Max != 123859 || sum["cache_hit_rate"].Max != 0.42 {
		t.Fatalf("sglang cache/kv gauge 取值错误: %+v", sum)
	}
}

const sglangSampleText = `# HELP sglang:prompt_tokens_total Number of prefill tokens processed.
# TYPE sglang:prompt_tokens_total counter
sglang:prompt_tokens_total{model_name="qwen3.8-27b"} 8128902
# HELP sglang:generation_tokens_total Number of generation tokens processed.
# TYPE sglang:generation_tokens_total counter
sglang:generation_tokens_total{model_name="qwen3.8-27b"} 7557572
# HELP sglang:token_usage The token usage
# TYPE sglang:token_usage gauge
sglang:token_usage{model_name="qwen3.8-27b"} 0.28
# HELP sglang:num_used_tokens The number of used tokens
# TYPE sglang:num_used_tokens gauge
sglang:num_used_tokens{model_name="qwen3.8-27b"} 123859
# HELP sglang:cache_hit_rate The cache hit rate
# TYPE sglang:cache_hit_rate gauge
sglang:cache_hit_rate{model_name="qwen3.8-27b"} 0.62
# HELP sglang:time_to_first_token_seconds Histogram of time to first token in seconds.
# TYPE sglang:time_to_first_token_seconds histogram
sglang:time_to_first_token_seconds_bucket{le="0.1",model_name="qwen3.8-27b"} 4
sglang:time_to_first_token_seconds_bucket{le="1.0",model_name="qwen3.8-27b"} 8
sglang:time_to_first_token_seconds_bucket{le="+Inf",model_name="qwen3.8-27b"} 10
sglang:time_to_first_token_seconds_sum{model_name="qwen3.8-27b"} 2.0
sglang:time_to_first_token_seconds_count{model_name="qwen3.8-27b"} 10
# HELP sglang:e2e_request_latency_seconds Histogram of End-to-end request latency in seconds
# TYPE sglang:e2e_request_latency_seconds histogram
sglang:e2e_request_latency_seconds_bucket{le="1.0",model_name="qwen3.8-27b"} 4
sglang:e2e_request_latency_seconds_bucket{le="10.0",model_name="qwen3.8-27b"} 8
sglang:e2e_request_latency_seconds_bucket{le="+Inf",model_name="qwen3.8-27b"} 10
sglang:e2e_request_latency_seconds_sum{model_name="qwen3.8-27b"} 20.0
sglang:e2e_request_latency_seconds_count{model_name="qwen3.8-27b"} 10
# HELP sglang:time_per_output_token_seconds Histogram of time per output token in seconds.
# TYPE sglang:time_per_output_token_seconds histogram
sglang:time_per_output_token_seconds_bucket{le="0.05",model_name="qwen3.8-27b"} 4
sglang:time_per_output_token_seconds_bucket{le="0.5",model_name="qwen3.8-27b"} 8
sglang:time_per_output_token_seconds_bucket{le="+Inf",model_name="qwen3.8-27b"} 10
sglang:time_per_output_token_seconds_sum{model_name="qwen3.8-27b"} 1.0
sglang:time_per_output_token_seconds_count{model_name="qwen3.8-27b"} 10
`

func TestSGLangCountersAndHistograms(t *testing.T) {
	before := Parse(sglangSampleText)
	after := Parse(strings.NewReplacer(
		"sglang:prompt_tokens_total{model_name=\"qwen3.8-27b\"} 8128902", "sglang:prompt_tokens_total{model_name=\"qwen3.8-27b\"} 8129902",
		"sglang:generation_tokens_total{model_name=\"qwen3.8-27b\"} 7557572", "sglang:generation_tokens_total{model_name=\"qwen3.8-27b\"} 7558572",
		"sglang:time_to_first_token_seconds_bucket{le=\"0.1\",model_name=\"qwen3.8-27b\"} 4", "sglang:time_to_first_token_seconds_bucket{le=\"0.1\",model_name=\"qwen3.8-27b\"} 14",
		"sglang:time_to_first_token_seconds_bucket{le=\"1.0\",model_name=\"qwen3.8-27b\"} 8", "sglang:time_to_first_token_seconds_bucket{le=\"1.0\",model_name=\"qwen3.8-27b\"} 28",
		"sglang:time_to_first_token_seconds_bucket{le=\"+Inf\",model_name=\"qwen3.8-27b\"} 10", "sglang:time_to_first_token_seconds_bucket{le=\"+Inf\",model_name=\"qwen3.8-27b\"} 30",
		"sglang:time_to_first_token_seconds_sum{model_name=\"qwen3.8-27b\"} 2.0", "sglang:time_to_first_token_seconds_sum{model_name=\"qwen3.8-27b\"} 3.0",
		"sglang:time_to_first_token_seconds_count{model_name=\"qwen3.8-27b\"} 10", "sglang:time_to_first_token_seconds_count{model_name=\"qwen3.8-27b\"} 30",
	).Replace(sglangSampleText))
	d := DiffCounters(before, after, SGLang())
	if d.PromptTokens != 1000 || d.GenerationTokens != 1000 {
		t.Fatalf("sglang counter delta 错误: prompt=%v generation=%v", d.PromptTokens, d.GenerationTokens)
	}
	h := HistDeltas(before, after, SGLang())
	ttft := h["sglang:time_to_first_token_seconds"]
	if ttft.Count != 20 || ttft.Mean != 0.05 || ttft.P50 != 0.1 {
		t.Fatalf("sglang TTFT histogram 错误: %+v", ttft)
	}
	if _, ok := h["sglang:e2e_request_latency_seconds"]; ok {
		t.Fatalf("无增量的 SGLang E2E histogram 不应出现: %+v", h)
	}
}

func TestPollerDegraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()

	ctx := context.Background()
	g := StartGaugePoller(ctx, NewScraperAt(srv.URL, "/metrics"), 5*time.Millisecond, VLLM())
	defer g.Stop()
	time.Sleep(120 * time.Millisecond)
	h := g.Health()
	if !h.Degraded() {
		t.Fatalf("持续失败应判定降级: %+v", h)
	}
	if h.ConsecutiveFailures < 2 || h.LastError == "" {
		t.Fatalf("失败计数/原因缺失: %+v", h)
	}
}

// metrics 抓取的认证头与 engine.Auth 语义一致（bearer 默认/raw 裸 key/none 不带）。
func TestScraperApplyAuth(t *testing.T) {
	mk := func() *http.Request { req, _ := http.NewRequest("GET", "http://x/metrics", nil); return req }

	req := mk()
	(&Scraper{APIKey: "k"}).applyAuth(req)
	if got := req.Header.Get("Authorization"); got != "Bearer k" {
		t.Fatalf("默认应为 Bearer: %q", got)
	}

	req = mk()
	(&Scraper{Auth: auth.Auth{Scheme: "raw", Header: "X-Key"}, APIKey: "k"}).applyAuth(req)
	if got := req.Header.Get("X-Key"); got != "k" {
		t.Fatalf("raw 应为裸 key: %q", got)
	}

	req = mk()
	(&Scraper{Auth: auth.Auth{Scheme: "none"}, APIKey: "k"}).applyAuth(req)
	if req.Header.Get("Authorization") != "" {
		t.Fatal("none 不应带认证头")
	}

	req = mk()
	(&Scraper{APIKey: ""}).applyAuth(req)
	if req.Header.Get("Authorization") != "" {
		t.Fatal("无 key 不应带认证头")
	}
}

// ── 对抗式审查回归：NaN/Inf 丢弃 + counter 负增量钳 0 ──

func TestParseSkipsNonFinite(t *testing.T) {
	s := Parse("vllm:good_total 5\n" +
		"vllm:nan_total NaN\n" +
		"vllm:pinf_total +Inf\n" +
		"vllm:ninf_total -Inf\n")
	if _, ok := s.Counters["vllm:nan"]; ok {
		t.Fatal("NaN 指标应被丢弃（json 序列化会失败）")
	}
	if _, ok := s.Counters["vllm:pinf"]; ok {
		t.Fatal("+Inf 指标应被丢弃")
	}
	if _, ok := s.Counters["vllm:ninf"]; ok {
		t.Fatal("-Inf 指标应被丢弃")
	}
	if s.Counters["vllm:good"] != 5 {
		t.Fatalf("正常指标不受影响: %v", s.Counters["vllm:good"])
	}
}

func TestDiffCountersClampsNegative(t *testing.T) {
	before := Parse("vllm:prefix_cache_hits_total 100\nvllm:prefix_cache_queries_total 200\n")
	// after < before：服务端重启 counter 归零
	after := Parse("vllm:prefix_cache_hits_total 3\nvllm:prefix_cache_queries_total 4\n")
	d := DiffCounters(before, after, VLLM())
	if d.PrefixCacheHitTokens != 0 || d.PrefixCacheQueryTokens != 0 {
		t.Fatalf("负增量应钳 0, got hits=%v queries=%v", d.PrefixCacheHitTokens, d.PrefixCacheQueryTokens)
	}
	if d.CacheHitRate() != 0 {
		t.Fatalf("钳 0 后命中率应为 0, got %v", d.CacheHitRate())
	}
}

// ── DeepSeek review 回归：/metrics 超限截断必须显式报错，不能静默丢指标 ──

func TestScrapeTruncatedBodyRejected(t *testing.T) {
	big := strings.Repeat("vllm:x_total 1\n", (16<<20)/15+2) // > 16MB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()
	sc := &Scraper{URL: srv.URL, Client: srv.Client()}
	if _, err := sc.Scrape(context.Background()); err == nil || !strings.Contains(err.Error(), "截断") {
		t.Fatalf("超限响应应报截断错误, got %v", err)
	}
}
