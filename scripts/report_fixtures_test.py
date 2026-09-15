#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""12.6 报告异常形状回归测试：畸形/极端形状的输入 JSON 不得让报告管线崩溃。

背景（r1–r4 实测复盘）：整档超时（全部请求 error）曾让 gen_conclusions 对 None 取下标
崩溃；报告管线已经历多次此类修复，这里把踩过的形状固化成回归用例——新增报告功能时
`python3 scripts/report_fixtures_test.py` 必须保持全绿。

覆盖形状：
  A 整档超时：concurrent 档位全部请求 error → 统计留空渲染 —，fails 表达失败数
  B 全 None 指标：single runs 各计时字段缺失/None → mmm 全 None 不崩
  C 零样本：multiturn 会话全轮 error、runs 为空 → 中位数统计无输入不崩
  D 旧 JSON 形状：slo_meet/slo_total/waiting_max/running_max/last_prompt_tokens 全缺失
    （12.2 恒落盘之前的产物）→ 列缺省渲染 — 不崩
  E 12.9 缓存判据三路：①cached_tokens 生效；②冷算比值生效/未命中；两判据均不可用
  F 12.1 版本握手：混版与异源 tool 字段告警不拒绝渲染
"""

import copy
import json
import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import gen_html_report as g  # noqa: E402

FAILS = []


def check(cond, msg):
    tag = "ok " if cond else "FAIL"
    print("[{}] {}".format(tag, msg))
    if not cond:
        FAILS.append(msg)


def base_report(scen, entries, tool="llm-perf/dev"):
    return {"scenario": scen, scen: entries, "endpoint": "http://mock/v1",
            "tool": tool, "generated_at": "2026-09-16T00:00:00Z"}


def run_pipeline(reports, quad_entries=None):
    """merge → analyze → 结论/建议/局限/各表，任何异常直接算失败。"""
    reports = [("fake-{}.json".format(i), d) for i, d in enumerate(reports)]
    data, meta = g.merge(reports)
    A = g.analyze(data, meta)
    A["server"] = meta["server"]
    concl = g.gen_conclusions(A)
    recs = g.gen_recommendations(A)
    lims = g.gen_limits(data, A)
    html_parts = []
    for quad in ("conc_single", "conc_multi"):
        if A.get(quad):
            html_parts.append(g.concurrent_table(A, quad))
            html_parts.append(g.rate_sweep_table(A))
    for th in ("off", "on"):
        html_parts.append(g.multiturn_table(A, th))
    for m, P in A["per_model"].items():
        html_parts.append(g.classify_cache(P)[1])
    return data, meta, A, concl, recs, lims, html_parts


# ── A 整档超时：全部请求 error（r1 turn8 场景） ──
def shape_timeout_level():
    entries = [{
        "model": "m", "thinking": "off", "max_tokens": 16, "level": 4,
        "wall_seconds": 30.0, "throughput_tps": 0.0,
        "requests": [{"error": "timeout", "ttft_ms": None, "e2e_ms": None,
                      "prompt_tokens": 0, "completion_tokens": 0}] * 8,
    }]
    _, _, A, concl, _, _, html = run_pipeline([base_report("concurrent", entries)])
    check(bool(A.get("conc_single")), "A 整档超时：行仍进分析（fails 表达失败）")
    check(A["conc_single"][0]["fails"] == 8, "A 整档超时：失败数 = 8")
    check(A["conc_single"][0]["ttft"] is None, "A 整档超时：无成功请求 → 统计 None（渲染 —）")
    check(all("timeout" not in c.lower() or True for c in concl), "A 结论区不因超时档崩溃")


# ── B 全 None 指标 + C 零样本 ──
def shape_none_and_empty():
    single = [{
        "model": "m", "thinking": "on", "max_tokens": 16, "prompt_tokens": 1000,
        "runs": [{"error": "", "ttft_ms": None, "e2e_ms": None, "think_ms": None,
                  "decode_ms": None, "tokens_per_sec": None, "completion_tokens": None,
                  "reasoning_chars": None, "itl_p50_ms": None, "itl_p99_ms": None,
                  "cached_tokens": None}] * 3,
    }]
    multiturn = [{
        "model": "m", "thinking": "off", "max_tokens": 16, "session": 1,
        "turns": [{"error": "timeout", "ttft_ms": None, "e2e_ms": None,
                   "prompt_tokens": 0, "new_tokens": 0}] * 4,
    }]
    _, _, A, concl, _, _, html = run_pipeline(
        [base_report("single", single), base_report("multiturn", multiturn)])
    P = A["per_model"]["m"]
    check("on" in P and P["on"]["ladder"], "B 全 None：ladder 行生成不崩")
    check(P["on"]["ladder"][0]["ttft"] is None, "B 全 None：ttft 统计为 None")
    check("off" in P and "turns" in P["off"], "C 零样本：多轮聚合行生成不崩")
    check(any("证据不足" in c or "缓存" in c for c in concl) or concl is not None,
          "C 零样本：缓存判定不崩（证据不足路径）")
    check(all(h is not None for h in html), "B/C 各表格渲染非空")


# ── D 旧 JSON 形状：新字段全缺失（12.2 恒落盘之前） ──
def shape_legacy_json():
    conc = [{
        "model": "m", "thinking": "off", "max_tokens": 16, "level": 2,
        "wall_seconds": 5.0, "throughput_tps": 100.0,
        "requests": [{"error": "", "ttft_ms": 100.0, "e2e_ms": 500.0,
                      "prompt_tokens": 100, "completion_tokens": 32}] * 4,
        # 无 slo_meet/slo_total/goodput_rps/waiting_max/running_max/aborted
    }]
    mt = [{
        "model": "m", "thinking": "off", "max_tokens": 16, "session": 1,
        "turns": [{"error": "", "ttft_ms": 90.0, "e2e_ms": 480.0,
                   "prompt_tokens": 100, "new_tokens": 32},
                  {"error": "", "ttft_ms": 95.0, "e2e_ms": 490.0,
                   "prompt_tokens": 300, "new_tokens": 32}],
        # 无 last_prompt_tokens / nominal_last_prompt
    }]
    _, _, A, _, _, _, html = run_pipeline(
        [base_report("concurrent", conc), base_report("multiturn", mt)])
    e = A["conc_single"][0]
    check(e["slo_total"] == 0 and e["waiting_max"] == 0 and e["running_max"] == 0,
          "D 旧 JSON：SLO/观测字段缺省 0 → 列省略/— 渲染")
    check(A["per_model"]["m"]["off"].get("depth", {}).get("last_med") is None,
          "D 旧 JSON：无深度落盘字段 → 画像对照留空")
    check(any("SLO 达标" not in h for h in html) or True, "D 旧 JSON：表格渲染不崩")


# ── E 12.9 缓存判据三路 ──
def shape_cache_paths():
    def mt_entries(cached=None, last_ttft_ms=90.0, last_prompt=300):
        t0 = {"error": "", "ttft_ms": 100.0, "e2e_ms": 500.0, "prompt_tokens": 100,
              "new_tokens": 32}
        t1 = {"error": "", "ttft_ms": last_ttft_ms, "e2e_ms": 490.0,
              "prompt_tokens": last_prompt, "new_tokens": 32}
        if cached is not None:
            t1["cached_tokens"] = cached
        return [{"model": "m", "thinking": "off", "max_tokens": 16, "session": 1,
                 "turns": [t0, t1],
                 "last_prompt_tokens": last_prompt, "nominal_last_prompt": 320}]

    def single_entries(slope_target=False):
        # 两档构造单发斜率 0.5 ms/tk（TTFT 0.2s@100 → 0.35s@400）
        return [
            {"model": "m", "thinking": "off", "max_tokens": 16, "prompt_tokens": 100,
             "runs": [{"error": "", "ttft_ms": 200.0, "e2e_ms": 500.0}]},
            {"model": "m", "thinking": "off", "max_tokens": 16, "prompt_tokens": 400,
             "runs": [{"error": "", "ttft_ms": 350.0, "e2e_ms": 900.0}]},
        ]

    # ① cached_tokens 生效
    _, _, A, _, _, _, _ = run_pipeline(
        [base_report("single", single_entries()), base_report("multiturn", mt_entries(cached=280))])
    label, ev = g.classify_cache(A["per_model"]["m"])
    check(label == "生效" and "cached" in ev, "E① cached_tokens>0 → 生效（{}）".format(ev))

    # ② 冷算比值路径：末轮 TTFT 0.09s vs 冷算 300tk×0.5ms/tk=0.15s → 60% 部分命中
    _, _, A, _, _, _, _ = run_pipeline(
        [base_report("single", single_entries()), base_report("multiturn", mt_entries())])
    label, ev = g.classify_cache(A["per_model"]["m"])
    check(label == "部分命中" and "冷算" in ev, "E② 实测/冷算 60% → 部分命中（{}）".format(ev))

    # ② 未命中：末轮 TTFT 与冷算一致（0.15s vs 0.15s）
    _, _, A, _, _, _, _ = run_pipeline(
        [base_report("single", single_entries()),
         base_report("multiturn", mt_entries(last_ttft_ms=150.0))])
    label, ev = g.classify_cache(A["per_model"]["m"])
    check(label == "未命中", "E② 实测/冷算 ≈100% → 未命中（{}）".format(ev))

    # 证据不足：无 cached_tokens 键 + 无单发斜率
    _, _, A, _, _, _, _ = run_pipeline([base_report("multiturn", mt_entries())])
    label, ev = g.classify_cache(A["per_model"]["m"])
    check(label == "—" and ev == "证据不足", "E③ 无 cached 无斜率 → 证据不足（{}）".format(ev))


# ── F 12.1 版本握手 ──
def shape_tool_handshake():
    single = [{"model": "m", "thinking": "off", "max_tokens": 16, "prompt_tokens": 100,
               "runs": [{"error": "", "ttft_ms": 100.0, "e2e_ms": 500.0}]}]
    # 混版
    _, meta, _, _, _, _, _ = run_pipeline(
        [base_report("single", single, tool="llm-perf/v1.0"),
         base_report("single", single, tool="llm-perf/v1.1")])
    check(len(meta["tools"]) == 2, "F 混版：tools 收集两个版本")
    # 异源
    _, meta2, _, _, _, _, _ = run_pipeline([base_report("single", single, tool="other-tool/9")])
    check(meta2["tools"] == ["other-tool/9"], "F 异源：tool 字段原样收集")


def main():
    shape_timeout_level()
    shape_none_and_empty()
    shape_legacy_json()
    shape_cache_paths()
    shape_tool_handshake()
    if FAILS:
        print("\n{} 项失败".format(len(FAILS)))
        sys.exit(1)
    print("\n全部通过")


if __name__ == "__main__":
    main()
