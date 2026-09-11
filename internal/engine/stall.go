package engine

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// StallGuard 降速熔断：按「聚合输出速度」判定服务端是否已退化到不值得继续跑。
//
// 口径（2026-09-11 与用户确认）：
//   - 聚合：窗口内**所有在飞请求**产生的输出 token 之和 / 窗口时长，而非单个请求的速度。
//     并发场景下单请求慢可能只是排队，聚合才反映服务端真实产出能力；单发串行时两者等价。
//   - 计数器按流式 chunk 近似 token（主流引擎 1 chunk ≈ 1 token，与 ITL 口径一致）；
//     非流式请求没有实时信号，不参与判定。
//
// 判定规则：速度连续低于 MinTPS 达 Window 时长即触发一次。期间任一次采样回升到阈值以上
// 就重置计时（对应「持续 10 分钟不升」的语义）。
//
// 两种不参与判定的情况（避免误触发）：
//   - 没有请求在飞（场景切换、请求间隙）——没东西可测；
//   - 有请求在飞但都还没吐出第一个 token（长 prompt 的 prefill 阶段）——这是预期耗时，
//     不是退化。只有「正在输出却输出得慢」才算降速。
//
// 触发后调用 onTrip（通常是一个场景级 cancel），由场景层决定停下来之后做什么：
// 本项目当前的做法是只中止当前场景、冷却后续跑下一场景，不终止整轮。
type StallGuard struct {
	MinTPS float64       // 阈值：低于该聚合速度即视为降速
	Window time.Duration // 连续低于阈值多久触发
	Sample time.Duration // 采样周期（默认 2s）；越低越灵敏、日志越密

	// LogEvery 慢速期的状态提示间隔（0 = 默认 60s，负数 = 不提示）。
	// 只提示不触发，方便在长窗口里看清"到底是慢还是卡死"。
	LogEvery time.Duration

	// W 降速采样序列侧文件（CSV，nil = 不落盘）。每次采样追加一行
	// `t_s,agg_tps,in_flight,emitting,phase`：正常段（高于阈值）也记录，否则事后
	// 只剩一个熔断点，无法回答"从第几档开始掉、是渐变还是断崖、掉了多久"。
	// agg_tps 只在 emit 相位有意义：空闲/prefill 相位留空（"测不出"≠ 0），
	// phase 取 emit / prefill / idle。熔断触发时以 `#` 注释行追加现场原因。
	// 仅 Run 的采样 goroutine 会写 W（trip 由 Run 调用），无并发写。
	W io.Writer

	tokens   atomic.Int64 // 累计输出 chunk（近似 token 数）
	inFlight atomic.Int64 // 在飞请求数
	emitting atomic.Int64 // 已吐出首个 token、正在输出的请求数

	mu      sync.Mutex
	tripped bool
	event   StallEvent

	onTrip func(StallEvent)
	clock  func() time.Time
}

// StallEvent 一次降速熔断的现场快照，用于日志与报告标注。
type StallEvent struct {
	Rate     float64       `json:"rate_tps"`   // 触发时的聚合输出速度
	MinTPS   float64       `json:"min_tps"`    // 阈值
	LowFor   time.Duration `json:"low_for_ms"` // 持续低于阈值的时长
	At       time.Time     `json:"at"`         //
	InFlight int64         `json:"in_flight"`  // 触发时的在飞请求数
	Emitting int64         `json:"emitting"`   // 触发时正在输出的请求数
}

// NewStallGuard 创建熔断器。onTrip 可为 nil（只置位不回调）。
func NewStallGuard(minTPS float64, window, sample time.Duration, onTrip func(StallEvent)) *StallGuard {
	if sample <= 0 {
		sample = 2 * time.Second
	}
	return &StallGuard{
		MinTPS: minTPS,
		Window: window,
		Sample: sample,
		onTrip: onTrip,
		clock:  time.Now,
	}
}

// Tokens 记一次输出增量（流式 chunk）。由 Client 在收到 content/reasoning 增量时调用。
func (g *StallGuard) Tokens(n int) {
	if g == nil || n <= 0 {
		return
	}
	g.tokens.Add(int64(n))
}

// Enter / Exit 标记一个请求的开始与结束（在飞数）。
func (g *StallGuard) Enter() {
	if g == nil {
		return
	}
	g.inFlight.Add(1)
}

func (g *StallGuard) Exit() {
	if g == nil {
		return
	}
	g.inFlight.Add(-1)
}

// DecodeStart / DecodeStop 标记一个请求进入/退出"正在输出"状态。
// 只有处于该状态的请求才让采样生效——prefill（首个 token 之前）不计入降速判定。
func (g *StallGuard) DecodeStart() {
	if g == nil {
		return
	}
	g.emitting.Add(1)
}

func (g *StallGuard) DecodeStop() {
	if g == nil {
		return
	}
	g.emitting.Add(-1)
}

// Tripped 是否已触发。
func (g *StallGuard) Tripped() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tripped
}

// Event 触发现场；未触发时 ok=false。
func (g *StallGuard) Event() (StallEvent, bool) {
	if g == nil {
		return StallEvent{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.event, g.tripped
}

// Run 采样循环：ctx 结束即退出。触发后立即返回（不再重复采样）。
func (g *StallGuard) Run(ctx context.Context) {
	if g == nil || g.Window <= 0 || g.MinTPS <= 0 {
		return
	}
	t := time.NewTicker(g.Sample)
	defer t.Stop()

	logEvery := g.LogEvery
	if logEvery == 0 {
		logEvery = time.Minute
	}

	last := g.now()
	lastTokens := g.tokens.Load()
	lastStatus := last
	var lowFor time.Duration

	// t_s 是场景内相对秒数（Run 启动 = 场景开始附近），每场景一个文件
	start := g.now()
	if g.W != nil {
		fmt.Fprintln(g.W, "t_s,agg_tps,in_flight,emitting,phase")
	}
	trace := func(at time.Time, phase string, rate float64, withRate bool) {
		if g.W == nil {
			return
		}
		tps := ""
		if withRate {
			tps = fmt.Sprintf("%.2f", rate)
		}
		fmt.Fprintf(g.W, "%.3f,%s,%d,%d,%s\n",
			at.Sub(start).Seconds(), tps, g.inFlight.Load(), g.emitting.Load(), phase)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			cur := g.tokens.Load()
			elapsed := now.Sub(last)
			last = now
			delta := cur - lastTokens
			lastTokens = cur
			rate := float64(delta) / elapsed.Seconds()

			// 没有请求正在输出（空闲 / 纯 prefill）：无法判定速度，也不该计入降速时长
			if g.emitting.Load() == 0 {
				lowFor = 0
				if g.inFlight.Load() == 0 {
					trace(now, "idle", rate, false)
				} else {
					trace(now, "prefill", rate, false)
				}
				continue
			}
			trace(now, "emit", rate, true)
			if rate >= g.MinTPS {
				lowFor = 0 // 速度回升即重置："持续不升"才算降速
				continue
			}
			lowFor += elapsed
			if logEvery > 0 && now.Sub(lastStatus) >= logEvery {
				lastStatus = now
				log.Printf("⚠️ 聚合输出速度 %.1f tok/s 已持续 %s 低于阈值 %.0f tok/s（达到 %s 将中止当前场景）",
					rate, lowFor.Round(time.Millisecond), g.MinTPS, g.Window)
			}
			if lowFor >= g.Window {
				g.trip(rate, lowFor)
				return
			}
		}
	}
}

// trip 置位并回调（只回调一次）。
func (g *StallGuard) trip(rate float64, lowFor time.Duration) {
	g.mu.Lock()
	if g.tripped {
		g.mu.Unlock()
		return
	}
	g.tripped = true
	g.event = StallEvent{
		Rate:     rate,
		MinTPS:   g.MinTPS,
		LowFor:   lowFor,
		At:       g.now(),
		InFlight: g.inFlight.Load(),
		Emitting: g.emitting.Load(),
	}
	ev, cb := g.event, g.onTrip
	g.mu.Unlock()

	// 熔断原因顺手落进采样序列文件：事后看 csv 时"为什么数据是半截的"就地有答案
	if g.W != nil {
		fmt.Fprintf(g.W, "# tripped: rate=%.2f tok/s low_for=%s min_tps=%.0f in_flight=%d emitting=%d\n",
			ev.Rate, ev.LowFor.Round(time.Millisecond), ev.MinTPS, ev.InFlight, ev.Emitting)
	}

	log.Printf("⛔ 降速熔断：聚合输出速度 %.1f tok/s 持续 %s 低于阈值 %.0f tok/s —— 中止当前场景（已完成数据照常保存）",
		ev.Rate, ev.LowFor.Round(time.Millisecond), ev.MinTPS)
	if cb != nil {
		cb(ev)
	}
}

func (g *StallGuard) now() time.Time {
	if g.clock == nil {
		return time.Now()
	}
	return g.clock()
}
