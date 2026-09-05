#!/usr/bin/env python3
"""llm-perf JSON → 自包含 HTML 分析报告（支持按场景出报告或总览）。

用法: python3 gen_html_report.py <output目录> [报告标题] [scenario]
  scenario: multiturn | concurrent | single | all（默认 all，按目录里实际存在的 JSON 出章节）
输出: <目录>/llm-perf-报告.html（scenario != all 时为 llm-perf-报告-<scenario>.html）
Chart.js 已内嵌（scripts/vendor/），离线可用。

章节：单发单轮 / 单发多轮 / 并发 / 服务端观测（/metrics，有数据才出）。
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


def pct(xs, p):
    """线性插值百分位。"""
    if not xs:
        return None
    xs = sorted(xs)
    k = (len(xs) - 1) * p / 100.0
    f = int(k)
    c = min(f + 1, len(xs) - 1)
    return xs[f] + (xs[c] - xs[f]) * (k - f)


def hit_rate(t):
    """单请求服务端 counter 命中率。"""
    d = t.get("server_counter_delta") or {}
    q = d.get("cache_query_tokens") or 0
    if q <= 0:
        return None
    return (d.get("cache_hit_tokens") or 0) / q


def table(headers, rows):
    h = "".join("<th>{}</th>".format(x) for x in headers)
    body = "".join("<tr>" + "".join("<td>{}</td>".format(c) for c in r) + "</tr>" for r in rows)
    return '<table><thead><tr>{}</tr></thead><tbody>{}</tbody></table>'.format(h, body)


def kpi_grid(items):
    return "".join('<div class="kpi"><div class="kpi-v">{}</div><div class="kpi-l">{}</div></div>'.format(v, l)
                   for l, v in items)


# 图表代码统一用 document.getElementById('c_xx') 引用画布；body 渲染时正则提取 id 生成 <canvas>。


# ── 各场景数据准备 ──

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
                           fmt_ms(run.get("decode_ms")), fmt_ms(run.get("tpot_ms")),
                           "{:.0f}".format(run.get("tokens_per_sec") or 0),
                           "{:,}".format(run["cached_tokens"]) if run.get("cached_tokens") else "—",
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
new Chart(document.getElementById('c_s1'),{type:'line',data:{labels:__TOKENS__,datasets:[
{label:'Run1 (冷缓存)',data:__RUN1__,borderColor:'#1652f0',tension:.3},
{label:'Run2 (缓存命中)',data:__RUN2__,borderColor:'#e5484d',borderDash:[6,4],tension:.3}]},
options:{plugins:{title:{display:true,text:'TTFT vs 上下文档位 (thinking=off, ms)'}},scales:{x:{title:{display:true,text:'prompt tokens'},...grid},y:{...grid,title:{display:true,text:'ms'}}}}});
new Chart(document.getElementById('c_s2'),{type:'bar',data:{labels:__THINK_SEC__.map((_,i)=>'有效样本'+(i+1)),datasets:[
{label:'思考时长 (s)',data:__THINK_SEC__,backgroundColor:'#7c5cff'}]},
options:{plugins:{title:{display:true,text:'思考模式下单请求思考时长'}},scales:{x:{...grid},y:{...grid,title:{display:true,text:'s'}}}}});
""".replace("__TOKENS__", tokens).replace("__RUN1__", run1) \
       .replace("__RUN2__", run2).replace("__THINK_SEC__", think_sec)

    return {"id": "single", "title": "单发单轮（prefill 扩展性 + 前缀缓存 + 思考 A/B）",
            "kpi": kpis,
            "table": table(["thinking", "档位 tk", "run", "TTFT", "TTFT reasoning", "TTFT content", "思考", "decode", "TPOT", "tok/s", "缓存命中 tk", "finish", "告警"], s_rows),
            "chart": chart, "verdict": "Run2 显著低于 Run1 即前缀缓存命中；TTFT 随档位的增长即 prefill 成本曲线。",
            "note": "fixed_seed 复用同一 prompt：Run1 冷缓存，Run2 复用前缀。TPOT 为 GenAI-Perf 口径（每 output token 时间，含思考）。"}


def prep_multiturn(mt):
    """mt: multiturn 数组（每元素一个会话）。"""
    rows = []
    for s in mt:
        prev_ttft = None
        for i, t in enumerate(s["turns"], 1):
            ttft = t.get("ttft_ms")
            new_tk = t.get("new_tokens")
            # 增量 prefill 速率：TTFT / 本轮新增 tokens * 1000 → ms / 千新 token
            incr = (ttft / new_tk * 1000) if (ttft and new_tk) else None
            rows.append({"turn": i, "thinking": s["thinking"], "ctx": t.get("prompt_tokens", 0),
                         "new_tk": new_tk, "ttft": ttft, "d_ttft": (ttft - prev_ttft) if (ttft and prev_ttft) else None,
                         "incr": incr, "think": t.get("think_ms"), "tpot": t.get("tpot_ms"),
                         "tokps": t.get("tokens_per_sec") or 0, "cached": t.get("cached_tokens"),
                         "hrate": hit_rate(t), "finish": t.get("finish_reason"),
                         "warn": t.get("warnings", []), "trunc": bool(t.get("thinking_no_content"))})
            if ttft:
                prev_ttft = ttft
    valid_think = [r["think"] for r in rows if r["think"] and not r["trunc"]]
    trunc = sum(1 for r in rows if r["trunc"])
    off = [r for r in rows if r["thinking"] == "off" and r["ttft"]]
    incrs = [r["incr"] for r in rows if r["incr"]]

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
    if incrs:
        kpis.append(("增量 prefill 中位", "{:.1f} ms/ktok".format(pct(incrs, 50))))
    kpis.append(("平均思考时长", fmt_ms(sum(valid_think) / len(valid_think)) if valid_think else "—"))
    kpis.append(("思考截断 turn", str(trunc)))

    tbl = table(["thinking", "turn", "上下文 tk", "新增 tk", "TTFT", "ΔTTFT", "增量 prefill ms/ktok", "思考", "TPOT", "tok/s", "缓存命中 tk", "命中率", "finish", "告警"],
                [[r["thinking"], r["turn"], "{:,}".format(r["ctx"]),
                  "{:,}".format(r["new_tk"]) if r["new_tk"] else "—",
                  fmt_ms(r["ttft"]), fmt_ms(r["d_ttft"]),
                  "{:.1f}".format(r["incr"]) if r["incr"] else "—",
                  fmt_ms(r["think"]), fmt_ms(r["tpot"]), "{:.0f}".format(r["tokps"]),
                  "{:,}".format(r["cached"]) if r["cached"] else "—",
                  "{:.0%}".format(r["hrate"]) if r["hrate"] is not None else "—",
                  r["finish"], "⚠️ " + ";".join(r["warn"]) if r["warn"] else ""]
                 for r in rows])

    # 图 1：TTFT vs ctx（off/on 两条线，直答"为什么 TTFT 一直在涨"）
    def series(name, color, dash=None):
        pts = [{"x": r["ctx"], "y": round(r["ttft"])} for r in rows if r["thinking"] == name and r["ttft"]]
        ds = {"label": "TTFT ({}), ms".format(name), "data": pts, "borderColor": color, "tension": .3}
        if dash:
            ds["borderDash"] = dash
        return ds
    body1 = json.dumps([series("off", '#1652f0'), series("on", '#7c5cff', [6, 4])], separators=(',', ':'))

    # 图 2：增量 prefill 速率 vs ctx（ms/ktok，衡量"越滚越贵"）
    def series2(name, color):
        pts = [{"x": r["ctx"], "y": round(r["incr"], 1)} for r in rows if r["thinking"] == name and r["incr"]]
        return {"label": "增量 prefill ({}), ms/千新token".format(name), "data": pts, "borderColor": color, "tension": .3}
    body2 = json.dumps([series2("off", '#1652f0'), series2("on", '#7c5cff')], separators=(',', ':'))

    chart = """
new Chart(document.getElementById('c_mt1'),{type:'line',data:{datasets:__BODY1__},
options:{plugins:{title:{display:true,text:'TTFT vs 上下文深度（每点一轮；陡峭=深上下文 prefill 更贵）'}},
scales:{x:{type:'linear',title:{display:true,text:'ctx tokens'},...grid},y:{...grid,title:{display:true,text:'ms'}}}}});
new Chart(document.getElementById('c_mt2'),{type:'line',data:{datasets:__BODY2__},
options:{plugins:{title:{display:true,text:'增量 prefill 速率 vs 上下文深度（本轮新增 tokens 的单价）'}},
scales:{x:{type:'linear',title:{display:true,text:'ctx tokens'},...grid},y:{...grid,title:{display:true,text:'ms / 千新token'}}}}});
""".replace("__BODY1__", body1).replace("__BODY2__", body2)

    return {"id": "multiturn", "title": "多轮会话（agent 模拟：system + tool defs + 逐轮 history）",
            "kpi": kpis, "table": tbl, "chart": chart, "verdict": verdict,
            "note": "每 turn 重新发送完整 messages 数组（system + history + 当前 user），与 agent 行为一致。"
                    "增量 prefill 速率 = TTFT ÷ 本轮新增 tokens，剔除了缓存复用的分母效应；思考截断 turn 已从思考统计剔除。"}


def prep_concurrent(conc, slo=None):
    """conc: concurrent 数组（每元素一个并发档位/到达率 × 思考变体）。"""
    rows = []
    for lv in conc:
        reqs = list(lv.get("requests", []))
        for s in lv.get("sessions", []):
            reqs.extend(s["turns"])
        ttfts = [r.get("ttft_ms") or 0 for r in reqs]
        tpots = [r.get("tpot_ms") for r in reqs if r.get("tpot_ms") and not r.get("thinking_no_content")]
        thinks = [r.get("think_ms") for r in reqs if r.get("think_ms") and not r.get("thinking_no_content")]
        trunc = sum(1 for r in reqs if r.get("thinking_no_content"))
        # 会话时长分布（每 worker 会话各轮 E2E 之和的近似）
        sess_durs = [sum(t.get("e2e_ms") or 0 for t in s["turns"]) for s in lv.get("sessions", [])]
        rows.append({"level": lv["level"], "rate": lv.get("request_rate") or 0, "thinking": lv["thinking"],
                     "multiturn": bool(lv.get("sessions")), "n": len(reqs),
                     "throughput": lv.get("throughput_tps", 0), "wall": lv.get("wall_seconds", 0),
                     "ttft_avg": sum(ttfts) / len(ttfts) if ttfts else 0,
                     "ttft_p50": pct(ttfts, 50), "ttft_p95": pct(ttfts, 95), "ttft_max": max(ttfts) if ttfts else 0,
                     "tpot_avg": sum(tpots) / len(tpots) if tpots else None,
                     "think_avg": sum(thinks) / len(thinks) if thinks else None, "trunc": trunc,
                     "sess": sess_durs,
                     "slo_meet": lv.get("slo_meet", 0), "slo_total": lv.get("slo_total", 0),
                     "goodput_rps": lv.get("goodput_rps", 0), "goodput_tps": lv.get("goodput_tps", 0)})
    by_key = {}
    for r in rows:
        by_key.setdefault((r["level"], r["rate"]), {})[r["thinking"]] = r

    def label(level, rate):
        return "rate={:g}/s".format(rate) if rate else "并发 {}".format(level)

    kpis = [("负载档位", ", ".join(label(l, rt) for (l, rt) in sorted(by_key)))]
    if by_key:
        lo = min(by_key)
        if "off" in by_key[lo]:
            kpis.append(("{} 吞吐 (off)".format(label(*lo)), "{:.0f} tok/s".format(by_key[lo]["off"]["throughput"])))
    if len(by_key) >= 2:
        hi = max(by_key)
        if "off" in by_key[hi]:
            kpis.append(("{} 吞吐 (off)".format(label(*hi)), "{:.0f} tok/s".format(by_key[hi]["off"]["throughput"])))
    on_thinks = [r["think_avg"] for r in rows if r["thinking"] == "on" and r["think_avg"]]
    if on_thinks:
        kpis.append(("平均思考时长 (on)", fmt_ms(sum(on_thinks) / len(on_thinks))))

    headers = ["档位", "thinking", "请求数", "TTFT P50", "TTFT P95", "TTFT 最大", "TPOT 均值", "思考均值", "整体吞吐", "墙钟"]
    c_rows = []
    for key in sorted(by_key):
        for th in ("off", "on"):
            if th in by_key[key]:
                r = by_key[key][th]
                row = [label(*key), th, r["n"],
                       "{:,.0f} ms".format(r["ttft_p50"]) if r["ttft_p50"] else "—",
                       "{:,.0f} ms".format(r["ttft_p95"]) if r["ttft_p95"] else "—",
                       "{:,.0f} ms".format(r["ttft_max"]), fmt_ms(r["tpot_avg"]), fmt_ms(r["think_avg"]),
                       "{:.0f} tok/s".format(r["throughput"]), "{:.1f} s".format(r["wall"])]
                if slo:
                    row += ["{}/{}".format(r["slo_meet"], r["slo_total"]) if r["slo_total"] else "—",
                            "{:.1f} req/s".format(r["goodput_rps"]) if r["goodput_rps"] else "—"]
                c_rows.append(row)
    if slo:
        headers += ["SLO 达标", "goodput"]
    tbl = table(headers, c_rows)

    keys = sorted(by_key)
    labels = json.dumps([label(l, rt) for l, rt in keys])
    thr_off = json.dumps([round(by_key[k]["off"]["throughput"]) if "off" in by_key[k] else None for k in keys])
    thr_on = json.dumps([round(by_key[k]["on"]["throughput"]) if "on" in by_key[k] else None for k in keys])
    p50_off = json.dumps([round(by_key[k]["off"]["ttft_p50"]) if "off" in by_key[k] and by_key[k]["off"]["ttft_p50"] else None for k in keys])
    p50_on = json.dumps([round(by_key[k]["on"]["ttft_p50"]) if "on" in by_key[k] and by_key[k]["on"]["ttft_p50"] else None for k in keys])
    p95_off = json.dumps([round(by_key[k]["off"]["ttft_p95"]) if "off" in by_key[k] and by_key[k]["off"]["ttft_p95"] else None for k in keys])
    p95_on = json.dumps([round(by_key[k]["on"]["ttft_p95"]) if "on" in by_key[k] and by_key[k]["on"]["ttft_p95"] else None for k in keys])
    chart = """
new Chart(document.getElementById('c_cc1'),{type:'bar',data:{labels:__LEVELS__,datasets:[
{label:'吞吐 tok/s (off)',data:__THR_OFF__,backgroundColor:'#1652f0'},
{label:'吞吐 tok/s (on)',data:__THR_ON__,backgroundColor:'#7c5cff'}]},
options:{plugins:{title:{display:true,text:'档位整体吞吐'}},scales:{x:{...grid},y:{...grid,title:{display:true,text:'tok/s'}}}}});
new Chart(document.getElementById('c_cc2'),{type:'line',data:{labels:__LEVELS__,datasets:[
{label:'TTFT P50 (off)',data:__P50_OFF__,borderColor:'#1652f0',tension:.3},
{label:'TTFT P50 (on)',data:__P50_ON__,borderColor:'#7c5cff',borderDash:[6,4],tension:.3},
{label:'TTFT P95 (off)',data:__P95_OFF__,borderColor:'#e5484d',tension:.3},
{label:'TTFT P95 (on)',data:__P95_ON__,borderColor:'#f0b429',borderDash:[6,4],tension:.3}]},
options:{plugins:{title:{display:true,text:'档位下 TTFT 分位（P50/P95）'}},scales:{x:{...grid},y:{...grid,title:{display:true,text:'ms'}}}}});
""".replace("__LEVELS__", labels).replace("__THR_OFF__", thr_off).replace("__THR_ON__", thr_on) \
       .replace("__P50_OFF__", p50_off).replace("__P50_ON__", p50_on) \
       .replace("__P95_OFF__", p95_off).replace("__P95_ON__", p95_on)

    is_mt = any(r["multiturn"] for r in rows)
    is_open = any(r["rate"] for r in rows)
    title = "并发多轮会话（N 个虚拟用户独立会话重放）" if is_mt else "并发单轮"
    if is_open:
        title += "（开环到达率）"
    note = ("每个虚拟用户独立跑完整多轮会话（system + tool defs + 逐轮 history），带并发压力；"
            if is_mt else "") + ("开环模式：请求按 Poisson 到达，TTFT 上升即排队成本，吞吐拐点即饱和点。"
                                 if is_open else "服务端 --max-num-seqs 2：并发 1→2 反映真实并行收益，更高档位测排队压力。")
    verdict = "吞吐在服务并行上限平台期后不再增长属预期；TTFT P95 相对 P50 的抬升即排队长尾。"
    if slo:
        verdict += " goodput 只统计同时满足 TTFT≤{:.0f}ms 与 TPOT≤{:.0f}ms 的请求。".format(slo.get("ttft_ms", 0), slo.get("tpot_ms", 0))

    return {"id": "concurrent", "title": title, "kpi": kpis, "table": tbl, "chart": chart,
            "verdict": verdict, "note": note}


def prep_server(all_data):
    """服务端观测章节：聚合各场景 Report.server_metrics。"""
    reps = []
    for k in ("single", "multiturn", "concurrent"):
        if k in all_data and all_data[k].get("server_metrics"):
            reps.append((k, all_data[k]["server_metrics"]))
    if not reps:
        return None
    total_hit = sum(s.get("cache_hit_tokens") or 0 for _, s in reps)
    total_query = sum(s.get("cache_query_tokens") or 0 for _, s in reps)
    preemptions = sum(s.get("preemptions") or 0 for _, s in reps)
    drafts = sum(s.get("spec_drafts") or 0 for _, s in reps)
    accepted = sum(s.get("spec_accepted_tokens") or 0 for _, s in reps)

    kpis = []
    if total_query > 0:
        kpis.append(("前缀缓存命中率（窗口）", "{:.1%}".format(total_hit / total_query)))
    kpis.append(("preemptions", "{:,.0f}".format(preemptions)))
    if drafts > 0:
        kpis.append(("MTP 接受率", "{:.0%}".format(accepted / drafts)))

    # 直方图表（任取一份有数据的）
    h_rows = []
    gauge_rows = []
    for name, s in reps:
        for hname, h in sorted((s.get("histograms") or {}).items()):
            short = hname.replace("vllm:", "").replace("_seconds", "")
            h_rows.append([name, short, "{:,.0f}".format(h["count"]),
                           "{:,.1f} ms".format(h.get("p50", 0) * 1000) if h.get("p50") else "—",
                           "{:,.1f} ms".format(h.get("p99", 0) * 1000) if h.get("p99") else "—",
                           "{:,.1f} ms".format(h.get("mean", 0) * 1000) if "mean" in h else "—"])
        for gname, g in sorted((s.get("gauges") or {}).items()):
            gauge_rows.append([name, gname, "{:.2f}".format(g["max"]), "{:.2f}".format(g["avg"]), str(g["samples"])])
    tbl = table(["场景", "直方图（服务端口径）", "观测数", "P50", "P99", "均值"], h_rows) + \
          '<div style="height:10px"></div>' + \
          table(["场景", "gauge", "峰值", "均值", "采样数"], gauge_rows)

    verdict = "服务端指标为窗口差值口径（并发窗口内为混合贡献）。"
    if total_query > 0:
        verdict += "命中率 {:.0%} 说明前缀缓存{}；".format(
            total_hit / total_query, "在工作" if total_hit / total_query > 0.1 else "几乎未命中——检查 --enable-prefix-caching")
    if preemptions > 0:
        verdict += " preemptions>0 说明出现 KV 淘汰重算（内存压力），TTFT 长尾与它强相关。"
    if drafts > 0:
        verdict += " MTP 接受率 {:.0%} 直接影响 decode 速度。".format(accepted / drafts)

    return {"id": "server", "title": "服务端观测（/metrics，vLLM 原生指标）",
            "kpi": kpis, "table": tbl, "chart": "", "verdict": verdict,
            "note": "counter 取场景窗口差值；gauge 按 metrics_interval_ms 轮询取峰值/均值；histogram 分位为桶边界估算（AIPerf 同口径）。"}


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
    # 正确性抽查结果
    for k in ("single", "multiturn", "concurrent"):
        if k in data:
            corr = data[k].get("correctness") or []
            if corr:
                passed = sum(1 for r in corr if r.get("match"))
                parts.append("<p>正确性抽查（{}）：{}/{} 通过{}".format(
                    k, passed, len(corr), "" if passed == len(corr) else "——⚠️ 存在回复与要求不符，警惕缓存污染/网关伪响应/截断"))
                bad = [r for r in corr if not r.get("match")]
                if bad:
                    parts.append("，例：期望 {} 实际 {!r}".format(bad[0]["number"], bad[0]["reply"][:60]))
                parts.append("。</p>")
            break
    return "".join(parts) or "<p>无数据</p>"


def main():
    d = sys.argv[1] if len(sys.argv) > 1 else "."
    title = sys.argv[2] if len(sys.argv) > 2 else "llm-perf 评测报告"
    scenario = sys.argv[3] if len(sys.argv) > 3 else "all"
    data = load(d)

    meta = {"endpoint": "?", "tool": "?"}
    slo = None
    for k in data:
        meta = {"endpoint": data[k].get("endpoint"), "tool": data[k].get("tool", "?")}
        slo = data[k].get("slo")
        break

    sections = []
    if scenario in ("all", "single") and "single" in data:
        sections.append(prep_single(data["single"]["single"]))
    if scenario in ("all", "multiturn") and "multiturn" in data:
        sections.append(prep_multiturn(data["multiturn"]["multiturn"]))
    if scenario in ("all", "concurrent") and "concurrent" in data:
        sections.append(prep_concurrent(data["concurrent"]["concurrent"], slo))
    server_section = prep_server(data)
    if server_section:
        sections.append(server_section)
    if not sections:
        print("目录里没有可用的 JSON 数据（需要 single/multiturn/concurrent-*.json）")
        sys.exit(1)

    kpi_html = kpi_grid([kv for s in sections for kv in s["kpi"]])

    body = ""
    import re as _re
    for i, s in enumerate(sections, 1):
        body += ('\n<h2>{i} · {title}</h2>\n'
                 '<div class="note">{note}</div>\n').format(
            i=i, title=html.escape(s["title"]), note=html.escape(s["note"]))
        if s["chart"]:
            # 图表代码统一 getElementById 引用，按出现顺序生成画布
            for cid in _re.findall(r"getElementById\('(c_[a-z0-9_]+)'\)", s["chart"]):
                body += '<div class="chart"><canvas id="{}" height="110"></canvas></div>\n'.format(cid)
        body += s["table"] + '\n<div class="finding"><b>判定：</b>{}</div>\n'.format(s["verdict"])

    chartjs = "<script>\nconst grid=%s;\n%s\n</script>" % (GRID_JS, "\n".join(s["chart"] for s in sections if s["chart"]))

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
<p>端点 <code>__ENDPOINT__</code> · 工具版本 __TOOL__ · 全部数据来自服务端 usage 与逐 chunk 计时（观测层开启时叠加 /metrics 服务端口径）</p>
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
