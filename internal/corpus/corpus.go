// Package corpus 提供内置真实文本语料，用于生成贴近自然文本分布的长上下文填充。
//
// 语料来源（均为公版/公开文本，随仓库分发、go:embed 打进二进制）：
//   - en: War and Peace + Moby Dick（Project Gutenberg #2600 / #2700，约 99 万 token）
//   - zh: 红楼梦（曹雪芹·高鹗，120 回全文，约 62 万 token，超出部分循环填充）
//
// 也支持通过文件路径加载自定义语料（纯文本或 gzip）。
package corpus

import (
	"bytes"
	"compress/gzip"
	"embed"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf8"
)

//go:embed data/corpus-en.txt.gz data/corpus-zh.txt.gz
var embedded embed.FS

// CharsPerToken 不同语言自然文本的近似字符/token 比（用于把目标 token 数换算成字符数）。
// 实际 token 数以服务端 usage 为准，这里只做生成侧的近似。
func CharsPerToken(lang string) float64 {
	if lang == "zh" {
		return 1.4 // 中文文学文本约 1.2~1.5 字符/token
	}
	return 4.0 // 英文散文（含空格标点）约 4 字符/token
}

// Corpus 一份已加载的语料（rune 切片，保证多字节安全切片）。
type Corpus struct {
	lang string
	text []rune
}

var (
	mu     sync.Mutex
	loaded = map[string]*Corpus{} // lang -> corpus
)

// Load 加载语料并注册到 lang 名下。
// spec 为 "en"/"zh" 时加载内置语料；否则视为文件路径（支持 .gz 与纯文本）。
// 重复 Load 同一 lang 会覆盖。
func Load(spec, lang string) error {
	if spec == "" || lang == "" {
		return fmt.Errorf("corpus: spec 与 lang 均不能为空")
	}
	var (
		data []byte
		err  error
	)
	switch spec {
	case "en", "zh":
		f := "data/corpus-" + spec + ".txt.gz"
		data, err = embedded.ReadFile(f)
		if err != nil {
			return fmt.Errorf("corpus: 内置语料缺失 %s: %w", f, err)
		}
	default:
		data, err = os.ReadFile(spec)
		if err != nil {
			return fmt.Errorf("corpus: 读语料文件: %w", err)
		}
	}
	// gzip 魔数 1f 8b
	if len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b {
		zr, zerr := gzip.NewReader(bytes.NewReader(data))
		if zerr != nil {
			return fmt.Errorf("corpus: gzip 解压失败: %w", zerr)
		}
		data, err = io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			return fmt.Errorf("corpus: gzip 读取失败: %w", err)
		}
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("corpus: 语料不是合法 UTF-8（自定义语料请先转码）")
	}
	text := strings.TrimSpace(string(data))
	if len(text) == 0 {
		return fmt.Errorf("corpus: 语料为空")
	}
	mu.Lock()
	loaded[lang] = &Corpus{lang: lang, text: []rune(text)}
	mu.Unlock()
	return nil
}

// Loaded 返回指定语言的已加载语料；未加载返回 nil。
func Loaded(lang string) *Corpus {
	mu.Lock()
	defer mu.Unlock()
	return loaded[lang]
}

// Len 语料 rune 长度。
func (c *Corpus) Len() int { return len(c.text) }

// Window 从语料中取约 targetChars 个字符的确定性文本窗口。
// seed 相同 → 窗口相同（前缀缓存实验可复现）；seed 不同 → 起点轮转不同。
// 窗口超出语料长度时循环回绕（内容重复，但对性能压测无影响）。
func (c *Corpus) Window(targetChars int, seed int64) string {
	n := len(c.text)
	if n == 0 || targetChars <= 0 {
		return ""
	}
	// 起点：seed 决定，步长取素数避免与常见档位产生周期性对齐
	seedMod := seed % int64(n)
	if seedMod < 0 {
		seedMod += int64(n)
	}
	start := int((seedMod * 7919) % int64(n))
	out := make([]rune, 0, targetChars)
	if targetChars <= n-start {
		out = append(out, c.text[start:start+targetChars]...)
	} else {
		out = append(out, c.text[start:]...)
		for len(out) < targetChars {
			take := targetChars - len(out)
			if take > n {
				take = n
			}
			out = append(out, c.text[:take]...)
		}
	}
	return string(out)
}
