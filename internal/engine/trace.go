// trace.go：真实负载回放（trace 模式）。filler 模式做 token 精确的变量控制实验
// （缓存对照、上下文深度阶梯），trace 模式回放真实会话分布——贴近客户实际流量形状。
//
// 支持两种格式（按文件内容自动识别，显式 format 可覆盖）：
//   - sharegpt：vLLM/SGLang 基准通用数据集格式
//     [ {"conversations": [{"from":"human","value":"..."}, {"from":"gpt","value":"..."}]} , ... ]
//     （兼容 "role"/"content" 键名与 user/assistant 取值）
//   - sessions：本工具自定义精简格式
//     [ {"turns": ["user1", "user2", ...]} , ... ]  或  [ ["user1", "user2"], ... ]
//
// 回放保真度（dataset.replay_mode）：
//   - user_only（默认）：只取 user 侧消息作为回放轮次，assistant 回复由被测服务实时生成——
//     与旧行为完全兼容，但真实会话里的 assistant/tool 消息（含大段工具结果）不进上下文，
//     回放上下文系统性偏小
//   - full：按原序注入全部 role（user / assistant / tool / system），第 i 轮请求 =
//     完整消息序列到第 i 条 user 消息为止的前缀——测的是真实 history 深度下的增量 prefill
package engine

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// TraceMessage 是 full 回放模式的一条原始消息（原序、原 role）。
type TraceMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// TraceSession 是一个回放会话。
type TraceSession struct {
	UserTurns []string       // user_only 视图：按序的 user 消息（兼容既有场景代码）
	Messages  []TraceMessage // full 视图：原序全部 role（replay_mode=full 时使用）
}

// TraceSet 是加载后的会话集合。
type TraceSet struct {
	Sessions []TraceSession
	Source   string // 文件路径（报告追溯用）
	Format   string // 实际识别的格式
	// FullReplay load 时的回放模式（true = 已填充 Messages）
	FullReplay bool
	// MissingToolCallID full 模式下 role=tool 但缺 tool_call_id 被跳过的消息数（不静默丢弃）
	MissingToolCallID int
	// NoAssistantContent 数据源不含 assistant/tool 内容（如 sessions 格式只有 user 轮），
	// full 模式下 Messages 退化为 user 序列——回放深度与 user_only 相同
	NoAssistantContent bool
}

// maxTraceFileBytes 单文件内存上限：工具全量载入 trace（非流式），超限直接给出可行动的错误。
const maxTraceFileBytes = 512 << 20

// LoadTrace 读取 trace 文件（.json / .json.gz），按 format 解析（""=自动识别）。
// replayMode 为 "full" 时按原序保留全部 role；minTurns 过滤掉 user 轮数不足的会话
// （多轮场景至少 2），maxSessions 限制总量（0=不限）。
func LoadTrace(path, format, replayMode string, minTurns, maxSessions int) (*TraceSet, error) {
	if path == "" {
		return nil, fmt.Errorf("trace 模式需要 dataset.path")
	}
	if minTurns <= 0 {
		minTurns = 2
	}
	full := replayMode == "full"
	// 非压缩文件先查大小：超限时给出可行动的错误，而不是截断后报晦涩的 JSON 解析失败
	if !strings.HasSuffix(path, ".gz") {
		if fi, statErr := os.Stat(path); statErr == nil && fi.Size() > maxTraceFileBytes {
			return nil, fmt.Errorf("trace 文件 %s 有 %.0fMB，超过 %dMB 内存上限（工具全量载入）——请预先切分或调低 dataset.max_sessions",
				path, float64(fi.Size())/(1<<20), maxTraceFileBytes/(1<<20))
		}
	}
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	sessions, detected, err := parseTrace(data, format, full)
	if err != nil {
		return nil, fmt.Errorf("parse trace %s: %w", path, err)
	}
	set := &TraceSet{Sessions: sessions, Source: path, Format: detected, FullReplay: full}
	// 过滤轮数不足的会话
	kept := sessions[:0]
	for _, s := range sessions {
		if len(s.UserTurns) >= minTurns {
			kept = append(kept, s)
		}
	}
	set.Sessions = kept
	if maxSessions > 0 && len(kept) > maxSessions {
		set.Sessions = kept[:maxSessions]
	}
	if len(set.Sessions) == 0 {
		return nil, fmt.Errorf("trace %s 过滤后没有可用会话（min_turns=%d）", path, minTurns)
	}
	// full 模式数据质量统计：tool_call_id 缺失已按消息跳过（不可静默）；assistant 缺失要如实告知
	if full {
		missing := 0
		for si := range set.Sessions {
			kept := set.Sessions[si].Messages[:0]
			for _, m := range set.Sessions[si].Messages {
				if m.Role == "tool" && strings.TrimSpace(m.ToolCallID) == "" {
					missing++ // 跳过并计数，不静默丢弃
					continue
				}
				kept = append(kept, m)
			}
			set.Sessions[si].Messages = kept
		}
		set.MissingToolCallID = missing
		noAssistant := true
		for _, s := range set.Sessions {
			for _, m := range s.Messages {
				if m.Role != "user" {
					noAssistant = false
				}
			}
		}
		set.NoAssistantContent = noAssistant
	}
	return set, nil
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

func parseTrace(data []byte, format string, full bool) ([]TraceSession, string, error) {
	if format == "" || format == "sharegpt" {
		if s, err := parseShareGPT(data, full); err == nil && len(s) > 0 {
			return s, "sharegpt", nil
		}
		if format == "sharegpt" {
			return nil, "", fmt.Errorf("sharegpt 格式解析失败或没有会话")
		}
	}
	// format == "" 自动识别落到 sessions；或显式 sessions
	s, err := parseSessions(data)
	if err != nil {
		return nil, "", fmt.Errorf("无法识别 trace 格式（尝试 sharegpt 与 sessions 均失败）: %w（若文件接近 512MB 内存上限，可能已被截断——请切分）", err)
	}
	return s, "sessions", nil
}

type sharegptTurn struct {
	From       string `json:"from"`
	Role       string `json:"role"`
	Value      string `json:"value"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id"`
}

func (t sharegptTurn) role() string {
	r := strings.ToLower(t.From)
	if r == "" {
		r = strings.ToLower(t.Role)
	}
	return r
}

func (t sharegptTurn) isUser() bool { return t.role() == "human" || t.role() == "user" }

// normRole ShareGPT 词表 → OpenAI role（human→user、gpt/bot→assistant、
// system→system、function/tool/observation→tool；其余原样保留并视为 assistant 兜底）。
func (t sharegptTurn) normRole() string {
	switch r := t.role(); r {
	case "human", "user":
		return "user"
	case "gpt", "bot", "chatgpt", "assistant":
		return "assistant"
	case "system":
		return "system"
	case "function", "tool", "observation":
		return "tool"
	default:
		return "assistant"
	}
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

func parseShareGPT(data []byte, full bool) ([]TraceSession, error) {
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
			if strings.TrimSpace(t.text()) == "" {
				continue
			}
			if t.isUser() {
				s.UserTurns = append(s.UserTurns, t.text())
			}
			if full {
				s.Messages = append(s.Messages, TraceMessage{
					Role: t.normRole(), Content: t.text(), ToolCallID: t.ToolCallID,
				})
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
