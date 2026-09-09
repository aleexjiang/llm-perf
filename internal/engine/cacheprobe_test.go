package engine

import (
	"strings"
	"testing"
)

func cr(label string, ttft float64, prompt, cached int) CacheRun {
	return CacheRun{Label: label, TTFTMS: ttft, PromptTokens: prompt, CachedTokens: cached}
}

func containsSubs(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestCacheVerdict(t *testing.T) {
	t.Run("缓存生效_tokens高命中+TTFT骤降", func(t *testing.T) {
		warm := []CacheRun{cr("warm#1", 16000, 40000, 0), cr("warm#2", 2000, 40000, 38500), cr("warm#3", 1800, 40000, 38500)}
		cold := []CacheRun{cr("cold#1", 15500, 40000, 0)}
		hit, verdict := cacheVerdict(warm, cold)
		if !hit {
			t.Fatalf("应判为命中: %s", verdict)
		}
	})

	t.Run("无缓存_tokens零且TTFT不降", func(t *testing.T) {
		warm := []CacheRun{cr("warm#1", 16000, 40000, 0), cr("warm#2", 15800, 40000, 0), cr("warm#3", 16200, 40000, 0)}
		cold := []CacheRun{cr("cold#1", 15900, 40000, 0)}
		hit, verdict := cacheVerdict(warm, cold)
		if hit {
			t.Fatalf("不应判为命中: %s", verdict)
		}
		if !containsSubs(verdict, "disable-radix-cache", "会话亲和") {
			t.Fatalf("判读应给出排查方向: %s", verdict)
		}
	})

	t.Run("路由打散_warm全冷", func(t *testing.T) {
		// round-robin 多副本：每次 TTFT 都与冷基线同量级、cached_tokens=0
		warm := []CacheRun{cr("warm#1", 16000, 40000, 0), cr("warm#2", 16000, 40000, 0), cr("warm#3", 16000, 40000, 0)}
		cold := []CacheRun{cr("cold#1", 16000, 40000, 0)}
		hit, _ := cacheVerdict(warm, cold)
		if hit {
			t.Fatal("全冷不应判命中")
		}
	})

	t.Run("无cached_tokens字段时退化为TTFT判据", func(t *testing.T) {
		warm := []CacheRun{cr("warm#1", 16000, 40000, 0), cr("warm#2", 1500, 40000, 0), cr("warm#3", 1400, 40000, 0)}
		cold := []CacheRun{cr("cold#1", 15800, 40000, 0)}
		hit, verdict := cacheVerdict(warm, cold)
		if !hit {
			t.Fatalf("TTFT 骤降应判命中: %s", verdict)
		}
	})

	t.Run("命中但收益低_单列提醒", func(t *testing.T) {
		// cached_tokens 高命中但 TTFT 只降 30%（如排队干扰/chunked prefill 分摊）
		warm := []CacheRun{cr("warm#1", 16000, 40000, 0), cr("warm#2", 11000, 40000, 38500), cr("warm#3", 11200, 40000, 38500)}
		cold := []CacheRun{cr("cold#1", 15800, 40000, 0)}
		hit, verdict := cacheVerdict(warm, cold)
		if !hit {
			t.Fatalf("tokens 高命中应判命中: %s", verdict)
		}
		if !containsSubs(verdict, "收益偏低") {
			t.Fatalf("应提示收益偏低: %s", verdict)
		}
	})

	t.Run("请求不足", func(t *testing.T) {
		if _, v := cacheVerdict([]CacheRun{cr("warm#1", 100, 10, 0)}, nil); v == "" {
			t.Fatal("warm 不足应给出说明")
		}
	})

	t.Run("首次warm失败应报失败而非TTFT缺失", func(t *testing.T) {
		bad := cr("warm#1", 0, 0, 0)
		bad.Error = "Post \"http://gw/v1/chat/completions\": context deadline exceeded"
		_, v := cacheVerdict([]CacheRun{bad, cr("warm#2", 2000, 40000, 0)}, nil)
		if !containsSubs(v, "失败") {
			t.Fatalf("首请求失败应明确报失败原因: %s", v)
		}
	})
}
