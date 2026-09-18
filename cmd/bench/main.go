// llm-perf：客户自部署 LLM 推理服务性能评测工具。
//
// 契约：输入 YAML 配置，输出 JSON 原始数据；报告呈现由外部工具基于 JSON 二次加工。
//
// 用法（显式子命令，见 docs/workload-refactor-plan.md）：
//
//	bench probe        -c cfg.yaml [模型名]   # 能力与健康探针
//	bench user         -c cfg.yaml            # 生成式多轮用户会话（profile 驱动）
//	bench rps          -c cfg.yaml            # 冻结请求快照的开环到达
//	bench concurrency  -c cfg.yaml            # 固定在飞齐射（对齐 vLLM bench serve）
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
	"strings"
	"syscall"
	"time"

	"github.com/aleexjiang/llm-perf/internal/auth"
	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/engine"
	"github.com/aleexjiang/llm-perf/internal/report"
	"github.com/aleexjiang/llm-perf/internal/scenario"
)

// agentBaselineMaxContext probe 上下文预警的默认基准（token）：现代 agent 产品基线上下文
// 即 ~35-40K（system + 工具定义 + RAG 注入），与 docs/latency-baselines.md 的判据档一致。
// 计划压测的最大上下文超过模型 max_model_len 时 probe 会显式告警；--max-ctx 可显式覆盖。
const agentBaselineMaxContext = 40000

func usage() {
	fmt.Fprint(os.Stderr, `llm-perf - LLM 推理服务性能评测

输入: YAML 配置    输出: JSON 原始数据（报告请用外部工具基于 JSON 生成）

用法（显式子命令）:
  bench probe        [-c 配置.yaml] [模型名]   能力与健康探针（引擎/usage/thinking/tool-call/metrics）
  bench user         [-c 配置.yaml]            生成式多轮用户会话（profile 驱动，动态 prefix cache）
  bench rps          [-c 配置.yaml]            冻结请求快照的开环到达（排队-延迟曲线，体验拐点）
  bench concurrency  [-c 配置.yaml]            固定在飞齐射（总吞吐拐点，对齐 vLLM bench serve）

通用选项:
  --thinking 变体名             只跑某个思考变体：on/off 或自定义档位名（如 low）；按变体名过滤，
                                模型无该变体则整个模型跳过；both=全部（缺省不过滤）
  --seed-salt N                 测试隔离：重跑/换变体必须换盐，否则命中服务端前缀缓存
  -o 路径                       输出 .json 或目录（默认配置 output_dir）
  -m 模型子串                   只测包含该子串的模型
  --max-ctx N                   上下文截止（tokens）：user 多轮到顶停轮；probe 上下文预警基准（默认 40000）

user 选项:
  --users N                     并行用户数（每用户一条生成式多轮会话）；>0 覆盖配置 user.users
  配置段: user.profile_path（profile_build.py 产出）、user.max_tokens、user.shared_base

rps/concurrency 选项（请求源 = request_set.sharegpt_path，与 vLLM bench serve 同口径）:
  配置段: rps.rates/max_concurrency/burstiness；concurrency.levels/request_rate/burstiness；
  request_set.num_prompts/seed/max_output_tokens

probe 选项:
  --no-toolcall                 关闭 tool-call 健康检查（默认开启）
  --probe-capture 目录          tool-call 检查的原始响应落盘
  --cache                       开启前缀缓存定性检查（默认关）
  --cache-size N                缓存检查的上下文大小（tokens，默认 40000）
  --corpus 路径|en|zh           probe 语料校准（filler_fidelity）用；覆盖配置 corpus_path

排查模式:
benchmark、失败和主动取消请求都保留完整原始指标（按 phase 分组落盘）。
配置里 debug: true 时，原始响应留存到 <output_dir>/raw/、日志同步写 <output_dir>/run.log。
  Ctrl+C / kill / SSH 断开（SIGHUP）优雅中断：停止发新请求，已完成数据照常落盘；
  再按一次强制退出。长跑建议 nohup/tmux 挂后台，防连接抖动。

按模型组织:
  model_overrides.<模型>.enabled: false 跳过该模型（分批重测时临时关掉，全部禁用报错）；
  多模型数据按模型分区落 <output_dir>/<模型>/<场景>-<ts>.json（单模型仍直接落 output_dir）；
  run.log 与 raw/ 仍在 output_dir 顶层（测试级共享）。

示例:
  bench probe -c configs/customer.yaml
  bench user -c configs/customer.yaml --users 8 --thinking off --seed-salt 1
  bench rps -c configs/customer.yaml -o output/
  bench concurrency -c configs/customer.yaml -o output/
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
		case "probe", "user", "rps", "concurrency":
			mode = args[0]
			args = args[1:]
		default:
			fmt.Fprintf(os.Stderr, "未知参数 %q——子命令：probe/user/rps/concurrency\n\n", args[0])
			usage()
		}
	}

	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	cfgPath := fs.String("c", "configs/example.yaml", "YAML 配置文件路径")
	modelFilter := fs.String("m", "", "只测包含该子串的模型")
	outFlag := fs.String("o", "", "输出路径：.json 文件或目录（默认用配置 output_dir）")
	corpusFlag := fs.String("corpus", "", "语料校准：en/zh（内置）或文件路径（.txt/.txt.gz）；覆盖配置 corpus_path（probe filler_fidelity 用）")
	maxCtxFlag := fs.Int("max-ctx", 0, "上下文截止（tokens）：user 多轮到顶停轮；probe 上下文预警基准；覆盖配置 max_prompt_tokens")
	saltFlag := fs.Int("seed-salt", 0, "种子盐值：测试隔离（服务端 prefix cache 未清空时重测用）；覆盖配置 seed_salt")
	thinkingFlag := fs.String("thinking", "", "只跑某个思考变体：on/off（开思考费 token，建议 off/on 分开两轮跑，互不连坐）；按变体名过滤，模型无该变体则跳过；both=全部")
	usersFlag := fs.Int("users", 0, "user: 并行用户数（每用户一条生成式多轮会话）；>0 覆盖配置 user.users")
	noToolCallFlag := fs.Bool("no-toolcall", false, "probe: 关闭 tool-call 健康检查（默认开启，多 4 次请求秒级）")
	captureFlag := fs.String("probe-capture", "", "probe: tool-call 检查原始响应落盘目录（排障证据/判据 fixture；含业务数据外发前脱敏）")
	cacheFlag := fs.Bool("cache", false, "probe: 开启前缀缓存定性检查（默认关；同 prompt 连发 3 次 + 乱序冷基线各 1 次，长上下文请求）")
	cacheSizeFlag := fs.Int("cache-size", 40000, "probe --cache 的上下文大小（tokens，默认 40000；自动收到模型上限以内）")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		os.Exit(1)
	}
	// 子命令前置校验：各自的必需配置段在开跑前确认，现场跑完才发现没生效是最贵的错误
	switch mode {
	case "user":
		if *usersFlag > 0 {
			cfg.User.Users = *usersFlag
		}
		if cfg.User.ProfilePath == "" {
			fmt.Fprintln(os.Stderr, "user 模式需要在配置里指定 user.profile_path（scripts/profile_build.py 产出）")
			os.Exit(1)
		}
	case "rps", "concurrency":
		if cfg.RequestSet.ShareGPTPath == "" {
			fmt.Fprintf(os.Stderr, "%s 模式需要在配置里指定 request_set.sharegpt_path（ShareGPT 数据集，与 vLLM bench serve 同源）\n", mode)
			os.Exit(1)
		}
		if mode == "rps" && len(cfg.RPS.Rates) == 0 {
			fmt.Fprintln(os.Stderr, "rps 模式需要在配置里指定 rps.rates（到达率档位 req/s）")
			os.Exit(1)
		}
		if mode == "concurrency" && len(cfg.Concurrency.Levels) == 0 {
			fmt.Fprintln(os.Stderr, "concurrency 模式需要在配置里指定 concurrency.levels（在飞请求数档位）")
			os.Exit(1)
		}
	}
	for _, w := range cfg.Warnings {
		log.Printf("配置提示: %s", w)
	}
	// 非默认类别才提示：performance 是常态不值得占一行；其它类别要让操作者知道
	// 报告的结论区口径与默认不同（同一份数据，首屏先说哪件事不一样）。
	if k := cfg.TestKind(); k != config.TestPerformance {
		log.Printf("测试类别: %s（报告结论区按此口径渲染）", k)
	}
	if *corpusFlag != "" {
		cfg.CorpusPath = *corpusFlag
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

	// 语料校准（probe filler_fidelity 用）：corpus_path 配置时加载，让 Filler 走真实
	// 文本语料而不是合成词表；user 模式不走这里（内置语料库自动加载）
	if cfg.CorpusPath != "" {
		if err := engine.LoadCorpus(cfg.CorpusPath, cfg.CorpusLang); err != nil {
			fmt.Fprintln(os.Stderr, "语料加载失败:", err)
			os.Exit(1)
		}
		log.Printf("校准语料: %s（lang=%s）", engine.CorpusInfo(cfg.CorpusLang), cfg.CorpusLang)
	}

	// SIGHUP 必须捕获：堡垒机上 SSH 连接断开时内核向前台进程组发 SIGHUP——2026-09-09 现场
	// 整个场景因此丢失（场景级落盘，进程被杀时内存中的部分结果全丢、连优雅保存都没触发）。
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-sigs
		log.Printf("🛑 收到中断信号（Ctrl+C 或连接断开）——停止发新请求，在飞请求将被取消，已完成数据照常保存（再按一次强制退出）")
		cancel()
		<-sigs
		fmt.Fprintln(os.Stderr, "强制退出，未保存的数据可能丢失")
		os.Exit(130)
	}()

	client := engine.NewClient(cfg.Endpoint, cfg.APIKey, cfg.Timeout(), *cfg.IncludeUsage)
	client.Auth = auth.Auth{Scheme: cfg.AuthScheme, Header: cfg.AuthHeader}
	client.ChatPath = cfg.ChatPath
	client.RawTimings = cfg.RawTimingsEnabled() // 原始 chunk 序列落盘（raw_timings，默认开）
	if cfg.AuthScheme != "" && cfg.AuthScheme != "bearer" || cfg.AuthHeader != "" {
		// 客户网关非标认证（裸 key/自定义 header）必须在日志可见——认证问题是现场第一常见故障
		log.Printf("认证方案: %s", client.Auth.Describe())
	}
	client.Retry = &engine.RetryPolicy{} // 默认不重试（压测语义）；retry 配置存在时按配置启用
	if cfg.Retry != nil && cfg.Retry.MaxAttempts > 1 {
		client.Retry = &engine.RetryPolicy{MaxAttempts: cfg.Retry.MaxAttempts, Backoff: time.Duration(cfg.Retry.BackoffMS) * time.Millisecond}
		log.Printf("重试策略: 最多 %d 次尝试，退避基数 %dms（只重试连接层瞬时失败）", client.Retry.MaxAttempts, client.Retry.Backoff.Milliseconds())
	}

	// 排查基础能力：run.log 始终写（现场排查时日志永远拿得到）；raw 转储由 debug 控制
	// run.log 追加而非覆盖：同目录多轮测试的日志都要留得住（两轮对照时踩过覆盖坑）
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "run.log 目录初始化失败: %v\n", err)
	} else if lf, err := os.OpenFile(filepath.Join(cfg.OutputDir, "run.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "run.log 打开失败: %v\n", err)
	} else {
		_ = lf.Chmod(0o600)
		fmt.Fprintf(lf, "\n===== test run %s（tool %s）=====\n", time.Now().Format(time.RFC3339), report.Version)
		log.SetOutput(io.MultiWriter(os.Stderr, lf))
		defer lf.Close()
	}

	run := func(name, outPath string, fn func() (*report.Report, error)) {
		start := time.Now()
		rep, err := fn()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] 失败: %v\n", name, err)
			os.Exit(1)
		}
		// 配置原文随每份分区落盘，环境 probe 结果由独立 probe JSON 提供。
		rep.ConfigRaw = cfg.Raw
		// 按模型分区落盘：多模型测试各落 <output_dir>/<模型>/，重测/作废单模型不纠缠；
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
				name, len(rep.Multiturn)+len(rep.Concurrent), outPath)
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

	// ── probe：能力与健康探针（不需要场景配置，独立 JSON）──
	if mode == "probe" {
		model := ""
		if fs.Arg(0) != "" {
			model = fs.Arg(0)
		} else if active := cfg.ActiveModels(); len(active) > 0 {
			model = active[0] // enabled=false 的模型不作为默认探测对象
		}
		th := cfg.ThinkingFor(model) // probe 也按模型解析思考配置（model_overrides.thinking 覆盖生效）
		probeOn, probeOff := th.ProbeExtraBodies()
		maxContext := *maxCtxFlag
		if maxContext == 0 {
			maxContext = agentBaselineMaxContext
		}
		res := engine.Probe(ctx, engine.ProbeOptions{
			Endpoint:    cfg.Endpoint,
			APIKey:      cfg.APIKey,
			Auth:        auth.Auth{Scheme: cfg.AuthScheme, Header: cfg.AuthHeader},
			ChatPath:    cfg.ChatPath,
			MetricsPath: cfg.MetricsPath,
			ModelsPath:  cfg.ModelsPath,
			Model:       model,
			// 逐模型实测 decode 速度并给最慢模型出建议（10.1）：统一 min_tps 会误熔断更慢的模型
			Models:          cfg.ActiveModels(),
			ThinkingOn:      probeOn,
			ThinkingOff:     probeOff,
			IncludeUsage:    *cfg.IncludeUsage,
			MaxContext:      maxContext,
			Timeout:         cfg.Timeout(),
			ToolCall:        !*noToolCallFlag,
			CaptureDir:      *captureFlag,
			XVPromptTokens:  10000, // 引擎识别探测的 prompt 规模（与旧默认值一致）
			XVMaxTokens:     256,   // probe 上下文探测按最大输出预算（prompt+output 最坏组合）
			ThinkingBudget:  th.MaxTokensFloorValue(),
			CacheCheck:      *cacheFlag,
			CacheSizeTokens: *cacheSizeFlag,
			FillerLang:      cfg.CorpusLang,
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
			// 扩展面未提供（NA）用 ➖ 而不是 ❌：没有 /metrics、没有 max_model_len
			// 是经网关代理后的常见形态，不是端点缺陷，更不该拉低通过率
			mark, suffix := "✅", ""
			switch {
			case c.NA:
				mark, suffix = "➖", "  [扩展面未提供，不计入结论]"
			case !c.OK:
				mark = "❌"
			}
			if c.Tier == engine.TierExt {
				suffix = "  [扩展面]" + suffix
			}
			fmt.Printf("%s %s: %s%s\n", mark, c.Name, c.Detail, suffix)
		}
		if res.Summary != "" {
			fmt.Printf("\n📊 %s\n", res.Summary)
		}
		if cp := res.CacheProbe; cp != nil {
			fmt.Printf("\n前缀缓存定性检查（上下文 %dtk，max_tokens=64 隔离 prefill）:\n", cp.SizeTokens)
			fmt.Printf("  %-8s %10s %10s %12s %14s\n", "轮次", "TTFT", "总耗时", "prompt_tk", "cached_tk")
			row := func(r engine.CacheRun) {
				if r.Error != "" {
					errStr := r.Error
					if len(errStr) > 80 {
						errStr = errStr[:80] + "..."
					}
					fmt.Printf("  %-8s %10s %10s %12s %14s  err=%s\n", r.Label, "-", "-", "-", "-", errStr)
					return
				}
				fmt.Printf("  %-8s %9.0fms %9.0fms %12d %14d\n", r.Label, r.TTFTMS, r.E2EMS, r.PromptTokens, r.CachedTokens)
			}
			for _, r := range cp.Warm {
				row(r)
			}
			for _, r := range cp.Cold {
				row(r)
			}
			mark := "❌"
			if cp.Hit {
				mark = "✅"
			}
			fmt.Printf("%s 判读: %s\n", mark, cp.Verdict)
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
		if res.Suggested != "" {
			fmt.Printf("\n📝 可粘贴回配置文件的片段（注释项来自引擎扩展面，换端点后需重新探测）:\n")
			for _, line := range strings.Split(strings.TrimRight(res.Suggested, "\n"), "\n") {
				fmt.Printf("    %s\n", line)
			}
		}
		fmt.Printf("探针完成，输出: %s\n", outPath)
		return
	}

	// ── 压测子命令：user / rps / concurrency（新场景架构，见 docs/workload-refactor-plan.md）──
	// 形状/请求集全部来自各自配置段；配错在 config.Load 与上方前置校验 fail-fast。
	sc, ok := scenario.Lookup(mode)
	if !ok {
		fmt.Fprintf(os.Stderr, "%s 场景未注册\n", mode)
		os.Exit(1)
	}
	outPath := resolveOutPath(*outFlag, cfg.OutputDir, mode)
	log.Printf("执行计划: %s 模式", mode)
	run(mode, outPath, func() (*report.Report, error) {
		return sc.Run(ctx, cfg, client, *modelFilter)
	})
}
