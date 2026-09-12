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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
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
  --seed-salt N                测试隔离：重跑/换变体必须换盐，否则命中服务端前缀缓存
  -o 路径                       输出 .json 或目录（默认配置 output_dir）
  -m 模型子串                   只测包含该子串的模型
  --corpus en|zh|路径           填充语料；--max-ctx N 上下文截止

probe 选项:
  --no-toolcall                关闭 tool-call 健康检查（默认开启：检出引擎能否正常调工具，
                               失败时给可行动结论；多 4 次请求、秒级、不进压测路径）
  --probe-capture 目录          tool-call 检查的原始响应落盘（厂商排障证据/判据回归 fixture；
                               含业务数据，外发前按需脱敏）
  --cache                      开启前缀缓存定性检查（默认关：多 4 次长上下文请求，40k 档约多花 1-2 分钟）
  --cache-size N               缓存检查的上下文大小（tokens，默认 40000，自动收到模型上限以内）

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
  run.log 与 raw/ 仍在 output_dir 顶层（测试级共享）。

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
	saltFlag := fs.Int("seed-salt", 0, "种子盐值：隔离测试（服务端 prefix cache 未清空时重测用）；覆盖配置 seed_salt")
	stallTPSFlag := fs.Float64("stall-tps", 0, "降速熔断阈值（tok/s，聚合输出速度）：>0 时开启并覆盖 stall_guard.min_tps（默认 10）")
	stallWindowFlag := fs.Int("stall-window", 0, "降速熔断判定窗口（秒）：覆盖 stall_guard.window_seconds（默认 600）")
	stallCooldownFlag := fs.Int("stall-cooldown", 0, "熔断后到下一个场景的冷却（秒）：覆盖 stall_guard.cooldown_seconds（默认 300；0 表示用配置值）")
	stallProbeFlag := fs.Float64("stall-probe-factor", 0, "熔断冷却后的恢复探针倍数：探针实测 tok/s ≥ 倍数×min_tps 才继续下一场景，否则停止整轮；>0 覆盖 stall_guard.probe_factor（默认 2，0=关闭退回纯计时；关闭只能走配置）")
	noStallFlag := fs.Bool("no-stall-guard", false, "关闭降速熔断（覆盖配置 stall_guard，用于需要跑完整轮的场景）")
	noStallTraceFlag := fs.Bool("no-stall-trace", false, "关闭降速采样序列落盘（<结果 JSON 同名>.stall.csv；默认开启，纯记录不影响行为）")
	satWaitingFlag := fs.Int("sat-waiting", 0, "饱和止损：服务端 waiting 排队深度阈值（需 server_metrics；持续超过达 --sat-window 秒即停止向当前档位发新请求并停止后续档位）；>0 覆盖 saturation_guard.max_waiting；标定：关着 guard 跑一轮，取各档 waiting 峰值 ×3–5（报告会自动给建议值）")
	satWindowFlag := fs.Int("sat-window", 0, "饱和止损判定窗口（秒）：持续超阈多久触发（默认 120）；覆盖 saturation_guard.window_seconds")
	satWallFlag := fs.Int("sat-max-wall", 0, "饱和止损：单个档位/到达率发射窗口上限（秒，0=不限）——到点后停止发新请求，在飞跑完保留全量；>0 覆盖 saturation_guard.max_wall_seconds")
	noSatFlag := fs.Bool("no-sat-guard", false, "关闭饱和止损（覆盖配置 saturation_guard）")
	thinkingFlag := fs.String("thinking", "", "只跑某个思考变体：on/off（开思考费 token，建议 off/on 分开两轮跑，互不连坐）；按变体名过滤，模型无该变体则跳过；both=全部")
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
	// 降速熔断：CLI 覆盖配置（配置校验已在 Load 内完成并填过默认值，这里补同样的默认口径）
	if *noStallFlag {
		cfg.StallGuard = nil
		log.Printf("降速熔断: 已由 CLI 关闭（--no-stall-guard）")
	} else if *stallTPSFlag > 0 || *stallWindowFlag > 0 || *stallCooldownFlag > 0 || *stallProbeFlag > 0 {
		sg := cfg.StallGuard
		if sg == nil {
			sg = &config.StallGuardCfg{}
			cfg.StallGuard = sg
		}
		on := true
		sg.Enabled = &on
		if *stallTPSFlag > 0 {
			sg.MinTPS = *stallTPSFlag
		}
		if *stallWindowFlag > 0 {
			sg.WindowSeconds = *stallWindowFlag
		}
		if *stallCooldownFlag > 0 {
			sg.CooldownSeconds = *stallCooldownFlag
		}
		if *stallProbeFlag > 0 {
			pf := *stallProbeFlag
			sg.ProbeFactor = &pf
		}
		if sg.MinTPS <= 0 {
			sg.MinTPS = 10
		}
		if sg.WindowSeconds <= 0 {
			sg.WindowSeconds = 600
		}
		if sg.CooldownSeconds <= 0 {
			sg.CooldownSeconds = 300
		}
		if sg.SampleSeconds <= 0 {
			sg.SampleSeconds = 2
		}
		log.Printf("降速熔断（CLI 覆盖）: 聚合输出速度持续低于 %.0f tok/s 达 %ds 即中止当前场景，冷却 %ds 后探针（×%.0f）决定续跑或停整轮",
			sg.MinTPS, sg.WindowSeconds, sg.CooldownSeconds, sg.EffProbeFactor())
	}
	// 饱和止损：CLI 覆盖配置（与降速熔断同一覆盖口径：flag >0 才写，默认值在 Load 已填）
	if *noSatFlag {
		cfg.SaturationGuard = nil
		log.Printf("饱和止损: 已由 CLI 关闭（--no-sat-guard）")
	} else if *satWaitingFlag > 0 || *satWindowFlag > 0 || *satWallFlag > 0 {
		sg := cfg.SaturationGuard
		if sg == nil {
			sg = &config.SaturationGuardCfg{}
			cfg.SaturationGuard = sg
		}
		on := true
		sg.Enabled = &on
		if *satWaitingFlag > 0 {
			sg.MaxWaiting = *satWaitingFlag
		}
		if *satWindowFlag > 0 {
			sg.WindowSeconds = *satWindowFlag
		}
		if *satWallFlag > 0 {
			sg.MaxWallSeconds = *satWallFlag
		}
		if sg.MaxWaiting > 0 {
			if sg.WindowSeconds <= 0 {
				sg.WindowSeconds = 120
			}
			if sg.SampleSeconds <= 0 {
				sg.SampleSeconds = 5
			}
			if !cfg.ServerMetrics {
				log.Printf("⚠️ --sat-waiting 需要 server_metrics: true（waiting 排队深度不可得）——waiting 判定不生效，墙钟上限仍有效")
			}
			log.Printf("饱和止损: waiting≥%d 持续 %ds 或单档墙钟超 %ds 即截断当前档位并停止后续档位",
				sg.MaxWaiting, sg.WindowSeconds, sg.MaxWallSeconds)
		} else if sg.MaxWallSeconds > 0 {
			log.Printf("饱和止损: 单个档位/到达率墙钟超过 %ds 即截断并停止后续档位", sg.MaxWallSeconds)
		}
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
			fmt.Fprintf(lf, "\n===== test run %s（tool %s）=====\n", time.Now().Format(time.RFC3339), report.Version)
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
		probeOn, probeOff := th.ProbeExtraBodies()
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
			MaxContext:      cfg.LargestPromptTokens(),
			Timeout:         cfg.Timeout(),
			ToolCall:        !*noToolCallFlag,
			CaptureDir:      *captureFlag,
			XVPromptTokens:  cfg.Concurrent.PromptTokens,
			XVMaxTokens:     cfg.Concurrent.MaxTokens.Max(), // probe 上下文探测按最大输出预算（prompt+output 最坏组合）
			ThinkingBudget:  th.MaxTokensFloor,
			CacheCheck:      *cacheFlag,
			CacheSizeTokens: *cacheSizeFlag,
			FillerLang:      cfg.FillerLang,
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

	// ── 环境存档：引擎识别 + 配置原文随每份场景 JSON 落盘 ──
	// 几周后回看数据时"当时是什么引擎、什么配置跑的"必须有据可查；probe 失败不阻塞压测。
	var envInfo *engine.ProbeResult
	if active := cfg.ActiveModels(); len(active) > 0 {
		m := active[0]
		th := cfg.ThinkingFor(m)
		probeOn, probeOff := th.ProbeExtraBodies()
		res := engine.Probe(ctx, engine.ProbeOptions{
			Endpoint:     cfg.Endpoint,
			APIKey:       cfg.APIKey,
			Auth:         auth.Auth{Scheme: cfg.AuthScheme, Header: cfg.AuthHeader},
			ChatPath:     cfg.ChatPath,
			MetricsPath:  cfg.MetricsPath,
			ModelsPath:   cfg.ModelsPath,
			Model:        m,
			ThinkingOn:   probeOn,
			ThinkingOff:  probeOff,
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

	// 测试画像（5.10）：开跑前打印"这次要跑什么形状"总览；同一份数据随每份 JSON 落盘
	planItems := make([]scenario.PlanItem, len(items))
	for i, it := range items {
		planItems[i] = scenario.PlanItem{Name: it.name, Highs: it.highs, MT: it.mt}
	}
	planSum := scenario.PlanSummary(cfg, *modelFilter, planItems)
	if planSum != nil {
		for _, l := range planSum.Render() {
			log.Print(l)
		}
	}

	if len(items) > 1 && strings.HasSuffix(*outFlag, ".json") {
		fmt.Fprintln(os.Stderr, "本次组合会跑多个场景（各落一个 JSON），-o 请给目录而不是单个 .json 文件")
		os.Exit(1)
	}

	// lastAbort 最近一次场景的中止原因（非空 = 该场景被降速熔断中止，run 据此换掉"完成"措辞）
	lastAbort := ""
	// lastWritten 最近一次场景实际落盘的 JSON 路径（按模型分区时有多条）：
	// 8.2 探针未恢复停止整轮时，把「探针未恢复」追加进被熔断场景的 note 留痕
	var lastWritten []string

	run := func(name, outPath, stallPath string, fn func() (*report.Report, error)) {
		start := time.Now()
		lastWritten = lastWritten[:0]
		rep, err := fn()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] 失败: %v\n", name, err)
			os.Exit(1)
		}
		// 环境存档随每份分区落盘（引擎识别 + 配置原文）；测试画像同附（回溯"当时的计划"）
		rep.Environment = envInfo
		rep.ConfigRaw = cfg.Raw
		rep.Plan = planSum
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
			// stall_trace 记相对路径：单模型布局 = 同目录文件名；分区布局 = ../<同名>.stall.csv
			if stallPath != "" {
				if rel, rerr := filepath.Rel(filepath.Dir(path), stallPath); rerr == nil {
					p.StallTrace = rel
				}
			}
			if err := p.SaveJSON(path); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] 写出 JSON 失败: %v\n", name, err)
				os.Exit(1)
			}
			lastWritten = append(lastWritten, path)
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
		if lastAbort != "" {
			fmt.Printf("[%s] ⛔ 已中止（%s）——已完成数据已保存: %s\n", name, lastAbort, outPath)
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

	// 8.2 恢复探针用的模型：与场景同源（ActiveModels 按 -m 过滤后取第一个）
	probeModel := ""
	for _, m := range cfg.ActiveModels() {
		if *modelFilter == "" || m == *modelFilter {
			probeModel = m
			break
		}
	}

	for idx, it := range items {
		it := it
		cc := *cfg // 场景间互不影响：并发档位/多轮开关按本项覆盖
		if len(it.highs) > 0 {
			cc.Concurrent.Levels = it.highs
			cc.Concurrent.Multiturn = it.mt
		}

		// 每个场景各自派生 ctx + 熔断器：降速熔断只中止**本场景**（避免一个慢场景把整轮
		// 后面的场景一起带走），冷却后续跑；真中断（Ctrl+C / SSH 断连）仍由根 ctx 决定，
		// 走 run() 里的"中断"分支直接返回。
		sctx, scancel := context.WithCancel(ctx)
		var guard *engine.StallGuard
		if cc.StallGuard.StallEnabled() {
			sg := cc.StallGuard
			guard = engine.NewStallGuard(
				sg.MinTPS,
				time.Duration(sg.WindowSeconds)*time.Second,
				time.Duration(sg.SampleSeconds*float64(time.Second)),
				func(engine.StallEvent) { scancel() },
			)
			client.Stall = guard
		}

		lastAbort = ""
		// 降速采样序列侧文件（8.3）：每场景一个，与结果 JSON 同名同层；正常段与
		// 空闲/prefill 段也记录，熔断原因以 # 注释行追加。文件在场景开始创建、
		// 场景结束（含熔断中止）关闭——已完成数据照常落盘，trace 也照常保留。
		outPath := resolveOutPath(*outFlag, cfg.OutputDir, it.name)
		stallPath := ""
		var stallFile *os.File
		if guard != nil && !*noStallTraceFlag {
			stallPath = strings.TrimSuffix(outPath, ".json") + ".stall.csv"
			// -o 目录可能还不存在（SaveJSON 在场景结束后才建目录），先补齐
			os.MkdirAll(filepath.Dir(stallPath), 0o755)
			f, ferr := os.Create(stallPath)
			if ferr != nil {
				log.Printf("⚠️ 降速采样序列文件创建失败（%v）——本次不落盘，其余行为不变", ferr)
			} else {
				stallFile = f
				guard.W = f
			}
		}
		guardDone := make(chan struct{})
		if guard != nil {
			go func() {
				guard.Run(sctx)
				close(guardDone)
			}()
		}
		run(it.name, outPath, stallPath, func() (*report.Report, error) {
			rep, err := it.sc.Run(sctx, &cc, client, *modelFilter)
			if guard != nil && guard.Tripped() {
				ev, _ := guard.Event()
				lastAbort = fmt.Sprintf("降速熔断: 聚合输出速度 %.1f tok/s 持续 %s 低于阈值 %.0f tok/s",
					ev.Rate, ev.LowFor.Round(time.Second), ev.MinTPS)
				if rep != nil {
					// 报告留痕：几周后回看这份 JSON 时，"为什么数据是半截的"必须有据可查
					n := lastAbort
					if rep.Note != "" {
						n = rep.Note + "；" + n
					}
					rep.Note = n
				}
			}
			return rep, err
		})

		scancel()
		client.Stall = nil
		if guard != nil {
			<-guardDone // 等采样 goroutine 退出再关文件，避免最后一行写进已关闭的 fd
			if stallFile != nil {
				guard.W = nil
				stallFile.Close()
			}
		}

		if ctx.Err() != nil {
			return // 真中断：不再启动后续场景
		}
		if lastAbort != "" && idx < len(items)-1 {
			// 8.2 冷却+探针：冷却只给恢复留时间窗，续跑与否由短探针实测决定——
			// 恢复了白等 5 分钟、没恢复照样熔断下一场景，两头都是纯计时冷却的堵点
			sg := cc.StallGuard
			if cd := time.Duration(sg.CooldownSeconds) * time.Second; cd > 0 {
				log.Printf("⏳ 冷却 %s 后继续下一个场景「%s」（Ctrl+C 可立即退出）",
					cd, items[idx+1].name)
				select {
				case <-ctx.Done():
					return
				case <-time.After(cd):
				}
			}
			if pf := sg.EffProbeFactor(); pf > 0 && sg.MinTPS > 0 {
				threshold := pf * sg.MinTPS
				tps, perr := recoveryProbe(ctx, client, cfg, probeModel)
				if perr != nil || tps < threshold {
					reason := fmt.Sprintf("熔断后探针 %.1f tok/s < 恢复阈值 %.1f tok/s（%.1f×min_tps），停止后续场景",
						tps, threshold, pf)
					if perr != nil {
						reason = fmt.Sprintf("熔断后探针失败（%v），停止后续场景", perr)
					}
					log.Printf("🛑 服务端未恢复：%s", reason)
					patchReportNote(lastWritten, reason) // 留痕进被熔断场景的 JSON note
					return
				}
				log.Printf("✅ 探针 %.1f tok/s ≥ 恢复阈值 %.1f tok/s，服务端已恢复，继续下一个场景", tps, threshold)
			} else {
				log.Printf("冷却结束，继续下一个场景")
			}
		}
	}
}

// recoveryProbe 8.2 恢复探针：发 3 条短请求（4k prompt / 256 输出，thinking 关）实测服务端
// decode 速度，取中位——单发串行下聚合速度即单请求速度。探针发生在场景之间，此时
// client.Stall 已置 nil，探针自身不会触发二次熔断。
func recoveryProbe(ctx context.Context, client *engine.Client, cfg *config.Config, model string) (float64, error) {
	msgs := []engine.Message{engine.UserMsg(cfg.ClampOne(4000), int64(cfg.SeedSalt+99001), cfg.FillerLang)}
	vOff := config.ThinkingVariant{Name: "off"} // 配置未启用 off 变体时退化为默认（无思考参数）
	for _, v := range cfg.Thinking.Variants() {
		if v.Name == "off" {
			vOff = v
			break
		}
	}
	var rates []float64
	for i := 0; i < 3; i++ {
		m, err := client.Chat(ctx, engine.ChatOptions{
			Model:     model,
			Messages:  msgs,
			MaxTokens: 256,
			Stream:    cfg.StreamEnabled(),
			Thinking:  vOff.Enabled,
			ExtraBody: vOff.ExtraBody,
		})
		if err != nil || m.Error != "" {
			if err == nil {
				err = fmt.Errorf("%s", m.Error)
			}
			return 0, fmt.Errorf("探针请求 %d/3 失败: %w", i+1, err)
		}
		rates = append(rates, m.TokensPerSec)
	}
	sort.Float64s(rates)
	return rates[1], nil // 中位：3 条里偶发 1 条慢不误判
}

// patchReportNote 给已落盘的结果 JSON 追加 note（8.2 探针未恢复留痕用）。
// 读写失败只告警不致命：日志已留痕，不为此丢已完成的数据。
func patchReportNote(paths []string, note string) {
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			log.Printf("⚠️ 读取 %s 失败，note 留痕跳过: %v", p, err)
			continue
		}
		var rep report.Report
		if err := json.Unmarshal(b, &rep); err != nil {
			log.Printf("⚠️ 解析 %s 失败，note 留痕跳过: %v", p, err)
			continue
		}
		if rep.Note != "" {
			rep.Note += "；" + note
		} else {
			rep.Note = note
		}
		if err := rep.SaveJSON(p); err != nil {
			log.Printf("⚠️ 回写 %s 失败，note 留痕跳过: %v", p, err)
		}
	}
}
