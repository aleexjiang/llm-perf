package engine

import (
	"context"
	"fmt"
	"io"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// StallGuard 降速熔断：按「单流 decode 速度中位」判定服务端是否已退化到不值得继续跑。
//
// 口径（2026-09-16 拍板，取代 2026-09-11 的聚合口径）：
//   - 单流：每条在飞流独立记账窗口内输出增量，判定取各流速度的**中位数**——抗单流
//     偶发抖动、反映普遍劣化。r1-off S5 实测：20 路并发单流已劣化到 6–16 tok/s，
//     聚合仍有 ~113 tok/s 躲过阈值；单流劣化到不可用才是该停的信号（客户端体验口径）。
//     min_tps 即单流阈值（与 probe decode_speed 建议值、恢复探针同口径）。
//   - 流需已出首 token（排除 TTFT/prefill 段）；刚出首 token 的流从**下一个采样窗**起
//     参与判定（首个输出窗不满窗，避免半窗样本把速度算低）。
//   - 计数器按流式 chunk 近似 token（主流引擎 1 chunk ≈ 1 token，与 ITL 口径一致）；
//     非流式请求没有实时信号，不参与判定。
//   - 聚合速度保留为 stall.csv 观测面（agg_tps 列），不作为熔断依据。
//
// 判定规则：单流速度中位连续低于 MinTPS 达 Window 时长即触发一次。期间任一次采样
// 回升到阈值以上即重置计时（对应「持续不升」的语义）；可判定的流不足（都在首个输出
// 窗口内）也重置计时——测不出 ≠ 降速。
//
// 两种不参与判定的情况（避免误触发）：
//   - 没有请求在飞（场景切换、请求间隙）——没东西可测；
//   - 有请求在飞但都还没吐出第一个 token（长 prompt 的 prefill 阶段）——这是预期耗时，
//     不是退化。只有「正在输出却输出得慢」才算降速。
//
// 触发后调用 onTrip（通常是一个场景级 cancel），由场景层决定停下来之后做什么：
// 本项目当前的做法是只中止当前场景、冷却后续跑下一场景，不终止整轮。
type StallGuard struct {
	MinTPS float64       // 阈值：单流 decode 速度中位低于该值即视为降速
	Window time.Duration // 连续低于阈值多久触发
	Sample time.Duration // 采样周期（默认 2s）；越低越灵敏、日志越密

	// LogEvery 慢速期的状态提示间隔（0 = 默认 60s，负数 = 不提示）。
	// 只提示不触发，方便在长窗口里看清"到底是慢还是卡死"。
	LogEvery time.Duration

	// W 降速采样序列侧文件（CSV，nil = 不落盘）。每次采样追加一行
	// `t_s,agg_tps,med_tps,in_flight,emitting,phase`：正常段（高于阈值）也记录，否则事后
	// 只剩一个熔断点，无法回答"从第几档开始掉、是渐变还是断崖、掉了多久"。
	// agg_tps = 窗口聚合速度（观测面，emit 相位恒有值）；med_tps = 参与判定流的单流速度
	// 中位（判定面，可判定的流不足时留空）。空闲/prefill 相位两列都留空（"测不出"≠ 0）。
	// phase 取 emit / prefill / idle。熔断触发时以 `#` 注释行追加现场原因。
	// 仅 Run 的采样 goroutine 会写 W（trip 由 Run 调用），无并发写。
	W io.Writer

	mu      sync.Mutex
	streams map[*StallStream]struct{}
	tripped bool
	event   StallEvent

	onTrip func(StallEvent)
	clock  func() time.Time
}

// StallStream 一条在飞流（一次请求尝试）的记账句柄。
// 生命周期：NewStream →（流式回调）Tokens* → Done；各方法 nil 安全（熔断未启用时调用方不做分支）。
type StallStream struct {
	g       *StallGuard
	tokens  atomic.Int64 // 本流累计输出 chunk（近似 token 数）
	firstNs atomic.Int64 // 首个 token 的时间（UnixNano；0 = 尚未输出，prefill 段）
}

// StallEvent 一次降速熔断的现场快照，用于日志与报告标注。
type StallEvent struct {
	Rate     float64       `json:"rate_tps"`   // 触发时的单流 decode 速度中位
	MinTPS   float64       `json:"min_tps"`    // 阈值（单流口径）
	LowFor   time.Duration `json:"low_for_ms"` // 持续低于阈值的时长
	At       time.Time     `json:"at"`         //
	InFlight int64         `json:"in_flight"`  // 触发时的在飞请求数
	Emitting int64         `json:"emitting"`   // 触发时已出首 token 的请求数
	Streams  int           `json:"streams"`    // 参与判定的流数（中位样本量）
}

// NewStallGuard 创建熔断器。onTrip 可为 nil（只置位不回调）。
func NewStallGuard(minTPS float64, window, sample time.Duration, onTrip func(StallEvent)) *StallGuard {
	if sample <= 0 {
		sample = 2 * time.Second
	}
	return &StallGuard{
		MinTPS:  minTPS,
		Window:  window,
		Sample:  sample,
		streams: map[*StallStream]struct{}{},
		onTrip:  onTrip,
		clock:   time.Now,
	}
}

// NewStream 登记一条在飞流；请求结束（无论成败）必须调用句柄的 Done。
// nil guard 返回 nil 句柄（全链路 nil 安全，与未启用熔断时同语义）。
func (g *StallGuard) NewStream() *StallStream {
	if g == nil {
		return nil
	}
	s := &StallStream{g: g}
	g.mu.Lock()
	if g.streams == nil {
		g.streams = map[*StallStream]struct{}{}
	}
	g.streams[s] = struct{}{}
	g.mu.Unlock()
	return s
}

// Tokens 记一次输出增量（流式 chunk）。首次调用即视为「已出首 token」（decode 起点）。
func (s *StallStream) Tokens(n int) {
	if s == nil || s.g == nil || n <= 0 {
		return
	}
	s.tokens.Add(int64(n))
	if s.firstNs.Load() == 0 {
		s.firstNs.Store(s.g.now().UnixNano())
	}
}

// Done 流结束（成功/失败/取消都要调用）：从在飞集合移除。
func (s *StallStream) Done() {
	if s == nil || s.g == nil {
		return
	}
	s.g.mu.Lock()
	delete(s.g.streams, s)
	s.g.mu.Unlock()
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
	lastStatus := last
	var lowFor time.Duration

	// t_s 是场景内相对秒数（Run 启动 = 场景开始附近），每场景一个文件
	start := g.now()
	if g.W != nil {
		fmt.Fprintln(g.W, "t_s,agg_tps,med_tps,in_flight,emitting,phase")
	}
	trace := func(at time.Time, phase string, agg, med *float64, inFlight, emitting int64) {
		if g.W == nil {
			return
		}
		aggS, medS := "", ""
		if agg != nil {
			aggS = fmt.Sprintf("%.2f", *agg)
		}
		if med != nil {
			medS = fmt.Sprintf("%.2f", *med)
		}
		fmt.Fprintf(g.W, "%.3f,%s,%s,%d,%d,%s\n",
			at.Sub(start).Seconds(), aggS, medS, inFlight, emitting, phase)
	}

	// lastSeen 各流上次采样时的 token 数（只被本采样 goroutine 读写，无需加锁；
	// 每拍重建顺带剔除已结束的流）
	lastSeen := map[*StallStream]int64{}

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			elapsed := now.Sub(last)
			windowStart := last
			last = now

			snap := g.snapshot()
			inFlight := int64(len(snap))
			var emitting, totalDelta int64
			var rates []float64
			nextSeen := make(map[*StallStream]int64, len(snap))
			for _, s := range snap {
				cur := s.tokens.Load()
				first := s.firstNs.Load()
				if first != 0 {
					emitting++
				}
				delta := cur - lastSeen[s]
				nextSeen[s] = cur
				totalDelta += delta
				// 参与判定：本窗起点已在输出态（满窗样本；刚出首 token 的流下窗起参与）
				if first != 0 && first <= windowStart.UnixNano() {
					rates = append(rates, float64(delta)/elapsed.Seconds())
				}
			}
			lastSeen = nextSeen
			agg := float64(totalDelta) / elapsed.Seconds()

			if emitting == 0 {
				lowFor = 0
				if inFlight == 0 {
					trace(now, "idle", nil, nil, inFlight, emitting)
				} else {
					trace(now, "prefill", nil, nil, inFlight, emitting)
				}
				continue
			}

			med, hasMed := medianRate(rates)
			if hasMed {
				trace(now, "emit", &agg, &med, inFlight, emitting)
			} else {
				trace(now, "emit", &agg, nil, inFlight, emitting)
			}
			if !hasMed {
				lowFor = 0 // 可判定的流不足（都在首个输出窗内）：测不出 ≠ 降速
				continue
			}
			if med >= g.MinTPS {
				lowFor = 0 // 速度回升即重置："持续不升"才算降速
				continue
			}
			lowFor += elapsed
			if logEvery > 0 && now.Sub(lastStatus) >= logEvery {
				lastStatus = now
				log.Printf("⚠️ 单流 decode 速度中位 %.1f tok/s 已持续 %s 低于阈值 %.0f tok/s（参与判定 %d/%d 流，达到 %s 将中止当前场景）",
					med, lowFor.Round(time.Millisecond), g.MinTPS, len(rates), inFlight, g.Window)
			}
			if lowFor >= g.Window {
				g.trip(med, lowFor, len(rates))
				return
			}
		}
	}
}

// snapshot 取当前在飞流快照（不持锁遍历）。
func (g *StallGuard) snapshot() []*StallStream {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*StallStream, 0, len(g.streams))
	for s := range g.streams {
		out = append(out, s)
	}
	return out
}

// medianRate 单流速度的中位数；无样本时 ok=false。
func medianRate(rates []float64) (float64, bool) {
	if len(rates) == 0 {
		return 0, false
	}
	sorted := make([]float64, len(rates))
	copy(sorted, rates)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2], true
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2, true
}

// trip 置位并回调（只回调一次）。
func (g *StallGuard) trip(medRate float64, lowFor time.Duration, streams int) {
	g.mu.Lock()
	if g.tripped {
		g.mu.Unlock()
		return
	}
	g.tripped = true
	var emitting int64
	for s := range g.streams {
		if s.firstNs.Load() != 0 {
			emitting++
		}
	}
	g.event = StallEvent{
		Rate:     medRate,
		MinTPS:   g.MinTPS,
		LowFor:   lowFor,
		At:       g.now(),
		InFlight: int64(len(g.streams)),
		Emitting: emitting,
		Streams:  streams,
	}
	ev, cb := g.event, g.onTrip
	g.mu.Unlock()

	// 熔断原因顺手落进采样序列文件：事后看 csv 时"为什么数据是半截的"就地有答案
	if g.W != nil {
		fmt.Fprintf(g.W, "# tripped: med_rate=%.2f tok/s streams=%d low_for=%s min_tps=%.0f in_flight=%d emitting=%d\n",
			ev.Rate, ev.Streams, ev.LowFor.Round(time.Millisecond), ev.MinTPS, ev.InFlight, ev.Emitting)
	}

	log.Printf("⛔ 降速熔断：单流 decode 速度中位 %.1f tok/s（%d 流参与判定）持续 %s 低于阈值 %.0f tok/s —— 中止当前场景（已完成数据照常保存）",
		ev.Rate, ev.Streams, ev.LowFor.Round(time.Millisecond), ev.MinTPS)
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
