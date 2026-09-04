// llm-perf：客户自部署 LLM 推理服务性能评测工具。
//
// 用法：
//
//	bench single    -c configs/example.yaml [-m 模型过滤]   # 单请求基线
//	bench multiturn -c configs/example.yaml [-m 模型过滤]   # 多轮会话重放（前缀缓存判定）
//	bench concurrent -c configs/example.yaml [-m 模型过滤]  # 阶梯并发
//	bench all       -c configs/example.yaml                 # 三个场景全跑
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/engine"
	"github.com/aleexjiang/llm-perf/internal/scenario"
)

func usage() {
	fmt.Fprint(os.Stderr, `llm-perf - LLM 推理服务性能评测

用法:
  bench <single|multiturn|concurrent|all> [-c 配置文件] [-m 模名过滤] [-o 输出根目录]

示例:
  bench all -c configs/example.yaml
  bench single -c configs/example.yaml -m DeepSeek
`)
	os.Exit(2)
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
	cfgPath := fs.String("c", "configs/example.yaml", "配置文件路径")
	modelFilter := fs.String("m", "", "只测包含该子串的模型")
	outRoot := fs.String("o", "", "输出根目录（默认取配置 output_dir）")
	fs.Parse(os.Args[2:])

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		os.Exit(1)
	}
	if *outRoot != "" {
		cfg.OutputDir = *outRoot
	}

	client := engine.NewClient(cfg.Endpoint, cfg.APIKey, cfg.Timeout(), *cfg.IncludeUsage)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	run := func(name string, fn func() (string, error)) {
		start := time.Now()
		dir, err := fn()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] 失败: %v\n", name, err)
			os.Exit(1)
		}
		fmt.Printf("[%s] 完成，用时 %s，报告: %s/report.html\n", name, time.Since(start).Round(time.Second), dir)
	}

	switch cmd {
	case "single":
		run("single", func() (string, error) { return scenario.Single(ctx, cfg, client, *modelFilter, cfg.OutputDir) })
	case "multiturn":
		run("multiturn", func() (string, error) { return scenario.Multiturn(ctx, cfg, client, *modelFilter, cfg.OutputDir) })
	case "concurrent":
		run("concurrent", func() (string, error) { return scenario.Concurrent(ctx, cfg, client, *modelFilter, cfg.OutputDir) })
	case "all":
		run("single", func() (string, error) { return scenario.Single(ctx, cfg, client, *modelFilter, cfg.OutputDir) })
		run("multiturn", func() (string, error) { return scenario.Multiturn(ctx, cfg, client, *modelFilter, cfg.OutputDir) })
		run("concurrent", func() (string, error) { return scenario.Concurrent(ctx, cfg, client, *modelFilter, cfg.OutputDir) })
	}
}
