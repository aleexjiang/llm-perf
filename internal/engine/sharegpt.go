// sharegpt.go：ShareGPT 数据集 → 冻结独立请求快照（rps/concurrency 模式的请求源）。
//
// 与 vLLM `bench serve --dataset-name sharegpt` 的口径对齐：
//   - prompt = 会话截至最后一条 user 的完整 history（chat messages）；
//   - 输出预算 = 其后 assistant 回复的估算 token 数；
//   - 按 seed 蓄水池抽样 num_prompts 条（同 seed 同样本，跨 run 可复现）；
//   - 每条请求相互独立——不运行多轮会话，历史内容在测试前冻结。
//
// 解析为**流式两遍**：真实 ShareGPT 数据集 >600MB，全量 json.Unmarshal 会放大数倍内存；
// 第一遍蓄水池选样（只记序号），第二遍仅物化被选中的会话——内存峰值 = num_prompts 条快照。
package engine

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"unicode"
)

// RequestSample 一条冻结请求快照。
type RequestSample struct {
	ID           string    `json:"id"`            // 溯源：数据集内会话序号（如 sharegpt-0042）
	Messages     []Message `json:"messages"`      // 截至（含）最后一条 user 的完整 history
	OutputTokens int       `json:"output_tokens"` // 输出预算：其后 assistant 回复的估算 token
}

// EstimateTokens 文本 token 估算：CJK 占比 >30% 按 1.4 字符/token，否则按 4.0（英文散文）。
// 仅用于输出预算与形状估算，实际 token 以服务端 usage 为准。
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	runes, cjk := 0, 0
	for _, r := range text {
		runes++
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hangul, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hiragana, r) {
			cjk++
		}
	}
	cpr := 4.0
	if runes > 0 && float64(cjk)/float64(runes) > 0.3 {
		cpr = 1.4
	}
	return int(float64(runes) / cpr)
}

// LoadShareGPTRequests 流式读取 ShareGPT 数据集（.json/.json.gz），蓄水池抽样
// numPrompts 条独立请求快照。seed 决定选样（同 seed 同样本；样本不足时选中集回绕补足）。
func LoadShareGPTRequests(path string, numPrompts int, seed int64, maxOutputTokens int) ([]RequestSample, error) {
	if numPrompts <= 0 {
		return nil, fmt.Errorf("request_set.num_prompts 必须 >0（得到 %d）", numPrompts)
	}
	return loadShareGPTTwoPass(path, numPrompts, rand.New(rand.NewSource(seed)), maxOutputTokens)
}

// sharegptTurn ShareGPT 词表的一条消息（兼容 role/content 与 from/value 两种键名变体）。
type sharegptTurn struct {
	From    string `json:"from"`
	Role    string `json:"role"`
	Value   string `json:"value"`
	Content string `json:"content"`
}

func (t sharegptTurn) role() string {
	r := strings.ToLower(t.From)
	if r == "" {
		r = strings.ToLower(t.Role)
	}
	return r
}

func (t sharegptTurn) isUser() bool { return t.role() == "human" || t.role() == "user" }

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

// sharegptStreamFn 返回 false 表示提前终止流。
type sharegptStreamFn func(ordinal int, conv *sharegptConv) bool

// streamShareGPT 流式遍历顶层 JSON 数组中的每个会话对象（内存只驻留单个会话）。
func streamShareGPT(path string, fn sharegptStreamFn) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("sharegpt 数据集打开失败: %w", err)
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	var r io.Reader = br
	// gzip 魔数 1f 8b（Peek 不消费）
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("sharegpt gzip 解压失败: %w", err)
		}
		defer zr.Close()
		r = zr
	}
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("sharegpt 解析失败（非 JSON）: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("sharegpt 顶层应为数组")
	}
	ordinal := 0
	for dec.More() {
		var c sharegptConv
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("sharegpt 第 %d 条解析失败: %w", ordinal+1, err)
		}
		if !fn(ordinal, &c) {
			return nil
		}
		ordinal++
	}
	return nil
}

// lastUserIdx 返回最后一条非空 user 的下标；-1 = 无 user。
func lastUserIdx(turns []sharegptTurn) int {
	last := -1
	for i, t := range turns {
		if t.isUser() && strings.TrimSpace(t.text()) != "" {
			last = i
		}
	}
	return last
}

// buildSample 把一条会话物化为冻结快照（截至最后一条 user；输出预算取其后 assistant）。
func buildSample(ordinal int, turns []sharegptTurn, lastUser, maxOutputTokens int) RequestSample {
	s := RequestSample{ID: fmt.Sprintf("sharegpt-%04d", ordinal+1)}
	for i := 0; i <= lastUser; i++ {
		t := turns[i]
		if strings.TrimSpace(t.text()) == "" {
			continue
		}
		role := "assistant"
		switch {
		case t.isUser():
			role = "user"
		case t.role() == "system":
			role = "system"
		}
		s.Messages = append(s.Messages, Message{Role: role, Content: t.text()})
	}
	if lastUser+1 < len(turns) {
		s.OutputTokens = EstimateTokens(turns[lastUser+1].text())
	}
	if s.OutputTokens <= 0 {
		s.OutputTokens = 16 // 无 assistant 回复的最小输出预算
	}
	if maxOutputTokens > 0 && s.OutputTokens > maxOutputTokens {
		s.OutputTokens = maxOutputTokens
	}
	return s
}

// loadShareGPTTwoPass 两遍流式：pass1 蓄水池记序号，pass2 物化选中会话。
func loadShareGPTTwoPass(path string, numPrompts int, rng *rand.Rand, maxOutputTokens int) ([]RequestSample, error) {
	// pass 1：对“有效会话流”（有 user 的）做蓄水池抽样，记录有效序号
	reservoir := []int{} // 选中项的有效序号（0 基）
	validSeen := 0
	err := streamShareGPT(path, func(_ int, conv *sharegptConv) bool {
		turns := conv.Conversations
		if len(turns) == 0 {
			turns = conv.Messages
		}
		if lastUserIdx(turns) < 0 {
			return true
		}
		i := validSeen
		validSeen++
		if len(reservoir) < numPrompts {
			reservoir = append(reservoir, i)
		} else if j := rng.Intn(i + 1); j < numPrompts {
			reservoir[j] = i
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	if validSeen == 0 {
		return nil, fmt.Errorf("sharegpt 数据集没有可用会话: %s", path)
	}
	selected := map[int]bool{}
	for _, i := range reservoir {
		selected[i] = true
	}

	// pass 2：物化被选中的会话（保持蓄水池顺序 → 确定性样本序）
	byValid := make([]RequestSample, len(reservoir))
	filled := make([]bool, len(reservoir))
	pos := map[int]int{} // 有效序号 → reservoir 下标
	for k, i := range reservoir {
		pos[i] = k
	}
	seen := 0 // pass2 独立的有效会话计数（不能复用 pass1 的 validSeen）
	err = streamShareGPT(path, func(_ int, conv *sharegptConv) bool {
		turns := conv.Conversations
		if len(turns) == 0 {
			turns = conv.Messages
		}
		lastUser := lastUserIdx(turns)
		if lastUser < 0 {
			return true
		}
		if k, ok := pos[seen]; ok && !filled[k] {
			byValid[k] = buildSample(seen, turns, lastUser, maxOutputTokens)
			filled[k] = true
		}
		seen++
		return true
	})
	if err != nil {
		return nil, err
	}
	var pool []RequestSample
	for k := range reservoir {
		if filled[k] {
			pool = append(pool, byValid[k])
		}
	}
	if len(pool) == 0 {
		return nil, fmt.Errorf("sharegpt 数据集没有可用会话: %s", path)
	}
	// 样本不足：选中集确定性回绕补足
	out := make([]RequestSample, 0, numPrompts)
	for i := 0; i < numPrompts; i++ {
		s := pool[i%len(pool)]
		if i >= len(pool) {
			s.ID = fmt.Sprintf("%s#r%d", s.ID, i/len(pool))
		}
		out = append(out, s)
	}
	return out, nil
}
