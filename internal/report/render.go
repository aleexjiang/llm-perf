package report

import (
	"embed"
	"fmt"
	"html"
	"strings"
)

//go:embed assets/style.css
var assets embed.FS

// series 一条折线。
type series struct {
	Name  string
	Color string
	Points [][2]float64 // [x, y]
}

// lineSVG 渲染一张极简 SVG 折线图。
func lineSVG(title, xLabel, yLabel string, seriesList []series) string {
	w, h := 640, 300
	padL, padR, padT, padB := 60, 20, 30, 45
	iw, ih := w-padL-padR, h-padT-padB

	minX, maxX, minY, maxY := 1e18, -1e18, 1e18, -1e18
	for _, s := range seriesList {
		for _, p := range s.Points {
			minX = minF(minX, p[0])
			maxX = maxF(maxX, p[0])
			minY = minF(minY, p[1])
			maxY = maxF(maxY, p[1])
		}
	}
	if minX == 1e18 {
		return ""
	}
	if maxX == minX {
		maxX = minX + 1
	}
	if maxY == minY {
		maxY = minY + 1
	}
	// y 轴从 0 起（延迟/吞吐类指标更直观）
	minY = 0

	sx := func(x float64) float64 { return float64(padL) + (x-minX)/(maxX-minX)*float64(iw) }
	sy := func(y float64) float64 { return float64(padT) + float64(ih) - (y-minY)/(maxY-minY)*float64(ih) }

	var b strings.Builder
	b.WriteString(fmt.Sprintf(`<svg viewBox="0 0 %d %d" xmlns="http://www.w3.org/2000/svg" class="chart">`, w, h))
	b.WriteString(fmt.Sprintf(`<text x="%d" y="18" class="c-title">%s</text>`, padL, html.EscapeString(title)))
	// 网格 + y 轴刻度
	for i := 0; i <= 4; i++ {
		yv := minY + (maxY-minY)*float64(i)/4
		y := sy(yv)
		b.WriteString(fmt.Sprintf(`<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" class="c-grid"/>`, padL, y, padL+iw, y))
		b.WriteString(fmt.Sprintf(`<text x="%d" y="%.1f" class="c-tick" text-anchor="end">%.0f</text>`, padL-8, y+4, yv))
	}
	// x 轴刻度（取第一条 series 的 x 值）
	if len(seriesList) > 0 && len(seriesList[0].Points) > 0 {
		pts := seriesList[0].Points
		step := 1
		if len(pts) > 6 {
			step = (len(pts) + 5) / 6
		}
		for i := 0; i < len(pts); i += step {
			b.WriteString(fmt.Sprintf(`<text x="%.1f" y="%d" class="c-tick" text-anchor="middle">%.0f</text>`, sx(pts[i][0]), h-padB+18, pts[i][0]))
		}
	}
	for _, s := range seriesList {
		var pts strings.Builder
		for i, p := range s.Points {
			if i > 0 {
				pts.WriteString(" ")
			}
			pts.WriteString(fmt.Sprintf("%.1f,%.1f", sx(p[0]), sy(p[1])))
		}
		color := s.Color
		b.WriteString(fmt.Sprintf(`<polyline points="%s" fill="none" stroke="%s" stroke-width="2"/>`, pts.String(), color))
		b.WriteString(fmt.Sprintf(`<circle cx="%.1f" cy="%.1f" r="3" fill="%s"/>`, sx(s.Points[0][0]), sy(s.Points[0][1]), color))
		last := s.Points[len(s.Points)-1]
		b.WriteString(fmt.Sprintf(`<circle cx="%.1f" cy="%.1f" r="3" fill="%s"/>`, sx(last[0]), sy(last[1]), color))
	}
	// 图例
	lx := padL
	for _, s := range seriesList {
		b.WriteString(fmt.Sprintf(`<rect x="%d" y="%d" width="12" height="3" fill="%s"/><text x="%d" y="%d" class="c-legend">%s</text>`, lx, h-10, s.Color, lx+16, h-5, html.EscapeString(s.Name)))
		lx += 16 + 7*len([]rune(s.Name)) + 20
	}
	b.WriteString(fmt.Sprintf(`<text x="%d" y="%d" class="c-axis" text-anchor="middle">%s</text>`, padL+iw/2, h-28, html.EscapeString(xLabel)))
	b.WriteString(`</svg>`)
	return b.String()
}

func minF(a, b float64) float64 { if a < b { return a }; return b }
func maxF(a, b float64) float64 { if a > b { return a }; return b }

var chartColors = []string{"#4a7ddb", "#d4713d", "#3d9a6c", "#8a5fd1", "#c2477c"}

// RenderHTML 渲染整个报告为单文件 HTML。
func (r *Report) RenderHTML() (string, error) {
	css, err := assets.ReadFile("assets/style.css")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="zh"><head><meta charset="utf-8"><title>llm-perf `)
	b.WriteString(html.EscapeString(r.Scenario))
	b.WriteString(`</title><style>`)
	b.Write(css)
	b.WriteString(`</style></head><body>`)
	b.WriteString(fmt.Sprintf(`<h1>llm-perf · %s</h1>`, html.EscapeString(r.Scenario)))
	b.WriteString(fmt.Sprintf(`<p class="meta">endpoint: %s &nbsp;|&nbsp; 生成时间: %s</p>`, html.EscapeString(r.Endpoint), r.GeneratedAt.Format("2006-01-02 15:04:05")))
	if r.Note != "" {
		b.WriteString(fmt.Sprintf(`<p class="note">%s</p>`, html.EscapeString(r.Note)))
	}

	switch r.Scenario {
	case "single":
		b.WriteString(renderSingle(r))
	case "multiturn":
		b.WriteString(renderMultiturn(r))
	case "concurrent":
		b.WriteString(renderConcurrent(r))
	}
	b.WriteString(`</body></html>`)
	return b.String(), nil
}

func table(headers []string, rows [][]string) string {
	var b strings.Builder
	b.WriteString(`<table><thead><tr>`)
	for _, h := range headers {
		b.WriteString(`<th>` + h + `</th>`)
	}
	b.WriteString(`</tr></thead><tbody>`)
	for _, row := range rows {
		b.WriteString(`<tr>`)
		for _, c := range row {
			b.WriteString(`<td>` + c + `</td>`)
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table>`)
	return b.String()
}

func f1(v float64) string { return fmt.Sprintf("%.1f", v) }

func renderSingle(r *Report) string {
	var b strings.Builder
	// 汇总表
	var rows [][]string
	models := map[string]bool{}
	var modelOrder []string
	for _, row := range r.Single {
		if !models[row.Model] {
			models[row.Model] = true
			modelOrder = append(modelOrder, row.Model)
		}
	}
	for _, row := range r.Single {
		ttfts := []float64{}
		think := []float64{}
		tps := []float64{}
		for _, m := range row.Runs {
			if m.Error == "" {
				ttfts = append(ttfts, m.TTFT)
				think = append(think, m.ThinkMS)
				tps = append(tps, m.TokensPerSec)
			}
		}
		runCells := make([]string, 0, len(row.Runs))
		for _, m := range row.Runs {
			if m.Error != "" {
				runCells = append(runCells, `<span class="err">ERR</span>`)
			} else {
				runCells = append(runCells, f1(m.TTFT))
			}
		}
		rows = append(rows, []string{
			html.EscapeString(shortModel(row.Model)),
			fmt.Sprintf("%d", row.PromptTokens),
			strings.Join(runCells, " / "),
			f1(avgOf(ttfts)),
			f1(maxOf(think)),
			f1(avgOf(tps)),
			fmt.Sprintf("%d", row.Runs[0].PromptTokens),
		})
	}
	b.WriteString(`<h2>汇总（TTFT 单位 ms；思考取各 run 最大值；tokens/s 为 decode 速率）</h2>`)
	b.WriteString(table([]string{"模型", "prompt tokens", "各 run TTFT", "TTFT avg", "思考时长", "decode tok/s", "服务端实际 prompt_tokens"}, rows))

	// TTFT 曲线：每条模型一条线
	var seriesList []series
	for i, model := range modelOrder {
		var pts [][2]float64
		var sums []float64
		counts := map[int]int{} // 会出现多个 row 同 tokens 的情况按 tokens 聚合
		sumsByTok := map[int]float64{}
		for _, row := range r.Single {
			if row.Model != model {
				continue
			}
			for _, m := range row.Runs {
				if m.Error == "" {
					sumsByTok[row.PromptTokens] += m.TTFT
					counts[row.PromptTokens]++
				}
			}
		}
		for tok, s := range sumsByTok {
			pts = append(pts, [2]float64{float64(tok), s / float64(counts[tok])})
		}
		sortPts(pts)
		if len(pts) > 0 {
			seriesList = append(seriesList, series{Name: shortModel(model), Color: chartColors[i%len(chartColors)], Points: pts})
			_ = sums
		}
	}
	if svg := lineSVG("TTFT vs prompt tokens (avg)", "prompt tokens", "TTFT ms", seriesList); svg != "" {
		b.WriteString(`<h2>TTFT 随上下文增长曲线</h2>` + svg)
	}
	return b.String()
}

func renderMultiturn(r *Report) string {
	var b strings.Builder
	var rows [][]string
	var seriesList []series
	colorIdx := 0
	for _, run := range r.Multiturn {
		var ctxGrowth [][2]float64
		for i, m := range run.Turns {
			turn := i + 1
			rows = append(rows, []string{
				html.EscapeString(shortModel(run.Model)),
				fmt.Sprintf("session %d · turn %d", run.Session, turn),
				f1(m.TTFT), f1(m.TTFTReasoning), f1(m.TTFTContent), f1(m.ThinkMS),
				fmt.Sprintf("%d", m.PromptTokens),
			})
			if m.Error == "" {
				ctxGrowth = append(ctxGrowth, [2]float64{float64(turn), m.TTFT})
			}
		}
		if len(ctxGrowth) > 0 {
			seriesList = append(seriesList, series{
				Name:   fmt.Sprintf("%s s%d", shortModel(run.Model), run.Session),
				Color:  chartColors[colorIdx%len(chartColors)],
				Points: ctxGrowth,
			})
			colorIdx++
		}
	}
	b.WriteString(`<h2>每轮明细（ms）</h2>`)
	b.WriteString(table([]string{"模型", "轮次", "TTFT", "TTFT reasoning", "TTFT content", "思考时长", "服务端 prompt_tokens"}, rows))
	if svg := lineSVG("TTFT vs turn（若前缀缓存命中，后续 turn 增长应远小于线性叠加）", "turn", "TTFT ms", seriesList); svg != "" {
		b.WriteString(`<h2>TTFT 随轮次变化</h2>` + svg)
	}
	b.WriteString(`<p class="note">判定要点：turn N 的 TTFT ≈ turn N-1 增量 + 新增 token 的 prefill 时间 ⇒ 缓存命中；若每轮都接近"全量 prefill"则未命中。</p>`)
	return b.String()
}

func renderConcurrent(r *Report) string {
	var b strings.Builder
	var rows [][]string
	models := map[string]bool{}
	var modelOrder []string
	for _, lv := range r.Concurrent {
		if !models[lv.Model] {
			models[lv.Model] = true
			modelOrder = append(modelOrder, lv.Model)
		}
	}
	ttftSeries := map[string][][2]float64{}
	tpsSeries := map[string][][2]float64{}
	for _, lv := range r.Concurrent {
		var ttfts []float64
		total := 0.0
		for _, m := range lv.Requests {
			if m.Error == "" {
				ttfts = append(ttfts, m.TTFT)
				total += float64(m.CompletionTokens)
			}
		}
		rows = append(rows, []string{
			html.EscapeString(shortModel(lv.Model)),
			fmt.Sprintf("%d", lv.Level),
			fmt.Sprintf("%d", len(ttfts)),
			f1(percentileOf(ttfts, 50)),
			f1(percentileOf(ttfts, 95)),
			f1(maxOf(ttfts)),
			f1(lv.ThroughputTPS),
			f1(lv.WallSeconds),
		})
		ttftSeries[lv.Model] = append(ttftSeries[lv.Model], [2]float64{float64(lv.Level), percentileOf(ttfts, 50)})
		tpsSeries[lv.Model] = append(tpsSeries[lv.Model], [2]float64{float64(lv.Level), lv.ThroughputTPS})
	}
	b.WriteString(`<h2>各并发档位汇总</h2>`)
	b.WriteString(table([]string{"模型", "并发", "成功请求数", "TTFT p50", "TTFT p95", "TTFT max", "整体吞吐 tok/s", "墙钟时间 s"}, rows))

	colorIdx := 0
	var s1, s2 []series
	for _, model := range modelOrder {
		if pts := ttftSeries[model]; len(pts) > 0 {
			s1 = append(s1, series{Name: shortModel(model), Color: chartColors[colorIdx%len(chartColors)], Points: sortPts(pts)})
		}
		if pts := tpsSeries[model]; len(pts) > 0 {
			s2 = append(s2, series{Name: shortModel(model), Color: chartColors[colorIdx%len(chartColors)], Points: sortPts(pts)})
		}
		colorIdx++
	}
	if svg := lineSVG("TTFT p50 vs 并发数", "并发数", "TTFT ms", s1); svg != "" {
		b.WriteString(`<h2>TTFT 衰减曲线</h2>` + svg)
	}
	if svg := lineSVG("整体吞吐 vs 并发数", "并发数", "tokens/s", s2); svg != "" {
		b.WriteString(`<h2>吞吐曲线</h2>` + svg)
	}
	return b.String()
}

func sortPts(pts [][2]float64) [][2]float64 {
	for i := 1; i < len(pts); i++ {
		for j := i; j > 0 && pts[j][0] < pts[j-1][0]; j-- {
			pts[j], pts[j-1] = pts[j-1], pts[j]
		}
	}
	return pts
}

func shortModel(m string) string {
	if i := strings.LastIndex(m, "/"); i >= 0 {
		return m[i+1:]
	}
	return m
}

func avgOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func maxOf(xs []float64) float64 {
	m := 0.0
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}

func percentileOf(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	idx := int(p / 100 * float64(len(s)-1))
	return s[idx]
}
