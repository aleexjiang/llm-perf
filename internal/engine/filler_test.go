package engine

import (
	"strings"
	"testing"

	"github.com/aleexjiang/llm-perf/internal/corpus"
)

func init() { // 保证合成词表测试不被语料注册表污染（Go 测试同包共享状态）
	UnloadCorpus("en")
	UnloadCorpus("zh")
}

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
	zhA := Filler(2000, 42, "zh")
	if zhA != Filler(2000, 42, "zh") {
		t.Fatal("zh filler not deterministic")
	}
	if Filler(2000, 43, "zh") == zhA {
		t.Fatal("zh filler ignored seed")
	}
}

// 生成量与目标 token 数的近似度（合成词表路径）：与语料路径共用 corpus.CharsPerToken
// 的字符/token 换算，断言锚在**字符数**上。
//
// 回归防护：早期按「1.33 词/token」（假设 1 词 ≈ 0.75 token，方向性错误）构造，真机实测
// 500tk 档实际发出 1246 token（3.99 chars/token，偏差 2.49×）。旧断言只数词数
// （12000~15000 词），换算系数错得再离谱也恒过——必须锚字符数才能抓住这类偏差。
func TestFiller_TokenApproximation(t *testing.T) {
	// en ≈4 字符/token：10000tk → ~40000 字符（追加式构造，末词最多溢出 ~8 字符）
	cptEn := corpus.CharsPerToken("en")
	en := Filler(10000, 7, "en")
	wantEn := float64(10000) * cptEn
	if got := float64(len(en)); got < wantEn*0.95 || got > wantEn*1.05 {
		t.Errorf("en 10000tk → %d 字符, want ~%.0f（%.1f chars/token）", len(en), wantEn, cptEn)
	}
	// 顺带守住"自然词流"形态：字符数达标的同时仍应是空格分词（防退化成无空格长串）
	if w := len(strings.Fields(en)); w < 4000 || w > 7000 {
		t.Errorf("en 10000tk → %d 词, 期望 ~5350（空格分隔的自然词流）", w)
	}
	// zh ≈1.4 字/token：5000tk → 7000 字（按 rune 精确截断）
	cptZh := corpus.CharsPerToken("zh")
	zh := Filler(5000, 7, "zh")
	if got, want := len([]rune(zh)), int(5000*cptZh); got != want {
		t.Errorf("zh 5000tk → %d 字, want %d（%.1f chars/token）", got, want, cptZh)
	}
	// 边界：非正目标返回空串（不产出无意义的单个词/句）
	if Filler(0, 7, "en") != "" || Filler(-1, 7, "zh") != "" {
		t.Error("targetTokens ≤ 0 应返回空串")
	}
}

// 两条填充路径（语料窗口 / 合成词表）必须共用同一长度口径：同一档位的字符数应落在同一
// 目标附近。否则"切语料"会变成换一个负载量级，受控变量实验失效——这正是早期合成路径
// 按词构造时的问题（比语料路径多发 2.49×）。
func TestFiller_PathScalesAgree(t *testing.T) {
	const tokens = 4000
	UnloadCorpus("en") // 先测合成路径
	synth := len(Filler(tokens, 7, "en"))
	if err := LoadCorpus("en", "en"); err != nil {
		t.Skip("内置语料不可用:", err)
	}
	t.Cleanup(func() { UnloadCorpus("en") })
	corp := len([]rune(Filler(tokens, 7, "en")))

	if ratio := float64(synth) / float64(corp); ratio < 0.9 || ratio > 1.1 {
		t.Errorf("同档位字符数 合成/语料 = %.2f（合成 %d vs 语料 %d）——两条路径的 chars/token 口径分叉",
			ratio, synth, corp)
	}
}

func BenchmarkFiller100k(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = Filler(102400, 1000, "en")
	}
}
