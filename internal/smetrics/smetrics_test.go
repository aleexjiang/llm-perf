package smetrics

import (
	"strings"
	"testing"
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

func TestCounterDelta(t *testing.T) {
	before := Parse(sampleText)
	afterText := strings.NewReplacer(
		"16188982", "16189982", // 查询 +1000
		`num_preemptions_total{engine="0"} 3`, `num_preemptions_total{engine="0"} 5`,
		"spec_decode_num_drafts_total{engine=\"0\"} 100", "spec_decode_num_drafts_total{engine=\"0\"} 160",
		"spec_decode_num_accepted_tokens_total{engine=\"0\"} 180", "spec_decode_num_accepted_tokens_total{engine=\"0\"} 288",
	).Replace(sampleText)
	after := Parse(afterText)
	d := DiffCounters(before, after)
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
	d := DiffCounters(b, a)
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
	ds := HistDeltas(before, after)
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
