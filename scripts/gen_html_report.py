#!/usr/bin/env python3
"""llm-perf JSON → 自包含 HTML 分析报告（支持按场景出报告或总览）。

用法: python3 gen_html_report.py <output目录> [报告标题] [scenario]
  scenario: multiturn | concurrent | single | all（默认 all，按目录里实际存在的 JSON 出章节）
输出: <目录>/llm-perf-报告.html（scenario != all 时为 llm-perf-报告-<scenario>.html）
Chart.js 已内嵌（scripts/vendor/），离线可用。
"""
import json, glob, sys, os, html

VENDOR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "vendor", "chart.umd.min.js")

GRID_JS = "{ticks:{color:'#999'},grid:{color:'#f0f0f5'}}"


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


def table(headers, rows):
    h = "".join("<th>{}</th>".format(x) for x in headers)
    body = "".join("<tr>" + "".join("<td>{}</td>".format(c) for c in r) + "</tr>" for r in rows)
    return '<table><thead><tr>{}</tr></thead><tbody>{}</tbody></table>'.format(h, body)


def kpi_grid(items):
    return "".join('<div class="kpi"><div class="kpi-v">{}</div><div class="kpi-l">{}</div></div>'.format(v, l)
                   for l, v in items)


# ── 各场景数据准备，返回 dict：kpi / table / chart / verdict / note ──

def prep_multiturn(mt):
    """mt: multiturn 数组（每元素一个会话）。"""
    rows = []
    for s in mt:
        for i, t in enumerate(s["turns"], 1):
            rows.append({"turn": i, "thinking": s["thinking"], "ctx": t.get("prompt_tokens", 0),
                         "ttft": t.get("ttft_ms"), "think": t.get("think_ms"),
                         "tokps": t.get("tokens_per_sec") or 0, "finish": t.get("finish_reason"),
                         "warn": t.get("warnings", []), "trunc": bool(t.get("thinking_no_content"))})
    valid_think = [r["think"] for r in rows if r["think"] and not r["trunc"]]
    trunc = sum(1 for r in rows if r["trunc"])
    off = [r for r in rows if r["thinking"] == "off" and r["ttft"]]

    verdict = "多轮有效样本不足。"
    if len(off) >= 2:
        growth = off[-1]["ttft"] - off[0]["ttft"]
        add_ctx = off[-1]["ctx"] - off[0]["ctx"]
        if growth > 0 and add_ctx > 0:
            marginal = add_ctx / (growth / 1000)
            if growth < 800:
                verdict = "turn1→末轮 TTFT 仅增长 {:.0f} ms（上下文 +{:,} tk），远低于全量 prefill 成本 ⇒ 前缀缓存跨 turn 复用良好（边际 prefill ≈ {:.0f} tok/s）。".format(growth, add_ctx, marginal)
            else:
                verdict = "turn1→末轮 TTFT 增长 {:.0f} ms（上下文 +{:,} tk，边际 prefill ≈ {:.0f} tok/s），缓存跨 turn 复用有限。".format(growth, add_ctx, marginal)

    kpis = []
    if off:
        kpis.append(("turn1 TTFT (off)", fmt_ms(off[0]["ttft"])))
        kpis.append(("末轮 TTFT (off)", fmt_ms(off[-1]["ttft"])))
        kpis.append(("末轮上下文", "{:,} tk".format(off[-1]["ctx"])))
    kpis.append(("平均思考时长", fmt_ms(sum(valid_think) / len(valid_think)) if valid_think else "—"))
    kpis.append(("思考截断 turn", str(trunc)))

    tbl = table(["thinking", "turn", "上下文 tk", "TTFT", "思考时长", "tok/s", "finish", "告警"],
                [[r["thinking"], r["turn"], "{:,}".format(r["ctx"]), fmt_ms(r["ttft"]), fmt_ms(r["think"]),
                  "{:.0f}".format(r["tokps"]), r["finish"], "⚠️ " + ";".join(r["warn"]) if r["warn"] else ""]
                 for r in rows])

    labels = json.dumps(["{}-turn{}".format(r["thinking"], r["turn"]) for r in rows])
    ttfts = json.dumps([round(r["ttft"]) if r["ttft"] else 0 for r in rows])
    ctxs = json.dumps([r["ctx"] for r in rows])
    chart = """
const grid=__GRID__;
new Chart(c_multiturn,{type:'line',data:{labels:__MT_LABELS__,datasets:[
{label:'逐轮 TTFT (ms)',data:__MT_TTFT__,borderColor:'#0e9f6e',tension:.3,yAxisID:'y'},
{label:'上下文 (tk)',data:__MT_CTX__,borderColor:'#f0b429',borderDash:[6,4],tension:.3,yAxisID:'y1'}]},
options:{plugins:{title:{display:true,text:'多轮逐轮 TTFT 与上下文增长'}},scales:{x:{...grid},y:{...grid,position:'left',title:{display:true,text:'ms'}},y1:{position:'right',grid:{display:false},title:{display:true,text:'tokens'}}}}});
""".replace("__GRID__", GRID_JS).replace("__MT_LABELS__", labels).replace("__MT_TTFT__", ttfts).replace("__MT_CTX__", ctxs)

    return {"id": "multiturn", "title": "多轮会话（agent 模拟：system + tool defs + 逐轮 history）",
            "kpi": kpis, "table": tbl, "chart": chart, "verdict": verdict,
            "note": "每 turn 重新发送完整 messages 数组（system + history + 当前 user），与 agent 行为一致；思考开启的 turn 若思考吃光 max_tokens 会标记 thinking_no_content 并从思考统计剔除。"}


def prep_concurrent(conc):
    """conc: concurrent 数组（每元素一个并发档位 × 思考变体）。"""
    rows = []
    for lv in conc:
        reqs = list(lv.get("requests", []))
        for s in lv.get("sessions", []):
            reqs.extend(s["turns"])
        ttfts = [r.get("ttft_ms") or 0 for r in reqs]
        thinks = [r.get("think_ms") for r in reqs if r.get("think_ms") and not r.get("thinking_no_content")]
        trunc = sum(1 for r in reqs if r.get("thinking_no_content"))
        rows.append({"level": lv["level"], "thinking": lv["thinking"], "multiturn": bool(lv.get("sessions")),
                     "n": len(reqs), "throughput": lv.get("throughput_tps", 0), "wall": lv.get("wall_seconds", 0),
                     "ttft_avg": sum(ttfts) / len(ttfts) if ttfts else 0,
                     "ttft_max": max(ttfts) if ttfts else 0,
                     "think_avg": sum(thinks) / len(thinks) if thinks else None, "trunc": trunc})
    by_level = {}
    for r in rows:
        by_level.setdefault(r["level"], {})[r["thinking"]] = r

    kpis = [("并发档位", ", ".join(str(l) for l in sorted(by_level)))]
    if by_level:
        l1 = by_level[min(by_level)]
        if "off" in l1:
            kpis.append(("L{} 吞吐 (off)".format(min(by_level)), "{:.0f} tok/s".format(l1["off"]["throughput"])))
    if len(by_level) >= 2:
        hi = max(by_level)
        if "off" in by_level[hi]:
            kpis.append(("L{} 吞吐 (off)".format(hi), "{:.0f} tok/s".format(by_level[hi]["off"]["throughput"])))
    on_thinks = [r["think_avg"] for r in rows if r["thinking"] == "on" and r["think_avg"]]
    if on_thinks:
        kpis.append(("平均思考时长 (on)", fmt_ms(sum(on_thinks) / len(on_thinks))))

    c_rows = []
    for lv in sorted(by_level):
        for th in ("off", "on"):
            if th in by_level[lv]:
                r = by_level[lv][th]
                c_rows.append([lv, th, r["n"], "{:,.0f} ms".format(r["ttft_avg"]), "{:,.0f} ms".format(r["ttft_max"]),
                               fmt_ms(r["think_avg"]), "{:.0f} tok/s".format(r["throughput"]), "{:.1f} s".format(r["wall"])])
    tbl = table(["并发", "thinking", "请求数", "TTFT 均值", "TTFT 最大", "思考均值", "整体吞吐", "墙钟"], c_rows)

    levels = json.dumps(sorted(by_level))
    thr_off = json.dumps([round(by_level[l]["off"]["throughput"]) if "off" in by_level[l] else None for l in sorted(by_level)])
    thr_on = json.dumps([round(by_level[l]["on"]["throughput"]) if "on" in by_level[l] else None for l in sorted(by_level)])
    ttft_off = json.dumps([round(by_level[l]["off"]["ttft_avg"]) if "off" in by_level[l] else None for l in sorted(by_level)])
    ttft_on = json.dumps([round(by_level[l]["on"]["ttft_avg"]) if "on" in by_level[l] else None for l in sorted(by_level)])
    chart = """
const grid=__GRID__;
new Chart(c_cc1,{type:'bar',data:{labels:__LEVELS__.map(l=>'并发 '+l),datasets:[
{label:'吞吐 tok/s (off)',data:__THR_OFF__,backgroundColor:'#1652f0'},
{label:'吞吐 tok/s (on)',data:__THR_ON__,backgroundColor:'#7c5cff'}]},
options:{plugins:{title:{display:true,text:'并发档位整体吞吐'}},scales:{x:{...grid},y:{...grid,title:{display:true,text:'tok/s'}}}}});
new Chart(c_cc2,{type:'line',data:{labels:__LEVELS__.map(l=>'并发 '+l),datasets:[
{label:'TTFT 均值 (off)',data:__TTFT_OFF__,borderColor:'#1652f0',tension:.3},
{label:'TTFT 均值 (on)',data:__TTFT_ON__,borderColor:'#7c5cff',borderDash:[6,4],tension:.3}]},
options:{plugins:{title:{display:true,text:'并发下的 TTFT 衰减'}},scales:{x:{...grid},y:{...grid,title:{display:true,text:'ms'}}}}});
""".replace("__GRID__", GRID_JS).replace("__LEVELS__", levels).replace("__THR_OFF__", thr_off) \
       .replace("__THR_ON__", thr_on).replace("__TTFT_OFF__", ttft_off).replace("__TTFT_ON__", ttft_on)

    is_mt = any(r["multiturn"] for r in rows)
    title = "并发多轮会话（N 个虚拟用户独立会话重放）" if is_mt else "并发单轮"
    note = ("每个虚拟用户独立跑完整多轮会话（system + tool defs + 逐轮 history），带并发压力；"
            if is_mt else "") + "服务端 --max-num-seqs 2：并发 1→2 反映真实并行收益，更高档位测排队压力。"

    return {"id": "concurrent", "title": title, "kpi": kpis, "table": tbl, "chart": chart,
            "verdict": "吞吐在 max-num-seqs 平台期后不再增长属预期；TTFT 均值随并发抬升即排队成本。",
            "note": note}


def prep_single(single):
    off = [r for r in single if r["thinking"] == "off"]
    all_runs = [run for r in single for run in r["runs"]]
    trunc = sum(1 for run in all_runs if run.get("thinking_no_content"))
    valid_think = [run["think_ms"] for run in all_runs if run.get("think_ms") and not run.get("thinking_no_content")]

    s_rows = []
    for r in single:
        for i, run in enumerate(r["runs"], 1):
            s_rows.append([r["thinking"], "{:,}".format(r["prompt_tokens"]), i,
                           fmt_ms(run.get("ttft_ms")), fmt_ms(run.get("ttft_reasoning_ms")),
                           fmt_ms(run.get("ttft_content_ms")), fmt_ms(run.get("think_ms")),
                           fmt_ms(run.get("decode_ms")), "{:.0f}".format(run.get("tokens_per_sec") or 0),
                           run.get("finish_reason", "—"), "⚠️ " + ";".join(run["warnings"]) if run.get("warnings") else ""])

    kpis = []
    if off:
        kpis.append(("TTFT @{}tk (off)".format(off[0]["prompt_tokens"]), fmt_ms(off[0]["runs"][0].get("ttft_ms"))))
    if len(off) >= 2:
        kpis.append(("TTFT @{}tk (off)".format(off[-1]["prompt_tokens"]), fmt_ms(off[-1]["runs"][0].get("ttft_ms"))))
    kpis.append(("平均思考时长", fmt_ms(sum(valid_think) / len(valid_think)) if valid_think else "—"))
    kpis.append(("思考截断", str(trunc)))

    tokens = json.dumps([r["prompt_tokens"] for r in off])
    run1 = json.dumps([round(r["runs"][0].get("ttft_ms") or 0) for r in off])
    run2 = json.dumps([round(r["runs"][1].get("ttft_ms") or 0) if len(r["runs"]) > 1 else None for r in off])
    think_sec = json.dumps([round(v / 1000, 1) for v in valid_think])
    chart = """
const grid=__GRID__;
new Chart(c_s1,{type:'line',data:{labels:__TOKENS__,datasets:[
{label:'Run1 (冷缓存)',data:__RUN1__,borderColor:'#1652f0',tension:.3},
{label:'Run2 (缓存命中)',data:__RUN2__,borderColor:'#e5484d',borderDash:[6,4],tension:.3}]},
options:{plugins:{title:{display:true,text:'TTFT vs 上下文档位 (thinking=off, ms)'}},scales:{x:{title:{display:true,text:'prompt tokens'},...grid},y:{...grid,title:{display:true,text:'ms'}}}}});
new Chart(c_s2,{type:'bar',data:{labels:__THINK_SEC__.map((_,i)=>'有效样本'+(i+1)),datasets:[
{label:'思考时长 (s)',data:__THINK_SEC__,backgroundColor:'#7c5cff'}]},
options:{plugins:{title:{display:true,text:'思考模式下单请求思考时长'}},scales:{x:{...grid},y:{...grid,title:{display:true,text:'s'}}}}});
""".replace("__GRID__", GRID_JS).replace("__TOKENS__", tokens).replace("__RUN1__", run1) \
       .replace("__RUN2__", run2).replace("__THINK_SEC__", think_sec)

    return {"id": "single", "title": "单发单轮（prefill 扩展性 + 前缀缓存 + 思考 A/B）",
            "kpi": kpis,
            "table": table(["thinking", "档位 tk", "run", "TTFT", "TTFT reasoning", "TTFT content", "思考", "decode", "tok/s", "finish", "告警"], s_rows),
            "chart": chart, "verdict": "Run2 显著低于 Run1 即前缀缓存命中；TTFT 随档位的增长即 prefill 成本曲线。",
            "note": "fixed_seed 复用同一 prompt：Run1 冷缓存，Run2 复用前缀。"}


def quality_block(data):
    parts = []
    for k in ("single", "multiturn", "concurrent"):
        if k not in data:
            continue
        d = data[k].get(k, [])
        runs = []
        if k == "single":
            runs = [r for row in d for r in row["runs"]]
        elif k == "multiturn":
            runs = [t for s in d for t in s["turns"]]
        else:
            for lv in d:
                runs.extend(lv.get("requests", []))
                for s in lv.get("sessions", []):
                    runs.extend(s["turns"])
        n_warn = sum(1 for r in runs if r.get("warnings"))
        kinds = sorted({w.split(":")[0].split("×")[0].strip() for r in runs for w in r.get("warnings", [])})
        trunc = sum(1 for r in runs if r.get("thinking_no_content"))
        p = "{}：{} 条请求，{} 条带兼容性告警".format(k, len(runs), n_warn)
        if kinds:
            p += "（" + "、".join(kinds) + "）"
        if trunc:
            p += "，思考截断 {} 条（已从思考统计剔除）".format(trunc)
        parts.append("<p>" + p + "。</p>")
    return "".join(parts) or "<p>无数据</p>"


def main():
    d = sys.argv[1] if len(sys.argv) > 1 else "."
    title = sys.argv[2] if len(sys.argv) > 2 else "llm-perf 评测报告"
    scenario = sys.argv[3] if len(sys.argv) > 3 else "all"
    data = load(d)

    meta = {"endpoint": "?", "tool": "?"}
    for k in data:
        meta = {"endpoint": data[k].get("endpoint"), "tool": data[k].get("tool", "?")}
        break

    sections = []
    if scenario in ("all", "single") and "single" in data:
        sections.append(prep_single(data["single"]["single"]))
    if scenario in ("all", "multiturn") and "multiturn" in data:
        sections.append(prep_multiturn(data["multiturn"]["multiturn"]))
    if scenario in ("all", "concurrent") and "concurrent" in data:
        sections.append(prep_concurrent(data["concurrent"]["concurrent"]))
    if not sections:
        print("目录里没有可用的 JSON 数据（需要 single/multiturn/concurrent-*.json）")
        sys.exit(1)

    kpi_html = kpi_grid([kv for s in sections for kv in s["kpi"]])

    body = ""
    for i, s in enumerate(sections, 1):
        body += ('\n<h2>{i} · {title}</h2>\n'
                 '<div class="note">{note}</div>\n'
                 '<div class="chart"><canvas id="c_{sid}" height="110"></canvas></div>\n'
                 '{tbl}\n'
                 '<div class="finding"><b>判定：</b>{verdict}</div>\n').format(
            i=i, title=html.escape(s["title"]), note=html.escape(s["note"]),
            sid=s["id"], tbl=s["table"], verdict=s["verdict"])

    # 多场景合并时避免 const grid 重复声明：只保留第一段里的声明
    decl = "const grid=%s;" % GRID_JS
    inner_charts = ""
    for i, s in enumerate(sections):
        c = s["chart"]
        if i > 0:
            c = c.replace(decl + "\n", "")
        inner_charts += c
    chartjs = "<script>\n" + inner_charts + "\n</script>"

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
<p>端点 <code>__ENDPOINT__</code> · 工具版本 __TOOL__ · 全部数据来自服务端 usage 与逐 chunk 计时</p>
<div class="kpis">__KPI__</div>
__BODY__
<h2>数据质量</h2>
__QUALITY__
__CHARTJS__
</body></html>"""

    with open(VENDOR, encoding="utf-8") as f:
        h = h.replace("__CHARTJS_LIB__", f.read().replace("</script>", "<\\/script>"))
    h = h.replace("__TITLE__", html.escape(title)).replace("__ENDPOINT__", html.escape(str(meta["endpoint"])))
    h = h.replace("__TOOL__", html.escape(str(meta["tool"]))).replace("__KPI__", kpi_html)
    h = h.replace("__BODY__", body).replace("__QUALITY__", quality_block(data))
    h = h.replace("__CHARTJS__", chartjs)

    suffix = "" if scenario == "all" else "-" + scenario
    out = os.path.join(d, "llm-perf-报告{}.html".format(suffix))
    open(out, "w").write(h)
    print("报告:", out)


if __name__ == "__main__":
    main()
