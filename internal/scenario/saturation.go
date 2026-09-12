// 饱和止损（saturation_guard）：负载已饱和时及时停止加压，别把时间烧在注定全错的
// 深饱和区。与降速熔断（stall.go）互补——stall 管「服务端变慢」，本文件管「负载积压」。
//
// 触发语义（drain，2026-09-12 二次迭代）：触发后**只停止发新请求，在飞的自然跑完**——
// 每个已发出的请求都保留完整计时与 usage，被截断的档位是干净的「前缀样本」，不是残缺
// 数据；代价是收尾最多多等一个 timeout_seconds。不选「取消在飞」：取消会把好数据变成
// error 记录，且「数据不完整」恰恰是该机制最该避免的次生伤害。
//
// 判据一（waiting）：服务端 waiting 排队深度持续 ≥ MaxWaiting 达 WindowSeconds。
// 判据二（墙钟）：本档位发射窗口超过 MaxWallSeconds——到点后同样只停发新请求。
//
// 触发后 Aborted 留痕，场景层据此停止后续档位（饱和之后更高档只会更糟）。
package scenario

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aleexjiang/llm-perf/internal/config"
	"github.com/aleexjiang/llm-perf/internal/smetrics"
)

// saturationDecider 饱和判定的纯逻辑核心（时间由调用方注入，单测锁定语义）：
//   - 单次超阈只记录起点，不触发（偶发抖动不截断）；
//   - 回落到阈值以下即重置（与 stall_guard 的「回升即重置」同语义）；
//   - 持续超阈达 window 才触发。
type saturationDecider struct {
	maxWaiting float64
	window     time.Duration
	overSince  time.Time // 零值 = 当前未处于超阈状态
}

// observe 喂一次排队深度采样。ok=false 表示本 tick 没采到数（观测层降级）——
// 观测缺失时无法证明「持续超阈」，重置计时是保守侧（宁可晚触发也不误触发）。
func (d *saturationDecider) observe(waiting float64, ok bool, now time.Time) (tripped bool, lowFor time.Duration) {
	if !ok || waiting < d.maxWaiting {
		d.overSince = time.Time{}
		return false, 0
	}
	if d.overSince.IsZero() {
		d.overSince = now
		return false, 0
	}
	lowFor = now.Sub(d.overSince)
	return lowFor >= d.window, lowFor
}

// levelRun 一档位的发射闸门（drain 语义的载体）：Stop()=true 后场景循环不再发新请求，
// 在飞请求继续跑完。饱和判据与墙钟判据都汇到 Trip()——先到者得，原因唯一。
type levelRun struct {
	started time.Time
	timer   *time.Timer // 墙钟判据的到点回调（nil = 未启用墙钟判据）

	stopped atomic.Bool
	mu      sync.Mutex
	tripped bool
	reason  string
}

func newLevelRun(sg *config.SaturationGuardCfg) *levelRun {
	lr := &levelRun{started: time.Now()}
	if sg != nil && sg.SatEnabled() && sg.MaxWallSeconds > 0 {
		cap := time.Duration(sg.MaxWallSeconds) * time.Second
		lr.timer = time.AfterFunc(cap, func() {
			lr.Trip(fmt.Sprintf("墙钟上限 %s 已到（saturation_guard.max_wall_seconds）——停止发新请求，在飞跑完保留全量", cap))
		})
	}
	return lr
}

// Trip 触发：记原因 + 关发射闸门（幂等——先到者的原因生效，后续 Trip 只留痕不覆盖）。
func (lr *levelRun) Trip(reason string) {
	lr.mu.Lock()
	if lr.tripped {
		lr.mu.Unlock()
		return
	}
	lr.tripped = true
	lr.reason = reason
	lr.mu.Unlock()
	lr.stopped.Store(true)
	log.Printf("⛔ %s", reason)
}

// Stop 发射闸门是否已关闭（场景循环每发一个请求前检查）。
func (lr *levelRun) Stop() bool { return lr.stopped.Load() }

// Reason 触发原因；未触发返回 ""。
func (lr *levelRun) Reason() string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.reason
}

// finish 档位结束：停掉墙钟定时器（正常跑完时防晚到的误触发）。
func (lr *levelRun) finish() {
	if lr.timer != nil {
		lr.timer.Stop()
	}
}

// startSaturationWatch 档位级 waiting 观测器。观测层可用（pol != nil）时**常开**：
// 即便饱和判据未启用也记录本档 waiting 峰值（读场景级 GaugePoller 的采样缓存，
// 不产生额外抓取请求）——这是 max_waiting 的标定数据源（lv.WaitingMax）。
// 启用判据（MaxWaiting > 0）时，持续超阈达窗口 → lr.Trip。
//
// 峰值口径 = **本档窗口内新采到的样本**（起档时记下标，只累计之后的样本）：
// 用 LatestWaiting 会把上一档的残留帧算进来（串档位），且采样 tick 比档位长时
// 会整个漏掉本档观测段 —— 两者都让标定建议值失真。窗口结束时额外定格一次。
//
// 返回 waitMax（读本档峰值，档位结束后读取）与 wait（等 goroutine 退出）。
func startSaturationWatch(ctx context.Context, sg *config.SaturationGuardCfg,
	pol *smetrics.GaugePoller, lr *levelRun) (waitMax func() float64, wait func()) {

	if pol == nil {
		return func() float64 { return 0 }, func() {}
	}
	armed := sg != nil && sg.SatEnabled() && sg.MaxWaiting > 0
	interval := 5 * time.Second
	if sg != nil && sg.SatEnabled() && sg.SampleSeconds > 0 {
		interval = time.Duration(sg.SampleSeconds * float64(time.Second))
	}
	var mu sync.Mutex
	maxSeen := 0.0
	// 本档窗口起点：只统计此后新增的样本（档位开始前已有的帧属于上一档/预热期）
	peakBase := pol.WaitingSampleCount()
	done := make(chan struct{})
	dec := &saturationDecider{
		maxWaiting: float64(sg.GetMaxWaiting()),
		window:     time.Duration(sg.GetWindowSeconds()) * time.Second,
	}
	loggedOver := false // 超阈状态沿只提示一次（进饱和区说一声，别每个 tick 刷屏）
	tripped := false    // 本判据是否已触发（仅 goroutine 内读写，无需加锁）
	// refreshPeak 把本档窗口内新采到的样本并入峰值（不触发抓取，纯读缓存）
	refreshPeak := func() {
		xs := pol.WaitingSamplesSince(peakBase)
		if len(xs) == 0 {
			return // 本档尚无观测：maxSeen 只记真实观测，不造假数
		}
		mu.Lock()
		for _, w := range xs {
			if w > maxSeen {
				maxSeen = w
			}
		}
		mu.Unlock()
	}
	// observe 单次采样：档位一开始立即观测一次（与 GaugePoller 的「首采立即」同语义，
	// 否则首采要等一个 ticker 周期，短档位/短窗口会错过整个观测段）
	observe := func(now time.Time) {
		refreshPeak()
		w, ok := pol.LatestWaiting()
		if !ok {
			return // 观测缺失：maxSeen 只记真实观测，判定侧由 dec 重置
		}
		mu.Lock()
		if w > maxSeen {
			maxSeen = w
		}
		mu.Unlock()
		if !armed {
			return
		}
		trip, lowFor := dec.observe(w, ok, now)
		if trip {
			lr.Trip(fmt.Sprintf("饱和止损：排队深度 waiting≥%d 持续 %s（阈值窗口 %s）——停止发新请求，在飞跑完保留全量",
				sg.GetMaxWaiting(), lowFor.Round(time.Second), dec.window))
			tripped = true
			return
		}
		over := !dec.overSince.IsZero()
		if over != loggedOver {
			loggedOver = over
			if over {
				log.Printf("⚠️ 排队深度 waiting=%.0f 已 ≥ 阈值 %d——持续 %s 不回落将停止发新请求",
					w, sg.GetMaxWaiting(), dec.window)
			}
		}
	}
	go func() {
		defer close(done)
		observe(time.Now())
		if tripped {
			return
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				refreshPeak() // 定格：最后一段采样不能被 tick 周期漏掉
				return
			case now := <-t.C:
				observe(now)
				if tripped {
					return // 本判据已触发：观测使命完成（峰值已定格）
				}
			}
		}
	}()
	return func() float64 {
		mu.Lock()
		defer mu.Unlock()
		return maxSeen
	}, func() { <-done }
}
