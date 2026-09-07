package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/aleexjiang/llm-perf/internal/report"
)

// resolveOutPath 三种输入形态：空 → outputDir/默认名；.json 后缀 → 原样；其他 → 视为目录拼接默认名。
func TestResolveOutPath(t *testing.T) {
	def := report.DefaultName("single")
	if def == "" || !strings.HasSuffix(def, ".json") {
		t.Fatalf("DefaultName 异常: %q", def)
	}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"空 → 配置输出目录 + 默认名", "", filepath.Join("output", def)},
		{".json 后缀 → 原样", "result/r1.json", "result/r1.json"},
		{"目录 → 拼接默认名", "results", filepath.Join("results", def)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveOutPath(c.in, "output", "single"); got != c.want {
				t.Fatalf("resolveOutPath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// modelDirName：取 "/" 后末段 + 非法字符清洗；空段回退 unknown
func TestModelDirName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/models/DeepSeek-V4-Flash-0731", "DeepSeek-V4-Flash-0731"},
		{"Qwen3.8-27B", "Qwen3.8-27B"},
		{"openai/gpt-4:latest", "gpt-4-latest"}, // ":" 清洗
		{"/models/中文 模型", "中文-模型"},              // 空格清洗，中文保留（Unicode 感知，避免碰撞）
		{"/models/..", "unknown"},               // 全是点：防路径逃逸，回退
	}
	for _, c := range cases {
		if got := modelDirName(c.in); got != c.want {
			t.Fatalf("modelDirName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
