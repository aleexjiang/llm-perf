// llm-perf：客户自部署 LLM 推理服务性能评测工具。
//
// 契约：输入 YAML 配置，输出 JSON 原始数据；报告呈现由外部工具基于 JSON 二次加工。
//
// 用法（模式 = --turns × --concurrency 组合，无场景子命令）：
//
//	bench -c configs/example.yaml                                  # 默认 turns=both concurrency=1（单发单轮+多轮，零并发压力）
//	bench -c ... --turns single --concurrency 1                    # 单发单轮
//	bench -c ... --turns multi  --concurrency 1                    # 单发多轮
//	bench -c ... --turns single --concurrency 1,2,4                # 闭环并发爬坡（单轮）
//	bench -c ... --turns multi  --concurrency 2,4                  # 闭环并发爬坡（每用户独立多轮会话）
//	bench probe -c configs/example.yaml [模型名]                    # 兼容性探针
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aleexjiang/llm-perf/internal/auth"
	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/engine"
	"github.com/aleexjiang/llm-perf/internal/report"
	"github.com/aleexjiang/llm-perf/internal/scenario"
)

func usage() {
	fmt.Fprint(os.Stderr, `llm-perf - LLM 推理服务性能评测

输入: YAML 配置    输出: JSON 原始数据（报告请用外部工具基于 JSON 生成）

用法（模式 = --turns × --concurrency 组合，无场景子命令）:
  bench [-c 配置.yaml] [选项]
  bench probe [-c 配置.yaml] [模型名]

核心选项:
  --turns single|multi|both    单轮 / 多轮会话 / 两者都跑（默认 both）
  --concurrency 1|1,2,4|cfg    并发=1 表示单发（串行）；逗号列表逐档爬坡；
                               cfg 用配置里 concurrent.levels（默认 1）
  --thinking 变体名             只跑某个思考变体：on/off 或自定义档位名（如 low）；按变体名过滤，
                                模型无该变体则整个模型跳过；both=全部（缺省不过滤）
  --seed-salt N                战役隔离：重跑/换变体必须换盐，否则命中服务端前缀缓存
  -o 路径                       输出 .json 或目录（默认配置 output_dir）
  -m 模型子串                   只测包含该子串的模型
  --corpus en|zh|路径           填充语料；--max-ctx N 上下文截止

probe 选项:
  --no-toolcall                关闭 tool-call 健康检查（默认开启：检出引擎能否正常调工具，
                               失败时给可行动结论；多 4 次请求、秒级、不进压测路径）
  --probe-capture 目录          tool-call 检查的原始响应落盘（厂商排障证据/判据回归 fixture；
                               含业务数据，外发前按需脱敏）

组合语义:
  --concurrency 1 --turns single            单发单轮档位矩阵（ladder × runs，缓存对照）
  --concurrency 1 --turns multi             单发多轮会话（逐轮 history 滚动）
  --concurrency 2,4 --turns single          闭环并发（固定 prompt，level 爬坡）
  --concurrency 2,4 --turns multi           闭环并发（每虚拟用户独立多轮会话）
  列表含 1 和更大值                          先跑单发场景再跑并发档位（仅 >1 的档位）

排查模式:
  配置里 debug: true 时，原始响应留存到 <output_dir>/raw/、日志同步写 <output_dir>/run.log；
  任何请求失败时即使不开 debug 也会自动留存转储（写到系统临时目录）。
  Ctrl+C / kill / SSH 断开（SIGHUP）优雅中断：停止发新请求，已完成数据照常落盘；
  再按一次强制退出。长跑建议 nohup/tmux 挂后台，防连接抖动。

按模型组织:
  model_overrides.<模型>.enabled: false 跳过该模型（分批重测时临时关掉，全部禁用报错）；
  多模型数据按模型分区落 <output_dir>/<模型>/<场景>-<ts>.json（单模型仍直接落 output_dir）；
  run.log 与 raw/ 仍在 output_dir 顶层（战役级共享）。

示例:
  bench probe -c configs/customer.yaml
  bench -c configs/customer.yaml --turns single --concurrency 1 --thinking off -o out-single-off --seed-salt 1
  bench -c configs/customer.yaml --turns both --concurrency 1,2,4 -o output/
`)
	os.Exit(2)
}

// resolveOutPath 解析输出路径：
//   - 空 → outputDir/<scenario>-<ts>.json
//   - 以 .json 结尾 → 原样
//   - 其他 → 视为目录，拼默认文件名
func resolveOutPath(o, outputDir, scenarioName string) string {
	def := report.DefaultName(scenarioName)
	switch {
	case o == "":
		return filepath.Join(outputDir, def)
	case strings.HasSuffix(o, ".json"):
		return o
	default:
		return filepath.Join(o, def)
	}
}

// modelDirName 模型名 → 输出子目录名：取 "/" 后末段并清洗路径非法字符。
// 与 engine.sanitize 不同：保留 Unicode 字母/数字（中文模型名不清洗成同形碰撞的
// 连字符串），只把控制字符与文件系统不安全字符（/ \ : * ? " < > | 空格等）替换为 '-'。
// "/models/DeepSeek-V4-Flash-0731" → "DeepSeek-V4-Flash-0731"；清洗后为空回退 unknown。
func modelDirName(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	name := strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return '-'
		case strings.ContainsRune(`/\:*?"<>| `, r):
			return '-'
		default:
			return r // 字母数字与 - . _ 等安全字符原样保留（含中文）
		}
	}, model)
	if strings.Trim(name, "-.") == "" {
		return "unknown"
	}
	return name
}

// applyThinkingCLI 统一处理 --thinking：语义是"按变体名过滤"（合并各模型生效变体后筛选），
// 优先级 CLI > model_overrides > 全局——不做全局 mode 覆写：那会被 overrides.thinking.mode
// 反超（2026-09-08 客户现场踩坑：-thinking off 但 Qwen 的 overrides mode=both 仍跑了 on）。
// both = 不过滤跑全部；on/off/自定义档位名 = SetFilter，模型无该变体则整个模型跳过。
func applyThinkingCLI(cfg *config.Config, val string) error {
	if val == "both" {
		log.Printf("思考变体过滤（CLI）: both=不过滤，跑全部变体")
		return nil
	}
	anyLevels := len(cfg.Thinking.Levels) > 0
	for _, ov := range cfg.ModelOverrides {
		if ov != nil && ov.Thinking != nil && len(ov.Thinking.Levels) > 0 {
			anyLevels = true
		}
	}
	if anyLevels && (val == "on" || val == "off") {
		return fmt.Errorf("配置已使用 thinking.levels（含 model_overrides 覆盖），--thinking on/off 不适用——请用档位名过滤（如 --thinking low）")
	}
	nameSet := map[string]bool{}
	var names []string
	addVariantNames := func(t config.Thinking) {
		for _, n := range t.VariantNames() {
			key := strings.ToLower(n)
			if !nameSet[key] {
				nameSet[key] = true
				names = append(names, n)
			}
		}
	}
	addVariantNames(cfg.Thinking)
	for _, ov := range cfg.ModelOverrides {
		if ov != nil && ov.Thinking != nil {
			addVariantNames(*ov.Thinking)
		}
	}
	if !nameSet[strings.ToLower(val)] {
		return fmt.Errorf("--thinking %s 不匹配任何变体（可用: %s）", val, strings.Join(names, "/"))
	}
	cfg.Thinking.SetFilter(val)
	log.Printf("思考变体过滤（CLI）: 只跑 %s", val)
	return nil
}

func main() {
	if len(os.Args) == 1 {
		usage() // 裸调用不给参数：展示用法而不是拿默认配置开跑
	}
	args := os.Args[1:]
	mode := "bench"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "probe":
			mode = "probe"
			args = args[1:]
		default:
			fmt.Fprintf(os.Stderr, "未知参数 %q——压测模式没有场景子命令，用 --turns × --concurrency 组合（见下）\n\n", args[0])
			usage()
		}
	}

	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	cfgPath := fs.String("c", "configs/example.yaml", "YAML 配置文件路径")
	turnsFlag := fs.String("turns", "both", "turns=single|multi|both：单轮 / 多轮会话 / 两者都跑")
	concFlag := fs.String("concurrency", "1", "并发=1 表示单发（串行）；逗号列表如 1,2,4 逐档爬坡；cfg 用配置 concurrent.levels")
	modelFilter := fs.String("m", "", "只测包含该子串的模型")
	outFlag := fs.String("o", "", "输出路径：.json 文件或目录（默认用配置 output_dir）")
	corpusFlag := fs.String("corpus", "", "填充语料：en/zh（内置公版书）或自定义文件路径（.txt/.txt.gz）；覆盖配置 filler_corpus")
	maxCtxFlag := fs.Int("max-ctx", 0, "上下文截止（tokens）：>0 时所有请求 prompt 不超过该值；覆盖配置 max_prompt_tokens")
	saltFlag := fs.Int("seed-salt", 0, "种子盐值：隔离测试战役（服务端 prefix cache 未清空时重测用）；覆盖配置 seed_salt")
	thinkingFlag := fs.String("thinking", "", "只跑某个思考变体：on/off（开思考费 token，建议 off/on 分开两轮跑，互不连坐）；按变体名过滤，模型无该变体则跳过；both=全部")
	noToolCallFlag := fs.Bool("no-toolcall", false, "probe: 关闭 tool-call 健康检查（默认开启，多 4 次请求秒级）")
	captureFlag := fs.String("probe-capture", "", "probe: tool-call 检查原始响应落盘目录（排障证据/判据 fixture；含业务数据外发前脱敏）")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		os.Exit(1)
	}
	for _, w := range cfg.Warnings {
		log.Printf("配置提示: %s", w)
	}
	if *corpusFlag != "" {
		cfg.FillerCorpus = *corpusFlag
	}
	if *maxCtxFlag > 0 {
		cfg.MaxPromptTokens = *maxCtxFlag
		// CLI 显式指定 > model_overrides：同步写进每个模型覆盖，防止 overrides 反超运行时意图
		for _, ov := range cfg.ModelOverrides {
			if ov != nil {
				v := *maxCtxFlag
				ov.MaxPromptTokens = &v
			}
		}
		log.Printf("上下文截止（CLI 覆盖，含按模型覆盖）: %d", *maxCtxFlag)
	}
	if *saltFlag > 0 {
		cfg.SeedSalt = *saltFlag
	}
	if *thinkingFlag != "" {
		if err := applyThinkingCLI(cfg, *thinkingFlag); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	// 语料模式：真实公版文本填充，比随机词表更贴近真实负载的 tokenization 分布
	if cfg.FillerCorpus != "" {
		if err := engine.LoadCorpus(cfg.FillerCorpus, cfg.FillerLang); err != nil {
			fmt.Fprintln(os.Stderr, "语料加载失败:", err)
			os.Exit(1)
		}
		log.Printf("填充语料: %s（lang=%s）", engine.CorpusInfo(cfg.FillerLang), cfg.FillerLang)
	}
	// 大上下文提示：1M 级 prefill 可能远超默认超时
	if maxLadder := cfg.LargestPromptTokens(); maxLadder >= 100_000 && cfg.TimeoutSeconds < 600 {
		log.Printf("⚠️ 最大档位 %dtk ≥ 100k 而 timeout_seconds=%d 偏小，超长上下文 prefill 可能超时，建议 ≥ 900", maxLadder, cfg.TimeoutSeconds)
	}

	// 排查基础能力：run.log 始终写（现场排查时日志永远拿得到）；raw 转储由 debug 控制
	// run.log 追加而非覆盖：同目录多轮测试的日志都要留得住（两轮对照时踩过覆盖坑）
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err == nil {
		if lf, err := os.OpenFile(filepath.Join(cfg.OutputDir, "run.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(lf, "\n===== campaign %s（tool %s）=====\n", time.Now().Format(time.RFC3339), report.Version)
			log.SetOutput(io.MultiWriter(os.Stderr, lf))
			defer lf.Close()
		}
	}

	client := engine.NewClient(cfg.Endpoint, cfg.APIKey, cfg.Timeout(), *cfg.IncludeUsage)
	client.Auth = auth.Auth{Scheme: cfg.AuthScheme, Header: cfg.AuthHeader}
	client.ChatPath = cfg.ChatPath
	if cfg.AuthScheme != "" && cfg.AuthScheme != "bearer" || cfg.AuthHeader != "" {
		log.Printf("认证方案: %s", client.Auth.Describe())
	}
	if cfg.ChatPath != "/chat/completions" {
		log.Printf("接口路径（自定义）: %s", cfg.ChatPath)
	}
	if cfg.Retry != nil && cfg.Retry.MaxAttempts > 1 {
		backoff := time.Duration(cfg.Retry.BackoffMS) * time.Millisecond
		client.Retry = &engine.RetryPolicy{MaxAttempts: cfg.Retry.MaxAttempts, Backoff: backoff}
		log.Printf("重试策略: 连接层瞬时失败最多尝试 %d 次", cfg.Retry.MaxAttempts)
	}
	if cfg.Debug {
		client.DebugDir = filepath.Join(cfg.OutputDir, "raw")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Ctrl+C / kill / SSH 断开 优雅中断：第一次停止新请求并保存已完成数据；再按一次强制退出。
	// SIGHUP 必须捕获：堡垒机上 SSH 连接断开时内核向前台进程组发 SIGHUP——2026-09-09 现场
	// 整个 multiturn 场景因此丢失（场景级落盘，进程被杀时内存中的部分结果全丢、连优雅保存都没触发）。
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sigs
		log.Printf("🛑 收到中断信号（Ctrl+C 或连接断开）——停止发新请求，在飞请求将被取消，已完成数据照常保存（再按一次强制退出）")
		cancel()
		<-sigs
		fmt.Fprintln(os.Stderr, "强制退出，未保存的数据可能丢失")
		os.Exit(130)
	}()

	// ── probe：兼容性探测（不需要场景配置） ──
	if mode == "probe" {
		model := ""
		if fs.Arg(0) != "" {
			model = fs.Arg(0)
		} else if active := cfg.ActiveModels(); len(active) > 0 {
			model = active[0] // enabled=false 的模型不作为默认探测对象
		}
		th := cfg.ThinkingFor(model) // probe 也按模型解析思考配置（model_overrides.thinking 覆盖生效）
		res := engine.Probe(ctx, engine.ProbeOptions{
			Endpoint:       cfg.Endpoint,
			APIKey:         cfg.APIKey,
			Auth:           auth.Auth{Scheme: cfg.AuthScheme, Header: cfg.AuthHeader},
			ChatPath:       cfg.ChatPath,
			MetricsPath:    cfg.MetricsPath,
			ModelsPath:     cfg.ModelsPath,
			Model:          model,
			ThinkingOn:     th.ExtraBodyOn,
			ThinkingOff:    th.ExtraBodyOff,
			IncludeUsage:   *cfg.IncludeUsage,
			MaxContext:     cfg.LargestPromptTokens(),
			Timeout:        cfg.Timeout(),
			ToolCall:       !*noToolCallFlag,
			CaptureDir:     *captureFlag,
			XVPromptTokens: cfg.Concurrent.PromptTokens,
			XVMaxTokens:    cfg.Concurrent.MaxTokens.Max(), // probe 上下文探测按最大输出预算（prompt+output 最坏组合）
			ThinkingBudget: th.MaxTokensFloor,
		})
		outPath := resolveOutPath(*outFlag, cfg.OutputDir, "probe")
		if !strings.HasSuffix(*outFlag, ".json") {
			// 按模型分区落盘：probe 结果归到模型子目录（显式 -o xxx.json 尊重用户路径）
			outPath = filepath.Join(filepath.Dir(outPath), modelDirName(model), filepath.Base(outPath))
		}
		if err := report.SaveJSONAny(res, outPath); err != nil {
			fmt.Fprintln(os.Stderr, "写出探针 JSON 失败:", err)
			os.Exit(1)
		}
		fmt.Printf("引擎猜测: %s（Server 头: %s）\n", res.EngineGuess, res.Server)
		for _, m := range res.Models {
			fmt.Printf("  模型: %s\n", m)
		}
		for _, c := range res.Checks {
			mark := "✅"
			if !c.OK {
				mark = "❌"
			}
			fmt.Printf("%s %s: %s\n", mark, c.Name, c.Detail)
		}
		for _, v := range res.Verdicts {
			fmt.Printf("💡 %s\n", v)
		}
		for _, x := range res.CrossChecks {
			fmt.Printf("🔎 交叉验证建议（工具结果存疑时复核用）: %s\n", x.Tool)
			if x.Command != "" {
				fmt.Printf("   等价命令: %s\n", x.Command)
			}
			if x.Note != "" {
				fmt.Printf("   说明: %s\n", x.Note)
			}
		}
		fmt.Printf("探针完成，输出: %s\n", outPath)
		return
	}

	// ── 环境存档：引擎识别 + 配置原文随每份场景 JSON 落盘 ──
	// 几周后回看数据时"当时是什么引擎、什么配置跑的"必须有据可查；probe 失败不阻塞压测。
	var envInfo *engine.ProbeResult
	if active := cfg.ActiveModels(); len(active) > 0 {
		m := active[0]
		th := cfg.ThinkingFor(m)
		res := engine.Probe(ctx, engine.ProbeOptions{
			Endpoint:     cfg.Endpoint,
			APIKey:       cfg.APIKey,
			Auth:         auth.Auth{Scheme: cfg.AuthScheme, Header: cfg.AuthHeader},
			ChatPath:     cfg.ChatPath,
			MetricsPath:  cfg.MetricsPath,
			ModelsPath:   cfg.ModelsPath,
			Model:        m,
			ThinkingOn:   th.ExtraBodyOn,
			ThinkingOff:  th.ExtraBodyOff,
			IncludeUsage: *cfg.IncludeUsage,
			Timeout:      30 * time.Second,
			ToolCall:     false, // 存档用轻量探针：识别引擎即可，不做 tool-call 检查
		})
		if res == nil {
			log.Printf("⚠️ 环境探测无结果（不影响压测继续）")
		} else {
			envInfo = res
			log.Printf("环境存档: 引擎猜测 %s（Server 头: %s）", res.EngineGuess, res.Server)
		}
	}

	// ── 组合模式解析：--turns × --concurrency → 场景执行计划 ──
	var concVals []int
	for _, p := range strings.Split(*concFlag, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p == "cfg" {
			concVals = append(concVals, cfg.Concurrent.Levels...)
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 {
			fmt.Fprintf(os.Stderr, "--concurrency %q 无效：用 1、逗号列表（1,2,4）或 cfg\n", p)
			os.Exit(1)
		}
		concVals = append(concVals, n)
	}
	if len(concVals) == 0 {
		fmt.Fprintln(os.Stderr, "--concurrency 解析结果为空")
		os.Exit(1)
	}
	seen := map[int]bool{}
	var vals []int
	for _, n := range concVals {
		if !seen[n] {
			seen[n] = true
			vals = append(vals, n)
		}
	}
	var turns []string
	switch *turnsFlag {
	case "single":
		turns = []string{"single"}
	case "multi":
		turns = []string{"multi"}
	case "both":
		turns = []string{"single", "multi"}
	default:
		fmt.Fprintf(os.Stderr, "--turns %q 无效：用 single|multi|both\n", *turnsFlag)
		os.Exit(1)
	}

	type runItem struct {
		name  string
		sc    scenario.Scenario
		highs []int // >1 的并发档位（交给 concurrent 场景）；空 = 纯单发场景
		mt    bool  // concurrent 场景是否跑多轮会话
	}
	var items []runItem
	for _, tm := range turns {
		var ones, highs []int
		for _, n := range vals {
			if n == 1 {
				ones = append(ones, n)
			} else {
				highs = append(highs, n)
			}
		}
		if len(ones) > 0 {
			name := "single"
			if tm == "multi" {
				name = "multiturn"
			}
			sc, _ := scenario.Lookup(name)
			items = append(items, runItem{name: name, sc: sc})
		}
		if len(highs) > 0 {
			name := "concurrent"
			if tm == "multi" {
				name = "concurrent-multi"
			}
			sc, _ := scenario.Lookup("concurrent")
			items = append(items, runItem{name: name, sc: sc, highs: highs, mt: tm == "multi"})
		}
	}
	var names []string
	for _, it := range items {
		names = append(names, it.name)
	}
	log.Printf("执行计划: turns=%s concurrency=%v → %s（并发=1 即单发串行）", *turnsFlag, vals, strings.Join(names, " → "))

	// 战役画像（5.10）：开跑前打印"这次要跑什么形状"总览；同一份数据随每份 JSON 落盘
	planItems := make([]scenario.PlanItem, len(items))
	for i, it := range items {
		planItems[i] = scenario.PlanItem{Name: it.name, Highs: it.highs, MT: it.mt}
	}
	campaignPlan := scenario.PlanSummary(cfg, *modelFilter, planItems)
	if campaignPlan != nil {
		for _, l := range campaignPlan.Render() {
			log.Print(l)
		}
	}

	if len(items) > 1 && strings.HasSuffix(*outFlag, ".json") {
		fmt.Fprintln(os.Stderr, "本次组合会跑多个场景（各落一个 JSON），-o 请给目录而不是单个 .json 文件")
		os.Exit(1)
	}

	run := func(name string, fn func() (*report.Report, error)) {
		start := time.Now()
		rep, err := fn()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] 失败: %v\n", name, err)
			os.Exit(1)
		}
		outPath := resolveOutPath(*outFlag, cfg.OutputDir, name)
		// 环境存档随每份分区落盘（引擎识别 + 配置原文）；战役画像同附（回溯"当时的计划"）
		rep.Environment = envInfo
		rep.ConfigRaw = cfg.Raw
		rep.Plan = campaignPlan
		// 按模型分区落盘：多模型战役各落 <output_dir>/<模型>/，重测/作废单模型不纠缠；
		// 单模型（或 -m 过滤后只剩一个）保持原布局直接落 output_dir，报告工具兼容两种布局
		parts := rep.PartitionByModel()
		if len(parts) == 0 {
			parts = []*report.Report{rep}
		}
		if len(parts) > 1 && strings.HasSuffix(*outFlag, ".json") {
			fmt.Fprintln(os.Stderr, "本次跑了多个模型（数据按模型分区落盘），-o 请给目录而不是单个 .json 文件")
			os.Exit(1)
		}
		write := func(p *report.Report, path string) {
			if err := p.SaveJSON(path); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] 写出 JSON 失败: %v\n", name, err)
				os.Exit(1)
			}
		}
		if len(parts) == 1 {
			write(parts[0], outPath)
		} else {
			dir := filepath.Dir(outPath)
			for _, p := range parts {
				write(p, filepath.Join(dir, modelDirName(p.PartitionModel), filepath.Base(outPath)))
			}
		}
		if ctx.Err() != nil {
			fmt.Printf("[%s] ⚠️ 中断——已完成的 %d 组数据已保存: %s\n",
				name, len(rep.Single)+len(rep.Multiturn)+len(rep.Concurrent), outPath)
			return
		}
		if len(parts) > 1 {
			var dirs []string
			for _, p := range parts {
				dirs = append(dirs, modelDirName(p.PartitionModel)+"/")
			}
			fmt.Printf("[%s] 完成，用时 %s，输出: %s（按模型分区: %s）\n",
				name, time.Since(start).Round(time.Second), filepath.Dir(outPath), strings.Join(dirs, " "))
		} else {
			fmt.Printf("[%s] 完成，用时 %s，输出: %s\n", name, time.Since(start).Round(time.Second), outPath)
		}
	}

	for _, it := range items {
		it := it
		cc := *cfg // 场景间互不影响：并发档位/多轮开关按本项覆盖
		if len(it.highs) > 0 {
			cc.Concurrent.Levels = it.highs
			cc.Concurrent.Multiturn = it.mt
		}
		run(it.name, func() (*report.Report, error) { return it.sc.Run(ctx, &cc, client, *modelFilter) })
		if ctx.Err() != nil {
			return // 中断后不再启动后续场景
		}
	}
}
