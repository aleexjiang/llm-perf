// sharegpt.go：ShareGPT 数据集 → 冻结独立请求快照（rps/concurrency 模式的请求源）。
//
// 与 vLLM `bench serve --dataset-name sharegpt` 的口径对齐：
//   - prompt = 会话截至最后一条 user 的完整 history（chat messages）；
//   - 输出预算 = 其后 assistant 回复的估算 token 数；
//   - 按 seed 随机抽样 num_prompts 条（样本不足时确定性回绕补足）；
//   - 每条请求相互独立——不运行多轮会话，历史内容在测试前冻结。
package engine

import (
	"bytes"
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
	ID           string    `json:"id"`            // 溯源：数据集内会话序号（如 sharegpt-042）
	Messages     []Message `json:"messages"`      // 截至（含）最后一条 user 的完整 history
	OutputTokens int       `json:"output_tokens"` // 输出预算：其后 assistant 回复的估算 token
}

// requestSampleCap 单请求输出预算的防御性上限（ShareGPT 存在超长回复；
// 0 = 不限制）。由调用方通过配置传入。
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

// LoadShareGPTRequests 读取 ShareGPT 数据集（.json/.json.gz），抽样 numPrompts 条独立请求快照。
// seed 决定抽样顺序（与 vLLM --seed 同语义：同 seed 同样本序）。
// 复用 trace.go 的 sharegptTurn/sharegptConv 解析（from/role、value/content 变体兼容）。
func LoadShareGPTRequests(path string, numPrompts int, seed int64, maxOutputTokens int) ([]RequestSample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("sharegpt 数据集读取失败: %w", err)
	}
	if len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b {
		zr, zerr := gzip.NewReader(bytes.NewReader(data))
		if zerr != nil {
			return nil, fmt.Errorf("sharegpt gzip 解压失败: %w", zerr)
		}
		data, err = io.ReadAll(io.LimitReader(zr, 1<<30))
		_ = zr.Close()
		if err != nil {
			return nil, err
		}
	}
	var convs []sharegptConv
	if err := json.Unmarshal(data, &convs); err != nil {
		return nil, fmt.Errorf("sharegpt 解析失败: %w", err)
	}

	// 展开为快照：每条会话在最后一条 user 处切一刀
	var pool []RequestSample
	for ci, c := range convs {
		turns := c.Conversations
		if len(turns) == 0 {
			turns = c.Messages
		}
		lastUser := -1
		for i, t := range turns {
			if t.isUser() && strings.TrimSpace(t.text()) != "" {
				lastUser = i
			}
		}
		if lastUser < 0 {
			continue
		}
		sample := RequestSample{ID: fmt.Sprintf("sharegpt-%04d", ci+1)}
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
			sample.Messages = append(sample.Messages, Message{Role: role, Content: t.text()})
		}
		if lastUser+1 < len(turns) {
			sample.OutputTokens = EstimateTokens(turns[lastUser+1].text())
		}
		if sample.OutputTokens <= 0 {
			sample.OutputTokens = 16 // 无 assistant 回复的最小输出预算
		}
		if maxOutputTokens > 0 && sample.OutputTokens > maxOutputTokens {
			sample.OutputTokens = maxOutputTokens
		}
		pool = append(pool, sample)
	}
	if len(pool) == 0 {
		return nil, fmt.Errorf("sharegpt 数据集没有可用会话: %s", path)
	}

	// seed 洗牌（Fisher-Yates，确定性）
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })

	if numPrompts <= 0 {
		numPrompts = len(pool)
	}
	out := make([]RequestSample, 0, numPrompts)
	for i := 0; i < numPrompts; i++ {
		s := pool[i%len(pool)] // 样本不足时确定性回绕
		if i >= len(pool) {
			s.ID = fmt.Sprintf("%s#r%d", s.ID, i/len(pool))
		}
		out = append(out, s)
	}
	return out, nil
}
