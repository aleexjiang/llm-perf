// cacheprobe.go：前缀缓存定性探针（bench probe --cache，默认关闭）。
//
// 背景：客户报告"已开启前缀缓存但 DeepSeek 请求全冷"。定性方法：
// 同一长上下文 prompt 连发 N 次（warm），对比第 1 次与后续的 TTFT 与
// usage.cached_tokens；再发一个同长度乱序变体（cold）作冷基线对照。
//
// 判读矩阵：
//   - warm 后续 TTFT 骤降 + cached_tokens 高命中 → 缓存生效
//   - cached_tokens=0 且 TTFT 无下降 → 无缓存（查 --disable-radix-cache / 引擎缺陷）
//   - 走多副本路由时全冷、直连单实例命中 → 路由打散会话亲和（round-robin vs cache_aware）
//
// 请求代价：默认 40000tk 上下文 × (3 warm + 1 cold) = 4 次长 prefill，只做定性不做压测，
// max_tokens=64 隔离 prefill 阶段，不进主压测路径。
package engine

import (
	"context"
	"fmt"
)

// CacheRun 单次缓存探针请求的结果摘要。
type CacheRun struct {
	Label            string  `json:"label"`
	TTFTMS           float64 `json:"ttft_ms"`
	E2EMS            float64 `json:"e2e_ms"`
	PromptTokens     int     `json:"prompt_tokens"`
	CachedTokens     int     `json:"cached_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	Error            string  `json:"error,omitempty"`
}

// CacheProbeResult 前缀缓存定性探针结果（随 probe JSON 落盘）。
type CacheProbeResult struct {
	Model      string     `json:"model"`
	SizeTokens int        `json:"size_tokens"`
	Warm       []CacheRun `json:"warm_runs"`
	Cold       []CacheRun `json:"cold_runs"`
	Hit        bool       `json:"cache_hit"`
	Verdict    string     `json:"verdict"`
}

// CacheProbeOptions 缓存探针参数。
type CacheProbeOptions struct {
	Model        string
	SizeTokens   int            // 探测上下文大小（tokens），默认 40000
	WarmRuns     int            // 同一 prompt 连发次数，默认 3（首冷 + 2 次验证）
	ThinkingOff  map[string]any // 思考关闭 extra_body（防思考噪声干扰 TTFT）
	FillerLang   string         // 填充语料语言（默认 zh）
	SeedWarm     int64          // warm prompt 种子（默认 421）
	SeedCold     int64          // cold 变体种子（默认 907，与 warm 必然不同文本）
	QuestionText string         // 追问句（默认概括类；答案正确性不影响计时）
}

const cacheQuestion = "基于上文内容，用一句话概括这份材料主要覆盖了哪些主题。"

// RunCacheProbe 执行前缀缓存定性探针。单次请求失败不终止（记录错误继续），
// 全部失败时 Verdict 给出排查方向。
func RunCacheProbe(ctx context.Context, c *Client, o CacheProbeOptions) *CacheProbeResult {
	if o.SizeTokens <= 0 {
		o.SizeTokens = 40000
	}
	if o.WarmRuns < 2 {
		o.WarmRuns = 3
	}
	if o.FillerLang == "" {
		o.FillerLang = "zh"
	}
	if o.SeedWarm == 0 {
		o.SeedWarm = 421
	}
	if o.SeedCold == 0 {
		o.SeedCold = 907
	}
	q := o.QuestionText
	if q == "" {
		q = cacheQuestion
	}

	res := &CacheProbeResult{Model: o.Model, SizeTokens: o.SizeTokens}
	run := func(label string, seed int64) CacheRun {
		cr := CacheRun{Label: label}
		msg := Message{Role: "user", Content: Filler(o.SizeTokens, seed, o.FillerLang) + "\n\n" + q}
		m, err := c.Chat(ctx, ChatOptions{
			Model:     o.Model,
			Messages:  []Message{msg},
			MaxTokens: 64,
			Stream:    true,
			ExtraBody: o.ThinkingOff,
		})
		if err != nil && (m == nil || m.Error == "") {
			cr.Error = err.Error()
			return cr
		}
		if m != nil {
			cr.TTFTMS = m.TTFT
			cr.E2EMS = m.E2EMS
			cr.PromptTokens = m.PromptTokens
			cr.CachedTokens = m.CachedTokens
			cr.CompletionTokens = m.CompletionTokens
			if m.Error != "" {
				cr.Error = m.Error
			}
		}
		return cr
	}

	for i := 0; i < o.WarmRuns; i++ {
		if ctx.Err() != nil {
			break
		}
		res.Warm = append(res.Warm, run(fmt.Sprintf("warm#%d", i+1), o.SeedWarm))
	}
	if ctx.Err() == nil {
		res.Cold = append(res.Cold, run("cold#1", o.SeedCold))
	}
	res.Hit, res.Verdict = cacheVerdict(res.Warm, res.Cold)
	return res
}

// cacheVerdict 纯函数判读：warm 首末次对比 + cold 基线。
// hit 判据（满足其一）：
//  1. 末次 warm 的 cached_tokens 覆盖 >50% prompt_tokens 且 TTFT 降到首次 60% 以下；
//  2. 服务端不回传 cached_tokens 时，退化为纯 TTFT 判据：末次 TTFT ≤ 首次 50% 且显著低于 cold。
func cacheVerdict(warm, cold []CacheRun) (bool, string) {
	if len(warm) < 2 {
		return false, "warm 请求不足 2 次有效完成，无法判读（先解决端点连通性/认证）"
	}
	first, last := warm[0], warm[len(warm)-1]
	if first.Error != "" {
		return false, "首次 warm 请求失败: " + first.Error
	}
	if last.Error != "" {
		return false, "末次 warm 请求失败: " + last.Error
	}
	if first.TTFTMS <= 0 || last.TTFTMS <= 0 {
		return false, "TTFT 缺失（流式首包没测到）——检查 include_usage/流式兼容性"
	}
	coldTTFT := 0.0
	if len(cold) > 0 && cold[0].TTFTMS > 0 {
		coldTTFT = cold[0].TTFTMS
	}
	ratio := last.TTFTMS / first.TTFTMS
	pct := func(v float64) string { return fmt.Sprintf("%.1fs", v/1000) }

	hitByTokens := last.CachedTokens > 0 && first.PromptTokens > 0 &&
		float64(last.CachedTokens)/float64(first.PromptTokens) > 0.5
	hitByTTFT := ratio <= 0.5 && (coldTTFT == 0 || last.TTFTMS < coldTTFT*0.8)

	switch {
	case hitByTokens && ratio <= 0.6:
		return true, fmt.Sprintf("前缀缓存生效: TTFT %s→%s（%.0f%%），cached_tokens %d/%d",
			pct(first.TTFTMS), pct(last.TTFTMS), ratio*100, last.CachedTokens, last.PromptTokens)
	case hitByTokens:
		return true, fmt.Sprintf("缓存命中但收益偏低: cached_tokens %d/%d 命中，但 TTFT %s→%s 仅 %.0f%% 降幅——"+
			"留意排队干扰或 chunked prefill 分摊；与 cold 基线(%s)对比确认收益",
			last.CachedTokens, last.PromptTokens, pct(first.TTFTMS), pct(last.TTFTMS), ratio*100, pct(coldTTFT))
	case hitByTTFT:
		return true, fmt.Sprintf("TTFT %s→%s 骤降（服务端未回传 cached_tokens，按 TTFT 判）——缓存大概率生效，"+
			"以服务端日志/metrics 复核", pct(first.TTFTMS), pct(last.TTFTMS))
	default:
		return false, fmt.Sprintf("无前缀缓存命中: TTFT %s→%s（%.0f%%）cached_tokens=%d。排查方向: "+
			"① 引擎启动参数 --disable-radix-cache；② 多副本路由（round_robin/random）打散会话亲和——直连单实例复测；"+
			"③ KV 空间过小导致缓存被逐出；④ 客户端 prompt 首部含每请求变化内容（时间戳/随机 ID）",
			pct(first.TTFTMS), pct(last.TTFTMS), ratio*100, last.CachedTokens)
	}
}

// cacheProbeInto 把缓存探针结果并入 ProbeResult（checks + verdicts + 结构化数据）。
// ctx 已取消时静默跳过（probe 被中断不硬塞半截数据）。
func cacheProbeInto(ctx context.Context, res *ProbeResult, c *Client, o CacheProbeOptions) {
	if ctx.Err() != nil {
		return
	}
	cp := RunCacheProbe(ctx, c, o)
	res.CacheProbe = cp
	if cp.Verdict != "" {
		res.Verdicts = append(res.Verdicts, "前缀缓存: "+cp.Verdict)
		if cp.Hit {
			res.Checks = append(res.Checks, ProbeCheck{Name: "prefix_cache", OK: true, Detail: cp.Verdict})
		} else {
			res.Checks = append(res.Checks, ProbeCheck{Name: "prefix_cache", OK: false, Detail: cp.Verdict})
		}
	}
}
