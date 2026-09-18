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
  ]},
  {"conversations":[
    {"from":"human","value":"c4"},
    {"from":"gpt","value":"r4"}
  ]},
  {"conversations":[
    {"from":"human","value":"c5"},
    {"from":"gpt","value":"r5"}
  ]},
  {"conversations":[
    {"from":"human","value":"c6"},
    {"from":"gpt","value":"r6"}
  ]},
  {"conversations":[
    {"from":"human","value":"c7"},
    {"from":"gpt","value":"r7"}
  ]}
]`

const sharegptFixtureBig = `[
  {"conversations":[{"from":"human","value":"a1"},{"from":"gpt","value":"b1"}]},
  {"conversations":[{"from":"human","value":"a2"},{"from":"gpt","value":"b2"}]},
  {"conversations":[{"from":"human","value":"a3"},{"from":"gpt","value":"b3"}]},
  {"conversations":[{"from":"human","value":"a4"},{"from":"gpt","value":"b4"}]},
  {"conversations":[{"from":"human","value":"a5"},{"from":"gpt","value":"b5"}]},
  {"conversations":[{"from":"human","value":"a6"},{"from":"gpt","value":"b6"}]}
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
	samples, err := LoadShareGPTRequests(writeShareGPT(t, sharegptFixture), 2, 42, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 {
		t.Fatalf("快照数 = %d, want 2", len(samples))
	}
	for _, s := range samples {
		// 无 user 的会话（原始 #3）不允许出现在任何 seed 的选样中
		if s.ID == "sharegpt-0003" {
			t.Fatalf("无 user 会话不应被选中: %s", s.ID)
		}
		if len(s.Messages) == 0 {
			t.Fatalf("快照无消息: %+v", s)
		}
		// 截至最后一条 user：不含其后 assistant
		if last := s.Messages[len(s.Messages)-1]; last.Role != "user" {
			t.Fatalf("快照最后一条应为 user: %+v", last)
		}
		if s.OutputTokens <= 0 {
			t.Fatalf("输出预算应 >0: %s %d", s.ID, s.OutputTokens)
		}
	}
}

func TestLoadShareGPTRequestsSampling(t *testing.T) {
	p := writeShareGPT(t, sharegptFixture)
	// 同 seed 同样本序（跨 run 可复现的前提）
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
	// 不同 seed：k=2 < pool=6，选样集合应不同
	big := writeShareGPT(t, sharegptFixtureBig)
	s1, _ := LoadShareGPTRequests(big, 2, 42, 0)
	s2, _ := LoadShareGPTRequests(big, 2, 7, 0)
	ids1 := s1[0].ID + s1[1].ID
	ids2 := s2[0].ID + s2[1].ID
	if ids1 == ids2 {
		t.Fatalf("不同 seed 选样集合相同: %s", ids1)
	}
	// 输出预算上限
	d, _ := LoadShareGPTRequests(p, 10, 42, 1)
	for _, s := range d {
		if s.OutputTokens != 1 {
			t.Fatalf("max_output_tokens 裁剪失败: %d", s.OutputTokens)
		}
	}
	// num_prompts <=0 拒绝
	if _, err := LoadShareGPTRequests(p, 0, 42, 0); err == nil {
		t.Fatal("num_prompts=0 应报错")
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
