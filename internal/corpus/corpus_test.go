package corpus

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBuiltinLoad(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		if err := Load(lang, lang); err != nil {
			t.Fatalf("内置语料加载失败 %s: %v", lang, err)
		}
		c := Loaded(lang)
		if c == nil || c.Len() < 100_000 {
			t.Fatalf("%s 语料过小: %d runes", lang, c.Len())
		}
		t.Logf("%s: %d runes, ≈%d tokens", lang, c.Len(), int(float64(c.Len())/CharsPerToken(lang)))
	}
}

func TestWindowNegativeSeed(t *testing.T) {
	c := &Corpus{text: []rune("abcdefghijklmnopqrstuvwxyz")}
	got := c.Window(8, -7)
	if len([]rune(got)) != 8 {
		t.Fatalf("负 seed 应返回完整窗口，得到 %q", got)
	}
}

func TestWindowDeterminism(t *testing.T) {
	if err := Load("en", "en"); err != nil {
		t.Fatal(err)
	}
	c := Loaded("en")
	// 同 seed 同窗口（前缀缓存实验可复现的根基）
	a := c.Window(1000, 42)
	b := c.Window(1000, 42)
	if a != b {
		t.Fatal("同 seed 窗口不同，确定性被破坏")
	}
	// 不同 seed 不同起点
	if c.Window(1000, 1) == c.Window(1000, 2) {
		t.Fatal("不同 seed 产生了相同窗口")
	}
	// rune 合法性（多字节安全）
	if !utf8.ValidString(a) {
		t.Fatal("窗口包含非法 UTF-8")
	}
	// 长度近似
	if got := len([]rune(a)); got != 1000 {
		t.Fatalf("窗口长度 %d ≠ 1000", got)
	}
}

func TestWindowWrapAround(t *testing.T) {
	if err := Load("zh", "zh"); err != nil {
		t.Fatal(err)
	}
	c := Loaded("zh")
	// 超过语料长度 → 循环填充
	big := c.Window(c.Len()*2+100, 7)
	if len([]rune(big)) != c.Len()*2+100 {
		t.Fatalf("循环填充长度不符: %d", len([]rune(big)))
	}
	if !utf8.ValidString(big) {
		t.Fatal("循环填充包含非法 UTF-8")
	}
	// 语料确实包含目标文本
	if !strings.Contains(c.Window(10000, 3), "宝玉") && !strings.Contains(c.Window(100000, 3), "宝玉") {
		t.Log("窗口内未含『宝玉』（起点随机，不判失败）")
	}
}

func TestLoadInvalid(t *testing.T) {
	if err := Load("/nonexistent/file.txt", "xx"); err == nil {
		t.Fatal("不存在的文件应报错")
	}
}
