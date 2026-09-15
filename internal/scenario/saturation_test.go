package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/engine"
	"github.com/aleexjiang/llm-perf/internal/smetrics"
)

// ── saturationDecider：判定核心纯逻辑 ──

func TestSaturationDecider(t *testing.T) {
	base := time.Unix(0, 0)
	d := &saturationDecider{maxWaiting: 32, window: 120 * time.Second}

	// 低于阈值：不触发、不计时
	if trip, lowFor := d.observe(10, true, base); trip || lowFor != 0 {
		t.Fatalf("低水位不应触发: trip=%v lowFor=%v", trip, lowFor)
	}
	// 首次超阈：只记起点
	if trip, _ := d.observe(64, true, base.Add(10*time.Second)); trip {
		t.Fatal("首次超阈不应触发")
	}
	// 持续不足窗口：不触发
	if trip, lowFor := d.observe(64, true, base.Add(60*time.Second)); trip || lowFor != 50*time.Second {
		t.Fatalf("50s 未达窗口不应触发: trip=%v lowFor=%v", trip, lowFor)
	}
	// 持续达窗口：触发
	if trip, lowFor := d.observe(64, true, base.Add(130*time.Second)); !trip || lowFor != 120*time.Second {
		t.Fatalf("持续 120s 应触发: trip=%v lowFor=%v", trip, lowFor)
	}
	// 回落：重置计时
	if trip, lowFor := d.observe(1, true, base.Add(140*time.Second)); trip || lowFor != 0 {
		t.Fatalf("回落应重置: trip=%v lowFor=%v", trip, lowFor)
	}
	// 重置后再次超阈需重新累计
	if trip, _ := d.observe(64, true, base.Add(200*time.Second)); trip {
		t.Fatal("重置后重新计起，不应立即触发")
	}
	// 采样缺失（观测层降级）：按未超阈处理并重置——宁可晚触发不误触发
	if trip, _ := d.observe(0, false, base.Add(210*time.Second)); trip {
		t.Fatal("采样缺失不应触发")
	}
	if trip, _ := d.observe(64, true, base.Add(220*time.Second)); trip {
		t.Fatal("采样缺失应重置计时，重新计起不应立即触发")
	}
	// 恰好等于阈值：≥ 语义，算超阈
	d2 := &saturationDecider{maxWaiting: 32, window: 10 * time.Second}
	d2.observe(32, true, base)
	if trip, _ := d2.observe(32, true, base.Add(10*time.Second)); !trip {
		t.Fatal("waiting 恰好等于阈值应按超阈计（≥ 语义）")
	}
}

// ── levelRun：发射闸门（drain 语义的载体） ──

func TestLevelRunWallCapFires(t *testing.T) {
	on := true
	sg := &config.SaturationGuardCfg{Enabled: &on, MaxWallSeconds: 1}
	lr := newLevelRun(sg)
	defer lr.finish()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if lr.Stop() {
			r := lr.Reason()
			if !strings.Contains(r, "墙钟上限 1s") || !strings.Contains(r, "在飞跑完") {
				t.Fatalf("墙钟触发现场原因不符: %q", r)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("1s 墙钟应按时触发")
}

func TestLevelRunNoCapNeverStops(t *testing.T) {
	lr := newLevelRun(nil)
	defer lr.finish()
	// 未配置段：2.5s 内不应触发（cap 若误生效早该到了）
	time.Sleep(1500 * time.Millisecond)
	if lr.Stop() {
		t.Fatalf("未配置墙钟不应触发: %q", lr.Reason())
	}
	off := false
	lr2 := newLevelRun(&config.SaturationGuardCfg{Enabled: &off, MaxWallSeconds: 1})
	defer lr2.finish()
	time.Sleep(1500 * time.Millisecond)
	if lr2.Stop() {
		t.Fatalf("enabled:false 不应触发: %q", lr2.Reason())
	}
}

func TestLevelRunTripIdempotent(t *testing.T) {
	lr := newLevelRun(nil)
	defer lr.finish()
	lr.Trip("第一个原因")
	lr.Trip("第二个原因")
	if r := lr.Reason(); r != "第一个原因" {
		t.Fatalf("先到者的原因应生效，got %q", r)
	}
	if !lr.Stop() {
		t.Fatal("Trip 后应关闭发射闸门")
	}
}

// ── startSaturationWatch：观测器（常开记录峰值）+ 饱和判据 ──

func TestStartSaturationWatchNilPoller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lr := newLevelRun(nil)
	defer lr.finish()
	waitMax, runMax, wait := startSaturationWatch(ctx, nil, nil, lr)
	if w := waitMax(); w != 0 {
		t.Fatalf("nil poller waiting 峰值应恒 0，got %v", w)
	}
	if w := runMax(); w != 0 {
		t.Fatalf("nil poller running 峰值应恒 0，got %v", w)
	}
	wait() // 应立即返回（未启动 goroutine）
}

func TestStartSaturationWatchObservesMax(t *testing.T) {
	// 判据未启用（MaxWaiting=0）：观测器只记录峰值，不触发——标定数据源
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "vllm:num_requests_waiting{engine=\"0\"} 7\n")
		fmt.Fprint(w, "vllm:num_requests_running{engine=\"0\"} 3\n")
	}))
	t.Cleanup(srv.Close)
	pol := smetrics.StartGaugePoller(context.Background(), smetrics.NewScraperAt(srv.URL, "/metrics"),
		5*time.Millisecond, smetrics.VLLM())
	defer pol.Stop()
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lr := newLevelRun(nil)
	defer lr.finish()
	waitMax, runMax, wait := startSaturationWatch(ctx, &config.SaturationGuardCfg{}, pol, lr)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if w := waitMax(); w == 7 {
			if r := runMax(); r != 3 {
				t.Fatalf("running 峰值应同步观测到 3，got %v", r)
			}
			if lr.Stop() {
				t.Fatalf("判据未启用不应触发: %q", lr.Reason())
			}
			cancel() // idempotent，与 defer 重复无害
			wait()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("观测器应在 3s 内记录 waiting 峰值")
}

func TestStartSaturationWatchTriggers(t *testing.T) {
	// 持续报 waiting=64：远快于判定窗口，LatestWaiting 每 tick 都取到超阈值
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "vllm:num_requests_waiting{engine=\"0\"} 64\n")
		fmt.Fprint(w, "vllm:num_requests_running{engine=\"0\"} 8\n")
	}))
	t.Cleanup(srv.Close)
	pol := smetrics.StartGaugePoller(context.Background(), smetrics.NewScraperAt(srv.URL, "/metrics"),
		5*time.Millisecond, smetrics.VLLM())
	defer pol.Stop()
	time.Sleep(30 * time.Millisecond)

	sgOn := true
	sg := &config.SaturationGuardCfg{Enabled: &sgOn, MaxWaiting: 32, WindowSeconds: 1, SampleSeconds: 0.2}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lr := newLevelRun(sg)
	defer lr.finish()
	waitMax, runMax, wait := startSaturationWatch(ctx, sg, pol, lr)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if lr.Stop() {
			r := lr.Reason()
			if !strings.Contains(r, "饱和止损") || !strings.Contains(r, "waiting≥32") || !strings.Contains(r, "在飞跑完") {
				t.Fatalf("触发现场原因不符: %q", r)
			}
			if w := waitMax(); w < 32 {
				t.Fatalf("触发前应已观测到超阈峰值，got %v", w)
			}
			if r := runMax(); r < 8 {
				t.Fatalf("running 峰值应同步观测，got %v", r)
			}
			if ctx.Err() != nil {
				t.Fatal("drain 语义：触发不应取消 ctx（在飞要跑完）")
			}
			wait()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("持续超阈 1s 应在 5s 内触发")
}

// ── 集成：drain 语义端到端（墙钟触发后停发新请求、在飞全部跑完保留） ──

// slowSSEStub 每个请求延迟 delay 再返回正常 SSE 流——制造在飞窗口。
func slowSSEStub(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		emit := func(v map[string]any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
			f.Flush()
		}
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"role": "assistant"}}}})
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "hi "}}}})
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func satCfg(t *testing.T, endpoint string, sg *config.SaturationGuardCfg) *config.Config {
	t.Helper()
	cfg := testCfg(t, endpoint)
	cfg.Concurrent = config.Concurrent{
		Levels:        []int{2},
		RunsPerWorker: 4,
		PromptTokens:  100,
		MaxTokens:     config.IntList{16},
		Ramp:          &[]bool{false}[0], // 齐射：发车行为确定，便于断言
	}
	cfg.SaturationGuard = sg
	return cfg
}

func TestClosedRoundWallCapDrains(t *testing.T) {
	// 每个请求 1.2s，墙钟 1s：触发时每个 worker 的首轮在飞 → drain 后各保留 1 条，
	// 剩余 3 轮不再发。全部请求完整（Error 空）= drain 语义核心断言。
	srv := slowSSEStub(t, 1200*time.Millisecond)
	sgOn := true
	sg := &config.SaturationGuardCfg{Enabled: &sgOn, MaxWallSeconds: 1}

	cfg := satCfg(t, srv.URL, sg)
	e, err := newEnv(context.Background(), cfg, engine.NewClient(srv.URL, "", 10*time.Second, true))
	if err != nil {
		t.Fatal(err)
	}
	vOff := config.ThinkingVariant{Name: "off"}
	lv := runClosedRound(context.Background(), e, cfg, "stub-model", vOff, 2, 16, nil)

	if !strings.Contains(lv.Aborted, "墙钟上限 1s") {
		t.Fatalf("应记墙钟触发现场: %q", lv.Aborted)
	}
	// drain 核心：已发出的每个请求都完整保留（旧语义下在飞被取消会成 error 记录）
	if len(lv.Requests) == 0 {
		t.Fatal("至少应保留触发前已发出的请求")
	}
	if len(lv.Requests) >= 2*4 {
		t.Fatalf("触发后应停止发新请求（发出 %d < 配置 8）", len(lv.Requests))
	}
	for i, m := range lv.Requests {
		if m.Error != "" {
			t.Fatalf("drain 后第 %d 条请求应为完整数据（旧语义会是取消错误），got err=%q", i+1, m.Error)
		}
		if m.CompletionTokens != 2 {
			t.Fatalf("第 %d 条请求 usage 不完整: completion=%d", i+1, m.CompletionTokens)
		}
	}
}

func TestOpenRoundWallCapDrains(t *testing.T) {
	// rate=2/s、num_prompts=20、墙钟 1s、单请求 800ms：固定种子的 Poisson 序列在触发前
	// 只发射前几个到达，之后发射闸门关闭；已发射的请求在飞跑完、数据完整。
	srv := slowSSEStub(t, 800*time.Millisecond)
	sgOn := true
	sg := &config.SaturationGuardCfg{Enabled: &sgOn, MaxWallSeconds: 1}

	cfg := satCfg(t, srv.URL, sg)
	cfg.Concurrent.Levels = nil
	cfg.Concurrent.RequestRate = 2
	cfg.Concurrent.NumPrompts = 20
	e, err := newEnv(context.Background(), cfg, engine.NewClient(srv.URL, "", 10*time.Second, true))
	if err != nil {
		t.Fatal(err)
	}
	vOff := config.ThinkingVariant{Name: "off"}
	lv := runOpenRound(context.Background(), e, cfg, "stub-model", vOff, 2, 16, nil)

	if !strings.Contains(lv.Aborted, "墙钟上限 1s") {
		t.Fatalf("应记墙钟触发现场: %q", lv.Aborted)
	}
	if len(lv.Requests) == 0 || len(lv.Requests) >= 20 {
		t.Fatalf("应保留在飞完整数据且停发后续到达: %d 条", len(lv.Requests))
	}
	for i, m := range lv.Requests {
		if m.Error != "" {
			t.Fatalf("drain 后第 %d 条请求应为完整数据，got err=%q", i+1, m.Error)
		}
	}
}
