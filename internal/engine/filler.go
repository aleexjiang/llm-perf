package engine

import (
	"fmt"
	"math/rand"
	"strings"

	"github.com/aleexjiang/llm-perf/internal/corpus"
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
// seed 相同则文本相同（用于前缀缓存测试）；seed 不同则文本不同（避免伪缓存命中）。
// 若该语言注册了语料（LoadCorpus），优先使用真实文本窗口——
// 自然文本的 tokenization 与语义分布都比随机词表更贴近真实负载。
//
// **长度口径按字符控制，两条路径共用同一个 chars/token 系数**（corpus.CharsPerToken）：
// 目标字符数 = targetTokens × charsPerToken。理由：英文 BPE 的「字符/token」是稳定量
// （≈4.0，cl100k / Qwen 系 / 与本地实测均吻合），而「词/token」随词表任意波动——
// 早期按 1.33 词/token（假设 1 词 ≈ 0.75 token，方向性错误）构造，真机实测 500tk 档
// 实际发出 1246 token（3.99 chars/token，偏差 2.49×），且与语料路径口径不一致
// （同一档位切语料会得到长度差 2.5× 的负载，受控变量失效）。
//
// 该系数仍是近似值（随 tokenizer/内容形状波动）：probe 的 filler_fidelity 检查会实测
// 本部署的真实偏差并告警；报告横轴一律以服务端 usage.prompt_tokens 实测中位为准，
// 不信构造侧的标称值（见 scripts/gen_html_report.py 的实测分箱）。
func Filler(targetTokens int, seed int64, lang string) string {
	if w := corpusWindow(targetTokens, seed, lang); w != "" {
		return w
	}
	if targetTokens <= 0 {
		return ""
	}
	targetChars := int(float64(targetTokens) * corpus.CharsPerToken(lang))
	if targetChars <= 0 {
		return ""
	}
	rng := rand.New(rand.NewSource(seed))
	switch lang {
	case "zh":
		var b strings.Builder
		for b.Len() < targetChars*3 { // UTF-8 中文每字 3 字节（写超再按 rune 截）
			b.WriteString(zhSentences[rng.Intn(len(zhSentences))])
		}
		// 按字节截断可能截断多字节字符，按 rune 截
		runes := []rune(b.String())
		if len(runes) > targetChars {
			runes = runes[:targetChars]
		}
		return string(runes)
	default: // en
		// 追加到字符数达标（最后可能多一个词，误差 < 1 词 ≈ 8 字符）
		var b strings.Builder
		for i := 0; b.Len() < targetChars; i++ {
			b.WriteString(enWords[rng.Intn(len(enWords))])
			if (i+1)%12 == 0 { // 按句号分组提升"自然度"
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
