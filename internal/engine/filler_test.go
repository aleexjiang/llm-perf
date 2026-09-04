package engine

import (
	"strings"
	"testing"
)

// 固定 seed 必须产出完全相同的文本——前缀缓存实验（fixed_seed: true）依赖这一点
func TestFiller_Deterministic(t *testing.T) {
	a := Filler(5000, 42, "en")
	b := Filler(5000, 42, "en")
	if a != b {
		t.Fatal("same seed produced different text — fixed_seed 缓存实验会失效")
	}
	if Filler(5000, 43, "en") == a {
		t.Fatal("different seed produced identical text — 并发实验会伪缓存命中")
	}
	if Filler(2000, 42, "zh") != Filler(2000, 42, "zh") {
		t.Fatal("zh filler not deterministic")
	}
}

// 生成量与目标 token 数的近似度（宽度上限内，防大档位内存/时长意外）
func TestFiller_TokenApproximation(t *testing.T) {
	// en: 1 word ≈ 0.75 token → target*1.33 词
	en := Filler(10000, 7, "en")
	words := len(strings.Fields(en))
	if words < 12000 || words > 15000 {
		t.Errorf("en 10000tk → %d words, want ~13300", words)
	}
	// zh: 1 字 ≈ 1 token
	zh := Filler(5000, 7, "zh")
	runes := len([]rune(zh))
	if runes < 4800 || runes > 5200 {
		t.Errorf("zh 5000tk → %d runes, want ~5000", runes)
	}
}

func BenchmarkFiller100k(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Filler(102400, 1000, "en")
	}
}
