// Package corpus 提供内置真实文本语料，用于生成贴近自然文本分布的长上下文填充。
//
// 语料来源（均为公版/公开文本，随仓库分发、go:embed 打进二进制）：
//   - en: War and Peace + Moby Dick（Project Gutenberg #2600 / #2700，约 99 万 token）
//   - zh: 红楼梦（曹雪芹·高鹗，120 回全文，约 62 万 token，超出部分循环填充）
//
// 也支持通过文件路径加载自定义语料（纯文本或 gzip）。
//
// 语料库（books，12 本公版书，user 模式生成器用）：
//   - 一用户一书：book_index = hash(session_seed) % len(books)，确定性选书；
//   - 见 SelectBook / Library；manifest 见 data/books/manifest.json。
package corpus

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

//go:embed data/corpus-en.txt.gz data/corpus-zh.txt.gz
var embedded embed.FS

//go:embed data/books/manifest.json
var manifestFS embed.FS

//go:embed data/books/en/*.gz data/books/zh/*.gz
var booksFS embed.FS

// CharsPerToken 不同语言自然文本的近似字符/token 比（用于把目标 token 数换算成字符数）。
// 实际 token 数以服务端 usage 为准，这里只做生成侧的近似。
func CharsPerToken(lang string) float64 {
	if lang == "zh" {
		return 1.4 // 中文文学文本约 1.2~1.5 字符/token
	}
	return 4.0 // 英文散文（含空格标点）约 4 字符/token
}

// ── 旧接口（filler 遗留路径；filler 从正式压测下线后将一并移除） ──

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
	data, err := readCorpusBytes(spec)
	if err != nil {
		return err
	}
	text, err := decodeText(data)
	if err != nil {
		return err
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
	return window(c.text, targetChars, seed)
}

func readCorpusBytes(spec string) ([]byte, error) {
	switch spec {
	case "en", "zh":
		f := "data/corpus-" + spec + ".txt.gz"
		data, err := embedded.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("corpus: 内置语料缺失 %s: %w", f, err)
		}
		return data, nil
	default:
		data, err := os.ReadFile(spec)
		if err != nil {
			return nil, fmt.Errorf("corpus: 读语料文件: %w", err)
		}
		return data, nil
	}
}

func decodeText(data []byte) (string, error) {
	// gzip 魔数 1f 8b
	if len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b {
		zr, zerr := gzip.NewReader(bytes.NewReader(data))
		if zerr != nil {
			return "", fmt.Errorf("corpus: gzip 解压失败: %w", zerr)
		}
		var err error
		data, err = io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			return "", fmt.Errorf("corpus: gzip 读取失败: %w", err)
		}
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("corpus: 语料不是合法 UTF-8（自定义语料请先转码）")
	}
	text := strings.TrimSpace(string(data))
	if len(text) == 0 {
		return "", fmt.Errorf("corpus: 语料为空")
	}
	return text, nil
}

// window 确定性取窗：seed 决定起点（负 seed 规范化），超出长度循环回绕。
// 步长取素数避免与常见档位产生周期性对齐。
func window(text []rune, targetChars int, seed int64) string {
	n := len(text)
	if n == 0 || targetChars <= 0 {
		return ""
	}
	seedMod := seed % int64(n)
	if seedMod < 0 {
		seedMod += int64(n)
	}
	start := int((seedMod * 7919) % int64(n))
	out := make([]rune, 0, targetChars)
	if targetChars <= n-start {
		out = append(out, text[start:start+targetChars]...)
	} else {
		out = append(out, text[start:]...)
		for len(out) < targetChars {
			take := targetChars - len(out)
			if take > n {
				take = n
			}
			out = append(out, text[:take]...)
		}
	}
	return string(out)
}

// ── 语料库（books）：12 本公版书，user 模式生成器的文本原料 ──

// Book 语料库中的一本书（rune 切片，多字节安全）。
type Book struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Lang  string `json:"lang"`
	text  []rune
}

// Len 书的 rune 长度。
func (b *Book) Len() int { return len(b.text) }

// Window 从书中取约 targetChars 个字符的确定性文本窗口（语义与 Corpus.Window 一致）。
func (b *Book) Window(targetChars int, seed int64) string {
	return window(b.text, targetChars, seed)
}

// EstimateTokens 按 lang 的 CharsPerToken 估算书的 token 量（展示/日志用，非精确值）。
func (b *Book) EstimateTokens() int {
	return int(float64(len(b.text)) / CharsPerToken(b.Lang))
}

type bookManifestEntry struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Lang  string `json:"lang"`
	File  string `json:"file"`
	Chars int    `json:"chars"`
}

var (
	libraryOnce sync.Once
	libraryErr  error
	library     = map[string][]*Book{} // lang -> books（按 ID 排序，保证选书确定性）
)

// loadLibrary 惰性加载语料库 manifest 与全部书目（一次性解析进内存，进程内复用）。
func loadLibrary() error {
	libraryOnce.Do(func() {
		mf, err := manifestFS.ReadFile("data/books/manifest.json")
		if err != nil {
			libraryErr = fmt.Errorf("corpus: 语料库 manifest 缺失: %w", err)
			return
		}
		var entries []bookManifestEntry
		if err := json.Unmarshal(mf, &entries); err != nil {
			libraryErr = fmt.Errorf("corpus: 语料库 manifest 解析失败: %w", err)
			return
		}
		for _, e := range entries {
			data, err := booksFS.ReadFile("data/books/" + e.File)
			if err != nil {
				libraryErr = fmt.Errorf("corpus: 书目缺失 %s: %w", e.File, err)
				return
			}
			text, err := decodeText(data)
			if err != nil {
				libraryErr = fmt.Errorf("corpus: 书目解码失败 %s: %w", e.File, err)
				return
			}
			library[e.Lang] = append(library[e.Lang], &Book{ID: e.ID, Title: e.Title, Lang: e.Lang, text: []rune(text)})
		}
		for lang := range library {
			bs := library[lang]
			sort.Slice(bs, func(i, j int) bool { return bs[i].ID < bs[j].ID })
		}
	})
	return libraryErr
}

// Library 返回某语言的全部已装订书目（按 ID 排序，只读）。
// lang 不合法或库为空时返回 nil。
func Library(lang string) []*Book {
	if err := loadLibrary(); err != nil {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	return library[lang]
}

// SelectBook 确定性选书：同一 seed 恒定命中同一本（一用户一书语义）。
// 书单为空返回 nil。
func SelectBook(lang string, seed int64) *Book {
	bs := Library(lang)
	if len(bs) == 0 {
		return nil
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte{byte('b'), byte('o'), byte('o'), byte('k')})
	var buf [8]byte
	u := uint64(seed)
	for i := 0; i < 8; i++ {
		buf[i] = byte(u >> (8 * i))
	}
	_, _ = h.Write(buf[:])
	return bs[h.Sum64()%uint64(len(bs))]
}
