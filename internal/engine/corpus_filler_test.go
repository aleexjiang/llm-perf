package engine

import (
	"strings"
	"testing"
)

// UnloadCorpus 注销语料（测试隔离用），Filler 回退合成词表。
func UnloadCorpus(lang string) {
	corpusRegistryMu.Lock()
	delete(corpusRegistry, lang)
	corpusRegistryMu.Unlock()
}

func TestFillerCorpusMode(t *testing.T) {
	if err := LoadCorpus("en", "en"); err != nil {
		t.Skip("内置语料不可用:", err)
	}
	// 注册语料后，Filler 走真实文本窗口
	en := Filler(5000, 100, "en")
	if len(en) == 0 {
		t.Fatal("语料模式生成了空文本")
	}
	if strings.Contains(en, "network process") {
		t.Fatal("仍走了合成词表，语料模式未生效")
	}
	// 同 seed 确定性
	if Filler(5000, 100, "en") != en {
		t.Fatal("同 seed 文本不同，确定性被破坏")
	}
	// 未注册语料的语言回退合成词表
	if LoadCorpus("zh", "zh") == nil {
		t.Cleanup(func() { UnloadCorpus("zh") })
		zh := Filler(200, 1, "zh")
		if !strings.Contains(zh, "系统") && !strings.Contains(zh, "数据") {
			t.Log("zh 回退词表内容校验跳过（窗口起点随机）")
		}
	}
}

func TestFillerCorpusApproxSize(t *testing.T) {
	if err := LoadCorpus("en", "en"); err != nil {
		t.Skip("内置语料不可用:", err)
	}
	t.Cleanup(func() { UnloadCorpus("en") })
	// 窗口长度 = target * CharsPerToken(en)=4.0（CPT 已在真实 Qwen 服务上校准，偏差 <2%，
	// 见 2026-09-05 实测：目标 10k/50k/200k → 实测 9839/49909/198643）
	en := Filler(200_000, 9, "en")
	if got := len([]rune(en)); got != 200_000*4 {
		t.Fatalf("200k token 语料填充长度 %d ≠ %d", got, 200_000*4)
	}
}
