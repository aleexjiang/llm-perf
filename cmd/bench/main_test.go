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
