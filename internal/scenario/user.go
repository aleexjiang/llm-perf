// user.go：user 模式——生成式多轮用户会话（docs/workload-refactor-plan.md 13）。
//
// 与旧 multiturn/filler 的本质区别：
//   - 会话形状来自外置 profile.json（trace 特征提炼），默认权重 6:3:1 可调；
//   - 运行时文本（system 基座 / user 输入 / 合成 context）全部取自经典书语料库，
//     窗口起点由 (session_seed, turn) 派生，内容确定可复现；
//   - 每轮把**被测模型真实生成的 assistant 回复**追加进下一轮 history——
//     prefix cache 反映当前模型真实回复形成的动态前缀（与冻结快照的本质差异）；
//   - agent 形状硬约束：首轮 prompt ≥ 35K token（基座 + 首轮 user/context）。
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
	"github.com/aleexjiang/llm-perf/internal/corpus"
	"github.com/aleexjiang/llm-perf/internal/engine"
	"github.com/aleexjiang/llm-perf/internal/report"
)

// firstTurnBaseTokens 首轮基座（system）目标 token：落在 [35K,40K] 首轮约束内，
// 余量留给首轮 user 输入与合成 context。基座一经生成本会话内冻结。
const firstTurnBaseTokens = 27000

// assistantReplyCap 真实 assistant 回复进 history 的截断上限（字符，rune 安全）。
// 输出长度受 max_tokens 请求约束，此上限只是防御性护栏（如服务端不尊重 max_tokens）。
const assistantReplyCap = 32768

// contextTag 合成 context 的包裹标记：拼在 user 消息内部（尾部注入，cache 安全）。
const contextTag = "reference-context"

func init() {
	Register(funcScenario{"user", UserScenario})
}

// UserScenario user 模式：profile 驱动的生成式多轮会话。
func UserScenario(ctx context.Context, cfg *config.Config, client *engine.Client, modelFilter string) (*report.Report, error) {
	if cfg.User.ProfilePath == "" {
		return nil, fmt.Errorf("user 模式需要 user.profile_path（scripts/profile_build.py 产出的 profile.json）")
	}
	prof, err := LoadProfile(cfg.User.ProfilePath)
	if err != nil {
		return nil, err
	}
	lang := cfg.CorpusLang
	if lang == "" {
		lang = "en"
	}
	if corpus.Library(lang) == nil {
		return nil, fmt.Errorf("user 模式语料库为空（lang=%s）——检查 corpus_lang/filler_lang 配置", lang)
	}

	e := &env{cfg: cfg, client: client, perReqSrv: false}
	rep := &report.Report{
		Tool:        report.Version,
		Scenario:    "user",
		GeneratedAt: time.Now(),
		Test:        cfg.TestKind(),
		Endpoint:    cfg.Endpoint,
		Note: fmt.Sprintf("生成式多轮用户会话 users=%d profile=%s（权重 %s）语料=%s shared_base=%v stream=%v thinking=%s；首轮≥%dtk；assistant=被测模型真实回复（动态 prefix cache）%s",
			cfg.User.GetUsers(), prof.Source, weightsDesc(prof), lang, cfg.User.GetSharedBase(),
			cfg.StreamEnabled(), cfg.Thinking.Mode, prof.FirstTurnTokens[0], thinkingNoteSuffix(cfg)),
	}
	applySLO(e, rep)

	before, poller, winStart := startWindow(ctx, e)
	defer func() { rep.Server = finishWindow(e, before, poller, winStart) }()

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
				if interrupted(ctx) {
					break
				}
				runs := runUserSessions(ctx, em, prof, lang, model, v, maxTok)
				rep.Multiturn = append(rep.Multiturn, runs...)
			}
		}
	}
	return rep, nil
}

// runUserSessions 并行执行 users 条生成式会话（每用户一条，模型串行保证 e.cfg 不被并发改写）。
func runUserSessions(ctx context.Context, e *env, prof *Profile, lang, model string,
	v config.ThinkingVariant, maxTok int) []report.MultiturnRun {

	users := e.cfg.User.GetUsers()
	selector := newSWRR(prof)
	labels := make([]string, users)
	for u := 0; u < users; u++ {
		labels[u] = selector.next()
	}
	runs := make([]report.MultiturnRun, users)
	var wg sync.WaitGroup
	for u := 0; u < users; u++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if interrupted(ctx) {
				return
			}
			runs[idx] = runOneUserSession(ctx, e, prof, lang, model, v, maxTok, idx, labels[idx])
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
	v config.ThinkingVariant, maxTok int, userIdx int, profileLabel string) report.MultiturnRun {

	run := report.MultiturnRun{
		Model:     model,
		Thinking:  v.Name,
		Session:   userIdx + 1,
		MaxTokens: maxTok,
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
	// 不把 MaxPromptTokens（token 数）当轮次上限——上下文到顶由 ctxLimitHit 运行时止损。
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
		Content: baseBook.Window(int(firstTurnBaseTokens*cpr), baseWindowSeed)}

	sample := func(r []int) int {
		if len(r) != 2 || r[1] <= r[0] {
			return r[0]
		}
		return r[0] + rng.Intn(r[1]-r[0]+1)
	}

	firstTotal := sample(prof.FirstTurnTokens)
	msgs := []engine.Message{baseMsg}
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
		ctxTk := 0
		if turn == 0 {
			// 首轮：user+context 补足到 [35K,40K]（agent 形状硬约束）
			ctxTk = firstTotal - firstTurnBaseTokens - userTk
			lo := spec.ContextTokens[0]
			if ctxTk < lo {
				ctxTk = lo // 档位下限优先：宁可超首轮上限也不产生空转轮
			}
		} else {
			ctxTk = sample(spec.ContextTokens)
		}
		if ctxTk > 0 {
			fmt.Fprintf(&content, "<%s>\n%s\n</%s>\n\n", contextTag,
				book.Window(int(float64(ctxTk)*cpr), rng.Int63()), contextTag)
		}
		content.WriteString(question)
		msgs = append(msgs, engine.Message{Role: "user", Content: content.String()})

		m := runOne(ctx, e, model, msgs, maxTok, v)
		run.Turns = append(run.Turns, m)
		// 首轮实测深度校验（review R1-L1）：估算保证的 35K 依赖 CharsPerToken 系数，
		// 真实 tokenizer 偏差在此暴露（session 内只告警一次）
		if turn == 0 && m.PromptTokens > 0 && m.PromptTokens < prof.FirstTurnTokens[0] {
			log.Printf("    ⚠️ 首轮实测 prompt %dtk < 约束 %dtk——语料换算比偏差，建议跑 bench probe 看 filler_fidelity",
				m.PromptTokens, prof.FirstTurnTokens[0])
		}
		// 真实 assistant 回复进 history：动态 prefix cache 的核心（不能用 trace 旧回复）
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
		if interrupted(ctx) {
			break
		}
	}
	run.FillLastPromptTokens()
	return run
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
