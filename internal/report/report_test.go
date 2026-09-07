package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SaveJSONAny：嵌套目录自动创建 + JSON 可回读 + 字段保真。
func TestSaveJSONAnyRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "deep", "out.json")

	in := Report{
		Tool:        "llm-perf/test",
		Scenario:    "single",
		GeneratedAt: time.Now(),
		Endpoint:    "http://stub/v1",
		Single: []SingleRow{{
			Model: "m1", Thinking: "off", PromptTokens: 4096,
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
	if out.Tool != "llm-perf/test" || out.Scenario != "single" || out.Endpoint != "http://stub/v1" {
		t.Fatalf("字段保真失败: %+v", out)
	}
	if len(out.Single) != 1 || out.Single[0].Model != "m1" || out.Single[0].PromptTokens != 4096 {
		t.Fatalf("嵌套字段保真失败: %+v", out.Single)
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
		Tool: "t", Scenario: "single", Endpoint: "http://stub/v1",
		Single: []SingleRow{
			{Model: "m1", Thinking: "off", PromptTokens: 100},
			{Model: "m2", Thinking: "off", PromptTokens: 200},
			{Model: "m1", Thinking: "on", PromptTokens: 300},
		},
		Multiturn: []MultiturnRun{{Model: "m2", Session: 1}},
		Correctness: []CorrectnessRow{
			{Model: "m2", Number: "12345", Match: true},
			{Number: "67890", Match: false}, // 历史缺省行：复制进每个分区
		},
	}
	parts := r.PartitionByModel()
	if len(parts) != 2 {
		t.Fatalf("应分 2 个分区: %d", len(parts))
	}
	if parts[0].PartitionModel != "m1" || parts[1].PartitionModel != "m2" {
		t.Fatalf("分区应按首次出现顺序: %s, %s", parts[0].PartitionModel, parts[1].PartitionModel)
	}
	if len(parts[0].Single) != 2 || len(parts[0].Multiturn) != 0 {
		t.Fatalf("m1 分区数据错误: %+v", parts[0])
	}
	if len(parts[1].Single) != 1 || len(parts[1].Multiturn) != 1 {
		t.Fatalf("m2 分区数据错误: %+v", parts[1])
	}
	// 缺 model 的 correctness 行两个分区都有
	if len(parts[0].Correctness) != 1 || len(parts[1].Correctness) != 2 {
		t.Fatalf("correctness 归属错误: m1=%d m2=%d", len(parts[0].Correctness), len(parts[1].Correctness))
	}
	// Endpoint 级字段原样带入
	for _, p := range parts {
		if p.Tool != "t" || p.Scenario != "single" || p.Endpoint != "http://stub/v1" {
			t.Fatalf("分区头字段丢失: %+v", p)
		}
	}
}
