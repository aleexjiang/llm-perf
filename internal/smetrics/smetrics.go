// Package smetrics 抓取推理服务端原生暴露的 Prometheus /metrics（vLLM 等默认开启），
// 给客户端计时补上"服务端视角"：前缀缓存命中、排队深度、prefill/decode 分解、投机解码接受率。
//
// 采集模型（对标 NVIDIA AIPerf 的 server metrics 层）：
//   - counter：请求/场景前后各抓一次，取差值（串行时精确归因到单个请求；并发窗口内为混合贡献）
//   - gauge：后台轮询取峰值/均值（running/waiting 排队深度、KV 池占用）
//   - histogram：场景窗口差值，从桶边界估算分位数（AIPerf 口径 p50/p99_estimate）
//
// 指标名做归一化：Prometheus counter 的 _total 后缀与 _created 时间线剔除，
// 兼容 vLLM 各版本命名差异（如 prefix_cache_hits / prefix_cache_hits_total）。
package smetrics

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aleexjiang/llm-perf/internal/auth"
)

// Bucket 是 histogram 的一个桶（LE 为上边界，+Inf 用 math.Inf(1)）。
type Bucket struct {
	LE    float64
	Count float64
}

// Hist 是一个 histogram family 的完整快照。
type Hist struct {
	Buckets []Bucket
	Sum     float64
	Count   float64
}

// Sample 是一次 /metrics 抓取的完整快照。
type Sample struct {
	Counters map[string]float64 // 归一化名（去 _total）→ 跨 label 系列求和
	Gauges   map[string]float64
	Hists    map[string]*Hist // family 名（无 _bucket/_sum/_count 后缀）
}

// Scraper 面向一个服务端 /metrics 端点。
type Scraper struct {
	URL        string // 如 http://host:port/metrics
	Client     *http.Client
	MaxRetries int // Scrape 失败后的额外重试次数（默认 2；轮询场景置 0——下一个 tick 天然是重试）

	// 认证：与 chat 请求共用 internal/auth（客户网关常把 /metrics 和业务接口用同一套认证
	// 保护——不带认证头时观测层会静默降级）。
	Auth   auth.Auth // "" = bearer；raw = 裸 key；none = 不带认证头
	APIKey string
}

// applyAuth 把认证头写进 metrics 请求。
func (s *Scraper) applyAuth(req *http.Request) {
	s.Auth.Apply(req, s.APIKey)
}

// NewScraperAt 显式指定 metrics 路径（客户环境不一定挂在根路径，如 /actuator/prometheus）。
func NewScraperAt(endpoint, metricsPath string) *Scraper {
	base := strings.TrimRight(endpoint, "/")
	base = strings.TrimSuffix(base, "/v1")
	return &Scraper{
		URL:        base + metricsPath,
		Client:     &http.Client{Timeout: 5 * time.Second},
		MaxRetries: 2,
	}
}

// Available 探测 /metrics 是否可达且含 vLLM 系指标。
func (s *Scraper) Available(ctx context.Context) (bool, string) {
	sample, err := s.Scrape(ctx)
	if err != nil {
		return false, err.Error()
	}
	n := len(sample.Counters) + len(sample.Gauges) + len(sample.Hists)
	if n == 0 {
		return false, "端点可达但没有任何指标"
	}
	return true, fmt.Sprintf("可达，%d 项指标", n)
}

// Scrape 抓取并解析一次 /metrics。瞬时 connection refused（服务端 accept 队列被打满的
// 场景起跑瞬间常见）自动重试 MaxRetries 次（默认 2）。
func (s *Scraper) Scrape(ctx context.Context) (*Sample, error) {
	var lastErr error
	for attempt := 0; attempt <= s.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(300 * time.Millisecond):
			}
		}
		sample, err := s.scrapeOnce(ctx)
		if err == nil {
			return sample, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (s *Scraper) scrapeOnce(ctx context.Context) (*Sample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, err
	}
	s.applyAuth(req)
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	const maxBody = 16 << 20 // 16MB：超长响应截断会产生不完整半行，静默丢指标比失败更糟
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("metrics 响应超过 16MB 上限被截断（多 label 大集群请精简暴露指标）")
	}
	return Parse(string(body)), nil
}

// Parse 解析 Prometheus 文本暴露格式。解析失败的行静默跳过（第三方指标混杂是常态）。
func Parse(text string) *Sample {
	s := &Sample{
		Counters: map[string]float64{},
		Gauges:   map[string]float64{},
		Hists:    map[string]*Hist{},
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, rest := splitMetricLine(line)
		val, err := strconv.ParseFloat(rest, 64)
		// NaN/±Inf（坏 exporter/采样窗口）会随 counter 差分扩散，最终让整份报告 JSON
		// 序列化失败（Go json 拒绝 NaN）——解析层直接丢弃
		if err != nil || name == "" || math.IsNaN(val) || math.IsInf(val, 0) {
			continue
		}
		switch {
		case strings.HasSuffix(name, "_bucket") && labels["le"] != "":
			family := strings.TrimSuffix(name, "_bucket")
			h := s.Hists[family]
			if h == nil {
				h = &Hist{}
				s.Hists[family] = h
			}
			le := parseLE(labels["le"])
			h.Buckets = append(h.Buckets, Bucket{LE: le, Count: val})
		case strings.HasSuffix(name, "_sum"):
			family := strings.TrimSuffix(name, "_sum")
			h := s.Hists[family]
			if h == nil {
				h = &Hist{}
				s.Hists[family] = h
			}
			h.Sum += val
		case strings.HasSuffix(name, "_count"):
			family := strings.TrimSuffix(name, "_count")
			h := s.Hists[family]
			if h == nil {
				h = &Hist{}
				s.Hists[family] = h
			}
			h.Count += val
		case strings.HasSuffix(name, "_created"):
			// Prometheus counter 伴生时间线，忽略
		default:
			// counter（_total 后缀）与 gauge 统一进 Counters/Gauges；
			// 文本格式无法严格区分语义，按调用方需要取用
			norm := strings.TrimSuffix(name, "_total")
			s.Counters[norm] += val
			s.Gauges[name] = val
		}
	}
	for _, h := range s.Hists {
		sort.Slice(h.Buckets, func(i, j int) bool { return h.Buckets[i].LE < h.Buckets[j].LE })
	}
	return s
}

// splitMetricLine 拆出指标名、label 映射与值部分。
func splitMetricLine(line string) (name string, labels map[string]string, value string) {
	labels = map[string]string{}
	sp := strings.IndexAny(line, " \t")
	if sp < 0 {
		return line, labels, ""
	}
	value = strings.TrimSpace(line[sp+1:])
	head := line[:sp]
	if i := strings.Index(head, "{"); i >= 0 && strings.HasSuffix(head, "}") {
		name = head[:i]
		for _, kv := range splitLabels(head[i+1 : len(head)-1]) {
			if eq := strings.Index(kv, "="); eq > 0 {
				k := strings.TrimSpace(kv[:eq])
				v := strings.Trim(strings.TrimSpace(kv[eq+1:]), `"`)
				labels[k] = v
			}
		}
	} else {
		name = head
	}
	return name, labels, value
}

// splitLabels 按逗号切 label 串（不处理值内嵌逗号的罕见转义——够用于引擎指标）。
func splitLabels(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	depth := 0
	start := 0
	for i, r := range s {
		switch r {
		case '"':
			depth = 1 - depth
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

func parseLE(s string) float64 {
	if s == "+Inf" {
		return inf()
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// MetricsProvider 抽象不同推理引擎的 /metrics 指标命名。
// 语义键固定（prefix_cache_hits / preemptions / running / waiting / kv_usage 等），
// 每个引擎提供候选名列表（按序匹配，兼容同引擎多版本）。probe 识别引擎或
// DetectProvider 按指标名前缀自动选择；无法识别回落 vLLM。
type MetricsProvider interface {
	Name() string
	CounterNames() map[string][]string // 语义键 → 候选指标名（counter 的 _total 后缀已由 Parse 剥离）
	GaugeNames() map[string][]string   // 语义键 → 候选 gauge 名
	HistNames() []string               // 关心的延迟分解直方图 family 名（单位秒）
}

// ── vLLM 原生命名（默认） ──

type vllmProvider struct{}

func (vllmProvider) Name() string { return "vllm" }

func (vllmProvider) CounterNames() map[string][]string {
	return map[string][]string{
		"prefix_cache_hits":    {"vllm:prefix_cache_hits", "vllm:prefix_cache_hits_total"},
		"prefix_cache_queries": {"vllm:prefix_cache_queries", "vllm:prefix_cache_queries_total"},
		"preemptions":          {"vllm:num_preemptions", "vllm:num_preemptions_total"},
		"spec_drafts":          {"vllm:spec_decode_num_drafts", "vllm:spec_decode_num_drafts_total"},
		"spec_accepted":        {"vllm:spec_decode_num_accepted_tokens", "vllm:spec_decode_num_accepted_tokens_total"},
	}
}

func (vllmProvider) GaugeNames() map[string][]string {
	return map[string][]string{
		"running":  {"vllm:num_requests_running"},
		"waiting":  {"vllm:num_requests_waiting"},
		"kv_usage": {"vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc"},
	}
}

func (vllmProvider) HistNames() []string {
	return []string{
		"vllm:request_queue_time_seconds",
		"vllm:request_prefill_time_seconds",
		"vllm:request_decode_time_seconds",
		"vllm:time_to_first_token_seconds",
		"vllm:inter_token_latency_seconds",
		"vllm:e2e_request_latency_seconds",
		"vllm:request_prefill_kv_computed_tokens",
	}
}

func VLLM() MetricsProvider { return vllmProvider{} }

// ── SGLang 命名（草案：排队 gauge 已按公开文档实现；缓存 counter 暂缺——
// 接真机时按其 /metrics 校准补充，缺失键即不出数不影响其余指标） ──

type sglangProvider struct{}

func (sglangProvider) Name() string { return "sglang" }

func (sglangProvider) CounterNames() map[string][]string { return map[string][]string{} }

func (sglangProvider) GaugeNames() map[string][]string {
	return map[string][]string{
		"running":  {"sglang:num_running_reqs"},
		"waiting":  {"sglang:num_queue_reqs"},
		"kv_usage": {"sglang:token_usage"},
	}
}

func (sglangProvider) HistNames() []string { return nil }

func SGLang() MetricsProvider { return sglangProvider{} }

// DetectProvider 按抓取样本里的指标名前缀识别引擎命名；无法识别回落 vLLM。
// 需要区分"识别到"与"回落"时用 DetectProviderName（"" = 未识别）。
func DetectProvider(sample *Sample) MetricsProvider {
	switch DetectProviderName(sample) {
	case "sglang":
		return SGLang()
	default:
		return VLLM()
	}
}

// DetectProviderName 返回识别出的命名族名（"vllm" / "sglang"），未识别返回 ""。
// 自研引擎的指标名不带 vllm:/sglang: 前缀——之前静默回落 vLLM 会"套错命名还无告警"，
// 调用方应显式提示（指标大概率拿不到数，属预期而非 bug）。
func DetectProviderName(sample *Sample) string {
	if sample == nil {
		return "" // 未识别（与文档口径一致；调用方负责显式回落并告警）
	}
	has := func(prefix string) bool {
		for k := range sample.Counters {
			if strings.HasPrefix(k, prefix) {
				return true
			}
		}
		for k := range sample.Gauges {
			if strings.HasPrefix(k, prefix) {
				return true
			}
		}
		for k := range sample.Hists {
			if strings.HasPrefix(k, prefix) {
				return true
			}
		}
		return false
	}
	if has("sglang:") && !has("vllm:") {
		return "sglang"
	}
	if has("vllm:") {
		return "vllm"
	}
	return ""
}

// CounterDelta 是两次快照之间关心的 counter 增量（tokens / 次）。
type CounterDelta struct {
	PrefixCacheHitTokens   float64 `json:"cache_hit_tokens,omitempty"`
	PrefixCacheQueryTokens float64 `json:"cache_query_tokens,omitempty"`
	Preemptions            float64 `json:"preemptions,omitempty"`
	SpecDrafts             float64 `json:"spec_drafts,omitempty"`
	SpecAcceptedTokens     float64 `json:"spec_accepted_tokens,omitempty"`
}

// CacheHitRate 返回窗口内前缀缓存 token 命中率（无查询时返回 0）。
func (d *CounterDelta) CacheHitRate() float64 {
	if d == nil || d.PrefixCacheQueryTokens <= 0 {
		return 0
	}
	return d.PrefixCacheHitTokens / d.PrefixCacheQueryTokens
}

// DiffCounters 计算 before→after 的 counter 增量（p 为 nil 时回落 vLLM 命名）。
func DiffCounters(before, after *Sample, p MetricsProvider) *CounterDelta {
	if p == nil {
		p = VLLM()
	}
	names := p.CounterNames()
	get := func(s *Sample, key string) float64 {
		if s == nil {
			return 0
		}
		for _, n := range names[key] {
			if v, ok := s.Counters[n]; ok {
				return v
			}
		}
		return 0
	}
	d := &CounterDelta{}
	for key, dst := range map[string]*float64{
		"prefix_cache_hits":    &d.PrefixCacheHitTokens,
		"prefix_cache_queries": &d.PrefixCacheQueryTokens,
		"preemptions":          &d.Preemptions,
		"spec_drafts":          &d.SpecDrafts,
		"spec_accepted":        &d.SpecAcceptedTokens,
	} {
		delta := get(after, key) - get(before, key)
		if delta < 0 {
			// counter 单调递增；负增量只会来自服务端重启归零/进程替换——钳 0，
			// 防止负命中率/负 preemptions 出现在报告里（该窗口的其他指标同样不可信）
			delta = 0
		}
		*dst = delta
	}
	return d
}

// HistDelta 是 histogram 窗口差值与分位估计。
type HistDelta struct {
	Count float64 `json:"count"`          // 窗口内落入的观测数
	Sum   float64 `json:"sum"`            // 单位随原指标（延迟类为秒）
	P50   float64 `json:"p50,omitempty"`  // 桶边界估算
	P99   float64 `json:"p99,omitempty"`  // 桶边界估算
	Mean  float64 `json:"mean,omitempty"` // Sum/Count
}

// HistDeltas 计算 HistNames 里各直方图的窗口差值分位估计。
// 并发窗口内直方图混入其他流量的观测属已知近似（AIPerf 同口径）。
// p 为 nil 时回落 vLLM 命名。
func HistDeltas(before, after *Sample, p MetricsProvider) map[string]HistDelta {
	if p == nil {
		p = VLLM()
	}
	out := map[string]HistDelta{}
	for _, name := range p.HistNames() {
		hb, ha := before.Hists[name], after.Hists[name]
		if hb == nil && ha == nil {
			continue
		}
		d := HistDelta{}
		if ha != nil {
			d.Count = ha.Count
			d.Sum = ha.Sum
		}
		if hb != nil {
			d.Count -= hb.Count
			d.Sum -= hb.Sum
		}
		if d.Count <= 0 {
			continue
		}
		d.Mean = d.Sum / d.Count
		d.P50 = histQuantile(hb, ha, 0.50)
		d.P99 = histQuantile(hb, ha, 0.99)
		out[name] = d
	}
	return out
}

// histQuantile 从桶边界估算分位。Prometheus histogram 的桶为累计计数：
// 总观测数取最后一个桶（+Inf）的计数差，累计到 p*total 时的桶上界即分位估计。
func histQuantile(before, after *Hist, p float64) float64 {
	if after == nil || len(after.Buckets) == 0 {
		return 0
	}
	beforeByLE := map[float64]float64{}
	if before != nil {
		for _, b := range before.Buckets {
			beforeByLE[b.LE] = b.Count
		}
	}
	last := after.Buckets[len(after.Buckets)-1]
	total := last.Count - beforeByLE[last.LE]
	if total <= 0 {
		return 0
	}
	target := p * total
	for _, b := range after.Buckets {
		if b.Count-beforeByLE[b.LE] >= target && !isInf(b.LE) {
			return b.LE
		}
	}
	return 0 // 只剩 +Inf 桶：无法估计
}

func isInf(f float64) bool { return math.IsInf(f, 1) }

func inf() float64 { return math.Inf(1) }

// GaugeSummary 是 gauge 轮询的聚合。
type GaugeSummary struct {
	Max     float64 `json:"max"`
	Avg     float64 `json:"avg"`
	Samples int     `json:"samples"`
}

// GaugePoller 周期抓取 gauge（排队深度、KV 占用），Stop 后可用 Summary 取聚合。
// Health 暴露观测健康度：连续失败达到阈值时上层应在报告里标注"观测降级"。
type GaugePoller struct {
	scraper  *Scraper
	interval time.Duration
	ctx      context.Context
	provider MetricsProvider

	mu          sync.Mutex
	samples     map[string][]float64
	okSamples   int    // 成功抓取次数
	totalFails  int    // 累计失败次数
	consecFails int    // 连续失败次数
	lastErr     string // 最后一次失败原因
	stopOnce    sync.Once
	done        chan struct{} // Stop 关闭：通知 loop 退出
	stopped     chan struct{} // loop 退出时关闭：外部可等待
}

// GaugeHealth 是轮询健康度汇总。
type GaugeHealth struct {
	Samples             int    `json:"samples"` // 成功抓取次数
	TotalFailures       int    `json:"total_failures"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastError           string `json:"last_error,omitempty"`
}

// Degraded 判定观测是否降级：从未成功，或连续失败达到阈值（窗口内基本无有效数据）。
func (h GaugeHealth) Degraded() bool {
	return h.Samples == 0 || h.ConsecutiveFailures >= 5
}

// StartGaugePoller 启动后台轮询（首次立即抓一次）。sc 为已构造好的 Scraper
// （调用方用 NewScraperAt 指定 metrics 路径），p 为指标命名提供者（nil 回落 vLLM）。
func StartGaugePoller(ctx context.Context, sc *Scraper, interval time.Duration, p MetricsProvider) *GaugePoller {
	if p == nil {
		p = VLLM()
	}
	sc.MaxRetries = 0 // 轮询快速失败：失败计数即降级信号，下一 tick 天然是重试
	g := &GaugePoller{
		scraper:  sc,
		interval: interval,
		ctx:      ctx,
		provider: p,
		samples:  map[string][]float64{},
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	go g.loop()
	return g
}

func (g *GaugePoller) loop() {
	defer close(g.stopped)
	t := time.NewTicker(g.interval)
	defer t.Stop()
	g.once()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-g.done:
			return
		case <-t.C:
			g.once()
		}
	}
}

func (g *GaugePoller) once() {
	sample, err := g.scraper.Scrape(g.ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		// 服务端瞬时不可达不致命，但必须留痕——观测降级要在报告里可见
		g.totalFails++
		g.consecFails++
		g.lastErr = err.Error()
		return
	}
	g.okSamples++
	g.consecFails = 0
	names := g.provider.GaugeNames()
	for key, cands := range names {
		for _, n := range cands {
			if v, ok := sample.Counters[n]; ok {
				g.samples[key] = append(g.samples[key], v)
				break
			}
		}
	}
}

// Health 返回轮询健康度（调用后轮询继续，可随时读取）。
func (g *GaugePoller) Health() GaugeHealth {
	g.mu.Lock()
	defer g.mu.Unlock()
	return GaugeHealth{
		Samples:             g.okSamples,
		TotalFailures:       g.totalFails,
		ConsecutiveFailures: g.consecFails,
		LastError:           g.lastErr,
	}
}

// Stop 停止轮询（幂等；不等待 loop 退出，Summary 会先 Stop 再读数据）。
func (g *GaugePoller) Stop() { g.stopOnce.Do(func() { close(g.done) }) }

// Summary 返回各 gauge 的峰值/均值。
func (g *GaugePoller) Summary() map[string]GaugeSummary {
	g.Stop()
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]GaugeSummary{}
	for k, xs := range g.samples {
		if len(xs) == 0 {
			continue
		}
		max, sum := xs[0], 0.0
		for _, v := range xs {
			if v > max {
				max = v
			}
			sum += v
		}
		out[k] = GaugeSummary{Max: max, Avg: sum / float64(len(xs)), Samples: len(xs)}
	}
	return out
}
