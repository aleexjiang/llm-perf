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
	ts, err := LoadTrace(p, "", "user_only", 2, 0) // min_turns=2 过滤掉单轮会话
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
	ts2, err := LoadTrace(p2, "sharegpt", "user_only", 2, 0)
	if err != nil || len(ts2.Sessions[0].UserTurns) != 2 {
		t.Fatalf("role/content 变体解析失败: %v, %+v", err, ts2.Sessions)
	}
}

func TestLoadTraceSessions(t *testing.T) {
	dir := t.TempDir()
	// 唯一合法形态：对象数组 [{"turns": [...]}]
	p := filepath.Join(dir, "s.json")
	os.WriteFile(p, []byte(`[{"turns":["u1","u2","u3"]},{"turns":["v1"]}]`), 0o644)
	ts, err := LoadTrace(p, "sessions", "user_only", 2, 0)
	if err != nil || len(ts.Sessions) != 1 || len(ts.Sessions[0].UserTurns) != 3 {
		t.Fatalf("sessions 对象形态解析失败: %v %+v", err, ts.Sessions)
	}
	// 裸数组形态（[["u1","u2"]]）已删除：必须报格式错误
	p2 := filepath.Join(dir, "a.json")
	os.WriteFile(p2, []byte(`[["u1","u2"],["v1","v2"]]`), 0o644)
	if _, err := LoadTrace(p2, "sessions", "user_only", 2, 0); err == nil {
		t.Fatal("裸数组形态应报 sessions 格式不合法")
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
	ts, err := LoadTrace(p, "sessions", "user_only", 2, 0)
	if err != nil || len(ts.Sessions) != 1 {
		t.Fatalf("gzip 解析失败: %v", err)
	}
}

func TestLoadTraceMaxSessions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.json")
	os.WriteFile(p, []byte(`[{"turns":["a","b"]},{"turns":["c","d"]},{"turns":["e","f"]}]`), 0o644)
	ts, err := LoadTrace(p, "sessions", "user_only", 2, 2)
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
	if _, err := LoadTrace(filepath.Join(t.TempDir(), "nope.json"), "", "user_only", 2, 0); err == nil {
		t.Fatal("缺文件应报错")
	} else if !strings.Contains(err.Error(), "nope.json") {
		t.Fatalf("错误信息应含文件路径: %v", err)
	}
}

// ── full 回放保真度 ──

const fullSample = `[
  {"conversations": [
    {"from": "human", "value": "北京天气如何"},
    {"from": "gpt", "value": "我来查一下"},
    {"from": "function", "value": "{\"temp\":\"25C\"}", "tool_call_id": "call_1"},
    {"from": "gpt", "value": "北京今天 25 度"},
    {"from": "human", "value": "那上海呢"},
    {"from": "gpt", "value": "上海 28 度"}
  ]}
]`

func TestLoadTraceFullReplay(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "full.json")
	os.WriteFile(p, []byte(fullSample), 0o644)
	ts, err := LoadTrace(p, "sharegpt", "full", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !ts.FullReplay {
		t.Fatal("FullReplay 应为 true")
	}
	s := ts.Pick(0)
	if len(s.UserTurns) != 2 {
		t.Fatalf("user 轮应为 2: %d", len(s.UserTurns))
	}
	// full：原序全部 role——user/assistant/tool 都在，role 词表已归一化
	if len(s.Messages) != 6 {
		t.Fatalf("full 应保留全部 6 条消息: %d", len(s.Messages))
	}
	roles := []string{}
	for _, m := range s.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{"user", "assistant", "tool", "assistant", "user", "assistant"}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("role 序列错误 @%d: got %v want %v", i, roles, want)
		}
	}
	if s.Messages[2].Role == "tool" && s.Messages[2].ToolCallID != "call_1" {
		t.Fatalf("tool 消息的 tool_call_id 应透传: %+v", s.Messages[2])
	}
}

// role=tool 缺 tool_call_id：跳过并计数，不静默丢弃
func TestLoadTraceFullMissingToolCallID(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.json")
	os.WriteFile(p, []byte(`[{"messages":[
		{"role":"user","content":"u1"},
		{"role":"assistant","content":"a1"},
		{"role":"tool","content":"result-no-id"},
		{"role":"user","content":"u2"},
		{"role":"assistant","content":"a2"}
	]}]`), 0o644)
	ts, err := LoadTrace(p, "sharegpt", "full", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ts.MissingToolCallID != 1 {
		t.Fatalf("缺失计数应为 1: %d", ts.MissingToolCallID)
	}
	if len(ts.Sessions[0].Messages) != 4 {
		t.Fatalf("缺失 tool_call_id 的消息应被跳过: %d", len(ts.Sessions[0].Messages))
	}
}

// sessions 格式只有 user 轮：full 模式退化为 user 序列，NoAssistantContent 如实标注
func TestLoadTraceFullSessionsNoAssistant(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.json")
	os.WriteFile(p, []byte(`[{"turns":["u1","u2"]}]`), 0o644)
	ts, err := LoadTrace(p, "sessions", "full", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !ts.NoAssistantContent {
		t.Fatal("sessions 格式 full 回放应标注 NoAssistantContent")
	}
}

// user_only 默认行为不变：Messages 不填充
func TestLoadTraceUserOnlyDefault(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sg.json")
	os.WriteFile(p, []byte(sharegptSample), 0o644)
	ts, err := LoadTrace(p, "", "user_only", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ts.FullReplay || ts.Sessions[0].Messages != nil {
		t.Fatal("user_only 不应填充 Messages")
	}
}
