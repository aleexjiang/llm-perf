#!/usr/bin/env python3
"""重建 v2 报告的图表脚本块：用 Python dict 生成 JSON（括号必然配平），
__GRIDOBJ__ 哨兵在 dumps 后替换为 {...grid} 展开。"""
import json, re

def spread(d=None):
    out = {"__GRIDOBJ__": 1}
    if d:
        out.update(d)
    return out

def chart(cid, ctype, labels, datasets, title, scales):
    cfg = {"type": ctype,
           "data": {"labels": labels, "datasets": datasets},
           "options": {"plugins": {"title": {"display": True, "text": title}},
                       "scales": scales}}
    js = json.dumps(cfg, ensure_ascii=False, separators=(",", ":"))
    js = js.replace('"__GRIDOBJ__":1', "...grid").replace('"__GRIDOBJ__"', "{...grid}")
    return "new Chart(document.getElementById('%s'),%s);" % (cid, js)

def bar(label, data, color, axis=None):
    d = {"label": label, "data": data, "backgroundColor": color}
    if axis:
        d["yAxisID"] = axis
    return d

def line(label, data, color, dash=None, axis=None):
    d = {"label": label, "data": data, "borderColor": color, "tension": 0.3}
    if dash:
        d["borderDash"] = dash
    if axis:
        d["yAxisID"] = axis
    return d

# ── c_v1 并发 TTFT P50 前后对照（对数轴）──
scales_v1 = {
    "x": spread(),
    "y": spread({"type": "logarithmic", "title": {"display": True, "text": "ms (对数)"}}),
}
datasets_v1 = [
    bar("TTFT P50 (on) seqs=2", [2145, 2097, 148818, 329408], "#c9c9de"),
    bar("TTFT P50 (on) seqs=4", [2161, 2092, 1981, 24629], "#7c5cff"),
    line("TTFT P50 (off) seqs=2", [2151, 2317, 8079, 22179], "#8a8aa8", [6, 4]),
    line("TTFT P50 (off) seqs=4", [2143, 1399, 2288, 10427], "#1652f0"),
]

# ── c_v2 并发吞吐（思考=on）──
scales_v2 = {
    "x": spread(),
    "y": spread({"title": {"display": True, "text": "tok/s"}}),
}
datasets_v2 = [
    bar("吞吐 (on) seqs=2", [55, 100, 92, 93], "#c9c9de"),
    bar("吞吐 (on) seqs=4", [55, 99, 171, 237], "#0e9f6e"),
]

# ── c_v3 单发冷/热 TTFT + prefill 速率（含 200K）──
y1_v3 = spread({"position": "right", "grid": {"display": False},
                "title": {"display": True, "text": "tok/s"}})
scales_v3 = {
    "x": spread(),
    "y": spread({"type": "logarithmic", "title": {"display": True, "text": "ms (对数)"}}),
    "y1": y1_v3,
}
datasets_v3 = [
    bar("冷 TTFT (ms)", [955, 2358, 4271, 8852, 28364, 66986], "#1652f0"),
    bar("热 TTFT (ms)", [588, 522, 540, 636, 884, 2035], "#0e9f6e"),
    line("冷 prefill 速率 (tok/s)", [4290, 4340, 4790, 4630, 3610, 3060], "#f0b429", axis="y1"),
]

# ── c_v4 多轮命中率/TTFT/实读量（三轴）──
y_v4 = spread({"min": 0, "max": 100, "position": "left",
               "title": {"display": True, "text": "命中率 %"}})
y1_v4 = spread({"position": "right", "grid": {"display": False},
                "title": {"display": True, "text": "ms"}})
y2_v4 = {"position": "right", "display": False}
scales_v4 = {"x": spread(), "y": y_v4, "y1": y1_v4, "y2": y2_v4}
datasets_v4 = [
    line("前缀缓存命中率 (%)", [0, 23, 53, 58, 61, 75, 75, 76], "#0e9f6e", axis="y"),
    line("TTFT (ms)", [1036, 1203, 957, 1102, 1166, 911, 1034, 1095], "#1652f0", axis="y1"),
    line("实读 tk（本轮实际 prefill 量）", [4786, 5290, 4226, 4693, 5027, 3737, 4266, 4611],
         "#f0b429", [6, 4], axis="y2"),
]

stmts = [
    chart("c_v1", "bar", ["并发1", "并发2", "并发4", "并发8"], datasets_v1,
          "并发 TTFT P50：max-num-seqs 2→4 前后（对数轴）", scales_v1),
    chart("c_v2", "bar", ["并发1", "并发2", "并发4", "并发8"], datasets_v2,
          "并发吞吐（思考=on）：92→171 / 93→237 tok/s", scales_v2),
    chart("c_v3", "bar", ["4K", "10K", "20K", "40K", "100K", "200K"], datasets_v3,
          "单发单轮（第二轮，真冷启动）：冷/热 TTFT 与 prefill 速率", scales_v3),
    chart("c_v4", "line", ["t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8"], datasets_v4,
          "多轮：命中率爬坡 vs TTFT 压平 vs 实读量锯齿（第一轮，第二轮一致）", scales_v4),
]

block = ("<script>\nconst grid={ticks:{color:'#999'},grid:{color:'#f0f0f5'}};\n"
         + "\n".join(stmts) + "\n</script>")

p = "output/真机评测报告-Qwen3.8-27B-20260905-v2.html"
s = open(p, encoding="utf-8").read()
s = re.sub(r"<script>\nconst grid=[\s\S]*?</script>", lambda m: block, s)
open(p, "w", encoding="utf-8").write(s)
open("/tmp/charts_new.js", "w").write(block.split("\n", 2)[2].rsplit("</script>", 1)[0])
print("rewritten")
