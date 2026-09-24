package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aleexjiang/llm-perf/internal/engine"
)

// SaveJSONAny：嵌套目录自动创建 + JSON 可回读 + 字段保真。
func TestSaveJSONAnyRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "deep", "out.json")

	in := Report{
		Tool:        "llm-perf/test",
		Scenario:    "user",
		GeneratedAt: time.Now(),
		Endpoint:    "http://stub/v1",
		Multiturn: []MultiturnRun{{
			Model: "m1", Thinking: "off", Session: 1,
			Turns: []*engine.TurnMetrics{{Model: "m1", PromptTokens: 4096}},
		}},
	}

	if err := SaveJSONAny(&in, p); err != nil {
		t.Fatalf("SaveJSONAny: %v", err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("回读: %v", err)
	}
	var out Report
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("JSON 解析: %v", err)
	}
	if out.Tool != "llm-perf/test" || out.Scenario != "user" || out.Endpoint != "http://stub/v1" {
		t.Fatalf("字段保真失败: %+v", out)
	}
	if len(out.Multiturn) != 1 || out.Multiturn[0].Model != "m1" || len(out.Multiturn[0].Turns) != 1 || out.Multiturn[0].Turns[0].PromptTokens != 4096 {
		t.Fatalf("嵌套字段保真失败: %+v", out.Multiturn)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("JSON 文件权限 = %o，want 600", got)
	}
}

func TestSaveJSONAnyTightensExistingPermissions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.json")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveJSONAny(map[string]string{"secret": "value"}, p); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("覆盖后的 JSON 文件权限 = %o，want 600", got)
	}
}

// DefaultName：<场景>-<时间戳>.json。
func TestDefaultName(t *testing.T) {
	n := DefaultName("single")
	if !strings.HasPrefix(n, "single-") || !strings.HasSuffix(n, ".json") {
		t.Fatalf("DefaultName 格式错误: %q", n)
	}
	// 时间戳部分长度固定（20060102-150405 = 15 字符）
	if len(n) != len("single-")+15+len(".json") {
		t.Fatalf("时间戳长度异常: %q", n)
	}
}

// Version 可被 -ldflags 注入（构建验证），此处仅确认变量存在且非空。
func TestVersionNonEmpty(t *testing.T) {
	if Version == "" {
		t.Fatal("Version 不应为空")
	}
}

// PartitionByModel：按行 Model 分桶、保序、缺 model 的 correctness 行复制进每个分区、归属字段回填
func TestPartitionByModel(t *testing.T) {
	r := &Report{
		Tool: "t", Scenario: "user", Endpoint: "http://stub/v1",
		Multiturn: []MultiturnRun{
			{Model: "m1", Session: 1},
			{Model: "m2", Session: 1},
			{Model: "m1", Session: 2},
		},
		Correctness: []CorrectnessRow{
			{Model: "m2", Number: "12345", Match: true},
			{Model: "m1", Number: "67890", Match: false},
		},
	}
	parts := r.PartitionByModel()
	if len(parts) != 2 {
		t.Fatalf("应分 2 个分区: %d", len(parts))
	}
	if parts[0].PartitionModel != "m1" || parts[1].PartitionModel != "m2" {
		t.Fatalf("分区应按首次出现顺序: %s, %s", parts[0].PartitionModel, parts[1].PartitionModel)
	}
	if len(parts[0].Multiturn) != 2 {
		t.Fatalf("m1 分区数据错误: %+v", parts[0])
	}
	if len(parts[1].Multiturn) != 1 {
		t.Fatalf("m2 分区数据错误: %+v", parts[1])
	}
	if len(parts[0].Correctness) != 1 || len(parts[1].Correctness) != 1 {
		t.Fatalf("correctness 归属错误: m1=%d m2=%d", len(parts[0].Correctness), len(parts[1].Correctness))
	}
	// Endpoint 级字段原样带入
	for _, p := range parts {
		if p.Tool != "t" || p.Scenario != "user" || p.Endpoint != "http://stub/v1" {
			t.Fatalf("分区头字段丢失: %+v", p)
		}
	}
}

func TestPartitionByModelAuxiliaryRequests(t *testing.T) {
	r := &Report{AuxiliaryRequests: []AuxiliaryRequest{
		{Phase: "warmup", Index: 0, Metrics: &engine.TurnMetrics{Model: "m1", Phase: "warmup"}},
		{Phase: "correctness", Index: 0, Metrics: &engine.TurnMetrics{Model: "m2", Phase: "correctness"}},
	}}
	parts := r.PartitionByModel()
	if len(parts) != 2 || len(parts[0].AuxiliaryRequests) != 1 || len(parts[1].AuxiliaryRequests) != 1 {
		t.Fatalf("辅助请求应按模型分区: %+v", parts)
	}
	if parts[0].AuxiliaryRequests[0].Phase != "warmup" || parts[1].AuxiliaryRequests[0].Phase != "correctness" {
		t.Fatalf("辅助请求 phase 丢失: %+v", parts)
	}
}

// preemptions 的 JSON tag 刻意不带 omitempty：0 表示「窗口内没有发生抢占」这一
// 有意义的结果（健康态），键一旦消失，读数据的人会把「实测 0」误读成「没采到这一项」。
// 真机上 vllm:num_preemptions_total 存在且为 0，旧产物里却查无此键，正是这个坑。
func TestServerMetricsPreemptionsZeroKept(t *testing.T) {
	b, err := json.Marshal(ServerMetricsSummary{Available: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"preemptions":0`) {
		t.Errorf("preemptions=0 必须显式出现在 JSON 中（否则与「未采集」不可区分）: %s", b)
	}
}

// SGLang 的 prompt/generation counter 必须随 ServerMetricsSummary 落盘；
// 缺字段时 omitempty 会隐藏，消费方无法区分“引擎没暴露”和“工具没接线”。
func TestServerMetricsTokenCountersKept(t *testing.T) {
	b, err := json.Marshal(ServerMetricsSummary{Available: true, PromptTokens: 1000, GenerationTokens: 800})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"prompt_tokens":1000`) ||
		!strings.Contains(s, `"generation_tokens":800`) {
		t.Fatalf("服务端 token counter 未落盘: %s", s)
	}
}

// user/rps/concurrency 共用总吞吐入口：吞吐只看完整成功请求；
// stop/length 分层的加权单流速度不能混成一个中位数。
func TestBuildThroughputSummary(t *testing.T) {
	base := time.Unix(100, 0)
	summary := BuildThroughputSummary([]*engine.TurnMetrics{
		{Stream: true, SentAt: base, EndAt: base.Add(1100 * time.Millisecond), TTFT: 100, E2EMS: 1100, CompletionTokens: 100, TPOTMS: 10, FinishReason: "stop"},
		{Stream: true, SentAt: base, EndAt: base.Add(2200 * time.Millisecond), TTFT: 200, E2EMS: 2200, CompletionTokens: 100, TPOTMS: 20, FinishReason: "length"},
		{Stream: true, Error: "HTTP 500", CompletionTokens: 50, FinishReason: "stop"},
		{Stream: true, Cancelled: true, CompletionTokens: 50},
	}, 2)
	if summary.CompletedRequests != 2 || summary.FailedRequests != 1 || summary.CancelledRequests != 1 {
		t.Fatalf("完成/失败/取消计数错误: %+v", summary)
	}
	if summary.CompletionTokens != 200 || summary.ThroughputTPS != 100 {
		t.Fatalf("总吞吐错误: tokens=%d tps=%v", summary.CompletionTokens, summary.ThroughputTPS)
	}
	if summary.Streaming.All.Count != 2 || summary.Streaming.All.WeightedTPS != 200.0/3.0 {
		t.Fatalf("全成功分层错误: %+v", summary.Streaming.All)
	}
	if summary.ActiveDecodeTokens != 200 || summary.ActiveDecodeTPS <= 0 {
		t.Fatalf("活跃 decode 聚合错误: %+v", summary)
	}
	if summary.Streaming.Stop.Count != 1 || summary.Streaming.Stop.WeightedTPS != 100 {
		t.Fatalf("stop 分层错误: %+v", summary.Streaming.Stop)
	}
	if summary.Streaming.Length.Count != 1 || summary.Streaming.Length.WeightedTPS != 50 {
		t.Fatalf("length 分层错误: %+v", summary.Streaming.Length)
	}
}

func TestBuildThroughputSummaryActiveDecodeUnion(t *testing.T) {
	base := time.Unix(100, 0)
	summary := BuildThroughputSummary([]*engine.TurnMetrics{
		{Stream: true, SentAt: base, TTFT: 100, E2EMS: 1000, EndAt: base.Add(time.Second), CompletionTokens: 10, FinishReason: "stop"},
		{Stream: true, SentAt: base.Add(200 * time.Millisecond), TTFT: 100, E2EMS: 1000, EndAt: base.Add(1200 * time.Millisecond), CompletionTokens: 10, FinishReason: "stop"},
	}, 2)
	// 两条 decode 区间并集 = [100ms,1200ms] = 1.1s；重叠区应相加，而不是把两条单流速率平均。
	if summary.ActiveDecodeSeconds < 1.09 || summary.ActiveDecodeSeconds > 1.11 {
		t.Fatalf("活跃 decode 并集时间错误: %v", summary.ActiveDecodeSeconds)
	}
	if got := summary.ActiveDecodeTPS; got < 18 || got > 19 {
		t.Fatalf("活跃 decode 聚合吞吐错误: %v", got)
	}
}

// 12.3：末轮实测深度取最后一个非零 prompt 轮——失败轮（0）不计入，避免名义对照
// 拿到 0/NaN；全零时保持 0（观测缺失语义，由 Python 侧标注）。
func TestFillLastPromptTokens(t *testing.T) {
	r := &MultiturnRun{Turns: []*engine.TurnMetrics{
		{PromptTokens: 100},
		{PromptTokens: 300},
		{PromptTokens: 0}, // 失败轮：usage 缺失
	}}
	r.FillLastPromptTokens()
	if r.LastPromptTokens != 300 {
		t.Fatalf("应取最后一个非零轮，got %d", r.LastPromptTokens)
	}
	// 全零 → 保持 0
	r2 := &MultiturnRun{Turns: []*engine.TurnMetrics{{PromptTokens: 0}}}
	r2.FillLastPromptTokens()
	if r2.LastPromptTokens != 0 {
		t.Fatalf("全零轮应保持 0，got %d", r2.LastPromptTokens)
	}
	// 空轮次 → 0
	r3 := &MultiturnRun{}
	r3.FillLastPromptTokens()
	if r3.LastPromptTokens != 0 {
		t.Fatalf("空 Turns 应保持 0，got %d", r3.LastPromptTokens)
	}
}
