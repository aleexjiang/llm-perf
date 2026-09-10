#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""llm-perf JSON → 自包含 HTML 分析报告。

用法:
  python3 gen_html_report.py output/                      # 目录模式：合并目录下全部 single-/multiturn-/concurrent-*.json
  python3 gen_html_report.py output/<模型子目录>/          # 按模型出报告：只合并该模型目录下的 JSON（推荐对外交付口径）
  python3 gen_html_report.py a.json b.json [标题]          # 文件模式：显式指定一份或多份报告 JSON

输出: <输入目录>/llm-perf-报告.html（Chart.js 内嵌，离线可用）

特性:
  - 自动合并同场景多份 JSON（例如 thinking=off / on 分开的测试），不丢变体
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
# 每模型占 2 色位（off/on 分色，见 color_of）——10 色支持 5 模型不撞色
PALETTE = ["#1652f0", "#0e9f6e", "#f0b429", "#e5484d", "#7c5cff", "#0ca678",
           "#d6409f", "#f97316", "#0891b2", "#65a30d"]

GRID_JS = "const grid={ticks:{color:'#999'},grid:{color:'#f0f0f5'}};"

# 多模型落地页 + 视图切换器：一进来只显示两张模型卡片与指标口径；点卡片进入该模型专属视图
# （表格行/列过滤、Chart.js 数据集过滤、空小节隐藏）；"← 返回首页"链接回落地页。
SWITCHER_JS = """(function(){
var MODELS=__MODELS__;
if(!document.getElementById('cards'))return;
function modelOf(text){for(var i=0;i<MODELS.length;i++){if(text.indexOf(MODELS[i])>=0)return MODELS[i];}return '';}
var tables=[];
document.querySelectorAll('table').forEach(function(tb){
  var head=tb.rows[0];if(!head)return;
  var cols=[],hits=0,single='',seen={},distinct=0;
  for(var c=0;c<head.cells.length;c++){var m=modelOf(head.cells[c].textContent);if(m){hits++;if(!seen[m]){seen[m]=1;distinct++;single=m;}}cols.push(m);}
  var info={tb:tb,colMap:distinct>=2?cols:null,rows:[],tag:distinct===1?single:''};
  if(!info.colMap){
    var prev=info.tag;
    for(var r=0;r<tb.rows.length;r++){
      var hit=modelOf(tb.rows[r].textContent);
      if(hit)prev=hit;
      info.rows.push({tr:tb.rows[r],m:hit||prev});
    }
  }
  tables.push(info);
});
var caps=[];
document.querySelectorAll('p.cap').forEach(function(p){
  caps.push({p:p,m:modelOf(p.textContent),next:p.nextElementSibling});
});
var kcards=[];
document.querySelectorAll('.kpi').forEach(function(k){
  kcards.push({el:k,m:modelOf(k.textContent)});
});
var lis=[];
document.querySelectorAll('li').forEach(function(li){
  var txt=li.textContent,m=modelOf(txt),multi=false;
  if(m){for(var i=0;i<MODELS.length;i++){if(MODELS[i]!==m&&txt.indexOf(MODELS[i])>=0)multi=true;}}
  lis.push({el:li,m:m,multi:multi});
});
var charts=[];
if(window.Chart&&Chart.instances){Object.keys(Chart.instances).forEach(function(k){
  var ch=Chart.instances[k];
  var labels=(ch.data.labels||[]).map(String);
  var byIndex=labels.some(function(l){return MODELS.indexOf(l)>=0;});
  var dsMap=ch.data.datasets.map(function(ds){return modelOf(ds.label||'');});
  charts.push({ch:ch,labels:labels,byIndex:byIndex,dsMap:dsMap,
    datasets:ch.data.datasets.map(function(ds){return{ds:ds,data:(ds.data||[]).slice()};})});
});}
function tidy(){
  document.querySelectorAll('h2,h3').forEach(function(h){
    var lvl=+h.tagName[1],node=h.nextElementSibling,empty=true,guard=0;
    while(node&&guard++<300){
      var t=node.tagName;
      if(t==='H2'||(t==='H3'&&+t[1]<=lvl))break;
      var vis=node.style.display!=='none';
      var skip=(lvl===3&&t==='DIV'&&node.className==='note');
      if(vis&&!skip){empty=false;break;}
      node=node.nextElementSibling;
    }
    h.style.display=empty?'none':'';
  });
}
var h1=document.querySelector('h1');
var baseTitle=h1.textContent;
var cards=document.getElementById('cards');
var kpis=document.querySelector('.kpis');
var foot=document.querySelector('.foot');
var secs=[];
document.querySelectorAll('h2').forEach(function(h){
  var els=[],node=h.nextElementSibling,guard=0;
  while(node&&guard++<500){if(node.tagName==='H2')break;els.push(node);node=node.nextElementSibling;}
  secs.push({h:h,els:els,metric:h.textContent.indexOf('指标口径')>=0});
});
function setTitle(sel){
  var t=sel?sel+'测试报告':baseTitle;
  h1.textContent=t;document.title=t;
}
var backlink=document.getElementById('backhome');
function apply(sel){
  var home=!sel;
  if(backlink)backlink.style.display=home?'none':'block';
  var home=!sel;
  secs.forEach(function(s){
    var show=home?s.metric:true;
    s.h.style.display=show?'':'none';
    s.els.forEach(function(el){if(el.tagName!=='SCRIPT')el.style.display=show?'':'none';});
  });
  if(kpis)kpis.style.display=home?'none':'';
  if(cards)cards.style.display=home?'':'none';
  if(foot)foot.style.display=home?'none':'';
  if(home){
    charts.forEach(function(rec){rec.ch.canvas.parentNode.style.display='none';});
    setTitle(sel);
    return;
  }
  tables.forEach(function(info){
    if(info.colMap){
      for(var r=0;r<info.tb.rows.length;r++){
        var tr=info.tb.rows[r];
        for(var c=0;c<tr.cells.length;c++){
          var cm=info.colMap[c];
          tr.cells[c].style.display=(sel&&cm&&cm!==sel)?'none':'';
        }
      }
    }else{
      info.rows.forEach(function(ro){ro.tr.style.display=(sel&&ro.m&&ro.m!==sel)?'none':'';});
    }
    var any=false;
    for(var r2=0;r2<info.tb.rows.length;r2++){if(info.tb.rows[r2].style.display!=='none'){any=true;break;}}
    var hideTable=(sel&&info.tag&&info.tag!==sel)||!any;
    info.tb.style.display=hideTable?'none':'';
  });
  caps.forEach(function(cp){
    var hide=!!sel&&!!cp.m&&cp.m!==sel;
    cp.p.style.display=hide?'none':'';
    if(cp.next&&(cp.next.tagName==='TABLE'||cp.next.tagName==='DETAILS')&&hide)cp.next.style.display='none';
  });
  kcards.forEach(function(k){k.el.style.display=(sel&&k.m&&k.m!==sel)?'none':'';});
  lis.forEach(function(r){r.el.style.display=(sel&&r.m&&(r.m!==sel||r.multi))?'none':'';});
  charts.forEach(function(rec){
    var ch=rec.ch;
    if(rec.byIndex){
      var idx=[];
      for(var i=0;i<rec.labels.length;i++){if(!sel||MODELS.indexOf(rec.labels[i])<0||rec.labels[i]===sel)idx.push(i);}
      ch.data.labels=idx.map(function(i){return rec.labels[i];});
      rec.datasets.forEach(function(r){r.ds.data=idx.map(function(i){return r.data[i];});});
      ch.update();ch.resize();
      ch.canvas.parentNode.style.display=idx.length?'':'none';
    }else{
      var keep=[];
      rec.datasets.forEach(function(r,i){if(!sel||!rec.dsMap[i]||rec.dsMap[i]===sel)keep.push(r.ds);});
      ch.data.datasets=keep;
      ch.update();ch.resize();
      ch.canvas.parentNode.style.display=keep.length?'':'none';
    }
  });
  setTitle(sel);
  tidy();
}
if(backlink)backlink.addEventListener('click',function(ev){ev.preventDefault();apply('');});
if(cards)document.querySelectorAll('.mcard').forEach(function(c){
  c.addEventListener('click',function(){apply(c.getAttribute('data-mv'));});
});
apply('');
})();"""


# ────────────────────────── 输入加载 ──────────────────────────

# 四象限场景：single=单发·单轮 multiturn=单发·多轮 conc-single=并发·单轮 conc-multi=并发·多轮
QUADS = {"single": "单发单轮", "multiturn": "单发多轮", "conc-single": "并发单轮", "conc-multi": "并发多轮"}
SCEN_ALIASES = {"单发单轮": "single", "单发多轮": "multiturn", "并发单轮": "conc-single",
                "并发多轮": "conc-multi", "全部": "all", "合并": "all", "all": "all"}


def parse_scenarios(spec):
    """--scenarios 取值：逗号分隔的象限名（支持中文别名）；all/缺省 = 四象限合并。"""
    toks = [t.strip() for t in (spec or "all").replace("，", ",").replace("+", ",").split(",") if t.strip()]
    out = set()
    for t in toks:
        t = SCEN_ALIASES.get(t, t)
        if t == "all":
            return set(QUADS)
        if t not in QUADS:
            sys.exit("--scenarios 无效值 {!r}（可选：single=单发单轮 multiturn=单发多轮 "
                     "conc-single=并发单轮 conc-multi=并发多轮 all=合并；支持中文别名）".format(t))
        out.add(t)
    return out or set(QUADS)


def lv_quad(lv):
    """并发等级条目属于哪个象限：有 sessions=多轮会话；有 requests=单轮。"""
    if lv.get("multiturn") or (lv.get("sessions") and "requests" not in lv):
        return "conc-multi"
    return "conc-single"


def filter_scenarios(data, sel):
    out = {k: [] for k in SCENARIOS}
    if "single" in sel:
        out["single"] = data["single"]
    if "multiturn" in sel:
        out["multiturn"] = data["multiturn"]
    for lv in data["concurrent"]:
        if lv_quad(lv) in sel:
            out["concurrent"].append(lv)
    return out


# 配置原文里的密钥形态：行级脱敏——匹配敏感键的整行值。
# 不做值内匹配的原因：`authorization: Bearer sk-xxx` 的值是两个词（值级正则只会吃掉
# "Bearer" 把 key 留下）、引号/冒号变体各异、auth_header 等自定义键名各有形态——
# 行级 `键: 整行值` 一律 REDACT 才不漏。
_SECRET_RE = re.compile(
    r'(?im)^(\s*-?\s*(?:api[_-]?key|api[_-]?token|authorization|auth[_-]?header|'
    r'access[_-]?token|secret|password|token)\s*[:=]\s*).+$')


def redact_secrets(text):
    """config_raw 原文可能内嵌客户 key，而报告是要外发的——落 HTML 前必须脱敏。"""
    return _SECRET_RE.sub(r'\1***REDACTED***', text)


def load_inputs(argv):
    """返回 (reports, title, out_dir, scenarios)。reports: [(路径, dict)]"""
    paths, title = [], None
    out_dir = None
    sel_spec = None
    rest = list(argv)
    i = 0
    while i < len(rest):
        a = rest[i]
        if a == "--scenarios":
            sel_spec = rest[i + 1]
            i += 2
            continue
        if a.startswith("--scenarios="):
            sel_spec = a.split("=", 1)[1]
            i += 1
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
        i += 1
    if not paths:
        sys.exit("没有输入：请给 output 目录或 .json 文件路径")
    reports = [(p, json.load(open(p, encoding="utf-8"))) for p in paths]
    return reports, title, out_dir, parse_scenarios(sel_spec)


def merge(reports):
    """合并多份同场景报告 → {scenario: [entries...]}，并收集元信息。"""
    data = {k: [] for k in SCENARIOS}
    meta = {"endpoint": "?", "tool": "?", "notes": [], "generated": [], "slo": None}
    for p, d in reports:
        scen = d.get("scenario")
        if scen not in data:
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
        if d.get("environment"):
            meta["environment"] = d["environment"]
        if d.get("config_raw"):
            meta["config_raw"] = redact_secrets(d["config_raw"])
        if d.get("plan"):
            meta["plan"] = d["plan"]
    return data, meta


# ────────────────────────── 统计辅助 ──────────────────────────

def mmm(vals, nd=2):
    """(median, min, max) 或 None。"""
    vals = [v for v in vals if v is not None]
    if not vals:
        return None
    return (round(st.median(vals), nd), round(min(vals), nd), round(max(vals), nd))


# p95/p99 最小样本量：低于它的分位数是纯噪音（n=3 时 p99 甚至超过 max），
# 宁可留空也不硬算。并发场景 level × runs_per_worker 通常 ≥ 20 才出分位。
MIN_PCT_SAMPLE = 20

# ── 体验基线（3 档制，5.8）：判据与出处见 docs/latency-baselines.md §7 ──
# 档1/档2（短输入 ≤4K）= MLPerf Interactive / Server 直引；档3（≥24K agent 大上下文）= 推导值
# TPOT 与输入长度无关（decode 只看逐 token 生成），各档共用 40/200ms
SLO_TIERS = {
    "short_max_tokens": 4000,   # 短输入档上界
    "long_min_tokens": 24000,   # agent 大上下文档下界（默认边界，未来可由 slo: 配置覆盖）
    "short_good_ttft": 0.45,    # s，MLPerf Interactive p99
    "short_pass_ttft": 2.0,     # s，MLPerf Server p99（锚定 ~240 wpm 阅读速度）
    "long_good_ttft": 3.0,      # s，推导值：particula 10K 实测 0.75–1.6s 线性外推
    "long_pass_ttft": 6.0,      # s，推导值：MLPerf 405B 档 6s @9.4K 作宽松上限佐证
    "good_tpot": 40.0,          # ms，ITL p99（MLPerf Interactive）
    "pass_tpot": 200.0,         # ms（MLPerf Server）
    "good_tps": 25.0,           # 单请求输出速度 tok/s（>30–50 超过所有读者感知，再快无感）
    "pass_tps": 10.0,           # 阅读速度 ~5–6 tok/s × 2 安全系数
}
BUCKET_LABEL = {"short": "≤4K", "mid": "4–24K", "long": "≥24K"}


def pct9599(vals, nd=2):
    """(p95, p99) 或 None（样本不足）。线性插值口径（与 numpy 默认一致）。"""
    vals = sorted(v for v in vals if v is not None)
    if len(vals) < MIN_PCT_SAMPLE:
        return None

    def p(q):
        k = (len(vals) - 1) * q
        f = int(k)
        c = min(f + 1, len(vals) - 1)
        if f == c:
            return vals[f]
        return vals[f] + (vals[c] - vals[f]) * (k - f)

    return (round(p(0.95), nd), round(p(0.99), nd))


def fpct(v, nd=2):
    """p95/p99 表格单元格；样本不足时明示，而非留空让人误以为没算。"""
    if v is None:
        return '<span class="rng">n&lt;{}</span>'.format(MIN_PCT_SAMPLE)
    return "{:.{nd}f} / {:.{nd}f}".format(v[0], v[1], nd=nd)


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
    """single: model×thinking×max_tokens → {size: entry}; multiturn: model×thinking×max_tokens → [session...]
    max_tokens 是输出长度扫描维度（配置可为列表多档），同 (model, thinking) 下可有多组输出档。"""
    s_by, m_by = defaultdict(dict), defaultdict(list)
    for e in data["single"]:
        # trace 模式下 prompt_tokens 是 len(content)/4 的估算值，不同会话可能撞档位——
        # 直接覆盖会丢会话数据，改为合并 runs（同估算档位视为同桶）
        bucket = s_by[(e["model"], e.get("thinking", "off"), e.get("max_tokens", 0))].setdefault(
            e["prompt_tokens"], {"runs": []})
        bucket["runs"].extend(e.get("runs", []))
    for e in data["multiturn"]:
        m_by[(e["model"], e.get("thinking", "off"), e.get("max_tokens", 0))].append(e)
    c_lvls = list(data["concurrent"])
    models = []
    for (m, _, _) in list(s_by) + list(m_by):
        if m not in models:
            models.append(m)
    for lv in c_lvls:
        if lv["model"] not in models:
            models.append(lv["model"])
    return s_by, m_by, c_lvls, models


def mts_of(d, model, thinking):
    """某 model×thinking 下出现过的输出长度档位（max_tokens，升序）。"""
    return sorted({mt for (m, t, mt) in d if m == model and t == thinking})


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
        "has_conc_single": any(not lv.get("multiturn") and not lv.get("sessions") for lv in c_lvls),
        "has_conc_multi": any(lv.get("multiturn") or lv.get("sessions") for lv in c_lvls),
    }

    # 并发四象限聚合：每个 model×thinking×level 一行
    for quad in ("conc_single", "conc_multi"):
        A[quad] = []
    for lv in c_lvls:
        quad = "conc_multi" if (lv.get("multiturn") or lv.get("sessions")) else "conc_single"
        if lv.get("multiturn") or lv.get("sessions"):
            turns = [t for sess in lv.get("sessions", []) for t in sess.get("turns", [])]
            n_units = len(lv.get("sessions", []))
        else:
            turns = lv.get("requests", [])
            n_units = len(turns)
        if not turns:
            continue
        # 失败请求（error 字段，如网关 504）：无 ttft/有效计时，混进统计会把中位数
        # 拉向 0（幸存者偏差的反向污染）——一律剔除，只报失败数。
        # 整档全失败时 stats 留空（渲染为 "—"，fails 表达失败数），
        # 不回退到含失败的全集——否则一整行 0 值进中位 = 0，看起来像"极快"。
        ok_turns = [t for t in turns if not t.get("error")]
        n_fails = len(turns) - len(ok_turns)
        src = ok_turns
        A[quad].append({
            "model": lv["model"],
            "thinking": lv.get("thinking", "off"),
            "mt": lv.get("max_tokens", 0),
            "level": lv.get("level", 0),
            "n_units": n_units,
            "n_turns": len(turns),
            "fails": n_fails,
            "wall": lv.get("wall_seconds"),
            "tps": lv.get("throughput_tps"),
            "ttft": mmm([t["ttft_ms"] / 1000 for t in src if t.get("ttft_ms") is not None], 2),
            "e2e": mmm([t["e2e_ms"] / 1000 for t in src if t.get("e2e_ms") is not None], 1),
            "ttft_p": pct9599([t["ttft_ms"] / 1000 for t in src if t.get("ttft_ms") is not None], 2),
            "e2e_p": pct9599([t["e2e_ms"] / 1000 for t in src if t.get("e2e_ms") is not None], 1),
            "think": mmm([(t.get("think_ms") or 0) / 1000 for t in src], 1),
            "tokps": mmm([t.get("tokens_per_sec") for t in src], 0),
            "finish": sorted({t.get("finish_reason", "?") for t in src}),
            "shapes": lv.get("shapes") or [],  # 5.6 混合负载：形状分解（非空 = 混跑轮）
            "request_rate": lv.get("request_rate", 0),
        })

    for m in models:
        P = {}
        # 单发各 thinking 变体（max_tokens 可为多档输出长度扫描，按输出档分组）
        for th in ("off", "on"):
            mts = mts_of(s_by, m, th)
            if not mts:
                continue
            ladder = []  # 全部（输出档 × 输入档）组合，表格用
            for mt in mts:
                for size in sorted(s_by[(m, th, mt)]):
                    rs_all = s_by[(m, th, mt)][size]["runs"]
                    # 失败 run（如 HTTP 504）剔除后再统计——与并发侧口径一致，防 ttft=0 拉低中位数；
                    # 整档全失败不回退全集（stats 为空渲染 "—"，fails/n 表达失败规模）
                    rs = [r for r in rs_all if not r.get("error")]
                    ladder.append({
                        "size": size,
                        "mt": mt,
                        "fails": len(rs_all) - len([r for r in rs_all if not r.get("error")]),
                        "ttft": mmm([r["ttft_ms"] / 1000 for r in rs if r.get("ttft_ms") is not None], 2),
                        "ttft_content": mmm([r["ttft_content_ms"] / 1000 for r in rs if r.get("ttft_content_ms") is not None], 2),
                        "ttft_rea": mmm([r["ttft_reasoning_ms"] / 1000 for r in rs if r.get("ttft_reasoning_ms") is not None], 2),
                        "think": mmm([(r.get("think_ms") or 0) / 1000 for r in rs], 1),
                        "e2e": mmm([r["e2e_ms"] / 1000 for r in rs if r.get("e2e_ms") is not None], 1),
                        "decode": mmm([r["decode_ms"] / 1000 for r in rs if r.get("decode_ms") is not None], 1),
                        "itl_p50": mmm([r.get("itl_p50_ms") for r in rs], 1),
                        "itl_p99": mmm([r.get("itl_p99_ms") for r in rs], 1),
                        "tokps": mmm([r.get("tokens_per_sec") for r in rs], 0),
                        "comp": mmm([r.get("completion_tokens") for r in rs], 0),
                        "rc": mmm([r.get("reasoning_chars", 0) for r in rs], 0),
                        "finish": sorted({r.get("finish_reason", "?") for r in rs}),
                        "n": len(rs_all),
                    })
            P.setdefault(th, {})["ladder"] = ladder
            P[th]["mt_list"] = mts
            # 斜率/decode 基准取最大输出档组：输出越长 decode 爬坡段占比越小，
            # TTFT 与 tok/s 越接近该输入规模下的成熟段读数（测量方法论 5.1）
            P[th]["ladder_top"] = [l for l in ladder if l["mt"] == mts[-1]]
            P[th]["slope"] = slope_ms_per_token(
                [(l["size"], l["ttft"][0]) for l in P[th]["ladder_top"] if l["ttft"]]) if len(P[th]["ladder_top"]) >= 2 else None
            P[th]["tokps"] = mmm([l["tokps"][0] for l in P[th]["ladder_top"] if l["tokps"]], 0)
            P[th]["finish_length"] = sum(1 for l in ladder if "length" in l["finish"])
            P[th]["e2e_all"] = [r["e2e_ms"] / 1000 for mt in mts
                                for size in sorted(s_by[(m, th, mt)])
                                for r in s_by[(m, th, mt)][size]["runs"]
                                if not r.get("error") and r.get("e2e_ms") is not None]
            P[th]["think_all"] = [(r.get("think_ms") or 0) / 1000 for mt in mts
                                  for size in sorted(s_by[(m, th, mt)])
                                  for r in s_by[(m, th, mt)][size]["runs"]
                                  if not r.get("error")]
            P[th]["rc_all"] = [r.get("reasoning_chars", 0) for mt in mts
                               for size in sorted(s_by[(m, th, mt)])
                               for r in s_by[(m, th, mt)][size]["runs"]
                               if not r.get("error")]
            P[th]["no_content_runs"] = [r for mt in mts
                                        for size in sorted(s_by[(m, th, mt)])
                                        for r in s_by[(m, th, mt)][size]["runs"]
                                        if not r.get("error") and
                                        (r.get("content_chunks") == 0 or r.get("thinking_no_content"))]
            # run1 vs run2+ TTFT（缓存冷/热形态）
            firsts, rests = [], []
            for mt in mts:
                for size in sorted(s_by[(m, th, mt)]):
                    rs = [r for r in s_by[(m, th, mt)][size]["runs"] if not r.get("error")]
                    if rs:
                        firsts.append(rs[0].get("ttft_ms") or 0)
                        rests.extend(r.get("ttft_ms") or 0 for r in rs[1:])
            if firsts and rests:
                P[th]["cold_warm_ratio"] = (st.median(firsts) / max(st.median(rests), 1e-9))
        # 多轮（max_tokens 可为多档输出长度扫描，按输出档分组聚合）
        for th in ("off", "on"):
            mts = mts_of(m_by, m, th)
            if not mts:
                continue
            turns_by_mt = {}
            for mt in mts:
                sess = m_by[(m, th, mt)]
                nturn = max(len(s["turns"]) for s in sess)
                turns = []
                for i in range(nturn):
                    ts_all = [s["turns"][i] for s in sess if i < len(s["turns"])]
                    # 失败轮剔除后再统计（与并发/单发口径一致）
                    ts = [t for t in ts_all if not t.get("error")] or ts_all
                    turns.append({
                        "i": i + 1,
                        "prompt": (min(t.get("prompt_tokens", 0) for t in ts),
                                   max(t.get("prompt_tokens", 0) for t in ts)),
                        "new": st.median([t.get("new_tokens", 0) for t in ts]),
                        "ttft": mmm([t["ttft_ms"] / 1000 for t in ts if t.get("ttft_ms") is not None], 2),
                        "think": mmm([(t.get("think_ms") or 0) / 1000 for t in ts], 1),
                        "e2e": mmm([t["e2e_ms"] / 1000 for t in ts if t.get("e2e_ms") is not None], 1),
                        "sess_ttft": [t.get("ttft_ms") or 0 for t in ts],
                        "sess_e2e": [t.get("e2e_ms") or 0 for t in ts],
                    })
                turns_by_mt[mt] = turns
            P.setdefault(th, {})["turns_by_mt"] = turns_by_mt
            # 同单发：逐轮斜率与会话总时长取最大输出档组
            top = turns_by_mt[mts[-1]]
            P[th]["turns"] = top
            P[th]["slope_multi"] = slope_ms_per_token(
                [(t["prompt"][0], t["ttft"][0]) for t in top if t["ttft"]]) if len(top) >= 2 else None
            P[th]["mt_total"] = sum(t["e2e"][0] for t in top if t["e2e"])
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

    # ── 5.8 体验基线评估单元（3 档制；判据与出处 docs/latency-baselines.md §7）──
    # TTFT 按输入档池化（跨输出档——TTFT 与输出长度无关）；TPOT/tok/s 取最大输出档组
    # （输出越长 decode 爬坡段占比越小，与 ladder_top 的口径一致）
    def bucket_of(p):
        if p <= SLO_TIERS["short_max_tokens"]:
            return "short"
        if p >= SLO_TIERS["long_min_tokens"]:
            return "long"
        return "mid"

    def prompt_lbl(ps):
        lo, hi = min(ps), max(ps)
        return "{:,}".format(lo) if lo == hi else "{:,}–{:,}".format(lo, hi)

    def mk_unit(scene, model, th, bkt, lbl, ttfts, itls, tpss, n):
        ttfts = [v for v in ttfts if v]
        itls = [v for v in itls if v]
        tpss = [v for v in tpss if v]
        pp = pct9599(ttfts)
        return {
            "scene": scene, "model": model, "thinking": th, "bucket": bkt,
            "prompt": lbl, "n": n,
            "ttft_med": round(st.median(ttfts), 2) if ttfts else None,
            "ttft_p99": pp[1] if pp else None,  # 样本 < MIN_PCT_SAMPLE 时退回中位数判级
            "itl_p99": round(st.median(itls), 1) if itls else None,
            "tps": round(st.median(tpss), 0) if tpss else None,
        }

    base = []
    for m, P in A["per_model"].items():
        for th in ("off", "on"):
            if th not in P:
                continue
            mts = P[th]["mt_list"]
            # 单发·单轮
            for bkt in ("short", "mid", "long"):
                groups = []
                for mt in mts:
                    for size in sorted(s_by[(m, th, mt)]):
                        if bucket_of(size) != bkt:
                            continue
                        # 显式剔除失败 run（n=成功数；不依赖 mk_unit 内 if v 的隐式滤 0）
                        rs = [r for r in s_by[(m, th, mt)][size]["runs"] if not r.get("error")]
                        if rs:
                            groups.append((mt, size, rs))
                if not groups:
                    continue
                ttfts = [r.get("ttft_ms", 0) / 1000 for _, _, rs in groups for r in rs]
                top_mt = max(mt for mt, _, _ in groups)
                top_rs = [r for mt, _, rs in groups if mt == top_mt for r in rs]
                base.append(mk_unit(
                    "单发·单轮", m, th, bkt, prompt_lbl([size for _, size, _ in groups]),
                    ttfts, [r.get("itl_p99_ms") for r in top_rs],
                    [r.get("tokens_per_sec") for r in top_rs], len(ttfts)))
            # 单发·多轮：逐轮按该轮 prompt 归档（会话越深输入越大）；失败轮显式剔除
            top_mt = mts[-1]
            turns = [t for sess in m_by[(m, th, top_mt)] for t in sess["turns"] if not t.get("error")]
            for bkt in ("short", "mid", "long"):
                ts = [t for t in turns if bucket_of(t.get("prompt_tokens", 0)) == bkt]
                if not ts:
                    continue
                base.append(mk_unit(
                    "单发·多轮", m, th, bkt, prompt_lbl([t.get("prompt_tokens", 0) for t in ts]),
                    [t.get("ttft_ms", 0) / 1000 for t in ts],
                    [t.get("itl_p99_ms") for t in ts],
                    [t.get("tokens_per_sec") for t in ts], len(ts)))
    # 并发：均匀轮按请求 prompt 归档；混跑轮逐形状评估（形状即输入档，只有中位数）
    for lv in c_lvls:
        is_mt = bool(lv.get("multiturn") or lv.get("sessions"))
        kind = "多轮" if is_mt else "单轮"
        load = "rate{}/s".format(lv["request_rate"]) if lv.get("request_rate") else "L{}".format(lv.get("level", 0))
        if lv.get("shapes"):
            scene = "并发混跑·{} {}".format(kind, load)
            for s in lv["shapes"]:
                base.append(mk_unit(scene, lv["model"], lv.get("thinking", "off"),
                                    bucket_of(s["prompt_tokens"]), "{:,}".format(s["prompt_tokens"]),
                                    [s["ttft_s"]] if s["ttft_s"] else [], [],
                                    [s["tok_s"]] if s["tok_s"] else [], s["count"]))
            continue
        turns = ([t for sess in lv.get("sessions", []) for t in sess.get("turns", [])] if is_mt
                 else lv.get("requests", []))
        turns = [t for t in turns if not t.get("error")]  # 失败请求显式剔除（n=成功数）
        scene = "并发·{} {}".format(kind, load)
        for bkt in ("short", "mid", "long"):
            ts = [t for t in turns if bucket_of(t.get("prompt_tokens", 0)) == bkt]
            if not ts:
                continue
            base.append(mk_unit(scene, lv["model"], lv.get("thinking", "off"), bkt,
                                prompt_lbl([t.get("prompt_tokens", 0) for t in ts]),
                                [t.get("ttft_ms", 0) / 1000 for t in ts],
                                [t.get("itl_p99_ms") for t in ts],
                                [t.get("tokens_per_sec") for t in ts], len(ts)))
    A["baseline"] = base

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


def color_of(models, m, variant=0):
    # 每模型占 2 个色位：variant=0（thinking=off/主线）、1（thinking=on/副线），
    # 同一图表内同模型多条线不再撞色
    return PALETTE[(models.index(m) * 2 + variant) % len(PALETTE)]


def build_charts(data, A):
    s_by, m_by, c_lvls, models = organize(data)
    stmts, canvases = [], []

    def add(cid, title):
        canvases.append((cid, title))

    # 单发 TTFT vs 档位（每个 thinking 变体一张图，每模型×输出档一条线）
    for th in ("off", "on"):
        sizes = sorted({size for (m2, t, _mt) in s_by for size in s_by[(m2, t, _mt)] if t == th})
        if not sizes:
            continue
        ds = []
        for m in models:
            mts = mts_of(s_by, m, th)
            for i_mt, mt in enumerate(mts):
                ys = []
                for size in sizes:
                    e = s_by[(m, th, mt)].get(size)
                    ys.append(round(st.median([r.get("ttft_ms", 0) for r in e["runs"]])) if e else None)
                lbl = "{} (thinking={})".format(short(m), th)
                if len(mts) > 1:
                    lbl += " out={}".format(mt)
                ds.append(line_ds(lbl, ys, color_of(models, m), dash=[5, 4] if i_mt > 0 else None))
        scales = {"x": spread({"title": {"display": True, "text": "prompt tokens（目标档位）"}}),
                  "y": spread({"title": {"display": True, "text": "TTFT ms"}})}
        stmts.append(chart_js("c_s_ttft_" + th, "line", [str(s) for s in sizes], ds,
                              "单发 TTFT vs 上下文档位（3 runs 中位数）", scales))
        add("c_s_ttft_" + th, "单发 TTFT vs 档位（thinking={}）".format(th))

    # 多轮 TTFT 逐轮（每个 thinking 变体一张图，输出档多时按输出档分线）
    for th in ("off", "on"):
        keys = [(m, t, mt) for (m, t, mt) in m_by if t == th]
        if not keys:
            continue
        multi_mt = len({k[2] for k in keys}) > 1
        nturn = max(len(s["turns"]) for k in keys for s in m_by[k])
        labels = ["T{}".format(i + 1) for i in range(nturn)]
        ds = []
        cnt_m = {}
        for (m, t, mt) in keys:
            if t != th:
                continue
            ys = []
            for i in range(nturn):
                vals = [s["turns"][i].get("ttft_ms") for s in m_by[(m, t, mt)] if i < len(s["turns"])
                        and s["turns"][i].get("ttft_ms")]
                ys.append(round(st.median(vals)) if vals else None)
            lbl = short(m) + (" out={}".format(mt) if multi_mt else "")
            pi = cnt_m.get(m, 0)
            cnt_m[m] = pi + 1
            ds.append(line_ds(lbl, ys, color_of(models, m), dash=[5, 4] if pi > 0 else None))
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
        labels, dmin, dmed, dmax, dthink = [], [], [], [], []
        for m, lo, md, hi in on_e2e:
            labels.append(short(m))
            dmin.append(round(lo, 1)); dmed.append(round(md, 1)); dmax.append(round(hi, 1))
            th_all = A["per_model"][m]["on"].get("think_all") or []
            dthink.append(round(st.median(th_all), 1) if th_all else None)
        ds = [line_ds("最快", dmin, "#93c5fd"), line_ds("中位", dmed, "#1652f0"),
              line_ds("最慢", dmax, "#e5484d"), line_ds("思考时长中位", dthink, "#f59e0b")]
        scales = {"x": spread(), "y": spread({"title": {"display": True, "text": "秒"}})}
        stmts.append(chart_js("c_on_e2e", "bar", labels, ds,
                              "thinking=on 单次请求耗时与思考时长（全部 run 汇总）", scales))
        add("c_on_e2e", "thinking=on E2E 与思考时长分布")

    # 并发四象限：吞吐 & TTFT vs 并发（每象限一张图，每模型×thinking 一条线）
    for quad, cid_tps, cid_ttft, qname in (
            ("conc_single", "c_cs_tps", "c_cs_ttft", "并发·单轮"),
            ("conc_multi", "c_cm_tps", "c_cm_ttft", "并发·多轮")):
        items = A.get(quad) or []
        if not items:
            continue
        levels = sorted({e["level"] for e in items if e["level"]})
        if not levels:
            continue
        groups = sorted({(e["model"], e["thinking"], e["mt"]) for e in items})
        multi_mt = len({g[2] for g in groups}) > 1
        pair_i = {}
        ds_tps, ds_ttft = [], []
        for m, th, mt in groups:
            ys_tps, ys_ttft = [], []
            for lv in levels:
                es = [e for e in items if e["model"] == m and e["thinking"] == th
                      and e["mt"] == mt and e["level"] == lv]
                if es:
                    ys_tps.append(round(sum(x["tps"] or 0 for x in es) / len(es), 1))
                    tts = [x["ttft"][0] for x in es if x["ttft"]]
                    med = st.median(tts) if tts else None  # 全失败档位为空列表，不喂 None 进 median
                    ys_ttft.append(round(med, 2) if med is not None else None)
                else:
                    ys_tps.append(None)
                    ys_ttft.append(None)
            if any(v is not None for v in ys_tps):
                lbl = "{} (thinking={})".format(short(m), th)
                if multi_mt:
                    lbl += " out={}".format(mt)
                pi = pair_i.get((m, th), 0)
                pair_i[(m, th)] = pi + 1
                col = color_of(models, m, 1 if th == "on" else 0)
                ds_tps.append(line_ds(lbl, ys_tps, col, dash=[5, 4] if pi > 0 else None))
                ds_ttft.append(line_ds(lbl, ys_ttft, col, dash=[5, 4] if pi > 0 else None))
        if ds_tps:
            scales = {"x": spread({"title": {"display": True, "text": "并发数"}}),
                      "y": spread({"title": {"display": True, "text": "吞吐 tok/s"}})}
            stmts.append(chart_js(cid_tps, "line", [str(l) for l in levels], ds_tps,
                                  "{}：吞吐 vs 并发（陡升转平=饱和点）".format(qname), scales))
            add(cid_tps, "{} 吞吐 vs 并发".format(qname))
        if ds_ttft:
            scales = {"x": spread({"title": {"display": True, "text": "并发数"}}),
                      "y": spread({"title": {"display": True, "text": "TTFT 秒"}})}
            stmts.append(chart_js(cid_ttft, "line", [str(l) for l in levels], ds_ttft,
                                  "{}：TTFT vs 并发（上翘=开始排队）".format(qname), scales))
            add(cid_ttft, "{} TTFT vs 并发".format(qname))

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
            fail_cell = str(l.get("fails") or 0)
            if l.get("fails"):
                fail_cell = '<span style="color:#e5484d;font-weight:600">{}</span>'.format(l["fails"])
            row = [esc(short(m)), str(l["mt"]), "{:,}".format(l["size"]), str(l["n"]), fail_cell,
                   f3(l["ttft"]), f3(l["ttft_content"])]
            if th == "on":
                # 思考占比 = 思考总时长 / E2E（思考与正文输出交错，占比为口径近似）
                pct = "{:.0%}".format(l["think"][0] / l["e2e"][0]) if l["e2e"][0] else "—"
                row += [f1(l["think"]), pct]
            row += [f1(l["e2e"]), f1(l["decode"]),
                    f1(l["itl_p50"], "ms") if has_itl else "—",
                    f1(l["itl_p99"], "ms") if has_itl else "—",
                    f0(l["tokps"]), f0(l["comp"]),
                    f0(l["rc"]) if th == "on" else "—",
                    " / ".join(l["finish"])]
            rows.append(row)
    head = ["模型", "输出 tk", "档位 tk", "runs", "失败", "TTFT s", "首内容 s"] + \
           (["思考 s", "思考占比"] if th == "on" else []) + \
           ["E2E s", "decode s", "ITL p50 ms", "ITL p99 ms", "tok/s", "输出 tok"] + \
           (["思考字符"] if th == "on" else []) + ["finish"]
    return table(head, rows) if rows else "<p>无数据</p>"


def concurrent_table(A, quad):
    qname = "并发·多轮" if quad == "conc_multi" else "并发·单轮"
    has_think = any(e["thinking"] != "off" or (e["think"] and e["think"][0] > 0) for e in A[quad])
    head = ["模型", "thinking", "输出 tk", "并发", "单元数", "请求总数", "失败", "墙钟 s", "吞吐 tok/s",
            "TTFT s", "TTFT p95/p99 s", "E2E s", "E2E p95/p99 s"] + (["思考 s"] if quad == "conc_multi" else []) + \
           ["单请求 tok/s", "finish"]
    rows = []
    for e in sorted(A[quad], key=lambda x: (x["model"], x["thinking"], x["mt"], x["level"])):
        mt_cell = "mix({})".format("/".join(s["label"] for s in e["shapes"])) if e["shapes"] else str(e["mt"])
        fail_cell = str(e.get("fails") or 0)
        if e.get("fails"):
            fail_cell = '<span style="color:#e5484d;font-weight:600">{}</span>'.format(e["fails"])
        row = [esc(short(e["model"])), e["thinking"], mt_cell, str(e["level"]),
               "{:,}".format(e["n_units"]), "{:,}".format(e["n_turns"]), fail_cell,
               "{:.1f}".format(e["wall"]) if e["wall"] else "—",
               "{:.0f}".format(e["tps"]) if e["tps"] else "—",
               f3(e["ttft"]), fpct(e["ttft_p"], 2), f1(e["e2e"]), fpct(e["e2e_p"], 1)]
        if quad == "conc_multi":
            row.append(f1(e["think"]))
        row += [f0(e["tokps"]), " / ".join(e["finish"])]
        rows.append(row)
    note = '<div class="note">单元数：{}。TTFT/E2E 为该并发等级下成功请求的中位数（min–max）；' \
           'p95/p99 仅在成功请求数 ≥ {} 时计算——长短混跑时中位数可能几乎不动而 p99 数倍膨胀，' \
           '请对照 p95/p99 列判断尾部时延风险。</div>'.format(
        "独立多轮会话（每用户各自跑完整会话）" if quad == "conc_multi" else "独立单轮请求", MIN_PCT_SAMPLE)
    if any(e.get("fails") for e in A[quad]):
        note += '<div class="note" style="color:#b45309;font-weight:600">⚠️ 该象限存在失败请求（已从延迟统计剔除）——' \
                '对应档位的中位数/分位数仅基于幸存请求，真实体验比表中更差；失败明细见数据质量章。</div>'
    return table(head, rows) + note if rows else "<p>无数据</p>"


def shapes_table(A, quad):
    """混合负载（concurrent.mix）形状分解：每轮各形状的实测占比与中位数（5.6）。"""
    rows = []
    for e in sorted(A[quad], key=lambda x: (x["model"], x["thinking"], x["level"])):
        if not e["shapes"]:
            continue
        lv_cell = "rate={:.1f}/s".format(e["request_rate"]) if e["request_rate"] else str(e["level"])
        total = sum(s["count"] for s in e["shapes"]) or 1
        for s in e["shapes"]:
            rows.append([esc(short(e["model"])), e["thinking"], lv_cell,
                         esc(s["label"]), str(s["weight"]), str(s["count"]),
                         "{:.0%}".format(s["count"] / total),
                         "{:,}".format(s["prompt_tokens"]), "{:,}".format(s["max_tokens"]),
                         "{:.2f}s".format(s["ttft_s"]) if s["ttft_s"] else "—",
                         "{:.1f}s".format(s["e2e_s"]) if s["e2e_s"] else "—",
                         "{:,.0f}".format(s["tok_s"]) if s["tok_s"] else "—"])
    if not rows:
        return ""
    note = '<div class="note">混跑：请求形状按权重确定性交错发射（平滑加权轮转），占比为实测值；' \
           'TTFT/E2E/tok·s 为该形状内请求的中位数。短形状 TTFT 被长形状排队抬高的幅度 = 混跑尾延迟风险。</div>'
    return "<h4>形状分解</h4>" + table(
        ["模型", "thinking", "并发", "形状", "权重", "请求数", "占比", "prompt tk", "输出 tk", "TTFT s", "E2E s", "tok/s"],
        rows) + note


def multiturn_table(A, th):
    per_model = [(m, P) for m, P in A["per_model"].items()
                 if th in P and P[th].get("turns_by_mt")]
    if not per_model:
        return "<p>无数据</p>"
    mts = sorted({mt for _, P in per_model for mt in P[th]["turns_by_mt"]})
    parts = []
    for mt in mts:
        head = ["轮次", "prompt tok 范围", "新增 tok"]
        cols = []
        for m, P in per_model:
            turns = P[th]["turns_by_mt"].get(mt)
            if not turns:
                continue
            cols.append((m, turns))
            head += ["{} TTFT s".format(short(m))]
            head += ["{} 思考 s".format(short(m))] if th == "on" else []
            head += ["{} E2E s".format(short(m))]
        if not cols:
            continue
        nturn = max(len(turns) for _, turns in cols)
        rows = []
        for i in range(nturn):
            row = ["T{}".format(i + 1), "", ""]
            first = True
            for m, turns in cols:
                if i < len(turns):
                    t = turns[i]
                    if first:
                        row[1] = "{:,}–{:,}".format(*t["prompt"])
                        row[2] = "{:,.0f}".format(t["new"])
                        first = False
                    row += [f3(t["ttft"])]
                    row += [f1(t["think"])] if th == "on" else []
                    row += [f1(t["e2e"])]
                else:
                    row += ["—"] + (["—"] if th == "on" else []) + ["—"]
            rows.append(row)
        cap = '<p class="cap">输出 {} tk</p>'.format("{:,}".format(mt)) if len(mts) > 1 else ""
        parts.append(cap + table(head, rows))
    return "".join(parts) or "<p>无数据</p>"


def appendix_single(data):
    rows = []
    for e in data["single"]:
        for i, r in enumerate(e["runs"], 1):
            rows.append([e.get("thinking", "?"), esc(short(e["model"])),
                         str(e.get("max_tokens", 0)), "{:,}".format(e["prompt_tokens"]), str(i),
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
    return table(["thinking", "模型", "输出 tk", "档位", "run", "TTFT s", "首内容 s", "E2E s", "ITLavg ms",
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
        parts.append('<p class="cap">{} · thinking={} · out={} tk · session {}（{} 轮）</p>{}'.format(
            esc(short(sess["model"])), sess.get("thinking", "?"), sess.get("max_tokens", 0),
            sess.get("session", "?"),
            len(sess["turns"]),
            table(["轮次", "prompt tok", "新增 tok", "TTFT s", "E2E s", "ITL p50 ms", "思考字符", "finish"], rows)))
    return "".join(parts)


# ────────────────────────── 结论/建议/局限（数据条件生成） ──────────────────────────

def baseline_section(A):
    """5.8 体验基线评估（3 档制）：徽章 + 结论段 + 出处。基线不进退出码、不污染原始 JSON。"""
    base = A.get("baseline") or []
    if not base:
        return ""

    def badge(v, good, pas):
        """越低越好的指标（TTFT/ITL，单位 s 或 ms）。"""
        if v is None:
            return "—"
        if v <= good:
            return "✅"
        if v <= pas:
            return "⚠️"
        return "❌"

    def badge_hi(v, good, pas):
        """越高越好的指标（输出速度 tok/s）。"""
        if v is None:
            return "—"
        if v >= good:
            return "✅"
        if v >= pas:
            return "⚠️"
        return "❌"

    seen_notes = set()

    def note_once(txt):
        if txt not in seen_notes:
            seen_notes.add(txt)
            return "<li>{}</li>".format(txt)
        return ""

    note_html = []
    rows, summary = [], {"short": [], "long": []}
    for u in base:
        bkt = u["bucket"]
        # TTFT：优先 p99，样本不足退回中位数（标注口径）
        t_v = u["ttft_p99"] if u["ttft_p99"] is not None else u["ttft_med"]
        t_kind = "p99" if u["ttft_p99"] is not None else "中位"
        t_txt = "—" if t_v is None else "{:.2f}s（{}）".format(t_v, t_kind)
        t_b = "—"
        if u["thinking"] == "on":
            note_html.append(note_once(
                "thinking=on 的 TTFT 含思考时长（TTFAT 口径），不套用 TTFT 徽章；TPOT 与输出速度仍可判级。"))
        elif bkt == "mid":
            note_html.append(note_once("输入 4K–24K 区间两档之间无权威锚点，只报数值不打徽章。"))
        else:
            g = SLO_TIERS["short_good_ttft"] if bkt == "short" else SLO_TIERS["long_good_ttft"]
            p = SLO_TIERS["short_pass_ttft"] if bkt == "short" else SLO_TIERS["long_pass_ttft"]
            t_b = badge(t_v, g, p)
        tp_b = badge(u["itl_p99"], SLO_TIERS["good_tpot"], SLO_TIERS["pass_tpot"])
        if u["thinking"] == "on" and u["tps"] is not None:
            note_html.append(note_once(
                "tok/s 为整响应吞吐（thinking=on 时含思考 token），速度判级仅供参考。"))
        ts_b = badge_hi(u["tps"], SLO_TIERS["good_tps"], SLO_TIERS["pass_tps"])
        bs = [x for x in (t_b, tp_b, ts_b) if x != "—"]
        if not bs:
            verdict = "—"
        elif "❌" in bs:
            verdict = "❌ 未达标"
        elif "⚠️" in bs:
            verdict = "⚠️ 及格"
        else:
            verdict = "✅ 优"
        if bkt in summary:
            summary[bkt].append(verdict)
        rows.append([u["scene"], esc(short(u["model"])), u["thinking"], BUCKET_LABEL[bkt],
                     u["prompt"], "{:,}".format(u["n"]), t_txt, t_b,
                     "{:.0f}ms".format(u["itl_p99"]) if u["itl_p99"] is not None else "—", tp_b,
                     "{:.0f}".format(u["tps"]) if u["tps"] is not None else "—", ts_b, verdict])

    ps = []
    for bkt, name in (("long", "agent 大上下文（≥24K）"), ("short", "短输入（≤4K）")):
        vs = summary.get(bkt) or []
        if not vs:
            continue
        n_g, n_p, n_b = vs.count("✅ 优"), vs.count("⚠️ 及格"), vs.count("❌ 未达标")
        ps.append("<b>{}</b>：{} 项配置中 {} 优 / {} 及格 / {} 未达标。".format(name, len(vs), n_g, n_p, n_b))
    if any(u["bucket"] == "long" for u in base):
        ps.append("现代 agent 产品基线上下文即约 35K（系统提示 + 工具定义 + RAG 注入，用户发一句「你好」请求就已带 35K），"
                  "<b>agent 场景的 TTFT 体验主判据是 ≥24K 档</b>，短输入档徽章代表不了 agent 体验。")
        ps.append("暖路径提示：prefix cache 命中时大输入的 TTFT 只由新增 token 决定，会显著好于档位数字——"
                  "≥24K 档判的是冷 prefill 最坏角落；命中率的实测见第 7 节缓存判定。")
    note_html.append("<li>判级口径：TTFT 优先 p99，样本不足 {} 时退回中位数（括号内标注）；"
                     "TPOT 以每请求 ITL p99 的中位数为代理。基线不进退出码、不影响原始数据。</li>"
                     .format(MIN_PCT_SAMPLE))
    note_html.append("<li>判据出处：<code>docs/latency-baselines.md</code> §7 —— "
                     "MLPerf Server（TTFT p99 ≤2s / TPOT ≤200ms，锚定 ~240 wpm 阅读速度）、"
                     "MLPerf Interactive（450ms / 40ms，基于 ChatGPT/Perplexity 实测修订）、"
                     "≥24K 档为推导值（particula 10K 实测 0.75–1.6s 线性外推 + MLPerf 405B 档 6s 上限佐证）。</li>")

    head = ["场景", "模型", "thinking", "输入档", "prompt tk", "样本", "TTFT", "TTFT 判级",
            "ITL p99", "TPOT 判级", "tok/s", "速度判级", "综合"]
    html_tbl = table(head, rows)
    concl = '<div class="finding">{}</div>'.format("".join("<p>{}</p>".format(x) for x in ps)) if ps else ""
    notes = '<ul class="tight">{}</ul>'.format("".join(note_html))
    return concl + html_tbl + '<div class="note">' + notes + "</div>"


def gen_conclusions(A):
    cs = []
    # 1 prefill 扩展性（斜率取自最大输出档组 ladder_top，避免多输出档混线）
    for m, P in A["per_model"].items():
        if "off" in P and P["off"].get("slope") is not None and len(P["off"].get("ladder_top") or []) >= 2:
            l0, l1 = P["off"]["ladder_top"][0], P["off"]["ladder_top"][-1]
            sl = P["off"]["slope"]
            if sl < CACHE_EFFECTIVE_MS_PER_TOKEN:
                cs.append("<b>{}</b>（单发·单轮）：TTFT 几乎不随档位变化（{}k→{}k 仅 {}s→{}s，≈{:.3f} ms/token）——"
                          "prefill 成本被前缀缓存掩盖。".format(
                    esc(short(m)), l0["size"] // 1000, l1["size"] // 1000, l0["ttft"][0], l1["ttft"][0], sl))
            else:
                cs.append("<b>{}</b>（单发·单轮）：TTFT 随档位线性增长，{}k={}s → {}k={}s（≈{:.2f} ms/token）。".format(
                    esc(short(m)), l0["size"] // 1000, l0["ttft"][0], l1["size"] // 1000, l1["ttft"][0], sl))
    # 2 缓存判定
    for m, P in A["per_model"].items():
        label, _ = classify_cache(P)
        if label == "—":
            continue
        r = P.get("cache_ratio")
        if label == "生效":
            cs.append("<b>{}</b>（多轮对话）：TTFT 逐轮几乎不涨（斜率 ≈ {:.3f} ms/token{}）⇒ 前缀缓存生效，"
                      "历史前缀跨轮复用。".format(
                esc(short(m)),
                P.get("off", P.get("on", {})).get("slope_multi") or 0,
                "，为单发斜率的 {:.0%}".format(r) if r is not None and r < 1 else ""))
        elif label == "未命中":
            if r is not None:
                cs.append("<b>{}</b>（多轮对话）：TTFT 斜率与单发一致（比值 {:.0%}）⇒ 未命中前缀缓存，每轮全量重算历史。".format(
                    esc(short(m)), r))
            else:
                sm = (P.get("off") or P.get("on") or {}).get("slope_multi")
                cs.append("<b>{}</b>（多轮对话）：多轮 TTFT 斜率 {:.3f} ms/token 超过绝对判据（{:.2f}）⇒ "
                          "未命中前缀缓存，每轮全量重算历史。".format(
                    esc(short(m)), sm if sm is not None else 0.0, CACHE_EFFECTIVE_MS_PER_TOKEN))
        else:
            if r is not None:
                cs.append("<b>{}</b>（多轮对话）：TTFT 斜率为单发的 {:.0%} ⇒ 前缀缓存部分命中。".format(esc(short(m)), r))
            else:
                cs.append("<b>{}</b>（多轮对话）：前缀缓存部分命中（比值数据不足，按斜率判定）。".format(esc(short(m))))
        # 冷/热形态佐证
        cw = P.get("off", {}).get("cold_warm_ratio")
        if cw and cw > 3:
            cs.append("{}（单发）同档位 run1 TTFT 约为 run2/3 的 {:.0f} 倍，符合缓存冷 miss 形态。".format(esc(short(m)), cw))
    # 3 decode
    # hi[1][0] 守卫：mock/异常数据下吞吐可为 0，直接除会 ZeroDivisionError
    decs = [(m, P.get("decode_tps")) for m, P in A["per_model"].items()
            if P.get("decode_tps") and P["decode_tps"][0] > 0]
    if len(decs) >= 2:
        decs_sorted = sorted(decs, key=lambda x: -x[1][0])
        hi, lo = decs_sorted[0], decs_sorted[-1]
        cs.append("<b>Decode 吞吐（单发·thinking=off）</b>：{} ≈ {:.0f} tok/s，{} ≈ {:.0f} tok/s（{} 约为 {} 的 {:.0f}%）。".format(
            esc(short(hi[0])), hi[1][0], esc(short(lo[0])), lo[1][0],
            esc(short(lo[0])), esc(short(hi[0])), lo[1][0] / hi[1][0] * 100))
    elif decs:
        cs.append("<b>Decode 吞吐（单发·thinking=off）</b>：{} ≈ {:.0f} tok/s。".format(esc(short(decs[0][0])), decs[0][1][0]))
    # 4 思考行为
    for m, P in A["per_model"].items():
        beh = P.get("thinking_behavior")
        if beh == "no_reasoning":
            cs.append("<b>{}</b>（单发·thinking=on）：<code>reasoning_content</code> 恒为空，TTFT 与 off 一致 ⇒ "
                      "思考开关在该模型上未产生独立思考输出，需核对网关参数透传。".format(esc(short(m))))
        elif beh == "budget_exhausted":
            n = len(P["on"].get("no_content_runs", []))
            cs.append("<b>{}</b>（单发·thinking=on）：出现 {} 次「思考独占输出预算」（正文 0 token，finish=length）；"
                      "思考输出 {:,}–{:,} 字符/次，E2E {}–{}s。".format(
                esc(short(m)), n, min(P["on"]["rc_all"]), max(P["on"]["rc_all"]),
                "{:.0f}".format(min(P["on"]["e2e_all"])), "{:.0f}".format(max(P["on"]["e2e_all"]))))
        elif beh == "normal":
            rc = P["on"]["rc_all"]; e2 = P["on"]["e2e_all"]
            n_exh = len(P["on"].get("no_content_runs", []))
            cs.append("<b>{}</b>（单发·thinking=on）：思考输出 {:,}–{:,} 字符/次，E2E {}–{}s（中位 {:.0f}s），"
                      "长草稿显著放大单次时延{}。".format(
                esc(short(m)), min(rc), max(rc), "{:.0f}".format(min(e2)), "{:.0f}".format(max(e2)), st.median(e2),
                "；其中 {} 次思考独占输出预算（正文 0 token）".format(n_exh) if n_exh else ""))
    # 5 并发扩展性（并发两象限）
    for quad, qname in (("conc_single", "并发·单轮"), ("conc_multi", "并发·多轮")):
        bymt = defaultdict(list)
        for e in A.get(quad, []):
            bymt[(e["model"], e["thinking"], e["mt"])].append(e)
        multi_mt = len({e["mt"] for e in A.get(quad, [])}) > 1
        for (m, th, mt), es in sorted(bymt.items()):
            th_lbl = th + ("·out={}tk".format(mt) if multi_mt else "")
            es.sort(key=lambda x: x["level"])
            lv_name = lambda e: e["level"] if e["level"] else "rate={:.1f}/s".format(e.get("request_rate") or 0)
            # 尾部时延风险：p99 显著高于中位数（≥3×）时单独提示——只看中位数会完全隐身
            for e in es:
                if e.get("ttft_p") and e["ttft_p"][1] > 3 * max(e["ttft"][0], 1e-9):
                    cs.append("<b>{}（{}·thinking={}，{}）</b>：TTFT 中位 {:.2f}s 但 p99 达 {:.2f}s"
                              "（{:.0f}×）——尾部请求（长短混跑/排队抖动）时延风险被中位数掩盖，"
                              "容量规划请按 p99 口径评估。".format(
                        esc(short(m)), qname, th_lbl, lv_name(e),
                        e["ttft"][0], e["ttft_p"][1], e["ttft_p"][1] / max(e["ttft"][0], 1e-9)))
            # 扩展性对比只取闭环档位：开环轮次 level=0（按 rate 分档），混入会破坏并发比计算
            es = [e for e in es if e["level"] > 0]
            if len(es) < 2 or not es[0]["tps"] or not es[-1]["tps"] or es[0]["level"] == es[-1]["level"]:
                continue
            scale = es[-1]["tps"] / es[0]["tps"]
            lv_ratio = es[-1]["level"] / es[0]["level"]
            ttft_infl = es[-1]["ttft"][0] / max(es[0]["ttft"][0], 1e-9)
            if scale / lv_ratio < 0.7:
                cs.append("<b>{}（{}·thinking={}）</b>：并发 {}→{} 吞吐仅 {:.1f}×（并发比 {:.0f}×）⇒ "
                          "扩展性受限（排队/抢占），TTFT 中位 {:.2f}s→{:.2f}s（{:.1f}×）。".format(
                    esc(short(m)), qname, th_lbl, es[0]["level"], es[-1]["level"], scale, lv_ratio,
                    es[0]["ttft"][0], es[-1]["ttft"][0], ttft_infl))
            else:
                cs.append("<b>{}（{}·thinking={}）</b>：并发 {}→{} 吞吐 {:.1f}×（并发比 {:.0f}×）近似线性，"
                          "TTFT 中位 {:.2f}s→{:.2f}s。".format(
                    esc(short(m)), qname, th_lbl, es[0]["level"], es[-1]["level"], scale, lv_ratio,
                    es[0]["ttft"][0], es[-1]["ttft"][0]))
    # 5.6 混合负载：形状间尾延迟差异 + 混跑 vs 均匀吞吐对比
    for quad, qname in (("conc_single", "并发·单轮"), ("conc_multi", "并发·多轮")):
        mix_es = [e for e in A.get(quad, []) if e.get("shapes")]

        def load_lbl(e):
            if e["level"] > 0:
                return "并发 {}".format(e["level"])
            if e.get("request_rate"):
                return "rate {}/s".format(e["request_rate"])
            return "开环"

        for e in mix_es:
            shs = sorted((s for s in e["shapes"] if s["ttft_s"] > 0), key=lambda s: s["ttft_s"])
            if len(shs) >= 2:
                lo, hi = shs[0], shs[-1]
                cs.append("<b>{}（{}·thinking={}，{}）</b>：混跑下 {}（{:,}tk）TTFT 中位 {:.2f}s vs "
                          "{}（{:,}tk）{:.2f}s（{:.1f}×）——短请求被长请求排队拖住，均匀负载测不出该差异；"
                          "容量规划请按混跑口径评估。".format(
                    esc(short(e["model"])), qname, e["thinking"], load_lbl(e),
                    esc(lo["label"]), lo["prompt_tokens"], lo["ttft_s"],
                    esc(hi["label"]), hi["prompt_tokens"], hi["ttft_s"],
                    hi["ttft_s"] / max(lo["ttft_s"], 1e-9)))
        # 对照项按 (model, thinking, level, request_rate) 匹配：开环轮次 level 全为 0，
        # 只按 level 匹配会把不同 rate 的均匀轮混在一起（rate_sweep 下只剩最后一条），
        # 混跑与均匀必须同 rate 才有可比性
        uni_by = {(e["model"], e["thinking"], e["level"], e["request_rate"]): e
                  for e in A.get(quad, [])
                  if not e.get("shapes") and e["tps"]}

        for e in mix_es:
            u = uni_by.get((e["model"], e["thinking"], e["level"], e["request_rate"]))
            if u and e["tps"]:
                ratio = e["tps"] / u["tps"]
                cs.append("<b>{}（{}·thinking={}，{}）</b>：混跑吞吐 {:.0f} tok/s 为均匀负载（{:,}tk）{:.0f} tok/s 的 "
                          "{:.0f}%{}——长短混跑放大排队与抢占开销，容量规划请以混跑数为基准。".format(
                    esc(short(e["model"])), qname, e["thinking"], load_lbl(e),
                    e["tps"], u["mt"], u["tps"], ratio * 100,
                    "（⚠️ 明显衰减）" if ratio < 0.85 else ""))
    # 6 agent 时延推算
    for m, P in A["per_model"].items():
        if "off" in P and P["off"].get("ladder_top"):
            top = P["off"]["ladder_top"][-1]
            if top["size"] >= 20000 and top["ttft"]:
                cs.append("<b>agent 场景推算（{}）</b>：单次响应 5–7 次模型调用、每次携带全量上下文，"
                          "按单发 {}k TTFT {:.1f}s 计，仅首字等待累计即 {}–{}s。".format(
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
    top_sizes = [P["off"]["ladder_top"][-1]["size"] for P in A["per_model"].values()
                 if "off" in P and P["off"].get("ladder_top")]
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
            lim.append("off 场景 {}% 的 run finish=length（输出上限 ≈{:,} tok，输出长度被钳制）——E2E / decode 列在 off 场景不可跨"
                       "模型或跨思考变体比较；TTFT / ITL / tok/s 不受影响。".format(
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
        n_fail = sum(1 for r in runs if r.get("error"))
        if n_fail:
            ekinds = sorted({(r.get("error") or "").split(":")[0].strip() for r in runs if r.get("error")})
            p += "；<span style=\"color:#e5484d;font-weight:600\">⚠️ {} 条失败（{}）</span>" \
                 "——失败请求未计入延迟统计，相关档位中位数为幸存者口径，并按网关侧超时上限截断" \
                 "（见 E2E 是否钉在整 300s 附近）".format(n_fail, "、".join(esc(x) for x in ekinds))
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
        "baseline": A.get("baseline", []),  # 5.8 体验基线评估单元（3 档制）
        "events": {k: (len(v) if isinstance(v, list) else v) for k, v in A["events"].items()},
        "per_model": {short(m): clean(P) for m, P in A["per_model"].items()},
        "conclusions": [re.sub(r"<[^>]+>", "", c) for c in conclusions],
        "recommendations": [{"priority": p, "text": re.sub(r"<[^>]+>", "", t)} for p, t in recommendations],
        "limitations": [re.sub(r"<[^>]+>", "", l) for l in limits],
    }
    return json.dumps(clean(doc), ensure_ascii=False, indent=1)


# ────────────────────────── 主流程 ──────────────────────────

def main():
    reports, title, out_dir, scenarios = load_inputs(sys.argv[1:])
    data, meta = merge(reports)
    data = filter_scenarios(data, scenarios)
    if not any(data[k] for k in SCENARIOS):
        sys.exit("输入中没有可用场景数据（过滤条件 --scenarios={}）".format("+".join(sorted(scenarios))))
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
        if "off" in P and P["off"].get("ladder_top"):
            l0, l1 = P["off"]["ladder_top"][0], P["off"]["ladder_top"][-1]
            kpis.append(("单发 TTFT @{}k ({})".format(l1["size"] // 1000, short(m)),
                         "{:.2f} s".format(l1["ttft"][0]) if l1["ttft"] else "—"))
        if P.get("decode_tps"):
            kpis.append(("Decode 单发 ({})".format(short(m)), "{:.0f} tok/s".format(P["decode_tps"][0])))
        if "on" in P and P["on"].get("e2e_all"):
            kpis.append(("思考 E2E 中位·单发 ({})".format(short(m)),
                         "{:.0f} s".format(st.median(P["on"]["e2e_all"]))))
    for quad, qname in (("conc_single", "并发单轮"), ("conc_multi", "并发多轮")):
        bym = defaultdict(list)
        for e in A.get(quad, []):
            bym[(e["model"], e["thinking"], e["mt"])].append(e)
        multi_mt = len({e["mt"] for e in A.get(quad, [])}) > 1
        for (m, th, mt), es in sorted(bym.items()):
            top = max(es, key=lambda x: x["level"])
            if top["tps"]:
                lbl_q = qname + ("·out={}tk".format(mt) if multi_mt else "")
                kpis.append(("吞吐@L{}·{} ({})".format(top["level"], lbl_q, short(m)),
                             "{:.0f} tok/s".format(top["tps"])))
    kpi_html = "".join('<div class="kpi"><div class="kpi-v">{}</div><div class="kpi-l">{}</div></div>'.format(
        esc(v), esc(l)) for l, v in kpis)

    # 章节
    sec = []
    sec.append(("<h2>1 · 摘要</h2>", '<div class="finding"><ol class="tight">{}</ol></div>'.format(
        "".join("<li>{}</li>".format(c) for c in conclusions))))
    cov = []
    if A["coverage"]["has_single"]:
        cov.append("单发·单轮（档位矩阵 × runs）")
    if A["coverage"]["has_multiturn"]:
        cov.append("单发·多轮（history 逐轮滚动）")
    if A["coverage"]["has_conc_single"]:
        cov.append("并发·单轮（独立单轮请求）")
    if A["coverage"]["has_conc_multi"]:
        cov.append("并发·多轮（每用户独立多轮会话）")
    sec2_body = table_kv([("端点", meta["endpoint"]), ("工具版本", meta["tool"]),
                          ("场景覆盖", "；".join(cov)),
                          ("请求总数", str(n_req))])
    # 环境存档：压测时自动探测的引擎信息 + 配置原文（复现"当时是什么配置跑的"）
    env = meta.get("environment")
    if env:
        env_rows = [["引擎猜测", env.get("engine_guess") or "—"],
                    ["Server 头", env.get("server_header") or "—"]]
        if env.get("model_max_len"):
            env_rows.append(["max_model_len", "{:,}".format(env["model_max_len"])])
        if env.get("models"):
            env_rows.append(["模型列表", "、".join(env["models"][:8]) + ("…" if len(env["models"]) > 8 else "")])
        sec2_body += "<h3>引擎环境（压测时自动探测）</h3>" + table(["项目", "值"], env_rows)
    else:
        sec2_body += '<div class="note">本份数据未包含引擎环境存档（旧版本工具产出）。</div>'
    sec.append(("<h2>2 · 测试配置与方法</h2>", sec2_body))
    sec.append(("<h2>3 · 指标口径</h2>", table(
        ["指标", "定义"],
        [["TTFT", "请求发出 → 首个流式 chunk（空首包不计）；本报告单位秒"],
         ["首内容", "请求发出 → 首个 content chunk（TTFT_content）"],
         ["思考时长", "模型思考输出的总时长（think_ms）；TTFT 在 thinking=on 时即思考首包"],
         ["思考占比", "思考时长 ÷ E2E（思考与正文输出可能交错，口径近似）"],
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
        sec.append(("<h2>4 · 单发·单轮结果</h2>", body))
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
        sec.append(("<h2>5 · 单发·多轮结果</h2>", body))
    # 并发（并发·单轮 / 并发·多轮）
    if A["coverage"]["has_concurrent"]:
        body = ""
        for quad, qname, cids in (("conc_single", "6.1 并发·单轮（独立单轮请求）", ("c_cs_tps", "c_cs_ttft")),
                                  ("conc_multi", "6.2 并发·多轮（每用户独立多轮会话）", ("c_cm_tps", "c_cm_ttft"))):
            if not A.get(quad):
                continue
            body += "<h3>{}</h3>".format(qname)
            for cid in cids:
                if any(c == cid for c, _ in canvases):
                    body += '<div class="chart"><canvas id="{}" height="110"></canvas></div>'.format(cid)
            body += concurrent_table(A, quad) + shapes_table(A, quad)
        if body:
            sec.append(("<h2>6 · 并发结果</h2>", body))
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
    sec.append(("<h2>7 · 分析：缓存 / 吞吐 / 思考</h2>", table(
        ["模型", "单发斜率 ms/tk", "多轮斜率 ms/tk", "多轮/单发", "缓存判定", "decode tok/s", "思考行为"],
        ana_rows) +
        '<div class="note">缓存判定规则：多轮 TTFT 斜率 &lt;{:.2f} ms/token（绝对判据）或 多轮/单发斜率比 &lt;20% ⇒ 生效；'
        "比值 &gt;80% ⇒ 未命中。单发参照系本身可能被缓存污染（同题 runs 全命中时两者斜率同样低），此时候比值失效、以绝对判据为准。"
        "思考行为：no_reasoning=开关未产生思考输出；budget_exhausted=思考耗尽 max_tokens（正文 0 token）；"
        "normal=有思考草稿且正文正常。</div>".format(CACHE_EFFECTIVE_MS_PER_TOKEN)))
    # 体验基线评估（5.8，3 档制）
    base_html = baseline_section(A)
    if base_html:
        sec.append(("<h2>8 · 体验基线评估（3 档制）</h2>", base_html))
    # 结论建议
    rec_html = "".join('<p><b>【{}】</b>{}</p>'.format(esc(p), t) for p, t in recommendations)
    sec.append(("<h2>9 · 结论与建议</h2>", '<div class="good">{}</div>'.format(rec_html)))
    sec.append(("<h2>10 · 局限与备注</h2>", "<ul class='tight'>{}</ul>".format(
        "".join("<li>{}</li>".format(esc(l)) for l in limits))))
    sec.append(("<h2>11 · 数据质量</h2>", quality_block(data)))
    sec.append(("<h2>附录 A · 单发逐 run 明细</h2>",
                "<details><summary>展开</summary>{}</details>".format(appendix_single(data))
                if data["single"] else ""))
    sec.append(("<h2>附录 B · 多轮逐会话明细</h2>",
                "<details><summary>展开</summary>{}</details>".format(appendix_multiturn(data))
                if data["multiturn"] else ""))
    if A["coverage"]["has_concurrent"]:
        sec.append(("<h2>附录 C · 并发逐等级明细</h2>",
                    "<details><summary>展开</summary>{}{}</details>".format(
                        ("<h3>并发·单轮</h3>" + concurrent_table(A, "conc_single") + shapes_table(A, "conc_single")) if A.get("conc_single") else "",
                        ("<h3>并发·多轮</h3>" + concurrent_table(A, "conc_multi") + shapes_table(A, "conc_multi")) if A.get("conc_multi") else "")))

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
body{font-family:-apple-system,'PingFang SC','Microsoft YaHei',sans-serif;max-width:1720px;margin:24px auto;padding:0 18px 80px;color:var(--ink);background:#f8fafc;line-height:1.7}
h1{font-size:25px;margin-bottom:4px} h2{font-size:19px;margin-top:44px;border-bottom:1px solid var(--line);padding-bottom:8px}
h3{font-size:15.5px;margin:20px 0 6px}
.sub{color:#6b7280;font-size:13px;margin-bottom:24px}
.kpis{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:12px;margin:18px 0}
.kpi{background:#fff;border:1px solid var(--line);border-radius:10px;padding:12px 14px}
.kpi-v{font-size:20px;font-weight:700;color:#1652f0}.kpi-l{font-size:12px;color:#777;margin-top:2px}
table{border-collapse:collapse;width:100%;font-size:12.5px;background:#fff;margin:10px 0;table-layout:auto}
th,td{border:1px solid var(--line);padding:5px 9px;text-align:left;overflow-wrap:anywhere;word-break:break-word}
th{background:#f1f5f9}
.rng{color:#6b7280;font-size:11px;overflow-wrap:anywhere}
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
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:16px;margin:26px 0}
.backhome{display:none;margin:2px 0 0}
.backhome a{color:#1652f0;font-size:13px;text-decoration:none;cursor:pointer}
.mcard{background:#fff;border:1px solid var(--line);border-radius:12px;padding:18px 20px;cursor:pointer;transition:box-shadow .15s,transform .15s,border-color .15s}
.mcard:hover{box-shadow:0 4px 18px rgba(22,82,240,.14);transform:translateY(-2px);border-color:#1652f0}
.mcard .mname{font-size:16.5px;font-weight:700;color:#111827}
.mcard .msub{font-size:12px;color:#6b7280;margin-top:2px}
.mcard .mstats{margin-top:12px;font-size:12.8px;color:#374151;line-height:2.0}
.mcard .mstats b{color:#1652f0}
.mcard .menter{margin-top:12px;color:#1652f0;font-size:13px;font-weight:600}
</style></head><body>
<h1>__TITLE__</h1>
<p class="sub">__SUB__</p>
<div class="kpis">__KPI__</div>
__CARDS__
<p class="backhome" id="backhome"><a href="#">← 返回首页（选择模型）</a></p>
__BODY__
<div class="foot">全部数字来自服务端 usage 与客户端逐 chunk 计时，原始 JSON 原始留存可复查。
本报告内嵌 <code>&lt;script id="perf-summary"&gt;</code> 数据块（聚合指标 + 自动观察）——将整份 HTML 交给 AI，
即可让它基于该数据块为报告追加通俗解读备注。</div>
__SUMMARY__
__CHARTS__
<script>__SWITCHER__</script>
</body></html>"""
    with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "vendor", "chart.umd.min.js"),
              encoding="utf-8") as f:
        lib = f.read().replace("</script>", "<\\/script>")
    model_names = list(A["per_model"].keys())
    cards_html, switcher = "", ""
    if len(model_names) > 1:
        switcher = SWITCHER_JS.replace(
            "__MODELS__", json.dumps([short(m) for m in model_names], ensure_ascii=False))
        cards_html = landing_cards(data, model_names)
        sub = sub + '　·　点击模型卡片进入该模型的报告'
    page = (page.replace("__LIB__", lib)
                .replace("__TITLE__", esc(title))
                .replace("__SUB__", sub)
                .replace("__KPI__", kpi_html)
                .replace("__CARDS__", cards_html)
                .replace("__BODY__", body)
                .replace("__SUMMARY__", summary_block)
                .replace("__CHARTS__", chart_block)
                .replace("__SWITCHER__", switcher))
    suffix = ""
    if scenarios != set(QUADS):
        suffix = "-" + "+".join(QUADS[q] for q in sorted(scenarios, key=list(QUADS).index))
    out = os.path.join(out_dir, "llm-perf-报告{}.html".format(suffix))
    open(out, "w", encoding="utf-8").write(page)
    print("报告:", out)


def table_kv(rows):
    return "".join('<tr><th>{}</th><td>{}</td></tr>'.format(esc(k), esc(v)) for k, v in rows)


def landing_cards(data, models):
    """落地页模型卡片：每模型 3 条头条指标（单发 TTFT / 多轮末端 TTFT / 并发最高档 TTFT+失败数）。"""
    def med(vals, nd=2):
        vals = [v for v in vals if v is not None]
        return round(st.median(vals), nd) if vals else None

    def fmt(ms, nd=2):
        return "{:.{}f} s".format(ms / 1000, nd) if ms is not None else "—"

    cards = []
    for m_full in models:
        m = short(m_full)  # 展示名与 data-mv 用短名；源数据匹配用完整名
        # 单发 TTFT（thinking=off，取最大输入档）
        single_ttft = None
        s_entries = [e for e in data["single"]
                     if e["model"] == m_full and e.get("thinking", "off") == "off" and e.get("runs")]
        if s_entries:
            e = max(s_entries, key=lambda x: x["prompt_tokens"])
            single_ttft = med([r.get("ttft_ms") for r in e["runs"]])
            pin = "{:,}".format(e["prompt_tokens"])
        else:
            pin = "—"
        # 多轮末端 TTFT（off，各 session 最后一轮）
        mt_ttft = None
        mt_sessions = [e for e in data["multiturn"]
                       if e["model"] == m_full and e.get("thinking", "off") == "off" and e.get("turns")]
        if mt_sessions:
            mt_ttft = med([e["turns"][-1].get("ttft_ms") for e in mt_sessions])
        # 并发最高档 TTFT 中位（off 优先）+ 该模型总失败数
        conc_ttft = None
        lvls = [lv for lv in data["concurrent"] if lv["model"] == m_full]
        fails = 0
        for lv in lvls:
            turns = ([t for s in lv.get("sessions", []) for t in s["turns"]]
                     if (lv.get("sessions") or lv.get("multiturn")) else lv.get("requests", []))
            fails += sum(1 for t in turns if t.get("error"))
        top = max((lv for lv in lvls if lv.get("thinking", "off") == "off"),
                  key=lambda x: x.get("level", 0), default=None)
        if top:
            turns = ([t for s in top.get("sessions", []) for t in s["turns"]]
                     if (top.get("sessions") or top.get("multiturn")) else top.get("requests", []))
            conc_ttft = med([t.get("ttft_ms") for t in turns if not t.get("error")])
        cards.append(
            '<div class="mcard" data-mv="{m}">'
            '<div class="mname">{m}</div>'
            '<div class="msub">单发 / 多轮 / 并发 头条指标（thinking=off）</div>'
            '<div class="mstats">'
            '单发 {pin} TTFT 中位　<b>{s1}</b><br>'
            '多轮末端 TTFT 中位　<b>{s2}</b><br>'
            '并发最高档 TTFT 中位　<b>{s3}</b>　·　总失败 <b>{f}</b>'
            '</div>'
            '<div class="menter">查看该模型报告 →</div>'
            '</div>'.format(m=esc(m), pin=esc(pin), s1=fmt(single_ttft), s2=fmt(mt_ttft),
                            s3=fmt(conc_ttft), f="{:,}".format(fails) if fails else "0"))
    return '<div class="cards" id="cards">' + "".join(cards) + "</div>"


if __name__ == "__main__":
    main()
