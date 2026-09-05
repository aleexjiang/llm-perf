#!/usr/bin/env python3
"""llm-perf JSON → 自包含 HTML 分析报告。

用法: python3 gen_html_report.py <output目录> [报告标题]
读取目录下最新的 single-*.json / multiturn-*.json / concurrent-*.json，
输出 llm-perf-报告.html 到同目录。
"""
import json, glob, sys, os, html

VENDOR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "vendor", "chart.umd.min.js")

def load(d):
    out = {}
    for k in ("single", "multiturn", "concurrent"):
        fs = sorted(glob.glob(os.path.join(d, k + "-*.json")))
        if fs:
            out[k] = json.load(open(fs[-1]))  # 每类取最新一份
    return out

def fmt_ms(x):
    if x is None:
        return "—"
    return "{:,.0f} ms".format(x) if x >= 100 else "{:.1f} ms".format(x)

def main():
    d = sys.argv[1] if len(sys.argv) > 1 else "output-final"
    title = sys.argv[2] if len(sys.argv) > 2 else "llm-perf 真机评测报告"
    data = load(d)
    meta = {}
    for k in data:
        meta = {"endpoint": data[k].get("endpoint"), "tool": data[k].get("tool", "?")}
        break

    single = data.get("single", {}).get("single", [])
    multiturn = data.get("multiturn", {}).get("multiturn", [])
    conc = data.get("concurrent", {}).get("concurrent", [])

    off_rows = [r for r in single if r["thinking"] == "off"]
    on_rows = [r for r in single if r["thinking"] == "on"]

    # 图1: prefill 曲线（off 变体 run1 vs run2）
    off_tokens = [r["prompt_tokens"] for r in off_rows]
    run1 = [round(r["runs"][0].get("ttft_ms") or 0) for r in off_rows]
    run2 = [round(r["runs"][1].get("ttft_ms") or 0) if len(r["runs"]) > 1 else None for r in off_rows]
    prefill_tps = []
    for r in off_rows:
        runs = r["runs"]
        if len(runs) >= 2:
            dtt = (runs[1].get("ttft_ms") or 0) - (runs[0].get("ttft_ms") or 0)
            if dtt > 0:
                prefill_tps.append(r["prompt_tokens"] * 2 / (dtt / 1000))

    # 思考 A/B
    on_runs = [run for r in on_rows for run in r["runs"]]
    on_valid = [run for run in on_runs if not run.get("thinking_no_content")]
    think_vals = [run["think_ms"] for run in on_valid if run.get("think_ms")]
    decode_on = [run.get("tokens_per_sec") or 0 for run in on_valid]
    decode_off = [run.get("tokens_per_sec") or 0 for r in off_rows for run in r["runs"]]
    trunc = len(on_runs) - len(on_valid)

    # multiturn 逐轮
    mt_rows = []
    for s in multiturn:
        for i, t in enumerate(s["turns"], 1):
            mt_rows.append({"turn": i, "thinking": s["thinking"], "ctx": t.get("prompt_tokens", 0),
                            "ttft": t.get("ttft_ms"), "think": t.get("think_ms"), "finish": t.get("finish_reason")})

    # concurrent
    conc_rows = []
    for lv in conc:
        reqs = list(lv.get("requests", []))
        for s in lv.get("sessions", []):
            reqs.extend(s["turns"])
        ttfts = [r.get("ttft_ms") or 0 for r in reqs]
        conc_rows.append({"level": lv["level"], "thinking": lv["thinking"],
                          "throughput": lv.get("throughput_tps", 0), "wall": lv.get("wall_seconds", 0),
                          "ttft_avg": sum(ttfts) / len(ttfts) if ttfts else 0,
                          "ttft_max": max(ttfts) if ttfts else 0, "n": len(reqs)})
    by_level = {}
    for row in conc_rows:
        by_level.setdefault(row["level"], {})[row["thinking"]] = row

    # 数据质量
    all_runs = [run for r in single for run in r["runs"]]
    n_warn = sum(1 for run in all_runs if run.get("warnings"))
    warn_kinds = sorted({w.split(":")[0].split("×")[0].strip() for run in all_runs for w in run.get("warnings", [])})
    quality = "<p>{} 条单发请求中 {} 条带兼容性告警".format(len(all_runs), n_warn)
    if warn_kinds:
        quality += "（" + "、".join(warn_kinds) + "）"
    quality += "。思考截断（thinking_no_content）{} 条，思考统计已剔除。工具版本 {}。</p>".format(trunc, html.escape(str(meta.get("tool"))))

    # 结论判定（run2 显著低于 run1 = 缓存命中；冷启动边际 prefill 速率用最大两档冷数据推算）
    hits = []
    for r in off_rows:
        if len(r["runs"]) >= 2:
            t1, t2 = r["runs"][0].get("ttft_ms") or 0, r["runs"][1].get("ttft_ms") or 0
            if t1 > 0 and t2 < t1:
                hits.append((r["prompt_tokens"], (1 - t2 / t1) * 100))
    marginal = None
    if len(off_rows) >= 2:
        big, small = off_rows[-1]["runs"][0].get("ttft_ms") or 0, off_rows[0]["runs"][0].get("ttft_ms") or 0
        dtok = off_rows[-1]["prompt_tokens"] - off_rows[0]["prompt_tokens"]
        if big > small and dtok > 0:
            marginal = dtok / ((big - small) / 1000)
    if hits:
        best = max(hits, key=lambda x: x[1])
        cache_verdict = "{} 个档位出现前缀缓存命中（{}tk 档收益最高，TTFT 节省 {:.0f}%）".format(len(hits), best[0], best[1])
        if marginal:
            cache_verdict += "；冷启动边际 prefill 速率约 {:.0f} tok/s".format(marginal)
        cache_verdict += "。"
    else:
        cache_verdict = "缓存命中不显著，两轮 TTFT 接近。"
    mt_off = [m for m in mt_rows if m["thinking"] == "off" and m["ttft"]]
    if len(mt_off) >= 2:
        growth = mt_off[-1]["ttft"] - mt_off[0]["ttft"]
        add_ctx = mt_off[-1]["ctx"] - mt_off[0]["ctx"]
        mt_verdict = ("turn1→末轮 TTFT 仅增长 {:.0f} ms（上下文 +{:,} tk），远低于全量 prefill 成本 ⇒ 前缀缓存跨 turn 复用良好。".format(growth, add_ctx)
                      if growth < 800 else
                      "turn1→末轮 TTFT 增长 {:.0f} ms，接近全量 prefill ⇒ 缓存跨 turn 复用有限。".format(growth))
    else:
        mt_verdict = "多轮有效样本不足。"

    def table(headers, rows):
        h = "".join("<th>{}</th>".format(x) for x in headers)
        body = "".join("<tr>" + "".join("<td>{}</td>".format(c) for c in r) + "</tr>" for r in rows)
        return '<table><thead><tr>{}</tr></thead><tbody>{}</tbody></table>'.format(h, body)

    kpi = [
        ("TTFT @1k (off)", fmt_ms(off_rows[0]["runs"][0].get("ttft_ms")) if off_rows else "—"),
        ("TTFT @10k (off)", fmt_ms(off_rows[-1]["runs"][0].get("ttft_ms")) if off_rows else "—"),
        ("平均思考时长", fmt_ms(sum(think_vals) / len(think_vals)) if think_vals else "—"),
        ("content 段 tok/s (on)", "{:.0f}".format(max(decode_on)) if decode_on else "—"),
        ("content 段 tok/s (off)", "{:.0f}".format(sum(decode_off) / len(decode_off)) if decode_off else "—"),
        ("思考截断请求数", "{} / {}".format(trunc, len(on_runs))),
    ]
    kpi_html = "".join('<div class="kpi"><div class="kpi-v">{}</div><div class="kpi-l">{}</div></div>'.format(v, l) for l, v in kpi)

    s_rows = []
    for r in single:
        for i, run in enumerate(r["runs"], 1):
            s_rows.append([r["thinking"], "{:,}".format(r["prompt_tokens"]), i,
                           fmt_ms(run.get("ttft_ms")), fmt_ms(run.get("ttft_reasoning_ms")),
                           fmt_ms(run.get("ttft_content_ms")), fmt_ms(run.get("think_ms")),
                           fmt_ms(run.get("decode_ms")), "{:.0f}".format(run.get("tokens_per_sec") or 0),
                           run.get("finish_reason", "—"),
                           "⚠️ " + ";".join(run["warnings"]) if run.get("warnings") else ""])

    mt_table = table(["thinking", "turn", "上下文 tk", "TTFT", "思考时长", "finish"],
                     [[m["thinking"], m["turn"], "{:,}".format(m["ctx"]), fmt_ms(m["ttft"]), fmt_ms(m["think"]), m["finish"]] for m in mt_rows])

    c_rows = []
    for lv in sorted(by_level):
        for th in ("off", "on"):
            if th in by_level[lv]:
                r = by_level[lv][th]
                c_rows.append([lv, th, r["n"], "{:,.0f} ms".format(r["ttft_avg"]), "{:,.0f} ms".format(r["ttft_max"]),
                               "{:.0f} tok/s".format(r["throughput"]), "{:.1f} s".format(r["wall"])])
    conc_table = table(["并发", "thinking", "请求数", "TTFT 均值", "TTFT 最大", "整体吞吐", "墙钟"], c_rows) if c_rows else "<p>无并发数据</p>"

    chart_tokens = json.dumps(off_tokens)
    chart_run1 = json.dumps(run1)
    chart_run2 = json.dumps(run2)
    think_sec = json.dumps([round(v / 1000, 1) for v in think_vals])
    mt_labels = json.dumps(["turn{}".format(m["turn"]) for m in mt_rows])
    mt_ttft = json.dumps([round(m["ttft"]) if m["ttft"] else 0 for m in mt_rows])
    mt_ctx = json.dumps([m["ctx"] for m in mt_rows])
    levels = json.dumps(sorted(by_level))
    thr_off = json.dumps([round(by_level[l]["off"]["throughput"]) if "off" in by_level[l] else None for l in sorted(by_level)])
    thr_on = json.dumps([round(by_level[l]["on"]["throughput"]) if "on" in by_level[l] else None for l in sorted(by_level)])

    h = """<!DOCTYPE html><html lang="zh"><head><meta charset="utf-8">
<title>__TITLE__</title>
<script>__CHARTJS_LIB__</script>
<style>
body{font-family:-apple-system,'PingFang SC','Microsoft YaHei',sans-serif;max-width:1080px;margin:24px auto;padding:0 16px;color:#1a1a2e;background:#fafafa}
h1{font-size:26px} h2{margin-top:36px;border-bottom:2px solid #e8e8ef;padding-bottom:6px}
.kpis{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:12px;margin:20px 0}
.kpi{background:#fff;border:1px solid #e8e8ef;border-radius:10px;padding:14px 16px}
.kpi-v{font-size:22px;font-weight:700;color:#1652f0} .kpi-l{font-size:12px;color:#777;margin-top:4px}
table{border-collapse:collapse;width:100%;font-size:13px;background:#fff;border-radius:8px;overflow:hidden}
th,td{border-bottom:1px solid #eee;padding:6px 10px;text-align:left;white-space:nowrap}
th{background:#f2f3f8}
.note{background:#fff8e6;border-left:4px solid #f0b429;padding:10px 14px;border-radius:4px;font-size:14px}
.chart{background:#fff;border:1px solid #e8e8ef;border-radius:10px;padding:16px;margin:14px 0}
.finding{margin:8px 0;padding:10px 14px;background:#fff;border-radius:8px;border:1px solid #e8e8ef}
.finding b{color:#1652f0}
</style></head><body>
<h1>__TITLE__</h1>
<p>端点 <code>__ENDPOINT__</code> · 单发/多轮/并发 × 思考 on/off · 全部数据来自服务端 usage 与逐 chunk 计时</p>
<div class="kpis">__KPI__</div>

<h2>1 · Prefill 扩展性（单发，思考关闭，fixed_seed）</h2>
<div class="chart"><canvas id="c1" height="110"></canvas></div>
<p>TTFT 随上下文规模的增长即 prefill 成本曲线。Run1 为冷缓存，Run2 复用同一 prompt：</p>
<div class="finding"><b>结论：</b>__CACHE_VERDICT__</div>

<h2>2 · 思考模式 A/B（on vs off）</h2>
<div class="note">思考开启时 TTFT content − TTFT reasoning = 思考时长。思考截断 __TRUNC__ 条（thinking_no_content，思考吃光 max_tokens）已从思考统计剔除。</div>
<div class="chart"><canvas id="c2" height="110"></canvas></div>

<h2>3 · 多轮会话（agent 模拟：system + tool defs + 逐轮 history）</h2>
__MT_TABLE__
<div class="chart"><canvas id="c3" height="110"></canvas></div>
<div class="finding"><b>判定：</b>__MT_VERDICT__</div>

<h2>4 · 并发衰减（单轮）</h2>
__CONC_TABLE__
<div class="chart"><canvas id="c4" height="110"></canvas></div>
<div class="finding"><b>口径：</b>服务端 <code>--max-num-seqs 2</code>，并发 1→2 反映真实并行收益，4 及以上主要测排队压力下的 TTFT 衰减。</div>

<h2>5 · 数据质量</h2>
__QUALITY__
<h2>附 · 单发明细</h2>
__S_TABLE__

__CHARTJS__
</body></html>"""

    chartjs = """
<script>
const grid={ticks:{color:'#999'},grid:{color:'#f0f0f5'}};
new Chart(c1,{type:'line',data:{labels:__TOKENS__,datasets:[
{label:'Run1 (冷缓存)',data:__RUN1__,borderColor:'#1652f0',tension:.3},
{label:'Run2 (缓存命中)',data:__RUN2__,borderColor:'#e5484d',borderDash:[6,4],tension:.3}]},
options:{plugins:{title:{display:true,text:'TTFT vs 上下文档位 (thinking=off, ms)'}},scales:{x:{title:{display:true,text:'prompt tokens'},...grid},y:{...grid,title:{display:true,text:'ms'}}}}});
new Chart(c2,{type:'bar',data:{labels:__THINK_SEC__.map((_,i)=>'有效样本'+(i+1)),datasets:[
{label:'思考时长 (s)',data:__THINK_SEC__,backgroundColor:'#7c5cff'}]},
options:{plugins:{title:{display:true,text:'思考模式下单请求思考时长'}},scales:{x:{...grid},y:{...grid,title:{display:true,text:'s'}}}}});
new Chart(c3,{type:'line',data:{labels:__MT_LABELS__,datasets:[
{label:'逐轮 TTFT (ms)',data:__MT_TTFT__,borderColor:'#0e9f6e',tension:.3,yAxisID:'y'},
{label:'上下文 (tk)',data:__MT_CTX__,borderColor:'#f0b429',borderDash:[6,4],tension:.3,yAxisID:'y1'}]},
options:{plugins:{title:{display:true,text:'多轮逐轮 TTFT 与上下文增长'}},scales:{x:{...grid},y:{...grid,position:'left',title:{display:true,text:'ms'}},y1:{position:'right',grid:{display:false},title:{display:true,text:'tokens'}}}}});
new Chart(c4,{type:'bar',data:{labels:__LEVELS__.map(l=>'并发 '+l),datasets:[
{label:'吞吐 tok/s (off)',data:__THR_OFF__,backgroundColor:'#1652f0'},
{label:'吞吐 tok/s (on)',data:__THR_ON__,backgroundColor:'#7c5cff'}]},
options:{plugins:{title:{display:true,text:'并发档位整体吞吐'}},scales:{x:{...grid},y:{...grid,title:{display:true,text:'tok/s'}}}}});
</script>
"""

    for k, v in [("__TOKENS__", chart_tokens), ("__RUN1__", chart_run1), ("__RUN2__", chart_run2),
                 ("__THINK_SEC__", think_sec), ("__MT_LABELS__", mt_labels), ("__MT_TTFT__", mt_ttft),
                 ("__MT_CTX__", mt_ctx), ("__LEVELS__", levels), ("__THR_OFF__", thr_off), ("__THR_ON__", thr_on)]:
        h = h.replace(k, v)
    with open(VENDOR, encoding="utf-8") as f:
        h = h.replace("__CHARTJS_LIB__", f.read().replace("</script>", "<\\/script>"))
    h = h.replace("__CHARTJS__", chartjs)
    h = h.replace("__TITLE__", html.escape(title)).replace("__ENDPOINT__", html.escape(str(meta.get("endpoint"))))
    h = h.replace("__KPI__", kpi_html).replace("__CACHE_VERDICT__", cache_verdict)
    h = h.replace("__MT_VERDICT__", mt_verdict).replace("__MT_TABLE__", mt_table)
    h = h.replace("__CONC_TABLE__", conc_table).replace("__QUALITY__", quality)
    h = h.replace("__S_TABLE__", table(["thinking", "档位 tk", "run", "TTFT", "TTFT reasoning", "TTFT content", "思考", "decode", "tok/s", "finish", "告警"], s_rows))
    h = h.replace("__TRUNC__", str(trunc))

    out = os.path.join(d, "llm-perf-报告.html")
    open(out, "w").write(h)
    print("报告:", out)

if __name__ == "__main__":
    main()
