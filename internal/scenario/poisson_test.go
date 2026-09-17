package scenario

import (
	"math"
	"math/rand"
	"testing"
)

// 泊松重整（方法论唯一"必抄"项）：随机抽样的间隔总和与理论值有偏差，
// 重整后末到达时刻必须严格等于 (n-1)/rate——不同 seed 的到达总量一致，
// 吞吐数据跨 run 才可比。同时覆盖三种突发度（1=泊松 / <1 突发 / >1 均匀）。
func TestPoissonDelaysNormalized(t *testing.T) {
	for _, burst := range []float64{1, 0.5, 4} {
		for _, n := range []int{1, 2, 64, 300} {
			rng := rand.New(rand.NewSource(42))
			ts := poissonDelays(n, 4.0, burst, rng)
			if len(ts) != n {
				t.Fatalf("burst=%v n=%d: len=%d", burst, n, len(ts))
			}
			for i := 1; i < n; i++ {
				if ts[i] <= ts[i-1] {
					t.Fatalf("burst=%v n=%d: ts 非单调（ts[%d]=%v ≤ ts[%d]=%v）",
						burst, n, i, ts[i], i-1, ts[i-1])
				}
			}
			if n > 1 {
				want := float64(n-1) / 4.0
				if math.Abs(ts[n-1]-want) > 1e-9 {
					t.Errorf("burst=%v n=%d: 重整后末到达 %v ≠ 理论 %v", burst, n, ts[n-1], want)
				}
			}
		}
	}
}

// rate<=0（等效满并发）＝零间隔齐射。
func TestPoissonDelaysZeroRate(t *testing.T) {
	ts := poissonDelays(8, 0, 1, rand.New(rand.NewSource(1)))
	for i, v := range ts {
		if v != 0 {
			t.Fatalf("ts[%d]=%v, want 0", i, v)
		}
	}
}

// 突发度形态：burstiness>1 趋向恒定间隔，间隔变异系数应显著小于指数分布（CV=1）。
func TestPoissonDelaysBurstinessShape(t *testing.T) {
	cv := func(burst float64) float64 {
		rng := rand.New(rand.NewSource(7))
		ts := poissonDelays(2000, 100.0, burst, rng)
		mean := 1 / 100.0 // 重整后每个间隔均值恒为 1/rate
		var m2, prev float64
		for i := 1; i < len(ts); i++ {
			d := ts[i] - prev
			m2 += (d - mean) * (d - mean)
			prev = ts[i]
		}
		return math.Sqrt(m2/float64(len(ts)-1)) / mean
	}
	if cv(8) > 0.5*cv(1) {
		t.Errorf("burstiness=8 的间隔变异系数 %.3f 应远小于泊松（CV≈1）的 %.3f", cv(8), cv(1))
	}
	if cv(0.5) < cv(1) {
		t.Errorf("burstiness=0.5 比泊松更突发，CV %.3f 应大于 %.3f", cv(0.5), cv(1))
	}
}
