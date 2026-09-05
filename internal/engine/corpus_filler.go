package engine

import (
	"fmt"

	"github.com/aleexjiang/llm-perf/internal/corpus"
)

// 语料注册表：main 启动时 LoadCorpus 注册，Filler 生成时优先走语料窗口。
// 场景层不需要感知 corpus 的存在——签名与调用方式都不变。
var corpusRegistry = map[string]*corpus.Corpus{}

// LoadCorpus 加载语料。spec 为 "en"/"zh"（内置公版书）或自定义文件路径（.txt/.txt.gz）。
// lang 是该语料对应的填充语言（与 config.filler_lang 匹配后才生效）。
func LoadCorpus(spec, lang string) error {
	if err := corpus.Load(spec, lang); err != nil {
		return err
	}
	corpusRegistry[lang] = corpus.Loaded(lang)
	return nil
}

// UnloadCorpus 注销语料（测试隔离用），Filler 回退合成词表。
func UnloadCorpus(lang string) {
	delete(corpusRegistry, lang)
}

// CorpusInfo 返回已注册语料的信息（日志用）。
func CorpusInfo(lang string) string {
	c := corpusRegistry[lang]
	if c == nil {
		return ""
	}
	return fmt.Sprintf("corpus[%s] %d 字符 ≈ %d token", lang, c.Len(), int(float64(c.Len())/corpus.CharsPerToken(lang)))
}

// corpusWindow 若指定语言已注册语料则返回文本窗口，否则返回空串（回退合成填充）。
func corpusWindow(targetTokens int, seed int64, lang string) string {
	c := corpusRegistry[lang]
	if c == nil {
		return ""
	}
	targetChars := int(float64(targetTokens) * corpus.CharsPerToken(lang))
	return c.Window(targetChars, seed)
}
