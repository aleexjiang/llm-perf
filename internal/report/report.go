// Package report 定义各场景的 JSON 输出结构。
// 本工具只负责产出原始 JSON；HTML/图表等报告呈现由外部工具（如 WorkBuddy）基于 JSON 二次加工。
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aleexjiang/llm-perf/internal/engine"
	"github.com/aleexjiang/llm-perf/internal/smetrics"
)

// SingleRow：一个模型在一个 token 档位 × 思考模式下的多次 run。
type SingleRow struct {
	Model        string                `json:"model"`
	Thinking     string                `json:"thinking"` // "on" / "off"
	PromptTokens int                   `json:"prompt_tokens"`
	Runs         []*engine.TurnMetrics `json:"runs"`
}

// MultiturnRun：一个模型的一次多轮会话（每 turn 均含思考时长）。
type MultiturnRun struct {
	Model    string                `json:"model"`
	Thinking string                `json:"thinking"` // "on" / "off"
	Session  int                   `json:"session"`
	Turns    []*engine.TurnMetrics `json:"turns"`
}

// ConcurrentLevel：一个模型在一个并发档位 × 思考模式下的结果。
// Multiturn=false 时 Requests 为各虚拟用户的单轮请求；true 时 Sessions 为各虚拟用户的完整会话重放。
// RequestRate>0 为开环到达率模式（Level=0，rate 为实际到达率）。
type ConcurrentLevel struct {
	Model         string                `json:"model"`
	Thinking      string                `json:"thinking"` // "on" / "off"
	Level         int                   `json:"level"`
	RequestRate   float64               `json:"request_rate,omitempty"` // 开环模式的到达率（req/s）
	Requests      []*engine.TurnMetrics `json:"requests,omitempty"`
	Sessions      []MultiturnRun        `json:"sessions,omitempty"`
	WallSeconds   float64               `json:"wall_seconds"`
	ThroughputTPS float64               `json:"throughput_tps"` // 整体 completion tokens/s

	// goodput（SLO 约束吞吐，配置了 goodput 时填充）：SLOMeet/SLOTotal 为达标/总请求数（多轮按 turn 计）
	SLOMeet    int     `json:"slo_meet,omitempty"`
	SLOTotal   int     `json:"slo_total,omitempty"`
	GoodputRPS float64 `json:"goodput_rps,omitempty"` // 达标请求 / 墙钟
	GoodputTPS float64 `json:"goodput_tps,omitempty"` // 达标请求的 completion tokens / 墙钟
}

// SLO 记录本次评测的 goodput 约束（报告侧据此计算达标口径）。
type SLO struct {
	TTFTMS float64 `json:"ttft_ms"`
	TPOTMS float64 `json:"tpot_ms"`
}

// CorrectnessRow 一条正确性金丝雀请求的结果。
type CorrectnessRow struct {
	Model  string  `json:"model"` // 该金丝雀请求发往的模型（按模型分区落盘时据此归属）
	Number string  `json:"number"` // 要求转写的目标数字
	Reply  string  `json:"reply"`
	Match  bool    `json:"match"`
	E2EMS  float64 `json:"e2e_ms"`
	Error  string  `json:"error,omitempty"`
}

// GaugeSummary / HistSummary / ServerMetricsSummary：服务端 /metrics 观测汇总。
// 挂在每个场景 Report 上，覆盖该场景执行窗口。
type ServerMetricsSummary struct {
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`

	// counter 窗口差值（并发窗口内为混合贡献；命中率 = hit/query）
	CacheHitTokens     float64 `json:"cache_hit_tokens,omitempty"`
	CacheQueryTokens   float64 `json:"cache_query_tokens,omitempty"`
	Preemptions        float64 `json:"preemptions,omitempty"`
	SpecDrafts         float64 `json:"spec_drafts,omitempty"`
	SpecAcceptedTokens float64 `json:"spec_accepted_tokens,omitempty"`

	// gauge 轮询聚合（running/waiting 排队深度、kv_usage KV 池占用率）
	Gauges map[string]GaugeSummary `json:"gauges,omitempty"`

	// histogram 窗口差值分位估计（服务端口径的延迟分解；key 为引擎指标名）
	Hists map[string]smetrics.HistDelta `json:"histograms,omitempty"`

	// 观测健康度：gauge 轮询从未成功或连续失败达到阈值时置位（报告应醒目标注）
	ObservationDegraded bool   `json:"observation_degraded,omitempty"`
	ObservationNote     string `json:"observation_note,omitempty"`
}

// GaugeSummary 复用 smetrics 的轮询聚合类型。
type GaugeSummary = smetrics.GaugeSummary

// Version 是工具版本，随每个 JSON 输出落盘（报告追溯用）。
// 默认 dev；Makefile 构建时用 -ldflags 注入 git describe 版本号。
var Version = "llm-perf/dev"

// Report 是一次场景执行的完整数据，整体落盘为单个 JSON 文件。
type Report struct {
	Tool        string                `json:"tool"`
	Scenario    string                `json:"scenario"`
	GeneratedAt time.Time             `json:"generated_at"`
	Endpoint    string                `json:"endpoint"`
	Note        string                `json:"note,omitempty"`
	SLO         *SLO                  `json:"slo,omitempty"`
	Single      []SingleRow           `json:"single,omitempty"`
	Multiturn   []MultiturnRun        `json:"multiturn,omitempty"`
	Concurrent  []ConcurrentLevel     `json:"concurrent,omitempty"`
	Correctness []CorrectnessRow      `json:"correctness,omitempty"`
	Server      *ServerMetricsSummary `json:"server_metrics,omitempty"`

	// PartitionModel 按模型分区时该分区归属的模型名（不落盘）：main 据此拼 <output_dir>/<模型>/ 子目录
	PartitionModel string `json:"-"`
}

// SaveJSON 将报告写入单个 JSON 文件（自动创建父目录）。
func (r *Report) SaveJSON(path string) error {
	return SaveJSONAny(r, path)
}

// SaveJSONAny 任意结构落盘为 JSON（probe 等非 Report 结构用）。
func SaveJSONAny(v any, path string) error {
	rj, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, rj, 0o644)
}

// DefaultName 生成默认输出文件名：<scenario>-<timestamp>.json
func DefaultName(scenario string) string {
	return fmt.Sprintf("%s-%s.json", scenario, time.Now().Format("20060102-150405"))
}

// PartitionByModel 按模型把报告拆成每模型一份（数据落盘以模型为单位：output/<model>/）。
// Single/Multiturn/Concurrent/Correctness 按各行 Model 字段分桶；Endpoint 级的
// Note/SLO/Server 原样带入每个分区。分区顺序按模型首次出现的顺序。
func (r *Report) PartitionByModel() []*Report {
	order := []string{}
	buckets := map[string]*Report{}
	get := func(model string) *Report {
		if p, ok := buckets[model]; ok {
			return p
		}
		p := &Report{
			Tool:           r.Tool,
			Scenario:       r.Scenario,
			GeneratedAt:    r.GeneratedAt,
			Endpoint:       r.Endpoint,
			Note:           r.Note,
			SLO:            r.SLO,
			Server:         r.Server,
			PartitionModel: model,
		}
		buckets[model] = p
		order = append(order, model)
		return p
	}
	for i := range r.Single {
		p := get(r.Single[i].Model)
		p.Single = append(p.Single, r.Single[i])
	}
	for i := range r.Multiturn {
		p := get(r.Multiturn[i].Model)
		p.Multiturn = append(p.Multiturn, r.Multiturn[i])
	}
	for i := range r.Concurrent {
		p := get(r.Concurrent[i].Model)
		p.Concurrent = append(p.Concurrent, r.Concurrent[i])
	}
	for _, row := range r.Correctness {
		get(row.Model).Correctness = append(buckets[row.Model].Correctness, row)
	}
	parts := make([]*Report, 0, len(order))
	for _, m := range order {
		parts = append(parts, buckets[m])
	}
	return parts
}
