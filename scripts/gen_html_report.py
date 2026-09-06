#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""llm-perf JSON → 自包含 HTML 分析报告。

用法:
  python3 gen_html_report.py output/                      # 目录模式：合并目录下全部 single-/multiturn-/concurrent-*.json
  python3 gen_html_report.py a.json b.json [标题]          # 文件模式：显式指定一份或多份报告 JSON
  python3 gen_html_report.py output/ 标题 multiturn       # 旧用法兼容（第三参数=场景过滤）

输出: <输入目录>/llm-perf-报告.html（Chart.js 内嵌，离线可用）

特性:
  - 自动合并同场景多份 JSON（例如 thinking=off / on 分开的战役），不丢变体
  - 数据驱动的分析章节：prefill 斜率（ms/token）、前缀缓存判定（多轮/单发斜率比）、
    decode 吞吐、思考行为分类（无思考输出 / 正常 / 思考独占输出预算）
  - 结论与建议由数据条件生成，不写死模型名
  - 报告末尾内嵌 <script type="application/json" id="perf-summary">：
    全部聚合数据 + 自动观察，可直接交给 AI 阅读并追加解读备注
"""
import glob
import html
import json
import os
import re
import statistics as st
import sys
from collections import OrderedDict, defaultdict

SCENARIOS = ("single", "multiturn", "concurrent")
PALETTE = ["#1652f0", "#0e9f6e", "#f0b429", "#e5484d", "#7c5cff", "#0ca678"]

GRID_JS = "const grid={ticks:{color:'#999'},grid:{color:'#f0f0f5'}};"


# ────────────────────────── 输入加载 ──────────────────────────

def load_inputs(argv):
    """返回 (reports, title, scenario, out_dir)。reports: [(路径, dict)]"""
    paths, title, scenario = [], None, "all"
    out_dir = None
    rest = list(argv)
    i = 0
    while i < len(rest):
        a = rest[i]
        if a == "--scenario":
            scenario = rest[i + 1]
            i += 2
            continue
        if os.path.isdir(a):
            for k in SCENARIOS:
                paths.extend(sorted(glob.glob(os.path.join(a, "**", k + "-*.json"), recursive=True)))
            out_dir = a
        elif a.endswith(".json") and os.path.exists(a):
            paths.append(a)
            out_dir = os.path.dirname(os.path.abspath(a))
        elif title is None:
            title = a
        else:
            scenario = a  # 旧用法第三参数
        i += 1
    if not paths:
        sys.exit("没有输入：请给 output 目录或 .json 文件路径")
    reports = [(p, json.load(open(p, encoding="utf-8"))) for p in paths]
    return reports, title, scenario, out_dir


def merge(reports, scenario_filter):
    """合并多份同场景报告 → {scenario: [entries...]}，并收集元信息。"""
    data = {k: [] for k in SCENARIOS}
    meta = {"endpoint": "?", "tool": "?", "notes": [], "generated": [], "slo": None}
    for p, d in reports:
        scen = d.get("scenario")
        if scen not in data or (scenario_filter != "all" and scen != scenario_filter):
            continue
        data[scen].extend(d.get(scen) or [])
        meta["endpoint"] = d.get("endpoint") or meta["endpoint"]
        if d.get("tool"):
            meta["tool"] = d["tool"]
        if d.get("note"):
            meta["notes"].append((os.path.basename(p), d["note"]))
        if d.get("generated_at"):
            meta["generated"].append(d["generated_at"])
        meta["slo"] = d.get("slo") or meta["slo"]
    return data, meta


# ────────────────────────── 统计辅助 ──────────────────────────

def mmm(vals, nd=2):
    """(median, min, max) 或 None。"""
    vals = [v for v in vals if v is not None]
    if not vals:
        return None
    return (round(st.median(vals), nd), round(min(vals), nd), round(max(vals), nd))


def f3(v, unit="s"):
    return "—" if v is None else "{:.2f}{}".format(v[0], unit) + \
        '<span class="rng"> ({:.2f}–{:.2f})</span>'.format(v[1], v[2])


def f1(v, unit="s"):
    return "—" if v is None else "{:.1f}{}".format(v[0], unit) + \
        '<span class="rng"> ({:.1f}–{:.1f})</span>'.format(v[1], v[2])


def f0(v, unit=""):
    return "—" if v is None else "{:,.0f}{}".format(v[0], unit) + \
        '<span class="rng"> ({:,.0f}–{:,.0f})</span>'.format(v[1], v[2])


def slope_ms_per_token(pts):
    """pts: [(token, seconds)] 两点以上 → ms/token（首尾差分）。"""
    if len(pts) < 2:
        return None
    (x0, y0), (x1, y1) = pts[0], pts[-1]
    if x1 == x0 or y1 is None or y0 is None:
        return None
    return (y1 - y0) / (x1 - x0) * 1000.0


# 缓存判定阈值：多轮斜率低于它即认为历史前缀被缓存复用（绝对判据，
# 防止单发参照系本身被缓存污染——同题 runs 全部命中时单发斜率≈多轮斜率，比值失效）
CACHE_EFFECTIVE_MS_PER_TOKEN = 0.05


def classify_cache(P):
    """返回 (标签, 斜率比值文本)。"""
    m_th = "off" if "off" in P else ("on" if "on" in P else None)
    multi = P.get(m_th, {}).get("slope_multi") if m_th else None
    ratio = P.get("cache_ratio")
    ratio_txt = "{:.0%}".format(ratio) if ratio is not None else "—"
    if multi is None:
        return "—", ratio_txt
    if multi < CACHE_EFFECTIVE_MS_PER_TOKEN or (ratio is not None and ratio < 0.2):
        return "生效", ratio_txt
    if ratio is None or ratio > 0.8:
        return "未命中", ratio_txt
    return "部分命中", ratio_txt


def table(headers, rows):
    h = "".join("<th>{}</th>".format(x) for x in headers)
    body = "".join("<tr>" + "".join("<td>{}</td>".format(c) for c in r) + "</tr>" for r in rows)
    return '<table><thead><tr>{}</tr></thead><tbody>{}</tbody></table>'.format(h, body)


def esc(s):
    return html.escape(str(s))


# ────────────────────────── 数据组织 ──────────────────────────

def organize(data):
    """single: model×thinking → {size: entry}; multiturn: model×thinking → [session...]"""
    s_by, m_by, c_lvls = defaultdict(dict), defaultdict(list), []
    for e in data["single"]:
        s_by[(e["model"], e.get("thinking", "off"))][e["prompt_tokens"]] = e
    for e in data["multiturn"]:
        m_by[(e["model"], e.get("thinking", "off"))].append(e)
    c_lvls = list(data["concurrent"])
    models = []
    for (m, _) in list(s_by) + list(m_by):
        if m not in models:
            models.append(m)
    return s_by, m_by, c_lvls, models


def single_runs(s_by, model, thinking):
    out = []
    for size in sorted(s_by.get((model, thinking), {})):
        out.extend(s_by[(model, thinking)][size]["runs"])
    return out


def mt_turns(m_by, model, thinking):
    return [t for s in m_by.get((model, thinking), []) for t in s["turns"]]


# ────────────────────────── 分析 ──────────────────────────

def analyze(data, meta):
    s_by, m_by, c_lvls, models = organize(data)
    A = {"models": models, "per_model": OrderedDict(), "coverage": {}, "events": []}

    all_single_runs = [r for e in data["single"] for r in e["runs"]]
    all_mt_turns = [t for sess in data["multiturn"] for t in sess["turns"]]
    n_total = len(all_single_runs) + len(all_mt_turns) + sum(
        len(lv.get("requests", [])) + sum(len(s["turns"]) for s in lv.get("sessions", [])) for lv in c_lvls)
    A["coverage"] = {
        "n_requests": n_total,
        "has_single": bool(data["single"]),
        "has_multiturn": bool(data["multiturn"]),
        "has_concurrent": bool(c_lvls),
    }

    for m in models:
        P = {}
        # 单发各 thinking 变体
        for th in ("off", "on"):
            sizes = sorted(s_by.get((m, th), {}))
            if not sizes:
                continue
            ladder = []
            for size in sizes:
                rs = s_by[(m, th)][size]["runs"]
                ladder.append({
                    "size": size,
                    "ttft": mmm([r.get("ttft_ms", 0) / 1000 for r in rs], 2),
                    "ttft_content": mmm([r.get("ttft_content_ms", 0) / 1000 for r in rs], 2),
                    "e2e": mmm([r.get("e2e_ms", 0) / 1000 for r in rs], 1),
                    "decode": mmm([r.get("decode_ms", 0) / 1000 for r in rs], 1),
                    "itl_p50": mmm([r.get("itl_p50_ms") for r in rs], 1),
                    "itl_p99": mmm([r.get("itl_p99_ms") for r in rs], 1),
                    "tokps": mmm([r.get("tokens_per_sec") for r in rs], 0),
                    "comp": mmm([r.get("completion_tokens") for r in rs], 0),
                    "rc": mmm([r.get("reasoning_chars", 0) for r in rs], 0),
                    "finish": sorted({r.get("finish_reason", "?") for r in rs}),
                    "n": len(rs),
                })
            P.setdefault(th, {})["ladder"] = ladder
            P[th]["slope"] = slope_ms_per_token(
                [(l["size"], l["ttft"][0]) for l in ladder if l["ttft"]]) if len(ladder) >= 2 else None
            P[th]["tokps"] = mmm([l["tokps"][0] for l in ladder if l["tokps"]], 0)
            P[th]["finish_length"] = sum(1 for l in ladder if "length" in l["finish"])
            P[th]["e2e_all"] = [r.get("e2e_ms", 0) / 1000 for l in ladder
                                for r in s_by[(m, th)][l["size"]]["runs"]]
            P[th]["rc_all"] = [r.get("reasoning_chars", 0) for l in ladder
                               for r in s_by[(m, th)][l["size"]]["runs"]]
            P[th]["no_content_runs"] = [r for l in ladder
                                        for r in s_by[(m, th)][l["size"]]["runs"]
                                        if r.get("content_chunks") == 0 or r.get("thinking_no_content")]
            # run1 vs run2+ TTFT（缓存冷/热形态）
            firsts, rests = [], []
            for l in ladder:
                rs = s_by[(m, th)][l["size"]]["runs"]
                if rs:
                    firsts.append(rs[0].get("ttft_ms", 0))
                    rests.extend(r.get("ttft_ms", 0) for r in rs[1:])
            if firsts and rests:
                P[th]["cold_warm_ratio"] = (st.median(firsts) / max(st.median(rests), 1e-9))
        # 多轮
        for th in ("off", "on"):
            sess = m_by.get((m, th), [])
            if not sess:
                continue
            nturn = max(len(s["turns"]) for s in sess)
            turns = []
            for i in range(nturn):
                ts = [s["turns"][i] for s in sess if i < len(s["turns"])]
                turns.append({
                    "i": i + 1,
                    "prompt": (min(t.get("prompt_tokens", 0) for t in ts),
                               max(t.get("prompt_tokens", 0) for t in ts)),
                    "new": st.median([t.get("new_tokens", 0) for t in ts]),
                    "ttft": mmm([t.get("ttft_ms", 0) / 1000 for t in ts], 2),
                    "e2e": mmm([t.get("e2e_ms", 0) / 1000 for t in ts], 1),
                    "sess_ttft": [t.get("ttft_ms", 0) / 1000 for t in ts],
                    "sess_e2e": [t.get("e2e_ms", 0) / 1000 for t in ts],
                })
            P.setdefault(th, {})["turns"] = turns
            P[th]["slope_multi"] = slope_ms_per_token(
                [(t["prompt"][0], t["ttft"][0]) for t in turns if t["ttft"]]) if len(turns) >= 2 else None
            P[th]["mt_total"] = sum(t["e2e"][0] for t in turns if t["e2e"])
        # 缓存判定：多轮斜率 / 单发斜率（优先 off）
        s_th = "off" if "off" in P else ("on" if "on" in P else None)
        m_th = "off" if "off" in P else ("on" if "on" in P else None)
        ss = P.get(s_th, {}).get("slope") if s_th else None
        ms = P.get(m_th, {}).get("slope_multi") if m_th else None
        P["cache_ratio"] = (ms / ss) if (ss and ms and ss > 0) else None
        # 思考行为分类
        beh = None
        if "on" in P:
            rc = [v for v in P["on"].get("rc_all", [])]
            if rc and all(v == 0 for v in rc):
                beh = "no_reasoning"
            elif rc and st.median(rc) > 0:
                beh = "normal"
            elif P["on"].get("no_content_runs"):
                beh = "budget_exhausted"
        P["thinking_behavior"] = beh
        # decode 基准（优先 off 单发）
        P["decode_tps"] = P.get("off", {}).get("tokps") or P.get("on", {}).get("tokps")
        A["per_model"][m] = P

    # 全局事件
    trunc = sum(1 for r in all_single_runs + all_mt_turns if r.get("thinking_no_content"))
    no_content = [r for r in all_single_runs if r.get("content_chunks") == 0]
    warn_n = sum(1 for r in all_single_runs + all_mt_turns if r.get("warnings"))
    A["events"] = {"thinking_trunc": trunc, "no_content": no_content, "warnings": warn_n}
    return A


# ────────────────────────── 图表（Chart.js） ──────────────────────────

def spread(d=None):
    out = {"__GRIDOBJ__": 1}
    if d:
        out.update(d)
    return out


def chart_js(cid, ctype, labels, datasets, title, scales):
    cfg = {"type": ctype,
           "data": {"labels": labels, "datasets": datasets},
           "options": {"plugins": {"title": {"display": True, "text": title}},
                       "scales": scales}}
    js = json.dumps(cfg, ensure_ascii=False, separators=(",", ":"))
    js = js.replace('"__GRIDOBJ__":1', "...grid").replace('"__GRIDOBJ__"', "{...grid}")
    return "new Chart(document.getElementById('%s'),%s);" % (cid, js)


def line_ds(label, data, color, dash=None):
    d = {"label": label, "data": data, "borderColor": color, "tension": 0.3,
         "spanGaps": True}
    if dash:
        d["borderDash"] = dash
    return d


def color_of(models, m):
    return PALETTE[models.index(m) % len(PALETTE)]


def build_charts(data, A):
    s_by, m_by, c_lvls, models = organize(data)
    stmts, canvases = [], []

    def add(cid, title):
        canvases.append((cid, title))

    # 单发 TTFT vs 档位（每个 thinking 变体一张图，每模型一条线）
    for th in ("off", "on"):
        sizes = sorted({size for (m, t) in s_by for size in s_by[(m, t)] if t == th})
        if not sizes:
            continue
        ds = []
        for m in models:
            if (m, th) not in s_by:
                continue
            ys = []
            for size in sizes:
                e = s_by[(m, th)].get(size)
                ys.append(round(st.median([r.get("ttft_ms", 0) for r in e["runs"]])) if e else None)
            ds.append(line_ds("{} (thinking={})".format(short(m), th), ys, color_of(models, m)))
        scales = {"x": spread({"title": {"display": True, "text": "prompt tokens（目标档位）"}}),
                  "y": spread({"title": {"display": True, "text": "TTFT ms"}})}
        stmts.append(chart_js("c_s_ttft_" + th, "line", [str(s) for s in sizes], ds,
                              "单发 TTFT vs 上下文档位（3 runs 中位数）", scales))
        add("c_s_ttft_" + th, "单发 TTFT vs 档位（thinking={}）".format(th))

    # 多轮 TTFT 逐轮（每个 thinking 变体一张图）
    for th in ("off", "on"):
        keys = [(m, t) for (m, t) in m_by if t == th]
        if not keys:
            continue
        nturn = max(len(s["turns"]) for k in keys for s in m_by[k])
        labels = ["T{}".format(i + 1) for i in range(nturn)]
        ds = []
        for (m, t) in keys:
            if t != th:
                continue
            ys = []
            for i in range(nturn):
                vals = [s["turns"][i].get("ttft_ms") for s in m_by[(m, t)] if i < len(s["turns"])
                        and s["turns"][i].get("ttft_ms")]
                ys.append(round(st.median(vals)) if vals else None)
            ds.append(line_ds(short(m), ys, color_of(models, m)))
        scales = {"x": spread({"title": {"display": True, "text": "多轮会话轮次"}}),
                  "y": spread({"title": {"display": True, "text": "TTFT ms"}})}
        stmts.append(chart_js("c_m_ttft_" + th, "line", labels, ds,
                              "多轮 TTFT 逐轮（各会话中位数；陡峭=逐轮全量重算，平坦=前缀缓存复用）", scales))
        add("c_m_ttft_" + th, "多轮 TTFT 逐轮（thinking={}）".format(th))

    # 思考=on 的 E2E 分布（min/中位/max）
    on_e2e = []
    for m in models:
        P = A["per_model"][m]
        if "on" in P and P["on"].get("e2e_all"):
            vals = P["on"]["e2e_all"]
            on_e2e.append((m, min(vals), st.median(vals), max(vals)))
    if on_e2e:
        labels, dmin, dmed, dmax = [], [], [], []
        for m, lo, md, hi in on_e2e:
            labels.append(short(m))
            dmin.append(round(lo, 1)); dmed.append(round(md, 1)); dmax.append(round(hi, 1))
        ds = [line_ds("最快", dmin, "#93c5fd"), line_ds("中位", dmed, "#1652f0"),
              line_ds("最慢", dmax, "#e5484d")]
        scales = {"x": spread(), "y": spread({"title": {"display": True, "text": "E2E 秒"}})}
        stmts.append(chart_js("c_on_e2e", "bar", labels, ds,
                              "thinking=on 单次请求总耗时（全部 run 汇总）", scales))
        add("c_on_e2e", "thinking=on E2E 分布")

    return canvases, stmts


def short(model):
    return model.split("/")[-1] if "/" in model else model


# ────────────────────────── 表格 ──────────────────────────

def single_table(A, th):
    rows = []
    for m, P in A["per_model"].items():
        if th not in P:
            continue
        for l in P[th]["ladder"]:
            has_itl = l["itl_p50"] is not None
            rows.append([esc(short(m)), "{:,}".format(l["size"]), str(l["n"]),
                         f3(l["ttft"]), f3(l["ttft_content"]), f1(l["e2e"]), f1(l["decode"]),
                         f1(l["itl_p50"], "ms") if has_itl else "—",
                         f1(l["itl_p99"], "ms") if has_itl else "—",
                         f0(l["tokps"]), f0(l["comp"]),
                         f0(l["rc"]) if th == "on" else "—",
                         " / ".join(l["finish"])])
    head = ["模型", "档位 tk", "runs", "TTFT s", "首内容 s", "E2E s", "decode s",
            "ITL p50 ms", "ITL p99 ms", "tok/s", "输出 tok"] + \
           (["思考字符"] if th == "on" else []) + ["finish"]
    return table(head, rows) if rows else "<p>无数据</p>"


def multiturn_table(A, th):
    head = ["轮次", "prompt tok 范围", "新增 tok"]
    per_model_cols = []
    for m, P in A["per_model"].items():
        if th in P and "turns" in P[th]:
            per_model_cols.append(m)
            head += ["{} TTFT s".format(short(m)), "{} E2E s".format(short(m))]
    if not per_model_cols:
        return "<p>无数据</p>"
    nturn = max(len(P[th]["turns"]) for m in per_model_cols for P in [A["per_model"][m]])
    rows = []
    for i in range(nturn):
        row = ["T{}".format(i + 1), "", ""]
        first = True
        for m in per_model_cols:
            P = A["per_model"][m]
            turns = P[th]["turns"]
            if i < len(turns):
                t = turns[i]
                if first:
                    row[1] = "{:,}–{:,}".format(*t["prompt"])
                    row[2] = "{:,.0f}".format(t["new"])
                    first = False
                row += [f3(t["ttft"]), f1(t["e2e"])]
            else:
                row += ["—", "—"]
        rows.append(row)
    return table(head, rows)


def appendix_single(data):
    rows = []
    for e in data["single"]:
        for i, r in enumerate(e["runs"], 1):
            rows.append([e.get("thinking", "?"), esc(short(e["model"])),
                         "{:,}".format(e["prompt_tokens"]), str(i),
                         "{:.2f}".format(r.get("ttft_ms", 0) / 1000),
                         "{:.2f}".format(r["ttft_content_ms"] / 1000) if r.get("ttft_content_ms") else "—",
                         "{:.1f}".format(r.get("e2e_ms", 0) / 1000),
                         "{:.1f}".format(r["itl_avg_ms"]) if r.get("itl_avg_ms") else "—",
                         "{:.1f}".format(r["tokens_per_sec"]) if r.get("tokens_per_sec") else "—",
                         "{:,}".format(r.get("completion_tokens", 0)),
                         "{:,}".format(r.get("reasoning_chars", 0)),
                         "{:,}".format(r["cached_tokens"]) if r.get("cached_tokens") else "—",
                         r.get("finish_reason", "—"),
                         "⚠️ " + ";".join(r["warnings"]) if r.get("warnings") else ""])
    return table(["thinking", "模型", "档位", "run", "TTFT s", "首内容 s", "E2E s", "ITLavg ms",
                  "tok/s", "输出 tok", "思考字符", "缓存命中 tk", "finish", "告警"], rows)


def appendix_multiturn(data):
    parts = []
    for sess in data["multiturn"]:
        rows = []
        for i, t in enumerate(sess["turns"], 1):
            rows.append(["T{}".format(i), "{:,}".format(t.get("prompt_tokens", 0)),
                         "{:,}".format(t.get("new_tokens", 0)) if t.get("new_tokens") else "—",
                         "{:.2f}".format(t.get("ttft_ms", 0) / 1000),
                         "{:.1f}".format(t.get("e2e_ms", 0) / 1000),
                         "{:.1f}".format(t["itl_p50_ms"]) if t.get("itl_p50_ms") else "—",
                         "{:,}".format(t.get("reasoning_chars", 0)),
                         t.get("finish_reason", "—")])
        parts.append('<p class="cap">{} · thinking={} · session {}（{} 轮）</p>{}'.format(
            esc(short(sess["model"])), sess.get("thinking", "?"), sess.get("session", "?"),
            len(sess["turns"]),
            table(["轮次", "prompt tok", "新增 tok", "TTFT s", "E2E s", "ITL p50 ms", "思考字符", "finish"], rows)))
    return "".join(parts)


# ────────────────────────── 结论/建议/局限（数据条件生成） ──────────────────────────

def gen_conclusions(A):
    cs = []
    # 1 prefill 扩展性
    for m, P in A["per_model"].items():
        if "off" in P and P["off"].get("slope") is not None and len(P["off"]["ladder"]) >= 2:
            l0, l1 = P["off"]["ladder"][0], P["off"]["ladder"][-1]
            sl = P["off"]["slope"]
            if sl < CACHE_EFFECTIVE_MS_PER_TOKEN:
                cs.append("<b>{}</b>：TTFT 几乎不随档位变化（{}k→{}k 仅 {}s→{}s，≈{:.3f} ms/token）——"
                          "prefill 成本被前缀缓存掩盖。".format(
                    esc(short(m)), l0["size"] // 1000, l1["size"] // 1000, l0["ttft"][0], l1["ttft"][0], sl))
            else:
                cs.append("<b>{}</b>：TTFT 随档位线性增长，{}k={}s → {}k={}s（≈{:.2f} ms/token）。".format(
                    esc(short(m)), l0["size"] // 1000, l0["ttft"][0], l1["size"] // 1000, l1["ttft"][0], sl))
    # 2 缓存判定
    for m, P in A["per_model"].items():
        label, _ = classify_cache(P)
        if label == "—":
            continue
        r = P.get("cache_ratio")
        if label == "生效":
            cs.append("<b>{}</b>：多轮 TTFT 逐轮几乎不涨（斜率 ≈ {:.3f} ms/token{}）⇒ 前缀缓存生效，"
                      "历史前缀跨轮复用。".format(
                esc(short(m)),
                P.get("off", P.get("on", {})).get("slope_multi") or 0,
                "，为单发斜率的 {:.0%}".format(r) if r is not None and r < 1 else ""))
        elif label == "未命中":
            cs.append("<b>{}</b>：多轮斜率与单发一致（比值 {:.0%}）⇒ 未命中前缀缓存，每轮全量重算历史。".format(
                esc(short(m)), r))
        else:
            cs.append("<b>{}</b>：多轮斜率为单发的 {:.0%} ⇒ 前缀缓存部分命中。".format(esc(short(m)), r))
        # 冷/热形态佐证
        cw = P.get("off", {}).get("cold_warm_ratio")
        if cw and cw > 3:
            cs.append("{} 同档位 run1 TTFT 约为 run2/3 的 {:.0f} 倍，符合缓存冷 miss 形态。".format(esc(short(m)), cw))
    # 3 decode
    decs = [(m, P.get("decode_tps")) for m, P in A["per_model"].items() if P.get("decode_tps")]
    if len(decs) >= 2:
        decs_sorted = sorted(decs, key=lambda x: -x[1][0])
        hi, lo = decs_sorted[0], decs_sorted[-1]
        cs.append("<b>Decode 吞吐</b>：{} ≈ {:.0f} tok/s，{} ≈ {:.0f} tok/s（{} 约为 {} 的 {:.0f}%）。".format(
            esc(short(hi[0])), hi[1][0], esc(short(lo[0])), lo[1][0],
            esc(short(lo[0])), esc(short(hi[0])), lo[1][0] / hi[1][0] * 100))
    elif decs:
        cs.append("<b>Decode 吞吐</b>：{} ≈ {:.0f} tok/s。".format(esc(short(decs[0][0])), decs[0][1][0]))
    # 4 思考行为
    for m, P in A["per_model"].items():
        beh = P.get("thinking_behavior")
        if beh == "no_reasoning":
            cs.append("<b>{}</b>：thinking=on 时 <code>reasoning_content</code> 恒为空，TTFT 与 off 一致 ⇒ "
                      "思考开关在该模型上未产生独立思考输出，需核对网关参数透传。".format(esc(short(m))))
        elif beh == "budget_exhausted":
            n = len(P["on"].get("no_content_runs", []))
            cs.append("<b>{}</b>：thinking=on 出现 {} 次「思考独占输出预算」（正文 0 token，finish=length）；"
                      "思考输出 {:,}–{:,} 字符/次，E2E {}–{}s。".format(
                esc(short(m)), n, min(P["on"]["rc_all"]), max(P["on"]["rc_all"]),
                "{:.0f}".format(min(P["on"]["e2e_all"])), "{:.0f}".format(max(P["on"]["e2e_all"]))))
        elif beh == "normal":
            rc = P["on"]["rc_all"]; e2 = P["on"]["e2e_all"]
            n_exh = len(P["on"].get("no_content_runs", []))
            cs.append("<b>{}</b>：thinking=on 思考输出 {:,}–{:,} 字符/次，E2E {}–{}s（中位 {:.0f}s），"
                      "长草稿显著放大单次时延{}。".format(
                esc(short(m)), min(rc), max(rc), "{:.0f}".format(min(e2)), "{:.0f}".format(max(e2)), st.median(e2),
                "；其中 {} 次思考独占输出预算（正文 0 token）".format(n_exh) if n_exh else ""))
    # 5 agent 时延推算
    for m, P in A["per_model"].items():
        if "off" in P and P["off"]["ladder"]:
            top = P["off"]["ladder"][-1]
            if top["size"] >= 20000 and top["ttft"]:
                cs.append("<b>agent 场景推算（{}）</b>：单次响应 5–7 次模型调用、每次携带全量上下文，"
                          "按 {}k TTFT {:.1f}s 计，仅首字等待累计即 {}–{}s。".format(
                    esc(short(m)), top["size"] // 1000, top["ttft"][0],
                    round(top["ttft"][0] * 5), round(top["ttft"][0] * 7)))
    return cs


def gen_recommendations(A):
    rs = []
    for m, P in A["per_model"].items():
        cache_label, _ = classify_cache(P)
        if cache_label == "未命中" or (cache_label == "—" and "off" in P and
                                       (P["off"].get("slope") or 0) > 0.3):
            rs.append(("高", "为 <b>{}</b> 开启前缀缓存（vLLM <code>enable_prefix_caching=True</code>，核对该实例为何未生效）。"
                            "同环境已命中的模型即为可行性证据；预期多轮首字等待收敛到 1–2s 量级。".format(esc(short(m)))))
    for m, P in A["per_model"].items():
        if P.get("thinking_behavior") in ("normal", "budget_exhausted"):
            rs.append(("高", "<b>agent/交互场景关闭 {} 的深度思考</b>（或改用无长草稿的模型承载 agent 主链路）。"
                            "思考草稿按其 decode 速度折算为分钟级时延，不可用。".format(esc(short(m)))))
        if P.get("thinking_behavior") == "no_reasoning":
            rs.append(("中", "核对 <b>{}</b> 思考链路：网关是否剥离 <code>reasoning_content</code>、"
                            "thinking 参数透传是否生效。".format(esc(short(m)))))
    decs = [(m, P.get("decode_tps")) for m, P in A["per_model"].items() if P.get("decode_tps")]
    if len(decs) >= 2:
        decs_sorted = sorted(decs, key=lambda x: -x[1][0])
        rs.append(("中", "交互型会话优先选择 decode 更快的 <b>{}</b>（{:.0f} vs {:.0f} tok/s）。".format(
            esc(short(decs_sorted[0][0])), decs_sorted[0][1][0], decs_sorted[-1][1][0])))
    if not A["coverage"]["has_concurrent"]:
        rs.append(("后续", "本轮未测并发：单发结论不外推服务吞吐，建议补充并发爬坡（--concurrency 1,2,4）。"))
    top_sizes = [P["off"]["ladder"][-1]["size"] for P in A["per_model"].values()
                 if "off" in P and P["off"].get("ladder")]
    if top_sizes and max(top_sizes) < 100000:
        rs.append(("后续", "本轮最大档位 {}k，如业务涉及更长上下文建议补测 100k/200k。".format(max(top_sizes) // 1000)))
    if not rs:
        rs.append(("提示", "数据覆盖完整，未触发预置建议条件；可结合业务 SLO 进一步评估。"))
    return rs


def gen_limits(data, A):
    lim = []
    off_runs = [r for e in data["single"] if e.get("thinking") == "off" for r in e["runs"]]
    if off_runs:
        n_len = sum(1 for r in off_runs if r.get("finish_reason") == "length")
        if n_len > len(off_runs) * 0.5:
            cap = max(r.get("completion_tokens", 0) for r in off_runs)
            lim.append("off 场景 {}% 的 run finish=length（输出上限 ≈{:,} tok）——<b>E2E / decode 列在 off 场景不可跨"
                       "模型或跨思考变体比较</b>（输出长度被钳制）；TTFT / ITL / tok/s 不受影响。".format(
                           round(n_len / len(off_runs) * 100), cap))
    ev = A["events"]
    if ev["no_content"]:
        lim.append("出现 {} 次「正文 0 token」（思考独占输出预算或截断），相关 run 的内容类指标缺失，已在表中以 — 标注。".format(
            len(ev["no_content"])))
    if ev["thinking_trunc"]:
        lim.append("思考截断（thinking_no_content）{} 次，已从思考时长统计剔除。".format(ev["thinking_trunc"]))
    if ev["warnings"]:
        lim.append("兼容性告警 {} 条，明细见附录（含网关伪响应/字段缺失类告警需人工复核）。".format(ev["warnings"]))
    lim.append("单发同档位 runs 复用固定 prompt（缓存对照设计）：单发 TTFT 代表「重复请求」分布；"
               "独立冷请求请参考多轮 T1 与单发各档 run1。")
    for k in SCENARIOS:
        if data[k]:
            lim.append("服务端观测（/metrics）{}。".format(
                "已随报告输出（见数据质量章）" if data[k][0].get("server_metrics") else "未开启，缓存判定基于客户端行为证据"))
            break
    return lim


def quality_block(data):
    parts = []
    for k in SCENARIOS:
        if not data[k]:
            continue
        runs = []
        if k == "single":
            runs = [r for row in data[k] for r in row["runs"]]
        elif k == "multiturn":
            runs = [t for s in data[k] for t in s["turns"]]
        else:
            for lv in data[k]:
                runs.extend(lv.get("requests", []))
                for s in lv.get("sessions", []):
                    runs.extend(s["turns"])
        n_warn = sum(1 for r in runs if r.get("warnings"))
        kinds = sorted({w.split(":")[0].split("×")[0].strip() for r in runs for w in r.get("warnings", [])})
        p = "<p>{}：{} 条请求，{} 条带兼容性告警".format(k, len(runs), n_warn)
        if kinds:
            p += "（" + "、".join(esc(x) for x in kinds) + "）"
        parts.append(p + "。</p>")
        # 服务端 metrics
    for k in SCENARIOS:
        if data[k] and data[k][0].get("server_metrics"):
            s = data[k][0]["server_metrics"]
            hit_q = s.get("cache_query_tokens") or 0
            hit = (s.get("cache_hit_tokens") or 0) / hit_q if hit_q > 0 else None
            row = ["preemptions", "{:,.0f}".format(s.get("preemptions") or 0)]
            parts.append("<p>服务端观测（{}）：{}{}{}。</p>".format(
                k,
                "前缀缓存命中率 {:.1%}。".format(hit) if hit is not None else "",
                "preemptions {:,.0f}。".format(s.get("preemptions") or 0) if s.get("preemptions") else "",
                "MTP 接受率 {:.0%}。".format((s.get("spec_accepted_tokens") or 0) / s["spec_drafts"])
                if s.get("spec_drafts") else ""))
            break
    # 正确性抽查
    for k in SCENARIOS:
        if data[k] and (data[k][0].get("correctness") or []):
            corr = data[k][0]["correctness"]
            passed = sum(1 for r in corr if r.get("match"))
            extra = "" if passed == len(corr) else "——⚠️ 存在回复与要求不符，警惕缓存污染/网关伪响应/截断"
            parts.append("<p>正确性抽查（{}）：{}/{} 通过{}。</p>".format(k, passed, len(corr), extra))
            break
    return "".join(parts) or "<p>无数据</p>"


# ────────────────────────── 内嵌 AI 摘要 ──────────────────────────

def summary_json(data, A, meta, conclusions, recommendations, limits):
    def clean(o):
        if isinstance(o, float):
            return round(o, 3)
        if isinstance(o, tuple):
            return list(o)
        if isinstance(o, dict):
            return {k: clean(v) for k, v in o.items()}
        if isinstance(o, (list,)):
            return [clean(v) for v in o]
        return o
    doc = {
        "about": "llm-perf 评测聚合数据。本块供 AI 阅读：请基于这些数字为报告添加解读备注（通俗版注释），"
                 "不要编造数字；所有时间单位见字段名。",
        "endpoint": meta["endpoint"], "tool": meta["tool"],
        "generated_at": meta["generated"],
        "coverage": A["coverage"],
        "events": {k: (len(v) if isinstance(v, list) else v) for k, v in A["events"].items()},
        "per_model": {short(m): clean(P) for m, P in A["per_model"].items()},
        "conclusions": [re.sub(r"<[^>]+>", "", c) for c in conclusions],
        "recommendations": [{"priority": p, "text": re.sub(r"<[^>]+>", "", t)} for p, t in recommendations],
        "limitations": [re.sub(r"<[^>]+>", "", l) for l in limits],
    }
    return json.dumps(clean(doc), ensure_ascii=False, indent=1)


# ────────────────────────── 主流程 ──────────────────────────

def main():
    reports, title, scenario, out_dir = load_inputs(sys.argv[1:])
    data, meta = merge(reports, scenario)
    if not any(data[k] for k in SCENARIOS):
        sys.exit("输入中没有可用场景数据（single/multiturn/concurrent）")
    A = analyze(data, meta)
    conclusions = gen_conclusions(A)
    recommendations = gen_recommendations(A)
    limits = gen_limits(data, A)
    canvases, stmts = build_charts(data, A)

    title = title or "llm-perf 性能测试报告"
    n_req = A["coverage"]["n_requests"]
    gens = meta["generated"]
    sub = "端点 <code>{}</code> · 工具 {} · {} 个请求 · 数据 {} · {} 份输入文件".format(
        esc(meta["endpoint"]), esc(meta["tool"]), n_req,
        esc(gens[0][:16]) if gens else "—", len(reports))

    # KPI
    kpis = []
    for m, P in A["per_model"].items():
        if "off" in P and P["off"]["ladder"]:
            l0, l1 = P["off"]["ladder"][0], P["off"]["ladder"][-1]
            kpis.append(("TTFT @{}k ({})".format(l1["size"] // 1000, short(m)),
                         "{:.2f} s".format(l1["ttft"][0]) if l1["ttft"] else "—"))
        if P.get("decode_tps"):
            kpis.append(("Decode ({})".format(short(m)), "{:.0f} tok/s".format(P["decode_tps"][0])))
        if "on" in P and P["on"].get("e2e_all"):
            kpis.append(("思考 E2E 中位 ({})".format(short(m)),
                         "{:.0f} s".format(st.median(P["on"]["e2e_all"]))))
    kpi_html = "".join('<div class="kpi"><div class="kpi-v">{}</div><div class="kpi-l">{}</div></div>'.format(
        esc(v), esc(l)) for l, v in kpis)

    # 章节
    sec = []
    sec.append(("<h2>1 · 摘要</h2>", '<div class="finding"><ol class="tight">{}</ol></div>'.format(
        "".join("<li>{}</li>".format(c) for c in conclusions))))
    cov = []
    if A["coverage"]["has_single"]:
        cov.append("单发（档位矩阵 × runs）")
    if A["coverage"]["has_multiturn"]:
        cov.append("多轮（逐轮 history 滚动）")
    if A["coverage"]["has_concurrent"]:
        cov.append("并发")
    notes_html = "".join("<p><b>{}</b>：{}</p>".format(esc(f), esc(n)) for f, n in meta["notes"][-4:])
    sec.append(("<h2>2 · 测试配置与方法</h2>",
                table_kv([("端点", meta["endpoint"]), ("工具版本", meta["tool"]),
                          ("场景覆盖", "；".join(cov)),
                          ("请求总数", str(n_req))] ) +
                (('<div class="note">' + notes_html + "</div>") if notes_html else "")))
    sec.append(("<h2>3 · 指标口径</h2>", table(
        ["指标", "定义"],
        [["TTFT", "请求发出 → 首个流式 chunk（空首包不计）；本报告单位秒"],
         ["首内容", "请求发出 → 首个 content chunk（TTFT_content）"],
         ["E2E", "请求发出 → 流结束（含思考全程）"],
         ["decode", "首 chunk → 流结束"],
         ["ITL p50/p99", "chunk 间间隔分位（不含 TTFT）"],
         ["tok/s", "completion_tokens ÷ decode 时长"],
         ["思考字符", "reasoning_content 累积字符数"],
         ["finish", "stop=自然结束；length=触达 max_tokens 截断"]])))
    # 单发
    if A["coverage"]["has_single"]:
        body = ""
        for th, label in (("off", "thinking=off"), ("on", "thinking=on")):
            if any(th in P for P in A["per_model"].values()):
                cid = "c_s_ttft_" + th
                chart_html = ('<div class="chart"><canvas id="{}" height="110"></canvas></div>'.format(cid)
                              if any(c == cid for c, _ in canvases) else "")
                body += "<h3>{}</h3>{}{}".format(label, chart_html, single_table(A, th))
        sec.append(("<h2>4 · 单发结果</h2>", body))
    # 多轮
    if A["coverage"]["has_multiturn"]:
        body = ""
        for th, label in (("off", "thinking=off"), ("on", "thinking=on")):
            if any(th in P and "turns" in P[th] for P in A["per_model"].values()):
                cid = "c_m_ttft_" + th
                chart_html = ('<div class="chart"><canvas id="{}" height="110"></canvas></div>'.format(cid)
                              if any(c == cid for c, _ in canvases) else "")
                body += "<h3>{}</h3>{}{}".format(label, chart_html, multiturn_table(A, th))
        if any(c == "c_on_e2e" for c, _ in canvases):
            body += '<div class="chart"><canvas id="c_on_e2e" height="110"></canvas></div>'
        sec.append(("<h2>5 · 多轮结果</h2>", body))
    # 分析
    ana_rows = []
    for m, P in A["per_model"].items():
        cache_label, ratio_txt = classify_cache(P)
        ana_rows.append([esc(short(m)),
                         "{:.2f}".format(P["off"]["slope"]) if P.get("off", {}).get("slope") is not None else "—",
                         "{:.3f}".format(P["off"]["slope_multi"]) if P.get("off", {}).get("slope_multi") is not None
                         else ("{:.3f}".format(P["on"]["slope_multi"]) if P.get("on", {}).get("slope_multi") is not None else "—"),
                         ratio_txt,
                         cache_label,
                         "{:.0f}".format(P["decode_tps"][0]) if P.get("decode_tps") else "—",
                         {"no_reasoning": "思考无输出", "budget_exhausted": "思考独占预算",
                          "normal": "正常长草稿"}.get(P.get("thinking_behavior"), "—")])
    sec.append(("<h2>6 · 分析：缓存 / 吞吐 / 思考</h2>", table(
        ["模型", "单发斜率 ms/tk", "多轮斜率 ms/tk", "多轮/单发", "缓存判定", "decode tok/s", "思考行为"],
        ana_rows) +
        '<div class="note">缓存判定规则：多轮 TTFT 斜率 &lt;{:.2f} ms/token（绝对判据）或 多轮/单发斜率比 &lt;20% ⇒ 生效；'
        "比值 &gt;80% ⇒ 未命中。单发参照系本身可能被缓存污染（同题 runs 全命中时两者斜率同样低），此时候比值失效、以绝对判据为准。"
        "思考行为：no_reasoning=开关未产生思考输出；budget_exhausted=思考耗尽 max_tokens（正文 0 token）；"
        "normal=有思考草稿且正文正常。</div>".format(CACHE_EFFECTIVE_MS_PER_TOKEN)))
    # 结论建议
    rec_html = "".join('<p><b>【{}】</b>{}</p>'.format(esc(p), t) for p, t in recommendations)
    sec.append(("<h2>7 · 结论与建议</h2>", '<div class="good">{}</div>'.format(rec_html)))
    sec.append(("<h2>8 · 局限与备注</h2>", "<ul class='tight'>{}</ul>".format(
        "".join("<li>{}</li>".format(esc(l)) for l in limits))))
    sec.append(("<h2>9 · 数据质量</h2>", quality_block(data)))
    sec.append(("<h2>附录 A · 单发逐 run 明细</h2>",
                "<details><summary>展开</summary>{}</details>".format(appendix_single(data))
                if data["single"] else ""))
    sec.append(("<h2>附录 B · 多轮逐会话明细</h2>",
                "<details><summary>展开</summary>{}</details>".format(appendix_multiturn(data))
                if data["multiturn"] else ""))

    body = "".join(h + '\n' + b + "\n" for h, b in sec)
    chart_block = ("<script>\n" + GRID_JS + "\n" + "\n".join(stmts) + "\n</script>") if stmts else ""
    summary_block = ('<script type="application/json" id="perf-summary">\n{}\n</script>').format(
        summary_json(data, A, meta, conclusions, recommendations, limits))

    page = """<!DOCTYPE html><html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>__TITLE__</title>
<script>__LIB__</script>
<style>
:root{--ink:#111827;--line:#e5e7eb}
body{font-family:-apple-system,'PingFang SC','Microsoft YaHei',sans-serif;max-width:960px;margin:24px auto;padding:0 18px 80px;color:var(--ink);background:#f8fafc;line-height:1.7}
h1{font-size:25px;margin-bottom:4px} h2{font-size:19px;margin-top:44px;border-bottom:1px solid var(--line);padding-bottom:8px}
h3{font-size:15.5px;margin:20px 0 6px}
.sub{color:#6b7280;font-size:13px;margin-bottom:24px}
.kpis{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:12px;margin:18px 0}
.kpi{background:#fff;border:1px solid var(--line);border-radius:10px;padding:12px 14px}
.kpi-v{font-size:20px;font-weight:700;color:#1652f0}.kpi-l{font-size:12px;color:#777;margin-top:2px}
table{border-collapse:collapse;width:100%;font-size:12.5px;background:#fff;margin:10px 0}
th,td{border:1px solid var(--line);padding:5px 9px;text-align:left;white-space:nowrap}
th{background:#f1f5f9}
.rng{color:#6b7280;font-size:11px;white-space:nowrap}
.note{background:#fff8e6;border-left:4px solid #f0b429;padding:10px 14px;border-radius:4px;font-size:13.5px;margin:10px 0}
.finding{background:#eff6ff;border-left:4px solid #1652f0;padding:12px 16px;border-radius:4px;margin:10px 0}
.good{background:#ecfdf5;border-left:4px solid #0e9f6e;padding:12px 16px;border-radius:4px;margin:10px 0}
.chart{background:#fff;border:1px solid var(--line);border-radius:10px;padding:14px;margin:12px 0}
code{background:#f1f5f9;border-radius:4px;padding:1px 5px;font-size:12px}
details{background:#fff;border:1px solid var(--line);border-radius:10px;padding:10px 16px;margin:10px 0}
summary{cursor:pointer;font-size:13.5px;color:#374151}
.cap{font-size:13px;font-weight:600;margin:10px 0 0}
.tight li{margin:3px 0}
.foot{color:#6b7280;font-size:12px;margin-top:44px;border-top:1px solid var(--line);padding-top:12px}
</style></head><body>
<h1>__TITLE__</h1>
<p class="sub">__SUB__</p>
<div class="kpis">__KPI__</div>
__BODY__
<div class="foot">全部数字来自服务端 usage 与客户端逐 chunk 计时，原始 JSON 原始留存可复查。
本报告内嵌 <code>&lt;script id="perf-summary"&gt;</code> 数据块（聚合指标 + 自动观察）——将整份 HTML 交给 AI，
即可让它基于该数据块为报告追加通俗解读备注。</div>
__SUMMARY__
__CHARTS__
</body></html>"""
    with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "vendor", "chart.umd.min.js"),
              encoding="utf-8") as f:
        lib = f.read().replace("</script>", "<\\/script>")
    page = (page.replace("__LIB__", lib)
                .replace("__TITLE__", esc(title))
                .replace("__SUB__", sub)
                .replace("__KPI__", kpi_html)
                .replace("__BODY__", body)
                .replace("__SUMMARY__", summary_block)
                .replace("__CHARTS__", chart_block))
    out = os.path.join(out_dir, "llm-perf-报告.html")
    open(out, "w", encoding="utf-8").write(page)
    print("报告:", out)


def table_kv(rows):
    return "".join('<tr><th>{}</th><td>{}</td></tr>'.format(esc(k), esc(v)) for k, v in rows)


if __name__ == "__main__":
    main()
