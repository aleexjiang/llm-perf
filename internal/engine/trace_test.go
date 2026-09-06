package engine

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sharegptSample = `[
  {"conversations": [
    {"from": "human", "value": "你好"},
    {"from": "gpt", "value": "你好！"},
    {"from": "human", "value": "介绍下自己"},
    {"from": "gpt", "value": "我是助手"}
  ]},
  {"conversations": [
    {"from": "user", "value": "单轮问题"}
  ]}
]`

func TestLoadTraceShareGPT(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sg.json")
	if err := os.WriteFile(p, []byte(sharegptSample), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, err := LoadTrace(p, "", 2, 0) // min_turns=2 过滤掉单轮会话
	if err != nil {
		t.Fatal(err)
	}
	if ts.Format != "sharegpt" || len(ts.Sessions) != 1 {
		t.Fatalf("格式/会话数错误: %s, %d", ts.Format, len(ts.Sessions))
	}
	s := ts.Pick(0)
	if len(s.UserTurns) != 2 || s.UserTurns[0] != "你好" {
		t.Fatalf("user 轮提取错误: %+v", s.UserTurns)
	}
	// role/content 键名变体也应支持
	variant := `[{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}]`
	p2 := filepath.Join(dir, "v.json")
	os.WriteFile(p2, []byte(variant), 0o644)
	ts2, err := LoadTrace(p2, "sharegpt", 2, 0)
	if err != nil || len(ts2.Sessions[0].UserTurns) != 2 {
		t.Fatalf("role/content 变体解析失败: %v, %+v", err, ts2.Sessions)
	}
}

func TestLoadTraceSessions(t *testing.T) {
	dir := t.TempDir()
	// 形态 1：对象数组
	p := filepath.Join(dir, "s.json")
	os.WriteFile(p, []byte(`[{"turns":["u1","u2","u3"]},{"turns":["v1"]}]`), 0o644)
	ts, err := LoadTrace(p, "sessions", 2, 0)
	if err != nil || len(ts.Sessions) != 1 || len(ts.Sessions[0].UserTurns) != 3 {
		t.Fatalf("sessions 对象形态解析失败: %v %+v", err, ts.Sessions)
	}
	// 形态 2：二维数组
	p2 := filepath.Join(dir, "a.json")
	os.WriteFile(p2, []byte(`[["u1","u2"],["v1","v2"]]`), 0o644)
	ts2, err := LoadTrace(p2, "sessions", 2, 0)
	if err != nil || len(ts2.Sessions) != 2 {
		t.Fatalf("sessions 数组形态解析失败: %v", err)
	}
}

func TestLoadTraceGzip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.json.gz")
	f, _ := os.Create(p)
	gz := gzip.NewWriter(f)
	gz.Write([]byte(`[{"turns":["u1","u2"]}]`))
	gz.Close()
	f.Close()
	ts, err := LoadTrace(p, "sessions", 2, 0)
	if err != nil || len(ts.Sessions) != 1 {
		t.Fatalf("gzip 解析失败: %v", err)
	}
}

func TestLoadTraceMaxSessions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.json")
	os.WriteFile(p, []byte(`[{"turns":["a","b"]},{"turns":["c","d"]},{"turns":["e","f"]}]`), 0o644)
	ts, err := LoadTrace(p, "sessions", 2, 2)
	if err != nil || len(ts.Sessions) != 2 {
		t.Fatalf("max_sessions 应截断: %v %d", err, len(ts.Sessions))
	}
}

func TestTracePickWrap(t *testing.T) {
	ts := &TraceSet{Sessions: []TraceSession{
		{UserTurns: []string{"a"}}, {UserTurns: []string{"b"}},
	}}
	if got := ts.Pick(0).UserTurns[0]; got != "a" {
		t.Fatalf("Pick(0) = %q", got)
	}
	if got := ts.Pick(2).UserTurns[0]; got != "a" {
		t.Fatalf("Pick(2) 应回绕到会话 0: %q", got)
	}
	if got := ts.Pick(3).UserTurns[0]; got != "b" {
		t.Fatalf("Pick(3) 应回绕到会话 1: %q", got)
	}
	// 空 trace 不 panic
	if got := (&TraceSet{}).Pick(0).UserTurns; got != nil {
		t.Fatal("空 trace 应返回空会话")
	}
	// 缺文件报错
	if _, err := LoadTrace(filepath.Join(t.TempDir(), "nope.json"), "", 2, 0); err == nil {
		t.Fatal("缺文件应报错")
	} else if !strings.Contains(err.Error(), "nope.json") {
		t.Fatalf("错误信息应含文件路径: %v", err)
	}
}
