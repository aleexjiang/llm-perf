package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sharegptFixture = `[
  {"conversations":[
    {"from":"system","value":"sys-a"},
    {"from":"human","value":"q1"},
    {"from":"gpt","value":"a1"},
    {"from":"human","value":"q2"}
  ]},
  {"conversations":[
    {"from":"human","value":"只问一句"},
    {"from":"gpt","value":"答"}
  ]},
  {"conversations":[
    {"from":"gpt","value":"没有 user 的会话应被跳过"}
  ]}
]`

func writeShareGPT(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sharegpt.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadShareGPTRequests(t *testing.T) {
	samples, err := LoadShareGPTRequests(writeShareGPT(t, sharegptFixture), 0, 42, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 2 条有效快照（无 user 的会话被跳过）
	if len(samples) != 2 {
		t.Fatalf("快照数 = %d, want 2", len(samples))
	}
	// 按 ID 定位（seed 洗牌后顺序不确定）：快照 1 截至最后一条 user（q2），
	// 不含其后的 assistant；输出预算 = a1 的估算
	var s *RequestSample
	for i := range samples {
		if samples[i].ID == "sharegpt-0001" {
			s = &samples[i]
		}
	}
	if s == nil {
		t.Fatal("缺少 sharegpt-0001")
	}
	if len(s.Messages) != 4 {
		t.Fatalf("快照结构错误: %+v", s)
	}
	if last := s.Messages[len(s.Messages)-1]; last.Role != "user" || last.Content != "q2" {
		t.Fatalf("最后一条应为 user q2: %+v", last)
	}
	if s.OutputTokens <= 0 {
		t.Fatalf("输出预算应 >0: %d", s.OutputTokens)
	}
}

func TestLoadShareGPTRequestsSampling(t *testing.T) {
	p := writeShareGPT(t, sharegptFixture)
	// 同 seed 同样本序（跨工具可比的前提）
	a, _ := LoadShareGPTRequests(p, 10, 42, 0)
	b, _ := LoadShareGPTRequests(p, 10, 42, 0)
	if len(a) != 10 { // 样本不足确定性回绕
		t.Fatalf("回绕后应 10 条: %d", len(a))
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Fatalf("同 seed 样本序不一致 at %d", i)
		}
	}
	// 输出预算上限
	c, _ := LoadShareGPTRequests(p, 10, 42, 1)
	for _, s := range c {
		if s.OutputTokens != 1 {
			t.Fatalf("max_output_tokens 裁剪失败: %d", s.OutputTokens)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	en := strings.Repeat("word ", 100) // 500 chars
	if got := EstimateTokens(en); got < 100 || got > 200 {
		t.Fatalf("英文估算异常: %d", got)
	}
	zh := strings.Repeat("测", 140)
	if got := EstimateTokens(zh); got < 80 || got > 120 {
		t.Fatalf("中文估算异常: %d", got)
	}
	if EstimateTokens("") != 0 {
		t.Fatal("空文本应为 0")
	}
}
