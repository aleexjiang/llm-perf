// Package report 定义各场景的输出数据结构，并渲染 HTML 报告。
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
	Model        string              `json:"model"`
	PromptTokens int                 `json:"prompt_tokens"`
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

// Report 是一次场景执行的完整数据，落盘为 report.json。
type Report struct {
	Scenario    string           `json:"scenario"`
	GeneratedAt time.Time        `json:"generated_at"`
	Endpoint    string           `json:"endpoint"`
	Note        string           `json:"note,omitempty"`
	Single      []SingleRow      `json:"single,omitempty"`
	Multiturn   []MultiturnRun   `json:"multiturn,omitempty"`
	Concurrent  []ConcurrentLevel `json:"concurrent,omitempty"`
}

// Save 将 Report 与渲染后的 HTML 写入 dir。
func (r *Report) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	rj, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), rj, 0o644); err != nil {
		return err
	}
	html, err := r.RenderHTML()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.html"), []byte(html), 0o644)
}

// ScenarioDir 生成形如 20260904-2118-single 的目录名。
func ScenarioDir(outputRoot, scenario string) string {
	return filepath.Join(outputRoot, fmt.Sprintf("%s-%s", time.Now().Format("20060102-150405"), scenario))
}
