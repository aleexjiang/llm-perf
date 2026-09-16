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

// 出首 token 后持续零产出（服务端卡死/极慢，客户端只看到流一直不动）→ 触发。
// 12.10 起"在输出"的判据 = 该流累计出过至少一个 chunk（firstNs != 0）；出首 token
// 前的静默是 prefill（预期耗时），不算降速。
func TestStallGuardTripsOnZeroOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var trips atomic.Int64
	g := NewStallGuard(20, testWindow, testSample, func(StallEvent) { trips.Add(1) })
	st := g.NewStream()
	st.Tokens(1) // 出过首 token，此后不再有任何增量
	go g.Run(ctx)

	waitFor(t, 2*time.Second, g.Tripped, "出首 token 后持续零产出应在窗口后触发熔断")
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
	if ev.Streams != 1 {
		t.Errorf("参与判定流数应为 1，实际 %d", ev.Streams)
	}
}

// 产出速度高于阈值 → 不触发（阈值口径是"低于"，等于阈值不算降速）。
func TestStallGuardDoesNotTripAboveThreshold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := NewStallGuard(50, testWindow, testSample, nil) // 阈值 50 tok/s
	st := g.NewStream()
	st.Tokens(1)
	go g.Run(ctx)

	// 节奏：每 10ms 采一次、每次采到 1 token → ≈100 tok/s > 50，稳定在阈值之上
	stop := time.After(6 * testWindow)
	tick := time.NewTicker(testSample)
	defer tick.Stop()
loop:
	for {
		select {
		case <-stop:
			break loop
		case <-tick.C:
			st.Tokens(1)
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
	st := g.NewStream()
	st.Tokens(1)
	go g.Run(ctx)

	// 先低产出熬过接近一个窗口（但不到），再猛灌 token 把速度拉回阈值之上
	time.Sleep(testWindow * 2 / 3)
	st.Tokens(5000) // 单次采样内的速度远超阈值 → 重置降速计时

	// 重置后继续保持高产出，累计时间早已超过窗口，仍不应触发
	deadline := time.Now().Add(2 * testWindow)
	for time.Now().Before(deadline) {
		st.Tokens(100)
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
	time.Sleep(4 * testWindow) // 全程无在飞流
	if g.Tripped() {
		t.Fatal("空闲期不应触发熔断")
	}
}

// 有请求在飞但还没吐出第一个 token（长 prompt 的 prefill 阶段）→ 不计入降速判定；
// 出首 token 后才进入判定（旧 API 的 DecodeStart 语义 ≈ 首次 Tokens）。
func TestStallGuardPrefillNeverTrips(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := NewStallGuard(20, testWindow, testSample, nil)
	st := g.NewStream() // 在飞，但尚未输出（prefill 中）
	go g.Run(ctx)
	time.Sleep(4 * testWindow)
	if g.Tripped() {
		t.Fatal("纯 prefill 阶段（尚未输出）不应触发熔断")
	}

	// prefill 完成后出首 token、随后零产出：此时才进入判定
	st.Tokens(1)
	waitFor(t, 2*time.Second, g.Tripped, "进入输出态后零产出应触发")
}

// 触发后 Run 立即返回，不再重复回调。
func TestStallGuardTripIsFinal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var trips atomic.Int64
	done := make(chan struct{})
	g := NewStallGuard(1e9, testWindow, testSample, func(StallEvent) { trips.Add(1) })
	st := g.NewStream()
	st.Tokens(1)
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
	s := g.NewStream() // nil guard → nil 句柄（全链路 nil 安全）
	s.Tokens(3)
	s.Done()
	if g.Tripped() {
		t.Fatal("nil guard 不应报告已触发")
	}
	if _, ok := g.Event(); ok {
		t.Fatal("nil guard 不应有事件")
	}
	g.Run(context.Background()) // 立即返回，不阻塞
}

// 非正增量忽略（避免上游误传污染速度），且不应把流标记为"已出首 token"
// （否则首个负增量会把 prefill 段误拉进判定）。
func TestStallStreamTokensIgnoresNonPositive(t *testing.T) {
	g := NewStallGuard(20, time.Second, time.Second, nil)
	s := g.NewStream()
	s.Tokens(0)
	s.Tokens(-5)
	if n := s.tokens.Load(); n != 0 {
		t.Fatalf("非正增量不应计数，实际 %d", n)
	}
	if s.firstNs.Load() != 0 {
		t.Fatal("非正增量不应标记出首 token")
	}
	s.Tokens(2)
	if n := s.tokens.Load(); n != 2 {
		t.Fatalf("正常增量应累加，实际 %d", n)
	}
	if s.firstNs.Load() == 0 {
		t.Fatal("首个正增量应标记出首 token")
	}
}

// 12.10 核心验收①：多流同降——各流都出过首 token 后**同时**停滞，中位跌破阈值 → 触发。
// 这正是聚合口径漏掉的形态：并发越高聚合越高、反而越难触发（r1-off S5 实测的根因）。
func TestStallGuardMultiStreamDegradationTrips(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := NewStallGuard(20, testWindow, testSample, nil)
	for i := 0; i < 4; i++ {
		s := g.NewStream()
		s.Tokens(1) // 都出过首 token（模拟已进入 decode 后被“卡住”）
	}
	go g.Run(ctx)
	// 全部停滞（不再有任何增量）→ 中位 0 < 20，窗口后触发
	waitFor(t, 2*time.Second, g.Tripped, "多流同降至零应触发（中位口径）")
	ev, _ := g.Event()
	if ev.Streams != 4 {
		t.Errorf("参与判定流数应为 4，实际 %d", ev.Streams)
	}
	if ev.InFlight != 4 || ev.Emitting != 4 {
		t.Errorf("现场在飞/输出数应为 4/4，实际 %d/%d", ev.InFlight, ev.Emitting)
	}
}

// 12.10 核心验收②：中位数抗单流异常——1 路停滞 + 3 路健康，中位仍高 → 不触发。
// 单流偶发抖动/单流卡顿不该熔断整个场景；只有"普遍劣化"才是服务端问题信号。
func TestStallGuardSingleSlowStreamDoesNotTrip(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := NewStallGuard(20, testWindow, testSample, nil)
	slow := g.NewStream()
	slow.Tokens(1) // 此路此后停滞（模拟单流卡顿）
	var fast []*StallStream
	for i := 0; i < 3; i++ {
		f := g.NewStream()
		f.Tokens(1)
		fast = append(fast, f)
	}
	go g.Run(ctx)

	// 3 路健康流保持 ≈100 tok/s；1 路停滞拖不垮中位（排序后取中两值仍是 100）
	stop := time.After(6 * testWindow)
	tick := time.NewTicker(testSample)
	defer tick.Stop()
loop:
	for {
		select {
		case <-stop:
			break loop
		case <-tick.C:
			for _, f := range fast {
				f.Tokens(1)
			}
		}
	}
	if g.Tripped() {
		t.Fatalf("单流慢、多数健康时中位不应触发（现场 %+v）", eventOf(g))
	}
}

// 采样序列落盘：正常段（高于阈值）、回升段、空闲/prefill 段都要有记录，且 t_s 单调递增；
// 熔断原因以 # 注释行追加在末尾。只记熔断点的话，事后无法回答"从第几档开始掉、掉了多久"。
// 12.10 列序：t_s,agg_tps,med_tps,in_flight,emitting,phase——agg 是观测面、med 是判定面。
func TestStallGuardTraceRecordsAllPhases(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var buf bytes.Buffer
	g := NewStallGuard(20, testWindow, testSample, nil) // 阈值 20 tok/s
	g.W = &buf
	s1 := g.NewStream()
	s1.Tokens(1)
	done := make(chan struct{})
	go func() { g.Run(ctx); close(done) }()

	// emit 段：每拍 1 token ≈ 100 tok/s > 20，正常段也必须落盘（否则看不出掉速起点）
	for i := 0; i < 30; i++ {
		s1.Tokens(1)
		time.Sleep(testSample)
	}
	// 空闲段：流结束退出在飞
	s1.Done()
	time.Sleep(3 * testSample)
	// prefill 段：新流在飞但尚未输出
	s2 := g.NewStream()
	time.Sleep(3 * testSample)
	// 出首 token 后零产出 → 熔断
	s2.Tokens(1)
	waitFor(t, 2*time.Second, g.Tripped, "零产出应触发熔断")
	<-done // Run 返回（trip 即返回）后再读 buffer，避免与采样 goroutine 竞争

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 6 {
		t.Fatalf("采样行数过少，buffer:\n%s", buf.String())
	}
	if lines[0] != "t_s,agg_tps,med_tps,in_flight,emitting,phase" {
		t.Fatalf("首行应为表头，实际 %q", lines[0])
	}

	type row struct{ tS, agg, med, phase string }
	var rows []row
	tripped := false
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "# tripped:") {
			tripped = true
			continue
		}
		f := strings.Split(l, ",")
		if len(f) != 6 {
			t.Fatalf("采样行应有 6 列，实际 %q", l)
		}
		rows = append(rows, row{f[0], f[1], f[2], f[5]})
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
	// 非 emit 相位两列都留空（"测不出"≠ 0）；emit 相位 agg 恒有值（观测面）、
	// med 仅在存在可判定流时才有值（判定面）
	above := 0
	for _, r := range rows {
		if r.phase != "emit" {
			if r.agg != "" || r.med != "" {
				t.Fatalf("非 emit 相位 agg/med 应留空，实际 %q/%q", r.agg, r.med)
			}
			continue
		}
		if _, err := strconv.ParseFloat(r.agg, 64); err != nil {
			t.Fatalf("emit 相位 agg_tps 应为数值，实际 %q", r.agg)
		}
		if r.med == "" {
			continue
		}
		mv, err := strconv.ParseFloat(r.med, 64)
		if err != nil {
			t.Fatalf("emit 相位 med_tps 应为数值，实际 %q", r.med)
		}
		if mv > 20 {
			above++
		}
	}
	if above == 0 {
		t.Fatal("应有高于阈值的正常段记录（med 有值且 > 阈值，正常段也落盘才能定位掉速起点）")
	}
	// 熔断前最后一拍必然是低于阈值的 emit 行（判定面 med 落值）
	last := rows[len(rows)-1]
	if last.phase != "emit" {
		t.Fatalf("熔断前最后一拍应为 emit 相位，实际 %q", last.phase)
	}
	if last.med == "" {
		t.Fatalf("熔断前最后一拍 med 应有值（行明细:\n%s）", buf.String())
	}
	if v, _ := strconv.ParseFloat(last.med, 64); v >= 20 {
		t.Fatalf("熔断前最后一拍速度应低于阈值 20，实际 %s", last.med)
	}
}

// W 为 nil（默认）：不落盘也不影响判定与触发。
func TestStallGuardNilWriterNoTrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := NewStallGuard(20, testWindow, testSample, nil) // W 未设置
	st := g.NewStream()
	st.Tokens(1)
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
