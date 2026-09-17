// 测试画像（5.10）：配置校验通过后、发首个请求前估算"这次要跑什么形状"。
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
	Name  string // multiturn | concurrent | concurrent-multi
	Highs []int  // >1 的并发档位（concurrent 场景用）；空 = 单发多轮
	MT    bool   // concurrent 场景是否跑多轮会话
}

// planTemplateOverhead chat template 开销估算系数（与 config 告警、agent 多轮配置口径一致）。
const planTemplateOverhead = 1.07

// traceSingleSampleLimit 与场景层同名常量同口径：trace 单发档位最多取样会话数。
const planTraceSampleLimit = 16

// PlanSummary 生成测试画像。cfg 须已过 Load（默认值与告警已就位）；
// modelFilter 与 main 的 -m 语义一致；items 为本次实际要跑的场景序列。
func PlanSummary(cfg *config.Config, modelFilter string, items []PlanItem) *report.Plan {
	models := filterModels(cfg.ActiveModels(), modelFilter)
	if len(models) == 0 || len(items) == 0 {
		return nil
	}
	p := &report.Plan{
		Endpoint:  cfg.Endpoint,
		Auth:      auth.Auth{Scheme: cfg.AuthScheme, Header: cfg.AuthHeader}.Describe(),
		TimeoutS:  cfg.TimeoutSeconds,
		NumModels: len(models),
		Warmup:    cfg.WarmupRequests,
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
		mixN := len(cc.Mix)
		profN := len(mc.Multiturn.Profiles)
		// 开环（request_rate/rate_sweep）在场景层优先于 levels，画像必须同口径——否则会报出
		// 一个永远不会执行的档位矩阵（默认 levels=[1,2,4,8,16] 在开环配置下仍在线上，
		// 曾把小配置的请求量估到实际值的 7 倍以上）。
		rates := cc.RateSweep
		if len(rates) == 0 && cc.RequestRate > 0 {
			rates = []float64{cc.RequestRate}
		}
		if len(rates) > 0 {
			n := cc.NumPrompts
			if n <= 0 {
				n = 32 // 与 runOpenRound 的兜底一致
			}
			unit := fmt.Sprintf("%d 请求/档", n)
			if it.MT {
				// 开环多轮：num_prompts 是**会话数**，每会话跑满 turns 轮——请求量按轮计
				// （与闭环 multiturn 的 level × turns 同口径，否则画像与执行量差一个 turns 倍数）
				if profN > 0 {
					unit = fmt.Sprintf("%d 会话/档（轮数按档位，混跑 %d 档）", n, profN)
				} else {
					unit = fmt.Sprintf("%d 会话/档（每会话 %d 轮）", n, effectiveTurns(mc))
				}
			}
			total := 0
			for _, v := range vs {
				tiers := 1 // 混跑不走外层输出扫描（与场景层 tiers=[0] 对应）
				if mixN == 0 {
					tiers = len(th.MaxTokensList(cc.MaxTokens, v))
				}
				per := n
				if it.MT {
					per = effectiveTurnsSum(mc, n) // 混合档逐会话求和（均匀档与 n×turns 等价）
				}
				total += tiers * len(rates) * per
			}
			name := it.Name
			if name == "concurrent-multi" {
				name = "concurrent"
			}
			return &report.PlanScenario{
				Name: name,
				Detail: fmt.Sprintf("开环到达率 %v req/s × %s × thinking %s（开环优先于 levels）",
					rates, unit, thDesc),
				Requests: total,
			}
		}
		levels := it.Highs
		if len(levels) == 0 {
			levels = cc.Levels
		}
		// 10.5 时长制（duration_seconds）：各档位跑满墙钟，runs_per_worker 被场景层忽略——
		// 请求量由吞吐决定、不可预先估算。画像此时不报数（Requests=0 + DurationS 标记，
		// 渲染层以「—」呈现），避免给出一个按已忽略配置算出来的假估算。
		dur := cc.DurationSeconds
		var mode string
		if it.MT {
			if mixN > 0 {
				mode = "多轮（mix 与 multiturn 互斥，此组合不会出现）"
			} else if profN > 0 {
				mode = fmt.Sprintf("每用户多轮会话 × turns 按档位（混跑 %d 档）", profN)
				if dur > 0 {
					mode += fmt.Sprintf(" × 每档跑满 %ds（时长制：请求数取决于吞吐）", dur)
				}
			} else if dur > 0 {
				mode = fmt.Sprintf("每用户多轮会话 × turns=%d × 每档跑满 %ds（时长制：请求数取决于吞吐）",
					effectiveTurns(mc), dur)
			} else {
				mode = fmt.Sprintf("每用户多轮会话 × turns=%d", effectiveTurns(mc))
			}
		} else if dur > 0 {
			mode = fmt.Sprintf("每档跑满 %ds（时长制：runs/worker 忽略，请求数取决于吞吐）", dur)
			if mixN > 0 {
				mode += fmt.Sprintf(" × 混跑 %d 形状", mixN)
			}
		} else {
			mode = fmt.Sprintf("单轮 × runs/worker=%d", cc.RunsPerWorker)
			if mixN > 0 {
				mode += fmt.Sprintf(" × 混跑 %d 形状", mixN)
			}
		}
		total := 0
		if dur <= 0 {
			for _, v := range vs {
				tiers := 1 // 混跑不走外层输出扫描（与场景层 tiers=[0] 对应）
				if mixN == 0 {
					tiers = len(th.MaxTokensList(cc.MaxTokens, v))
				}
				for _, level := range levels {
					per := level * cc.RunsPerWorker
					if it.MT {
						per = effectiveTurnsSum(mc, level) // 混合档逐会话求和（均匀档等价 level×turns）
					}
					total += tiers * per
				}
			}
		}
		name := it.Name
		if name == "concurrent-multi" {
			name = "concurrent"
		}
		return &report.PlanScenario{
			Name:      name,
			Detail:    fmt.Sprintf("levels %v × %s × thinking %s", levels, mode, thDesc),
			Requests:  total,
			DurationS: dur,
		}
	}
	return nil
}

// effectiveTurns 多轮会话的有效轮数（filler 口径：max_prompt_tokens 截止可能提前停轮）。
func effectiveTurns(mc *config.Config) int {
	return effTurnsOf(mc, mc.Multiturn)
}

// effectiveTurnsAt 会话 idx 的有效轮数（混合档按所属档位口径；无 profiles 时同 effectiveTurns）。
func effectiveTurnsAt(mc *config.Config, sessionIdx int) int {
	mt := mc.Multiturn
	if p, ok := profileAt(mt, sessionIdx); ok {
		mt.TurnTokens = p.TurnTokens
		if p.Turns > 0 {
			mt.Turns = p.Turns
		}
	}
	return effTurnsOf(mc, mt)
}

// effectiveTurnsSum 前 n 个会话（序号 0..n-1，与发车序号一一对应）的有效轮数之和——
// 混合档下各会话轮数不同，请求量须逐会话求和（均匀档下与 n×effectiveTurns 相等）。
func effectiveTurnsSum(mc *config.Config, n int) int {
	sum := 0
	for idx := 0; idx < n; idx++ {
		sum += effectiveTurnsAt(mc, idx)
	}
	return sum
}

func effTurnsOf(mc *config.Config, mt config.Multiturn) int {
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
