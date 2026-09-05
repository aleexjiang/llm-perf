package engine

import (
	"fmt"
	"math/rand"
	"strings"
)

// 英文填充词表（低信息量、确定性可复现）。
var enWords = []string{
	"system", "network", "process", "result", "service", "request", "model",
	"buffer", "stream", "record", "device", "signal", "cluster", "storage",
	"memory", "session", "token", "query", "report", "sample", "feature",
	"pattern", "method", "object", "module", "packet", "latency", "throughput",
}

var zhSentences = []string{
	"系统需要处理大量的并发请求以保证服务质量。",
	"数据在各个模块之间流转并生成相应的统计报表。",
	"性能测试应当在接近真实生产环境的条件下进行。",
	"用户会话的上下文长度会随着对话轮次不断增长。",
	"缓存策略对推理服务的首字延迟有显著的影响。",
}

// Filler 生成近似 targetTokens 的确定性填充文本。
// lang: "en" 按词生成（约 1.33 word/token），"zh" 按字生成（约 1 char/token）。
// seed 相同则文本相同（用于前缀缓存测试）；seed 不同则文本不同（避免伪缓存命中）。
// 若该语言注册了语料（LoadCorpus），优先使用真实文本窗口——
// 自然文本的 tokenization 与语义分布都比随机词表更贴近真实负载。
func Filler(targetTokens int, seed int64, lang string) string {
	if w := corpusWindow(targetTokens, seed, lang); w != "" {
		return w
	}
	rng := rand.New(rand.NewSource(seed))
	switch lang {
	case "zh":
		need := targetTokens // 每字约 1 token
		var b strings.Builder
		for b.Len() < need*3 { // UTF-8 中文每字 3 字节
			b.WriteString(zhSentences[rng.Intn(len(zhSentences))])
		}
		// 按字节截断可能截断多字节字符，按 rune 截
		runes := []rune(b.String())
		if len(runes) > need {
			runes = runes[:need]
		}
		return string(runes)
	default: // en
		need := int(float64(targetTokens) * 1.33) // 1 word ≈ 0.75 token
		words := make([]string, 0, need)
		for i := 0; i < need; i++ {
			words = append(words, enWords[rng.Intn(len(enWords))])
		}
		// 按句号分组提升"自然度"
		var b strings.Builder
		for i, w := range words {
			b.WriteString(w)
			if (i+1)%12 == 0 {
				b.WriteString(". ")
			} else {
				b.WriteString(" ")
			}
		}
		return strings.TrimSpace(b.String())
	}
}

// UserMsg 构造一条指定近似 token 数的 user 消息。
func UserMsg(targetTokens int, seed int64, lang string) Message {
	return Message{Role: "user", Content: Filler(targetTokens, seed, lang)}
}

// SystemMsg 构造 system 消息，可包含模拟的 tool definitions 段落。
func SystemMsg(sysTokens, toolDefsTokens int, seed int64, lang string) Message {
	var b strings.Builder
	if sysTokens > 0 {
		b.WriteString("You are a helpful assistant. Follow the operating guidelines below.\n")
		b.WriteString(Filler(sysTokens, seed, lang))
	}
	if toolDefsTokens > 0 {
		b.WriteString("\n\n# Available tools\n")
		for i := 0; i < 10 && i < toolDefsTokens/200+1; i++ {
			b.WriteString(fmt.Sprintf("- tool_%d(name, params): perform operation %d on the target resource.\n", i, i))
		}
		b.WriteString(Filler(toolDefsTokens, seed+1, lang))
	}
	return Message{Role: "system", Content: b.String()}
}
