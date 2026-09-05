// trace.go：真实负载回放（trace 模式）。filler 模式做 token 精确的变量控制实验
//（缓存对照、上下文深度阶梯），trace 模式回放真实会话分布——贴近客户实际流量形状。
//
// 支持两种格式（按文件内容自动识别，显式 format 可覆盖）：
//   - sharegpt：vLLM/SGLang 基准通用数据集格式
//     [ {"conversations": [{"from":"human","value":"..."}, {"from":"gpt","value":"..."}]} , ... ]
//     （兼容 "role"/"content" 键名与 user/assistant 取值）
//   - sessions：本工具自定义精简格式
//     [ {"turns": ["user1", "user2", ...]} , ... ]  或  [ ["user1", "user2"], ... ]
//
// 只取 user 侧消息作为回放轮次；assistant 回复由被测服务实时生成（keep_assistant 决定是否进 history）。
package engine

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// TraceSession 是一个回放会话（按序的 user 消息列表）。
type TraceSession struct {
	UserTurns []string
}

// TraceSet 是加载后的会话集合。
type TraceSet struct {
	Sessions []TraceSession
	Source   string // 文件路径（报告追溯用）
	Format   string // 实际识别的格式
}

// LoadTrace 读取 trace 文件（.json / .json.gz），按 format 解析（""=自动识别）。
// minTurns 过滤掉 user 轮数不足的会话（多轮场景至少 2），maxSessions 限制总量（0=不限）。
func LoadTrace(path, format string, minTurns, maxSessions int) (*TraceSet, error) {
	if path == "" {
		return nil, fmt.Errorf("trace 模式需要 dataset.path")
	}
	if minTurns <= 0 {
		minTurns = 2
	}
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	sessions, detected, err := parseTrace(data, format)
	if err != nil {
		return nil, fmt.Errorf("parse trace %s: %w", path, err)
	}
	// 过滤轮数不足的会话
	kept := sessions[:0]
	for _, s := range sessions {
		if len(s.UserTurns) >= minTurns {
			kept = append(kept, s)
		}
	}
	if maxSessions > 0 && len(kept) > maxSessions {
		kept = kept[:maxSessions]
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("trace %s 过滤后没有可用会话（min_turns=%d）", path, minTurns)
	}
	return &TraceSet{Sessions: kept, Source: path, Format: detected}, nil
}

// Pick 第 i 个会话（越界回绕），空集合返回空会话。
func (t *TraceSet) Pick(i int) TraceSession {
	if t == nil || len(t.Sessions) == 0 {
		return TraceSession{}
	}
	return t.Sessions[i%len(t.Sessions)]
}

func readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		return io.ReadAll(io.LimitReader(gz, 512*1024*1024))
	}
	return io.ReadAll(io.LimitReader(f, 512*1024*1024))
}

func parseTrace(data []byte, format string) ([]TraceSession, string, error) {
	if format == "" || format == "sharegpt" {
		if s, err := parseShareGPT(data); err == nil && len(s) > 0 {
			return s, "sharegpt", nil
		}
		if format == "sharegpt" {
			return nil, "", fmt.Errorf("sharegpt 格式解析失败或没有会话")
		}
	}
	// format == "" 自动识别落到 sessions；或显式 sessions
	s, err := parseSessions(data)
	if err != nil {
		return nil, "", fmt.Errorf("无法识别 trace 格式（尝试 sharegpt 与 sessions 均失败）: %w", err)
	}
	return s, "sessions", nil
}

type sharegptTurn struct {
	From    string `json:"from"`
	Role    string `json:"role"`
	Value   string `json:"value"`
	Content string `json:"content"`
}

func (t sharegptTurn) isUser() bool {
	role := strings.ToLower(t.From)
	if role == "" {
		role = strings.ToLower(t.Role)
	}
	return role == "human" || role == "user"
}

func (t sharegptTurn) text() string {
	if t.Value != "" {
		return t.Value
	}
	return t.Content
}

type sharegptConv struct {
	Conversations []sharegptTurn `json:"conversations"`
	Messages      []sharegptTurn `json:"messages"` // 兼容变体
}

func parseShareGPT(data []byte) ([]TraceSession, error) {
	var convs []sharegptConv
	if err := json.Unmarshal(data, &convs); err != nil {
		return nil, err
	}
	out := make([]TraceSession, 0, len(convs))
	for _, c := range convs {
		turns := c.Conversations
		if len(turns) == 0 {
			turns = c.Messages
		}
		s := TraceSession{}
		for _, t := range turns {
			if t.isUser() && strings.TrimSpace(t.text()) != "" {
				s.UserTurns = append(s.UserTurns, t.text())
			}
		}
		if len(s.UserTurns) > 0 {
			out = append(out, s)
		}
	}
	return out, nil
}

type sessionsFile struct {
	Turns []string `json:"turns"`
}

func parseSessions(data []byte) ([]TraceSession, error) {
	// 形态 1：[{"turns": [...]}, ...]
	var objs []sessionsFile
	if err := json.Unmarshal(data, &objs); err == nil && len(objs) > 0 && len(objs[0].Turns) > 0 {
		out := make([]TraceSession, 0, len(objs))
		for _, o := range objs {
			if len(o.Turns) > 0 {
				out = append(out, TraceSession{UserTurns: o.Turns})
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	// 形态 2：[["u1","u2"], ...]
	var arrs [][]string
	if err := json.Unmarshal(data, &arrs); err == nil && len(arrs) > 0 {
		out := make([]TraceSession, 0, len(arrs))
		for _, a := range arrs {
			if len(a) > 0 {
				out = append(out, TraceSession{UserTurns: a})
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("sessions 格式不合法")
}
