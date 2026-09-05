// llm-perf：客户自部署 LLM 推理服务性能评测工具。
//
// 契约：输入 YAML 配置，输出 JSON 原始数据；报告呈现由外部工具基于 JSON 二次加工。
//
// 用法：
//
//	bench single    -c configs/example.yaml [-o out.json] [-m 模型过滤]
//	bench multiturn -c configs/example.yaml [-o out.json] [-m 模型过滤]
//	bench concurrent -c configs/example.yaml [-o out.json] [-m 模型过滤]
//	bench all       -c configs/example.yaml [-o 输出目录]
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/engine"
	"github.com/aleexjiang/llm-perf/internal/report"
	"github.com/aleexjiang/llm-perf/internal/scenario"
)

func usage() {
	fmt.Fprint(os.Stderr, `llm-perf - LLM 推理服务性能评测

输入: YAML 配置    输出: JSON 原始数据（报告请用外部工具基于 JSON 生成）

用法:
  bench probe     -c configs/example.yaml            # 兼容性探针：先摸清引擎实现细节再压测
  bench <single|multiturn|concurrent|all> [-c 配置.yaml] [-o 输出路径] [-m 模名过滤]

-o 说明:
  - 指定 .json 路径  → 直接作为输出文件（单场景时）
  - 指定目录        → 在该目录下生成 <场景>-<时间戳>.json
  - 缺省            → 使用配置 output_dir

排查模式:
  配置里 debug: true 时，原始响应留存到 <output_dir>/raw/、日志同步写 <output_dir>/run.log；
  任何请求失败时即使不开 debug 也会自动留存转储（写到系统临时目录）。

示例:
  bench probe -c configs/example.yaml
  bench all -c configs/example.yaml
  bench single -c configs/example.yaml -o result/deepseek-40k.json -m DeepSeek
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

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	switch cmd {
	case "probe", "single", "multiturn", "concurrent", "all":
	default:
		usage()
	}

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("c", "configs/example.yaml", "YAML 配置文件路径")
	modelFilter := fs.String("m", "", "只测包含该子串的模型")
	outFlag := fs.String("o", "", "输出路径：.json 文件或目录（默认用配置 output_dir）")
	corpusFlag := fs.String("corpus", "", "填充语料：en/zh（内置公版书）或自定义文件路径（.txt/.txt.gz）；覆盖配置 filler_corpus")
	maxCtxFlag := fs.Int("max-ctx", 0, "上下文截止（tokens）：>0 时所有请求 prompt 不超过该值；覆盖配置 max_prompt_tokens")
	fs.Parse(os.Args[2:])

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		os.Exit(1)
	}
	if *corpusFlag != "" {
		cfg.FillerCorpus = *corpusFlag
	}
	if *maxCtxFlag > 0 {
		cfg.MaxPromptTokens = *maxCtxFlag
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
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err == nil {
		if lf, err := os.Create(filepath.Join(cfg.OutputDir, "run.log")); err == nil {
			log.SetOutput(io.MultiWriter(os.Stderr, lf))
			defer lf.Close()
		}
	}

	client := engine.NewClient(cfg.Endpoint, cfg.APIKey, cfg.Timeout(), *cfg.IncludeUsage)
	if cfg.Debug {
		client.DebugDir = filepath.Join(cfg.OutputDir, "raw")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// bench all 的 -o 只接受目录：三个场景各自落一个文件，给 .json 会互相覆盖
	if cmd == "all" && strings.HasSuffix(*outFlag, ".json") {
		fmt.Fprintln(os.Stderr, "bench all 的 -o 请给目录（三场景各落一个 JSON），不要指定单个 .json 文件")
		os.Exit(1)
	}

	// ── probe：兼容性探测（不需要场景配置） ──
	if cmd == "probe" {
		model := ""
		if fs.Arg(0) != "" {
			model = fs.Arg(0)
		} else if len(cfg.Models) > 0 {
			model = cfg.Models[0]
		}
		res := engine.Probe(ctx, engine.ProbeOptions{
			Endpoint:     cfg.Endpoint,
			APIKey:       cfg.APIKey,
			Model:        model,
			ThinkingOn:   cfg.Thinking.ExtraBodyOn,
			ThinkingOff:  cfg.Thinking.ExtraBodyOff,
			IncludeUsage: *cfg.IncludeUsage,
			MaxContext:   cfg.LargestPromptTokens(),
		})
		outPath := resolveOutPath(*outFlag, cfg.OutputDir, "probe")
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
		fmt.Printf("探针完成，输出: %s\n", outPath)
		return
	}

	run := func(name string, fn func() (*report.Report, error)) {
		start := time.Now()
		rep, err := fn()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] 失败: %v\n", name, err)
			os.Exit(1)
		}
		outPath := resolveOutPath(*outFlag, cfg.OutputDir, name)
		if err := rep.SaveJSON(outPath); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] 写出 JSON 失败: %v\n", name, err)
			os.Exit(1)
		}
		fmt.Printf("[%s] 完成，用时 %s，输出: %s\n", name, time.Since(start).Round(time.Second), outPath)
	}

	switch cmd {
	case "single":
		run("single", func() (*report.Report, error) { return scenario.Single(ctx, cfg, client, *modelFilter) })
	case "multiturn":
		run("multiturn", func() (*report.Report, error) { return scenario.Multiturn(ctx, cfg, client, *modelFilter) })
	case "concurrent":
		run("concurrent", func() (*report.Report, error) { return scenario.Concurrent(ctx, cfg, client, *modelFilter) })
	case "all":
		run("single", func() (*report.Report, error) { return scenario.Single(ctx, cfg, client, *modelFilter) })
		run("multiturn", func() (*report.Report, error) { return scenario.Multiturn(ctx, cfg, client, *modelFilter) })
		run("concurrent", func() (*report.Report, error) { return scenario.Concurrent(ctx, cfg, client, *modelFilter) })
	}
}
