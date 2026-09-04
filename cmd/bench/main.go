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
  bench <single|multiturn|concurrent|all> [-c 配置.yaml] [-o 输出路径] [-m 模名过滤]

-o 说明:
  - 指定 .json 路径  → 直接作为输出文件（单场景时）
  - 指定目录        → 在该目录下生成 <场景>-<时间戳>.json
  - 缺省            → 使用配置 output_dir

示例:
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
	case "single", "multiturn", "concurrent", "all":
	default:
		usage()
	}

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("c", "configs/example.yaml", "YAML 配置文件路径")
	modelFilter := fs.String("m", "", "只测包含该子串的模型")
	outFlag := fs.String("o", "", "输出路径：.json 文件或目录（默认用配置 output_dir）")
	fs.Parse(os.Args[2:])

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		os.Exit(1)
	}

	client := engine.NewClient(cfg.Endpoint, cfg.APIKey, cfg.Timeout(), *cfg.IncludeUsage)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
