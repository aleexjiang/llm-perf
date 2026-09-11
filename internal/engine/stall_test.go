package engine

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 采样与判定窗口都用毫秒级：真时钟即可，不必注入假时钟（ticker 本就基于真时钟）。
const (
	testSample = 10 * time.Millisecond
	testWindow = 60 * time.Millisecond
)

// 持续零产出（有请求在输出态但一个 token 都不来）→ 触发。
// 这是最常见的现场：服务端卡死/极慢，客户端只看到流一直不动。
func TestStallGuardTripsOnZeroOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var trips atomic.Int64
	g := NewStallGuard(20, testWindow, testSample, func(StallEvent) { trips.Add(1) })
	g.Enter()
	g.DecodeStart() // 已开始输出，但不再有任何 token
	go g.Run(ctx)

	waitFor(t, 2*time.Second, g.Tripped, "持续零产出应在窗口后触发熔断")
	// 置位与回调之间存在时序窗口（先置位再回调），按回调计数等待
	waitFor(t, 2*time.Second, func() bool { return trips.Load() == 1 }, "熔断回调应触发一次")
	if n := trips.Load(); n != 1 {
		t.Fatalf("回调应恰好触发一次，实际 %d 次", n)
	}
	ev, ok := g.Event()
	if !ok {
		t.Fatal("触发后 Event() 应可用")
	}
	if ev.Rate != 0 {
		t.Errorf("零产出时现场速度应为 0，实际 %.2f", ev.Rate)
	}
	if ev.LowFor < testWindow {
		t.Errorf("LowFor=%s 应不小于窗口 %s", ev.LowFor, testWindow)
	}
	if ev.InFlight != 1 || ev.Emitting != 1 {
		t.Errorf("现场在飞/输出数应为 1/1，实际 %d/%d", ev.InFlight, ev.Emitting)
	}
}

// 产出速度虽慢但高于阈值 → 不触发（阈值口径是"低于"，等于阈值不算降速）。
func TestStallGuardDoesNotTripAboveThreshold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := NewStallGuard(50, testWindow, testSample, nil) // 阈值 50 tok/s
	g.Enter()
	g.DecodeStart()
	go g.Run(ctx)

	// 节奏：每 10ms 采一次、每次采到 1 token → 100 tok/s > 50，稳定在阈值之上
	stop := time.After(6 * testWindow)
	tick := time.NewTicker(testSample)
	defer tick.Stop()
loop:
	for {
		select {
		case <-stop:
			break loop
		case <-tick.C:
			g.Tokens(1)
		}
	}
	if g.Tripped() {
		t.Fatalf("速度高于阈值不应触发（现场 %+v）", eventOf(g))
	}
}

// 降速中途回升 → 计时重置："持续不升"才算降速。
func TestStallGuardResetsOnRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := NewStallGuard(1000, testWindow, testSample, nil) // 阈值极高：正常产出即算"恢复"
	g.Enter()
	g.DecodeStart()
	go g.Run(ctx)

	// 先低产出熬过接近一个窗口（但不到），再猛灌 token 把速度拉回阈值之上
	time.Sleep(testWindow * 2 / 3)
	g.Tokens(5000) // 单次采样内的速度远超阈值 → 重置降速计时

	// 重置后继续保持高产出，累计时间早已超过窗口，仍不应触发
	deadline := time.Now().Add(2 * testWindow)
	for time.Now().Before(deadline) {
		g.Tokens(100)
		time.Sleep(testSample)
	}
	if g.Tripped() {
		t.Fatalf("速度回升后不应触发（现场 %+v）", eventOf(g))
	}
}

// 没有请求在飞（场景切换/请求间隙）→ 不参与判定，零产出也不能触发。
func TestStallGuardIdleNeverTrips(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := NewStallGuard(20, testWindow, testSample, nil)
	go g.Run(ctx)
	time.Sleep(4 * testWindow) // 全程无 Enter/DecodeStart
	if g.Tripped() {
		t.Fatal("空闲期不应触发熔断")
	}
}

// 有请求在飞但还没吐出第一个 token（长 prompt 的 prefill 阶段）→ 不计入降速判定。
func TestStallGuardPrefillNeverTrips(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := NewStallGuard(20, testWindow, testSample, nil)
	g.Enter() // 在飞，但未 DecodeStart（prefill 中）
	go g.Run(ctx)
	time.Sleep(4 * testWindow)
	if g.Tripped() {
		t.Fatal("纯 prefill 阶段（尚未输出）不应触发熔断")
	}

	// prefill 完成后开始输出：此时才进入判定
	g.DecodeStart()
	waitFor(t, 2*time.Second, g.Tripped, "进入输出态后零产出应触发")
}

// 触发后 Run 立即返回，不再重复回调。
func TestStallGuardTripIsFinal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var trips atomic.Int64
	done := make(chan struct{})
	g := NewStallGuard(1e9, testWindow, testSample, func(StallEvent) { trips.Add(1) })
	g.Enter()
	g.DecodeStart()
	go func() { g.Run(ctx); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("触发后 Run 应立即返回")
	}
	time.Sleep(2 * testWindow)
	if n := trips.Load(); n != 1 {
		t.Fatalf("回调应只触发一次，实际 %d 次", n)
	}
}

// nil 接收者安全：未启用熔断时调用方不做分支判断也不该 panic。
func TestStallGuardNilSafe(t *testing.T) {
	var g *StallGuard
	g.Enter()
	g.Exit()
	g.DecodeStart()
	g.DecodeStop()
	g.Tokens(3)
	if g.Tripped() {
		t.Fatal("nil guard 不应报告已触发")
	}
	if _, ok := g.Event(); ok {
		t.Fatal("nil guard 不应有事件")
	}
	g.Run(context.Background()) // 立即返回，不阻塞
}

// Tokens 忽略非正值（避免上游误传导致计数被拉低/拉高）。
func TestStallGuardTokensIgnoresNonPositive(t *testing.T) {
	g := NewStallGuard(20, time.Second, time.Second, nil)
	g.Tokens(0)
	g.Tokens(-5)
	if n := g.tokens.Load(); n != 0 {
		t.Fatalf("非正增量不应计数，实际 %d", n)
	}
	g.Tokens(2)
	if n := g.tokens.Load(); n != 2 {
		t.Fatalf("正常增量应累加，实际 %d", n)
	}
}

// 采样序列落盘：正常段（高于阈值）、回升段、空闲/prefill 段都要有记录，且 t_s 单调递增；
// 熔断原因以 # 注释行追加在末尾。只记熔断点的话，事后无法回答"从第几档开始掉、掉了多久"。
func TestStallGuardTraceRecordsAllPhases(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var buf bytes.Buffer
	g := NewStallGuard(20, testWindow, testSample, nil) // 阈值 20 tok/s
	g.W = &buf
	g.Enter()
	g.DecodeStart()
	done := make(chan struct{})
	go func() { g.Run(ctx); close(done) }()

	// emit 段：每拍 1 token ≈ 100 tok/s > 20，正常段也必须落盘（否则看不出掉速起点）
	for i := 0; i < 30; i++ {
		g.Tokens(1)
		time.Sleep(testSample)
	}
	// 空闲 + prefill 段：停掉输出、再退出在飞，各留几拍
	g.DecodeStop()
	time.Sleep(3 * testSample)
	g.Exit()
	time.Sleep(3 * testSample)
	// 重新在飞并进入输出态、零 token → 熔断
	g.Enter()
	g.DecodeStart()
	waitFor(t, 2*time.Second, g.Tripped, "零产出应触发熔断")
	<-done // Run 返回（trip 即返回）后再读 buffer，避免与采样 goroutine 竞争

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 6 {
		t.Fatalf("采样行数过少，buffer:\n%s", buf.String())
	}
	if lines[0] != "t_s,agg_tps,in_flight,emitting,phase" {
		t.Fatalf("首行应为表头，实际 %q", lines[0])
	}

	type row struct{ tS, tps, phase string }
	var rows []row
	tripped := false
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "# tripped:") {
			tripped = true
			continue
		}
		f := strings.Split(l, ",")
		if len(f) != 5 {
			t.Fatalf("采样行应有 5 列，实际 %q", l)
		}
		rows = append(rows, row{f[0], f[1], f[4]})
	}
	if !tripped {
		t.Fatal("末尾应有 # tripped 注释行（熔断原因就地留痕）")
	}

	phases := map[string]int{}
	var lastTS float64
	for _, r := range rows {
		v, err := strconv.ParseFloat(r.tS, 64)
		if err != nil {
			t.Fatalf("t_s 应为数值，实际 %q", r.tS)
		}
		if v <= lastTS {
			t.Fatalf("t_s 应单调递增，%.3f <= %.3f", v, lastTS)
		}
		lastTS = v
		phases[r.phase]++
	}
	if phases["emit"] == 0 || phases["prefill"] == 0 || phases["idle"] == 0 {
		t.Fatalf("emit/prefill/idle 三种相位都应出现，实际 %v（行明细:\n%s）", phases, buf.String())
	}
	// 正常段（emit 且高于阈值）的 agg_tps 必须有值；空闲/prefill 段留空（"测不出"≠ 0）
	above := 0
	for _, r := range rows {
		if r.phase != "emit" {
			if r.tps != "" {
				t.Fatalf("非 emit 相位 agg_tps 应留空，实际 %q", r.tps)
			}
			continue
		}
		v, err := strconv.ParseFloat(r.tps, 64)
		if err != nil {
			t.Fatalf("emit 相位 agg_tps 应为数值，实际 %q", r.tps)
		}
		if v > 20 {
			above++
		}
	}
	if above == 0 {
		t.Fatal("应有高于阈值的正常段记录（正常段也落盘才能定位掉速起点）")
	}
	// 熔断前最后一拍必然是低于阈值的 emit 行
	last := rows[len(rows)-1]
	if last.phase != "emit" {
		t.Fatalf("熔断前最后一拍应为 emit 相位，实际 %q", last.phase)
	}
	if v, _ := strconv.ParseFloat(last.tps, 64); v >= 20 {
		t.Fatalf("熔断前最后一拍速度应低于阈值 20，实际 %s", last.tps)
	}
}

// W 为 nil（默认）：不落盘也不影响判定与触发。
func TestStallGuardNilWriterNoTrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := NewStallGuard(20, testWindow, testSample, nil) // W 未设置
	g.Enter()
	g.DecodeStart()
	go g.Run(ctx)
	waitFor(t, 2*time.Second, g.Tripped, "无 trace writer 时仍应正常触发")
}

// waitFor 轮询等待条件成立，超时则失败。
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(msg)
}

func eventOf(g *StallGuard) StallEvent {
	ev, _ := g.Event()
	return ev
}
