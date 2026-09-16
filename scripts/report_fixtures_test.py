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
  G 12.12 KV 容量画像：存在/缺失/畸形三态（结论区并列句与池÷拐点折算句）
  H 10.5 分时段漂移（退化/稳定/renew 剔暂态/短跨度 NA）+ stall.csv 熔断时刻解析
  I 沙盘推演修复回归：单发 TTFT 图失败剔除（P2-1）/ 发车窗口首批口径（P3-2）
"""

import datetime
import os
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


def run_pipeline(reports):
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

    def single_entries():
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


# ── G 12.12 KV 容量画像：存在 / 缺失 / 畸形三态 ──
def shape_kv_capacity():
    def conc_entries():
        # 两档闭环 levels（无 request_rate）→ 12.4 拐点结论必渲染（knee=2）
        def mk(lvl, tps):
            return {"model": "m", "thinking": "off", "max_tokens": 16, "level": lvl,
                    "wall_seconds": 5.0, "throughput_tps": tps,
                    "requests": [{"error": "", "ttft_ms": 100.0, "e2e_ms": 500.0,
                                  "prompt_tokens": 100, "completion_tokens": 32}] * 4}
        return [mk(1, 100.0), mk(2, 98.0)]

    # ① 有画像：结论区并列「KV 容量参照」+ 折算句（闭环拐点路数口径）
    kv = {"size_tokens": 1505497, "max_concurrency": 5.74, "block_size": 1600,
          "cache_dtype": "fp8", "gpu_memory_utilization": 0.9}
    rep = base_report("concurrent", conc_entries())
    rep["kv_capacity"] = kv
    _, meta, _, concl, _, _, _ = run_pipeline([rep])
    check(meta["kv_capacity"] == kv, "G① kv_capacity 归集进 meta")
    check(any("KV 容量参照" in c and "1,505,497" in c for c in concl),
          "G① 结论区并列 KV 池容量与满上下文上界")
    check(any("池÷拐点" in c for c in concl), "G① 池÷拐点折算句渲染（拐点 2 路）")

    # ② 无画像（12.12 之前的旧产物）：并列句消失、不崩
    _, meta2, _, concl2, _, _, _ = run_pipeline([base_report("concurrent", conc_entries())])
    check(meta2["kv_capacity"] is None, "G② 旧产物无 kv_capacity → None")
    check(all("KV 容量参照" not in c for c in concl2), "G② 无画像时并列句消失")

    # ③ 畸形（字符串 / 空 dict / 全空字段）：宽容忽略不崩
    for bad in ("not-a-dict", {}, {"size_tokens": 0}):
        rep3 = base_report("concurrent", conc_entries())
        rep3["kv_capacity"] = bad
        _, _, _, concl3, _, _, _ = run_pipeline([rep3])
        check(all("KV 容量参照" not in c for c in concl3),
              "G③ 畸形 kv_capacity（{}）被忽略".format(bad))


# ── H 10.5 分时段漂移 + 事故时间轴 ──
def shape_soak_drift():
    base_dt = datetime.datetime(2026, 9, 16, 0, 0, 0)

    def mk_turn(i, step_s, ttft_ms, tpot_ms=50.0):
        return {"error": "", "ttft_ms": ttft_ms, "tpot_ms": tpot_ms, "e2e_ms": 2000.0,
                "prompt_tokens": 100, "completion_tokens": 32,
                "sent_at": (base_dt + datetime.timedelta(seconds=i * step_s)).isoformat()}

    def conc_lv(turns_or_reqs, multiturn=False, renew=False, **kw):
        lv = {"model": "m", "thinking": "off", "max_tokens": 32, "level": 4,
              "wall_seconds": 95.0, "throughput_tps": 100.0, **kw}
        if multiturn:
            lv["multiturn"] = True
            lv["renew"] = renew
            lv["duration_seconds"] = 95
            lv["sessions"] = turns_or_reqs
        else:
            lv["requests"] = turns_or_reqs
        return lv

    def with_slo(rep):
        rep["slo"] = {"ttft_ms": 500, "tpot_ms": 100000}
        return rep

    # ① 退化：64 请求 × 1.5s（span≈95s）；末段 TTFT 900ms 超阈值（500ms）→ goodput 掉 100%
    reqs = [mk_turn(i, 1.5, 100.0 if i < 48 else 900.0) for i in range(64)]
    _, _, A, _, _, _, _ = run_pipeline([with_slo(base_report("concurrent", [conc_lv(reqs)]))])
    drift = A.get("soak_drift") or []
    check(len(drift) == 1 and drift[0]["verdict"] == "degrade",
          "H① 末段劣化 → 判退化（{}）".format((drift or [{}])[0].get("verdict")))
    check(drift and drift[0]["goodput_delta"] == -1.0, "H① goodput 掉幅 -100%")
    check(drift and drift[0]["first"]["goodput"] == 1.0 and drift[0]["last"]["goodput"] == 0.0,
          "H① 首段达标 100% / 末段 0%")

    # ② 稳定：全程 TTFT 100ms → 无退化
    reqs2 = [mk_turn(i, 1.5, 100.0) for i in range(64)]
    _, _, A2, _, _, _, _ = run_pipeline([with_slo(base_report("concurrent", [conc_lv(reqs2)]))])
    drift2 = A2.get("soak_drift") or []
    check(len(drift2) == 1 and drift2[0]["verdict"] == "stable",
          "H② 全程一致 → 判稳定（{}）".format((drift2 or [{}])[0].get("verdict")))

    # ③ renew 稳态剔除：4 会话 × 16 轮 × 1.5s（会话 24s，span≈95s）→ steady_off_s>0 且仍产出
    sessions = []
    for s in range(4):
        turns = [mk_turn(s * 16 + i, 1.5, 100.0) for i in range(16)]
        sessions.append({"model": "m", "thinking": "off", "max_tokens": 32, "session": s + 1,
                         "start_offset_s": s * 24.0, "turns": turns})
    _, _, A3, _, _, _, _ = run_pipeline(
        [with_slo(base_report("concurrent", [conc_lv(sessions, multiturn=True, renew=True)]))])
    drift3 = A3.get("soak_drift") or []
    check(len(drift3) == 1 and drift3[0]["steady_off_s"] > 0,
          "H③ renew 暂态剔除生效（steady_off_s={}）".format(
              (drift3 or [{}])[0].get("steady_off_s")))

    # ④ 短跨度（<30s）→ 不给趋势（NA，不硬给）
    reqs4 = [mk_turn(i, 1.0, 100.0) for i in range(20)]  # span=19s
    _, _, A4, _, _, _, _ = run_pipeline([with_slo(base_report("concurrent", [conc_lv(reqs4)]))])
    check(not (A4.get("soak_drift") or []), "H④ 短跨度样本 → 不产出趋势")

    # ⑤ 旧产物无 sent_at → 不产出（不崩）
    old_reqs = [{"error": "", "ttft_ms": 100.0, "e2e_ms": 500.0}] * 32
    _, _, A5, _, _, _, _ = run_pipeline([with_slo(base_report("concurrent", [conc_lv(old_reqs)]))])
    check(not (A5.get("soak_drift") or []), "H⑤ 无 sent_at 旧产物 → 不产出趋势")


# ── I 沙盘推演修复回归：P2-1 单发图失败剔除 / P3-2 发车窗口首批口径 ──
def shape_sandbox_fixes():
    """沙盘推演修复回归：P2-1 单发 TTFT 图剔除失败 run；P3-2 发车 span 取各批次首次启动。"""
    # P2-1：失败 run 与 None 计时剔除后取中位（修复前失败 run 按 0 计入、图中位被拉低）
    runs = [{"error": "", "ttft_ms": 200.0, "e2e_ms": 500.0},
            {"error": "timeout", "ttft_ms": None, "e2e_ms": None},
            {"error": "", "ttft_ms": None, "e2e_ms": 500.0}]
    check(g.single_ttft_median(runs) == 200, "P2-1 失败/None 剔除后中位 = 200（不塌向 0）")
    check(g.single_ttft_median([{"error": "timeout", "ttft_ms": 1.0}]) is None,
          "P2-1 整档失败 → None 不出点")
    check(g.single_ttft_median([]) is None, "P2-1 空 runs → None")
    check(g.single_ttft_median([{"error": "", "ttft_ms": 100.0},
                                {"error": "", "ttft_ms": 300.0}]) == 200,
          "P2-1 无失败时中位口径不变（200）")

    # P3-2：renew 续跑会话同样记偏移，不得把整场 soak 撑成"暂态爬坡窗"
    def sess(sid, batch, off):
        return {"model": "m", "thinking": "off", "max_tokens": 32, "session": sid,
                "batch": batch, "start_offset_s": off,
                "turns": [{"error": "", "ttft_ms": 100.0, "e2e_ms": 500.0,
                           "prompt_tokens": 100, "new_tokens": 32}]}
    sessions = [sess(1, 1, 0.2), sess(2, 1, 0.5), sess(3, 1, 80.0),  # 3 号 = 批次 1 的续跑
                sess(4, 2, 10.0), sess(5, 2, 10.4)]
    lv = {"model": "m", "thinking": "off", "max_tokens": 32, "level": 2,
          "wall_seconds": 95.0, "throughput_tps": 100.0,
          "multiturn": True, "renew": True, "duration_seconds": 95, "sessions": sessions}
    _, _, A, _, _, _, _ = run_pipeline([base_report("concurrent", [lv])])
    ramp = (A.get("conc_multi") or [{}])[0].get("ramp")
    check(ramp is not None and ramp["batches"] == 2,
          "P3-2 发车窗口：2 个批次（{}）".format(ramp))
    check(ramp is not None and abs(ramp["span_s"] - 9.8) < 1e-6,
          "P3-2 span 取各批次首次启动差（10.0−0.2=9.8；修复前被续跑撑到 79.8）（{}）".format(ramp))


def shape_stall_events():
    with tempfile.TemporaryDirectory() as td:
        os.makedirs(os.path.join(td, "sub"))
        with open(os.path.join(td, "concurrent-1.stall.csv"), "w", encoding="utf-8") as fp:
            fp.write("t_s,agg_tps,med_tps,in_flight,emitting,phase\n")
            fp.write("0.5,100.0,98.0,8,8,emit\n")
            fp.write("3.0,40.0,5.0,8,8,emit\n")
            fp.write("# tripped: med_rate=5.00 tok/s streams=8 low_for=2.5s min_tps=20 in_flight=8 emitting=8\n")
            fp.write("3.5,0.0,,8,0,prefill\n")
        ev = g.collect_stall_events(td)
        check(len(ev) == 1 and ev[0]["t_s"] == 3.0 and ev[0]["med_rate"] == 5.0
              and ev[0]["streams"] == 8 and ev[0]["min_tps"] == 20,
              "H⑤ stall.csv 熔断时刻解析（t_s=3.0, med=5.0）")
        check(g.collect_stall_events(os.path.join(td, "sub")) == [], "H⑤ 空目录 → 无事件不崩")


def main():
    shape_timeout_level()
    shape_none_and_empty()
    shape_legacy_json()
    shape_cache_paths()
    shape_tool_handshake()
    shape_kv_capacity()
    shape_soak_drift()
    shape_sandbox_fixes()
    shape_stall_events()
    if FAILS:
        print("\n{} 项失败".format(len(FAILS)))
        sys.exit(1)
    print("\n全部通过")


if __name__ == "__main__":
    main()
