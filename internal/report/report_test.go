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
