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
)

// SingleRow：一个模型在一个 token 档位下的多次 run。
type SingleRow struct {
	Model        string                `json:"model"`
	PromptTokens int                   `json:"prompt_tokens"`
	Runs         []*engine.TurnMetrics `json:"runs"`
}

// MultiturnRun：一个模型的一次多轮会话。
type MultiturnRun struct {
	Model   string                `json:"model"`
	Session int                   `json:"session"`
	Turns   []*engine.TurnMetrics `json:"turns"`
}

// ConcurrentLevel：一个模型在一个并发档位下的结果。
type ConcurrentLevel struct {
	Model         string                `json:"model"`
	Level         int                   `json:"level"`
	Requests      []*engine.TurnMetrics `json:"requests"`
	WallSeconds   float64               `json:"wall_seconds"`
	ThroughputTPS float64               `json:"throughput_tps"` // 整体 completion tokens/s
}

// Report 是一次场景执行的完整数据，整体落盘为单个 JSON 文件。
type Report struct {
	Scenario    string            `json:"scenario"`
	GeneratedAt time.Time         `json:"generated_at"`
	Endpoint    string            `json:"endpoint"`
	Note        string            `json:"note,omitempty"`
	Single      []SingleRow       `json:"single,omitempty"`
	Multiturn   []MultiturnRun    `json:"multiturn,omitempty"`
	Concurrent  []ConcurrentLevel `json:"concurrent,omitempty"`
}

// SaveJSON 将报告写入单个 JSON 文件（自动创建父目录）。
func (r *Report) SaveJSON(path string) error {
	rj, err := json.MarshalIndent(r, "", "  ")
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
