// user.go：user 模式——生成式多轮用户会话（docs/workload-refactor-plan.md 13）。
//
// 会话形状来自外置受控 profile.json，默认权重 6:3:1 可调；
//   - 运行时文本（system 基座 / user 输入 / 合成 context）全部取自经典书语料库，
//     窗口起点由 (session_seed, turn) 派生，内容确定可复现；
//   - 每轮把**被测模型真实生成的 assistant 回复**追加进下一轮 history——
//     prefix cache 反映当前模型真实回复形成的动态前缀（与冻结快照的本质差异）；
//   - first_turn_tokens 决定首轮总 prompt；后续轮少数按概率成为上下文突增轮。
//
// cache 安全双约束（13.4 复盘修正）：
//  1. 合成 context 只能尾部注入（拼进当前 user 消息内），严禁进 system——
//     system 位于序列最前端，逐轮变化会使其后整段历史失效；
//  2. system 基座会话开始时一次定型，全程不变（固定 seed 确定性生成）。
package scenario

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/contract"
	"github.com/aleexjiang/llm-perf/internal/corpus"
	"github.com/aleexjiang/llm-perf/internal/engine"
)

// assistantReplyCap 真实 assistant 回复进 history 的截断上限（字符，rune 安全）。
// 输出长度受 max_tokens 请求约束，此上限只是防御性护栏（如服务端不尊重 max_tokens）。
const assistantReplyCap = 32768

// contextTag 合成 context 的包裹标记：拼在 user 消息内部（尾部注入，cache 安全）。
const contextTag = "reference-context"

// userContextSafetyMargin 为模型上下文上限预留模板、序列化和服务端实现差异空间。
// 可用 prompt 上限 = max_model_len - max_tokens - safety margin。
const userContextSafetyMargin = 4096

func init() {
	Register(funcScenario{"user", UserScenario})
}

// UserScenario user 模式：profile 驱动的生成式多轮会话。
func UserScenario(ctx context.Context, cfg *config.Config, client *engine.Client, modelFilter string, options RunOptions) (*contract.Report, error) {
	if cfg.User.ProfilePath == "" {
		return nil, fmt.Errorf("user 模式需要 user.profile_path（scripts/profile_build.py 产出的 profile.json）")
	}
	prof, err := LoadProfile(cfg.User.ProfilePath)
	if err != nil {
		return nil, err
	}
	if err := applyFirstTurnConfig(prof, cfg.User.FirstTurnTokens, cfg.User.GetSharedBaseTokens()); err != nil {
		return nil, err
	}
	lang := cfg.CorpusLang
	if lang == "" {
		lang = "en"
	}
	if corpus.Library(lang) == nil {
		return nil, fmt.Errorf("user 模式语料库为空（lang=%s）——检查 corpus_lang/filler_lang 配置", lang)
	}
	e := &env{cfg: cfg, client: client}
	// 服务端观测层：之前漏装配导致 user 场景 JSON 恒无 server_metrics（真机发现 1.3），
	// cache hit/preemption 等归因数据全部缺失——与 rps/concurrency 同口径装配。
	if err := setupServerMetrics(ctx, e, cfg); err != nil {
		return nil, err
	}
	rep := &contract.Report{
		Tool:        contract.Version,
		Scenario:    "user",
		GeneratedAt: time.Now(),
		Test:        cfg.TestKind(),
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("生成式多轮用户会话 levels=%v profile=%s（权重 %s）语料=%s shared_base=%v stream=%v thinking=%s；首轮=%v tk，基座=%dtk；assistant=被测模型真实回复（动态 prefix cache）%s",
			cfg.User.Levels, prof.Source, weightsDesc(prof), lang, cfg.User.GetSharedBase(),
			cfg.StreamEnabled(), cfg.Thinking.Mode, prof.FirstTurnTokens, cfg.User.GetSharedBaseTokens(), thinkingNoteSuffix(cfg)),
	}
	applySLO(e, rep)

	for _, model := range cfg.ActiveModels() {
		if modelFilter != "" && !strings.Contains(model, modelFilter) {
			continue
		}
		em, mc := forModel(e, cfg, model)
		th := mc.Thinking
		if len(th.Variants()) == 0 {
			log.Printf("  %s: 无匹配的思考变体，跳过", model)
			continue
		}
		maxToks := mc.User.MaxTokens
		if len(maxToks) == 0 {
			maxToks = config.IntList{256}
		}
		for _, v := range th.Variants() {
			for _, maxTok := range th.MaxTokensList(maxToks, v) {
				// 每个 max_tokens 档位都重新计算可用 prompt 预算；输出预算越大，
				// 留给 prompt 的空间越小。
				tokenBudget := userTokenBudget(mc, prof, model, maxTok)
				for _, users := range mc.User.Levels {
					if interrupted(ctx) {
						break
					}
					before, poller, winStart := startWindow(ctx, e)
					levelStart := time.Now()
					runs := runUserSessions(ctx, em, prof, lang, model, v, maxTok, tokenBudget, users)
					levelWall := time.Since(levelStart).Seconds()
					server := finishWindow(e, before, poller, winStart)
					var levelTurns []*engine.TurnMetrics
					for _, run := range runs {
						levelTurns = append(levelTurns, run.Turns...)
					}
					level := contract.UserLevel{
						Model: model, Thinking: v.Name, Users: users, MaxTokens: maxTok,
						Workload: userWorkload(prof, tokenBudget, mc.User.GetSharedBaseTokens()),
						Sessions: runs, Server: server,
						Metrics: contract.BuildMetricsSummary(levelTurns, levelWall),
					}
					rep.UserLevels = append(rep.UserLevels, level)
					if options.Checkpoint != nil {
						options.Checkpoint(rep)
					}
				}
			}
		}
	}
	return rep, nil
}

func userTokenBudget(cfg *config.Config, prof *Profile, model string, maxTok int) int {
	if cfg.ContextBudgetTokens <= 0 {
		return 0
	}
	// 统一预算入口：配置值通常就是模型 max_model_len；这里先扣安全余量，
	// runOneUserSession 再加上当前 maxTok 判断，实际 prompt 上限为
	// context_budget_tokens - max_tokens - safety margin。
	budget := cfg.ContextBudgetTokens - userContextSafetyMargin
	if budget <= 0 {
		log.Printf("⚠️ %s context_budget_tokens=%d 小于安全余量=%d——本档位会在首轮前止损",
			model, cfg.ContextBudgetTokens, userContextSafetyMargin)
		// 返回最小的 prompt+output 预算，保留预算止损路径；不能返回 0，
		// 否则 runOneUserSession 会把它当成“未启用保护”。
		return maxTok
	}
	promptLimit := budget - maxTok
	if promptLimit <= 0 {
		log.Printf("⚠️ %s context_budget_tokens=%d、max_tokens=%d 和安全余量=%d 无法留下可用 prompt 空间——本档位会在首轮前止损",
			model, cfg.ContextBudgetTokens, maxTok, userContextSafetyMargin)
	} else if est := estMaxContext(prof); est > promptLimit {
		log.Printf("⚠️ %s profile 形状预计最大上下文 ≈%dtk > 可用 prompt 上限=%dtk（context_budget_tokens=%d - max_tokens=%d - 安全余量=%d）——长会话会提前止损",
			model, est, promptLimit, cfg.ContextBudgetTokens, maxTok, userContextSafetyMargin)
	}
	return budget
}

// runUserSessions 并行执行 users 条生成式会话（每用户一条，模型串行保证 e.cfg 不被并发改写）。
func runUserSessions(ctx context.Context, e *env, prof *Profile, lang, model string,
	v config.ThinkingVariant, maxTok, tokenBudget, users int) []contract.MultiturnRun {

	stagger := time.Duration(e.cfg.User.GetStaggerMS()) * time.Millisecond
	selector := newSWRR(prof)
	labels := make([]string, users)
	for u := 0; u < users; u++ {
		labels[u] = selector.next()
	}
	runs := make([]contract.MultiturnRun, users)
	var wg sync.WaitGroup
	for u := 0; u < users; u++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// 会话启动错峰（stagger_ms，默认 0）：users>1 时全部会话同时发首轮，
			// 并发 prefill 互抢使首轮 TTFT 差异达 1.7 倍且逐轮曲线双峰（真机发现 2.2）。
			// 错峰后逐轮 TTFT 斜率更干净；分派（swrr labels）不受启动顺序影响。
			if stagger > 0 && idx > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(stagger):
				}
			}
			if interrupted(ctx) {
				return
			}
			runs[idx] = runOneUserSession(ctx, e, prof, lang, model, v, maxTok, tokenBudget, idx, labels[idx])
		}(u)
	}
	wg.Wait()
	out := runs[:0]
	for _, r := range runs {
		if len(r.Turns) > 0 {
			out = append(out, r)
		}
	}
	return out
}

// runOneUserSession 单个用户的完整多轮会话：profile 定形状，语料定内容，模型定回复。
func runOneUserSession(ctx context.Context, e *env, prof *Profile, lang, model string,
	v config.ThinkingVariant, maxTok, tokenBudget int, userIdx int, profileLabel string) contract.MultiturnRun {

	run := contract.MultiturnRun{
		Model:       model,
		Thinking:    v.Name,
		Session:     userIdx + 1,
		MaxTokens:   maxTok,
		TokenBudget: tokenBudget,
	}
	spec := prof.Profiles[profileLabel]
	run.Profile = profileLabel

	seed := userSeed(e.cfg.SeedSalt, userIdx)
	rng := rand.New(rand.NewSource(seed))
	book := corpus.SelectBook(lang, seed)
	if book == nil {
		log.Printf("    语料库为空，会话跳过")
		return run
	}
	cpr := corpus.CharsPerToken(lang)

	// 档位轮次：[lo,hi] 均匀采样；单元素 = 下限 + 运行时上限 32（review R1-H2 修正）。
	// 不把 ContextBudgetTokens 当轮次上限——统一预算保护按 prompt+output 止损。
	turnLo, turnHi := spec.turnBounds(0)
	turns := turnLo
	if turnHi > turnLo {
		turns = turnLo + rng.Intn(turnHi-turnLo+1)
	}

	// system 基座：一次定型，全程冻结（cache 约束 2）。
	// shared_base=true（默认）：全部用户同一基座内容——基座种子/取窗 seed 与 userIdx 解耦
	// （review R1-H1 修正：会话 rng 是 per-session 的，直接用它取窗会让每个用户基座互异，
	// "跨用户共享前缀 cache 收益"恒为 0，与配置注释矛盾）。基座书也固定（与用户自选书解耦，
	// 避免不同书文长度不一致导致基座差异）；用户的 user/context 仍取自各自的书。
	// shared_base=false：每用户独立基座（per-session seed 与书）。
	baseSeed := seed
	baseBook := book
	baseWindowSeed := rng.Int63()
	if e.cfg.User.GetSharedBase() {
		baseSeed = sharedBaseSeed(e.cfg.SeedSalt)
		if sb := corpus.SelectBook(lang, baseSeed); sb != nil {
			baseBook = sb
		}
		baseWindowSeed = baseSeed
	}
	baseMsg := engine.Message{Role: "system",
		Content: baseBook.Window(int(float64(e.cfg.User.GetSharedBaseTokens())*cpr), baseWindowSeed)}
	baseTokens := e.cfg.User.GetSharedBaseTokens()

	sample := func(r []int) int {
		if len(r) != 2 || r[1] <= r[0] {
			return r[0]
		}
		return r[0] + rng.Intn(r[1]-r[0]+1)
	}

	msgs := []engine.Message{baseMsg}
	firstTurnTokens := sample(prof.FirstTurnTokens)
	// lastBaseline 是下一轮 prompt 的下限估计：上一轮实测 prompt + assistant 回复 token。
	// usage 缺失时置 0，只保留原有的运行时兜底，不做错误截断。
	lastBaseline := 0
	for turn := 0; turn < turns; turn++ {
		if interrupted(ctx) {
			break
		}
		userTk := sample(spec.UserInputTokens)
		if userTk < 20 {
			userTk = 20 // 保底可读长度
		}
		question := book.Window(int(float64(userTk)*cpr), rng.Int63())
		var content strings.Builder
		// 合成 context：尾部注入（cache 约束 1）——拼进 user 消息内部，绝不进 system
		ctxTk, burstTk := 0, 0
		burst := false
		if turn == 0 {
			ctxTk = firstTurnTokens - baseTokens - userTk
			if ctxTk < 0 {
				ctxTk = 0
			}
		} else {
			ctxTk = sample(spec.ContextTokens)
			if rng.Float64() < spec.ContextBurstProbability {
				burst = true
				burstTk = sample(spec.ContextBurstTokens)
			}
		}
		totalCtxTk := ctxTk + burstTk
		if totalCtxTk > 0 {
			fmt.Fprintf(&content, "<%s>\n%s\n</%s>\n\n", contextTag,
				book.Window(int(float64(totalCtxTk)*cpr), rng.Int63()), contextTag)
		}
		content.WriteString(question)

		// 输出预算预留：把本轮计划 user/context 增量、上一轮实测历史和 assistant 回复
		// 一并计入下一轮估算。只在 estimate > 0 且确定性超过预算时止损；
		// usage 缺失或估算不足时继续执行，让原有 ctxLimitHit 兜底。
		// userTk/ctxTk 已经是 profile 定义的 token 数；cpr 只用于生成对应字符长度。
		// 再除一次 cpr 会让英文预算低估约 4 倍，使保护逻辑放过必然超限的请求。
		planIncrement := userTk + totalCtxTk
		plan := contract.UserTurnInput{
			Turn: turn + 1, UserInputTokens: userTk, ContextTokens: ctxTk,
			ContextBurst: burst, ContextBurstTokens: burstTk, PlannedIncrementTokens: planIncrement,
		}
		nextPrompt := lastBaseline
		if nextPrompt == 0 {
			nextPrompt = baseTokens
		}
		estimate := nextPrompt + planIncrement
		if tokenBudget > 0 && estimate > 0 && estimate+maxTok > tokenBudget {
			now := time.Now()
			m := &engine.TurnMetrics{
				Model: model, Stream: e.cfg.StreamEnabled(), Thinking: v.Enabled, Phase: "benchmark",
				SentAt: now, EndAt: now, Cancelled: true,
				Warnings: []string{fmt.Sprintf(
					"token_budget_exhausted: estimated_prompt=%d + max_tokens=%d > %d——停止本轮，避免 context 400",
					estimate, maxTok, tokenBudget)},
			}
			m.Finalize()
			run.Turns = append(run.Turns, m)
			run.InputPlan = append(run.InputPlan, plan)
			log.Printf("    🛑 上下文预算不足（estimated_prompt=%d + max_tokens=%d > %d）——提前结束会话",
				estimate, maxTok, tokenBudget)
			break
		}

		msgs = append(msgs, engine.Message{Role: "user", Content: content.String()})

		m := runOne(ctx, e, model, msgs, maxTok, v)
		if tokenBudget > 0 && m.PromptTokens > 0 && m.Error == "" && !m.Cancelled {
			nextPromptBase := m.PromptTokens + m.CompletionTokens
			if m.ReplyText == "" {
				// finish=length 时 completion 也可能进思考/输出预算；没有可见回复则下轮
				// assistant 消息为空，但服务端模型上限仍按完整 usage 评估，保守不减。
				nextPromptBase = m.PromptTokens + maxTok
			}
			if nextPromptBase > lastBaseline {
				lastBaseline = nextPromptBase
			}
		}
		run.Turns = append(run.Turns, m)
		run.InputPlan = append(run.InputPlan, plan)
		// 首轮实测深度校验（review R1-L1）：估算保证依赖 CharsPerToken 系数，
		// 真实 tokenizer 偏差在此暴露（session 内只告警一次）
		if turn == 0 && m.PromptTokens > 0 {
			if m.PromptTokens < prof.FirstTurnTokens[0] {
				log.Printf("    ⚠️ 首轮实测 prompt %dtk < 约束 %dtk——语料换算比偏差，建议跑 bench probe 看 filler_fidelity",
					m.PromptTokens, prof.FirstTurnTokens[0])
			} else if m.PromptTokens > prof.FirstTurnTokens[1] {
				// 真机发现 2.3：档位增量下限与首轮上限冲突时静默超限——至少让超限可见
				log.Printf("    ⚠️ 首轮实测 prompt %dtk > 约束上限 %dtk——语料换算比偏差或模板开销偏大",
					m.PromptTokens, prof.FirstTurnTokens[1])
			}
		}
		// 真实 assistant 回复进 history：动态 prefix cache 的核心
		if m.ReplyText != "" {
			msgs = append(msgs, engine.Message{Role: "assistant",
				Content: engine.TruncateRunes(m.ReplyText, assistantReplyCap)})
		}
		if limit := ctxLimitHit(m); limit != "" {
			log.Printf("    🛑 触发模型上下文上限（limit=%stk）——提前结束会话", limit)
			break
		}
		// 失败轮终止会话（review R1-M1）：失败轮无 assistant 回复，若继续下一轮会产生
		// 连续 user 消息（违反方案 A 不变量，部分端点直接 400 使失败扩散到后续所有轮）。
		// 失败轮的完整指标已保留在 Turns 里（含 Error），已完成轮不受影响。
		if m.Error != "" {
			log.Printf("    🛑 轮次失败（%s）——终止该会话，避免连续 user 消息", previewErr(m.Error))
			break
		}
		// 空回复轮终止会话（真机发现 1.2）：finish=stop 但**无任何可见输出**
		//（content 与 reasoning 全空）——模型偶发直接吐 stop（真机实测 ~7%，
		// 长上下文轮更频）。以可见输出为准而非 completion 位数：极短但正常的回复
		//（如 "OK"）不该终止会话。该轮 assistant 为空，与失败轮同语义终止，
		// 避免连续 user 消息；异常留痕在 warnings 里可分析。
		if !m.Cancelled && m.FinishReason == "stop" && m.ReplyText == "" && m.ReasoningChars == 0 {
			m.Warnings = append(m.Warnings, fmt.Sprintf("empty_reply: finish=stop completion=%d content=0 reasoning=0——终止会话，避免空 assistant 连续 user", m.CompletionTokens))
			log.Printf("    🛑 空回复轮（completion=%d，finish=stop，无可见输出）——终止该会话", m.CompletionTokens)
			break
		}
		if interrupted(ctx) {
			break
		}
	}
	run.FillLastPromptTokens()
	return run
}

// userWorkload 把 profile 的比例和上下文突增设置作为一等 workload 元数据落盘。
func userWorkload(prof *Profile, budget, baseTokens int) contract.UserWorkload {
	w := contract.UserWorkload{
		Profile:             prof.Source,
		FirstTurnTokens:     append([]int(nil), prof.FirstTurnTokens...),
		SharedBaseTokens:    baseTokens,
		Tiers:               map[string]contract.UserWorkloadTier{},
		ContextBudgetTokens: budget,
	}
	for name, spec := range prof.Profiles {
		if spec == nil {
			continue
		}
		tier := contract.UserWorkloadTier{
			Weight:                  spec.Weight,
			TurnsRange:              append([]int(nil), spec.TurnsRange...),
			UserInputTokens:         append([]int(nil), spec.UserInputTokens...),
			ContextTokens:           append([]int(nil), spec.ContextTokens...),
			ContextBurstProbability: spec.ContextBurstProbability,
		}
		if spec.ContextBurstProbability > 0 {
			tier.ContextBurstTokens = append([]int(nil), spec.ContextBurstTokens...)
		}
		w.Tiers[name] = tier
	}
	return w
}

// estMaxContext 按 profile 形状估算单个会话可滚到的最大 prompt token（保守上界：
// 首轮上限 + 后续最大轮数 × (最大 user 输入 + 最大常规增量 + 可能的最大突增)）。
func estMaxContext(prof *Profile) int {
	mx := prof.FirstTurnTokens[1]
	for _, spec := range prof.Profiles {
		_, turnHi := spec.turnBounds(0)
		ctxHi, userHi, burstHi := 0, 0, 0
		if len(spec.ContextTokens) == 2 {
			ctxHi = spec.ContextTokens[1]
		}
		if len(spec.UserInputTokens) == 2 {
			userHi = spec.UserInputTokens[1]
		}
		if spec.ContextBurstProbability > 0 && len(spec.ContextBurstTokens) == 2 {
			burstHi = spec.ContextBurstTokens[1]
		}
		followingTurns := turnHi - 1
		if followingTurns < 0 {
			followingTurns = 0
		}
		if v := prof.FirstTurnTokens[1] + followingTurns*(ctxHi+userHi+burstHi); v > mx {
			mx = v
		}
	}
	return mx
}

// previewErr 错误摘要（日志用）。
func previewErr(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

// swrr 平滑加权轮转（nginx 语义）：档位分派在用户序上交错展开，
// 小用户数也精确收敛到权重比例（10 用户 × 6:3:1 = 6 轻/3 中/1 重）。
type swrr struct {
	names   []string
	weights []int
	current []int
	total   int
}

func newSWRR(prof *Profile) *swrr {
	names := make([]string, 0, len(prof.Profiles))
	for name := range prof.Profiles {
		names = append(names, name)
	}
	sort.Strings(names) // 稳定顺序，保证同配置重跑分派一致
	s := &swrr{names: names}
	for _, n := range names {
		w := int(prof.Profiles[n].Weight*100 + 0.5)
		if w < 1 {
			w = 1
		}
		s.weights = append(s.weights, w)
		s.current = append(s.current, 0)
		s.total += w
	}
	return s
}

// next 返回下一个档位名（确定性）。
func (s *swrr) next() string {
	best := 0
	for i := range s.names {
		s.current[i] += s.weights[i]
		if s.current[i] > s.current[best] {
			best = i
		}
	}
	s.current[best] -= s.total
	return s.names[best]
}

func weightsDesc(prof *Profile) string {
	names := make([]string, 0, len(prof.Profiles))
	for name := range prof.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s=%.0f%%", n, prof.Profiles[n].Weight*100))
	}
	return strings.Join(parts, "/")
}

// userSeed 会话种子：盐 + 用户序号派生（确定性；同配置重跑同内容）。
func userSeed(salt, userIdx int) int64 {
	return int64(salt)*1000003 + int64(userIdx+1)*7919 + 42
}

// sharedBaseSeed 共享基座种子（shared_base=true）：与 userIdx 解耦，
// 全部用户得到同一基座内容；盐值仍参与（换盐隔离冷缓存）。
func sharedBaseSeed(salt int) int64 {
	return int64(salt)*1000003 + 977
}
