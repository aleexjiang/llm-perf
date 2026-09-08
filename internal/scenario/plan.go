// 战役画像（5.10）：配置校验通过后、发首个请求前估算"这次要跑什么形状"。
//
// 设计约束：展示口径 = 执行口径。估算不新造逻辑——档位列表复用 ClampLadder（含
// max_prompt_tokens 截断）、输出档与思考 floor 复用 MaxTokensList、变体展开复用
// Variants、模型差异复用 ForModel，与三个场景的循环结构一一对应。所有 token 数字
// 都是估算值（4 字符/token + ~1.07 chat template 开销），展示带 ~ 前缀。
package scenario

import (
	"fmt"
	"strings"

	"github.com/aleexjiang/llm-perf/internal/auth"
	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/report"
)

// PlanItem 描述一个待执行场景（与 main 的执行计划项一一对应）。
type PlanItem struct {
	Name  string // single | multiturn | concurrent | concurrent-multi
	Highs []int  // >1 的并发档位（concurrent 场景用）；空 = 纯单发场景
	MT    bool   // concurrent 场景是否跑多轮会话
}

// planTemplateOverhead chat template 开销估算系数（与 config 告警、ROADMAP 5.9 同一口径）。
const planTemplateOverhead = 1.07

// traceSingleSampleLimit 与场景层同名常量同口径：trace 单发档位最多取样会话数。
const planTraceSampleLimit = 16

// PlanSummary 生成战役画像。cfg 须已过 Load（默认值与告警已就位）；
// modelFilter 与 main 的 -m 语义一致；items 为本次实际要跑的场景序列。
func PlanSummary(cfg *config.Config, modelFilter string, items []PlanItem) *report.Plan {
	models := filterModels(cfg.ActiveModels(), modelFilter)
	if len(models) == 0 || len(items) == 0 {
		return nil
	}
	p := &report.Plan{
		Endpoint:    cfg.Endpoint,
		Auth:        auth.Auth{Scheme: cfg.AuthScheme, Header: cfg.AuthHeader}.Describe(),
		TimeoutS:    cfg.TimeoutSeconds,
		NumModels:   len(models),
		Warmup:      cfg.WarmupRequests,
	}
	if cfg.Correctness != nil {
		p.Correctness = cfg.Correctness.Samples
	}
	for _, model := range models {
		mc := cfg.ForModel(model) // model_overrides 覆盖后各模型请求量可能不同
		pm := report.PlanModel{Model: model}
		for _, it := range items {
			if ps := planScenario(mc, it); ps != nil {
				pm.Scenarios = append(pm.Scenarios, *ps)
				pm.Requests += ps.Requests
			}
		}
		p.Models = append(p.Models, pm)
		p.TotalRequests += pm.Requests
	}
	return p
}

// planScenario 单场景估算。请求口径与场景循环逐层对应：
// 变体 × 输出档 × (档位×runs | sessions×turns | levels×每档请求)。
func planScenario(mc *config.Config, it PlanItem) *report.PlanScenario {
	th := mc.Thinking
	vs := th.Variants()
	if len(vs) == 0 {
		return nil
	}
	vnames := make([]string, len(vs))
	for i, v := range vs {
		vnames[i] = v.Name
	}
	thDesc := strings.Join(vnames, "/")
	traceMode := mc.Dataset.Mode == "trace"

	switch it.Name {
	case "single":
		runs := mc.Single.Runs
		if traceMode {
			// trace 模式档位来自回放首轮（精确数需加载文件，按取样上限估算）
			total := 0
			for _, v := range vs {
				total += len(th.MaxTokensList(mc.Single.MaxTokens, v)) * planTraceSampleLimit * runs
			}
			return &report.PlanScenario{
				Name: it.Name,
				Detail: fmt.Sprintf("trace 回放 ≤%d 会话 × runs=%d × thinking %s",
					planTraceSampleLimit, runs, thDesc),
				Requests: total,
			}
		}
		ladder, _ := mc.ClampLadder(mc.Single.PromptTokens) // 与执行同源：含 max_prompt_tokens 截断
		total := 0
		for _, v := range vs {
			total += len(th.MaxTokensList(mc.Single.MaxTokens, v)) * len(ladder) * runs
		}
		return &report.PlanScenario{
			Name: it.Name,
			Detail: fmt.Sprintf("档位 %v × runs=%d × thinking %s（fixed_seed=%v 缓存对照）",
				ladder, runs, thDesc, mc.Single.FixedSeed),
			Requests: total,
		}

	case "multiturn":
		mt := mc.Multiturn
		effTurns := mt.Turns
		ctxDesc := "上下文由回放会话决定"
		if !traceMode && mt.TurnTokens > 0 {
			base := mt.SystemTokens + mt.ToolDefsTokens
			// 与 config 告警同口径的可达深度；max_prompt_tokens 截止时提前停轮
			if mc.MaxPromptTokens > 0 {
				if fit := (mc.MaxPromptTokens - base) / mt.TurnTokens; fit < effTurns {
					if fit < 1 {
						fit = 1
					}
					effTurns = fit
				}
			}
			ctxDesc = fmt.Sprintf("上下文 ~%.0fk → ~%.0fk",
				float64(base+mt.TurnTokens)*planTemplateOverhead/1000,
				float64(base+effTurns*mt.TurnTokens)*planTemplateOverhead/1000)
		}
		total := 0
		for _, v := range vs {
			total += len(th.MaxTokensList(mt.MaxTokens, v)) * mt.Sessions * effTurns
		}
		return &report.PlanScenario{
			Name: it.Name,
			Detail: fmt.Sprintf("sessions=%d × turns=%d（%s）× thinking %s",
				mt.Sessions, effTurns, ctxDesc, thDesc),
			Requests: total,
		}

	case "concurrent", "concurrent-multi":
		cc := mc.Concurrent
		levels := it.Highs
		if len(levels) == 0 {
			levels = cc.Levels
		}
		mixN := len(cc.Mix)
		var mode string
		if it.MT {
			mode = fmt.Sprintf("每用户多轮会话 × turns=%d", effectiveTurns(mc))
			if mixN > 0 {
				mode = "多轮（mix 与 multiturn 互斥，此组合不会出现）"
			}
		} else {
			mode = fmt.Sprintf("单轮 × runs/worker=%d", cc.RunsPerWorker)
			if mixN > 0 {
				mode += fmt.Sprintf(" × 混跑 %d 形状", mixN)
			}
		}
		total := 0
		for _, v := range vs {
			tiers := 1 // 混跑不走外层输出扫描（与场景层 tiers=[0] 对应）
			if mixN == 0 {
				tiers = len(th.MaxTokensList(cc.MaxTokens, v))
			}
			for _, level := range levels {
				per := level * cc.RunsPerWorker
				if it.MT {
					per = level * effectiveTurns(mc)
				}
				total += tiers * per
			}
		}
		name := it.Name
		if name == "concurrent-multi" {
			name = "concurrent"
		}
		return &report.PlanScenario{
			Name:     name,
			Detail:   fmt.Sprintf("levels %v × %s × thinking %s", levels, mode, thDesc),
			Requests: total,
		}
	}
	return nil
}

// effectiveTurns 多轮会话的有效轮数（filler 口径：max_prompt_tokens 截止可能提前停轮）。
func effectiveTurns(mc *config.Config) int {
	mt := mc.Multiturn
	if mt.TurnTokens <= 0 || mc.MaxPromptTokens <= 0 {
		return mt.Turns
	}
	base := mt.SystemTokens + mt.ToolDefsTokens
	fit := (mc.MaxPromptTokens - base) / mt.TurnTokens
	if fit < 1 {
		fit = 1
	}
	if fit < mt.Turns {
		return fit
	}
	return mt.Turns
}
