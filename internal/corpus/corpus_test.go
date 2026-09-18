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

// ── 语料库（books）：12 本公版书，user 模式文本原料 ──

func TestLibraryLoaded(t *testing.T) {
	for lang, want := range map[string]int{"en": 7, "zh": 5} {
		bs := Library(lang)
		if len(bs) != want {
			t.Fatalf("%s 书目数 %d ≠ %d", lang, len(bs), want)
		}
		total := 0
		for _, b := range bs {
			if b.ID == "" || b.Title == "" || b.Len() < 50_000 {
				t.Fatalf("书目元数据异常: %+v len=%d", b, b.Len())
			}
			total += b.EstimateTokens()
		}
		t.Logf("%s: %d books, ≈%d tokens", lang, len(bs), total)
	}
	if Library("xx") != nil {
		t.Fatal("未知语言应返回 nil")
	}
}

func TestSelectBookDeterministic(t *testing.T) {
	a := SelectBook("en", 42)
	b := SelectBook("en", 42)
	if a == nil || a.ID != b.ID {
		t.Fatalf("同 seed 应命中同一本: %v vs %v", a, b)
	}
	// 覆盖率：足够多的 seed 应命中多本书（不是永远同一本）
	seen := map[string]bool{}
	for s := int64(0); s < 200; s++ {
		seen[SelectBook("en", s).ID] = true
	}
	if len(seen) < 5 {
		t.Fatalf("200 个 seed 只命中 %d 本，选书分布异常", len(seen))
	}
	if SelectBook("xx", 1) != nil {
		t.Fatal("未知语言选书应返回 nil")
	}
}

func TestBookWindow(t *testing.T) {
	b := SelectBook("zh", 7)
	if b == nil {
		t.Fatal("zh 书库不应为空")
	}
	w := b.Window(500, 7)
	if len([]rune(w)) != 500 || !utf8.ValidString(w) {
		t.Fatalf("Book.Window 结果异常: len=%d", len([]rune(w)))
	}
	if b.Window(500, 7) != w {
		t.Fatal("Book.Window 同 seed 不同结果，确定性被破坏")
	}
	if b.Window(b.Len()*2+10, 3) == "" {
		t.Fatal("循环回绕不应返回空")
	}
}
