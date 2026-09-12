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
    """合并多份同场景报告 → {scenario: [entries...]}，并收集元信息。

    注意：Report 的 `server_metrics` 是**顶层字段**（每个场景一份窗口汇总），不在场景数组里。
    历史上这里只 extend 了场景数组，导致服务端观测被采集、写进 JSON，却在出报告时静默丢弃，
    报告里那两段「服务端观测」代码成了死代码。现在按场景归集到 meta["server"]。
    """
    data = {k: [] for k in SCENARIOS}
    meta = {"endpoint": "?", "tool": "?", "notes": [], "generated": [], "slo": None,
            "slo_baseline": None, "source_check": None, "server": {}, "correctness": {}}
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
        # 9.1 合流：slo.baseline 阈值随 JSON 透出（键名与内置 SLO_TIERS 一致）——此前 Go 侧
        # goodput 判定与报告侧基线判据是两份互不相识的常量，改一处漂移一处，现在以 JSON 为准。
        meta["slo_baseline"] = d.get("slo_baseline") or meta["slo_baseline"]
        # 10.1 两源一致性（客户端实测 vs 服务端 /metrics 生成吞吐）判定结果，由 Go 侧算好落盘
        meta["source_check"] = d.get("source_check") or meta["source_check"]
        if d.get("environment"):
            meta["environment"] = d["environment"]
        if d.get("config_raw"):
            meta["config_raw"] = redact_secrets(d["config_raw"])
        if d.get("plan"):
            meta["plan"] = d["plan"]
        # 正确性金丝雀按模型归集（10.2 四个数之一）：每份产物顶层一份 correctness，
        # 多模型报告下逐文件归到各自模型，一页纸才能给出「哪个模型答错」。
        for cr in (d.get("correctness") or []):
            cm = cr.get("model") or "?"
            p_, t_ = meta["correctness"].get(cm, (0, 0))
            meta["correctness"][cm] = (p_ + (1 if cr.get("match") else 0), t_ + 1)
        sm = d.get("server_metrics")
        if isinstance(sm, dict):  # probe 的 server_metrics 是字符串，别混进来
            cur = meta["server"].get(scen)
            # 同场景可能有多份产物（按模型/思考变体分区）：优先保留窗口差值取到的那份；
            # 全都没取到则留首份（带 note），报告据此如实说明「已启用但未取到」。
            if cur is None or (sm.get("available") and not cur.get("available")):
                meta["server"][scen] = sm
    return data, meta


# ────────────────────────── 统计辅助 ──────────────────────────

def mmm(vals, nd=2):
    """(median, min, max) 或 None。"""
    vals = [v for v in vals if v is not None]
    if not vals:
        return None
    return (round(st.median(vals), nd), round(min(vals), nd), round(max(vals), nd))


def think_sec(r):
    """单 run 的思考时长（秒）；无法界定返回 None。

    为什么不能写 `(r.get("think_ms") or 0)`：think_ms 的 JSON tag 带 omitempty，
    值为 0 时**整个键会消失**。若把「键缺失」当 0 塞进统计，一个档位里只要有
    部分 run 无法界定思考段，中位数就会被拉塌到 0——实测 R2 报告 pt=102400 行
    因此渲染成「思考 0.0s / 占比 0%」，而该行 E2E 154s、思考字符 15,687–35,994。

    判定规则（区分「真的没思考」与「思考了但测不出时长」）：
      - think_ms 存在            → 用它
      - 缺失 + reasoning_chars>0 → None（思考吃光预算、正文 0 字符，终点不可界定）
      - 缺失 + 无思考字符        → 0.0（thinking=off 的 0 是真实的 0，必须保留）
    """
    v = r.get("think_ms")
    if v is not None:
        return v / 1000.0
    if r.get("thinking_no_content") or (r.get("reasoning_chars") or 0) > 0:
        return None
    return 0.0


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
    "good_tpot": 40.0,          # ms，per-token TPOT（MLPerf Interactive）
    "pass_tpot": 200.0,         # ms（MLPerf Server）
    "good_tps": 25.0,           # 单请求输出速度 tok/s（comfortable 区上沿；依据 docs/latency-baselines.md §8）
    "pass_tps": 10.0,           # 阅读速度 ~4 tok/s × 2 安全系数（勉强区上沿；依据 §8）
}
def bucket_label(bkt, tiers):
    """输入档标签按**生效阈值**生成。

    slo.baseline 可在 JSON 里覆盖档位边界（9.1 合流），标签写死「≤4K / ≥24K」会在客户
    改过阈值之后说谎——档位名必须跟着阈值走。
    """
    if bkt == "short":
        return "≤{:,}".format(tiers["short_max_tokens"])
    if bkt == "long":
        return "≥{:,}".format(tiers["long_min_tokens"])
    return "{:,}–{:,}".format(tiers["short_max_tokens"] + 1, tiers["long_min_tokens"] - 1)


def eff_tiers(meta):
    """生效的体验基线阈值：JSON 的 `slo_baseline`（9.1 合流）覆盖脚本内置默认。

    未写的键（0/缺省）与旧产物（无该字段）自动回落 SLO_TIERS，存量数据照样出报告。
    """
    t = dict(SLO_TIERS)
    sb = (meta or {}).get("slo_baseline") or {}
    for k in SLO_TIERS:
        v = sb.get(k)
        if isinstance(v, (int, float)) and not isinstance(v, bool) and v != 0:
            t[k] = v
    return t


def baseline_enabled(meta):
    """是否出「体验基线评估」节：slo.baseline.enabled=false 整节跳过（缺省 = 评估）。"""
    sb = (meta or {}).get("slo_baseline") or {}
    return sb.get("enabled", True) is not False


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


def plan_table(plan):
    """5.10 测试画像：开跑前估算的「这次跑什么形状、多少请求」。

    此前 plan 三件套（Go 落盘 → py 收集 → 无人渲染）是唯一完整的死管线——落盘了却没有任何
    读者。画像的价值恰恰是回答「这份数据是什么形状跑出来的」，因此必须渲染。
    """
    rows = []
    for m in plan.get("models") or []:
        for s in m.get("scenarios") or []:
            rows.append([esc(short(m.get("model", ""))), esc(s.get("name", "")),
                         esc(s.get("detail", "")), "{:,}".format(s.get("requests", 0))])
    if not rows:
        return ""
    rows.append(["<b>合计</b>", "", "不含预热与金丝雀", "<b>{:,}</b>".format(plan.get("total_requests", 0))])
    note = '<div class="note">画像按<b>执行口径</b>估算（档位截断、思考 floor、变体展开、模型覆盖逐层同构），' \
           '用于回答「这次跑了什么形状、总共多少请求」；上下文 reach 为估算值（4 字符/token × 1.07 模板开销），' \
           '带 <code>~</code> 前缀。</div>'
    return "<h3>测试画像（开跑前估算）</h3>" + table(["模型", "场景", "明细", "请求估算"], rows) + note


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
    tiers = eff_tiers(meta)
    A = {"models": models, "per_model": OrderedDict(), "coverage": {}, "events": [],
         "tiers": tiers, "baseline_enabled": baseline_enabled(meta),
         "slo": (meta or {}).get("slo"), "source_check": (meta or {}).get("source_check"),
         # 10.3 横轴实测分箱：{(model, thinking, max_tokens, 标称档位): 实测 usage 中位数}。
         # 构造 filler 的 chars/token 是近似系数（bench probe 的 filler_fidelity 检查实测本部署
         # 保真度并告警），trace 模式下更只是估算——报告横轴与档位分箱一律以服务端 usage 为准，
         # 配置档位只在偏差 >5% 时并列标出。
         "usage_med": {}}

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
        # 5.7 爬坡发车窗口：批次号 >1 才算出窗口（齐射/单批次不标），span 取首批→末批的启动偏移差
        ramp = None
        if lv.get("sessions"):
            offs = [s.get("start_offset_s", 0) or 0 for s in lv["sessions"]]
            batches = sorted({s.get("batch", 0) or 0 for s in lv["sessions"]})
            if len(batches) > 1:
                ramp = {"batches": len(batches), "span_s": round(max(offs) - min(offs), 1)}
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
            "think": mmm([think_sec(t) for t in src], 1),
            "tokps": mmm([t.get("tokens_per_sec") for t in src], 0),
            "finish": sorted({t.get("finish_reason", "?") for t in src}),
            "shapes": lv.get("shapes") or [],  # 5.6 混合负载：形状分解（非空 = 混跑轮）
            "request_rate": lv.get("request_rate", 0),
            # 9.1/9.4 goodput 与排队观测：slo_total=0 表示未配置 slo.goodput（不出这两列）；
            # waiting_max 为服务端 /metrics gauge 峰值（0 = 观测层不可用或未采样）
            "slo_meet": lv.get("slo_meet", 0),
            "slo_total": lv.get("slo_total", 0),
            "goodput_rps": lv.get("goodput_rps", 0),
            "goodput_tps": lv.get("goodput_tps", 0),
            "waiting_max": lv.get("waiting_max", 0),
            "aborted": lv.get("aborted", ""),
            "ramp": ramp,
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
                    # 实测输入规模（usage）：横轴与档位分箱的权威口径（10.3）
                    _us = [r.get("prompt_tokens") for r in rs if r.get("prompt_tokens")]
                    _usage = int(round(st.median(_us))) if _us else None
                    A["usage_med"][(m, th, mt, size)] = _usage
                    ladder.append({
                        "size": size,
                        "usage": _usage,
                        "mt": mt,
                        "fails": len(rs_all) - len([r for r in rs_all if not r.get("error")]),
                        "ttft": mmm([r["ttft_ms"] / 1000 for r in rs if r.get("ttft_ms") is not None], 2),
                        "ttft_content": mmm([r["ttft_content_ms"] / 1000 for r in rs if r.get("ttft_content_ms") is not None], 2),
                        "ttft_rea": mmm([r["ttft_reasoning_ms"] / 1000 for r in rs if r.get("ttft_reasoning_ms") is not None], 2),
                        "think": mmm([think_sec(r) for r in rs], 1),
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
            # 思考时长汇总：先算再滤 None（think_sec 把「测不出」标成 None，
            # 若直接当 0 进统计会让「思考时长中位」这条曲线整体塌到 0）
            _think_all = [think_sec(r) for mt in mts
                          for size in sorted(s_by[(m, th, mt)])
                          for r in s_by[(m, th, mt)][size]["runs"]
                          if not r.get("error")]
            P[th]["think_all"] = [v for v in _think_all if v is not None]
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
                        # 与单发/并发同一口径（think_sec）：think_ms 缺失且有思考字符 ⇒ 不可界定，
                        # 整体剔除不计。此前这里是 ((think_ms) or 0)，多轮又恰是思考最容易被
                        # 截断的场景（长上下文 + thinking on），该轮「思考」列会塌成 0.0s。
                        "think": mmm([think_sec(t) for t in ts], 1),
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
        if p <= tiers["short_max_tokens"]:
            return "short"
        if p >= tiers["long_min_tokens"]:
            return "long"
        return "mid"

    def prompt_lbl(ps):
        lo, hi = min(ps), max(ps)
        return "{:,}".format(lo) if lo == hi else "{:,}–{:,}".format(lo, hi)

    def axis_size(m, th, mt, size):
        """横轴/分箱用的输入规模：优先服务端实测 usage，缺失才退回配置标称档位（10.3）。"""
        u = A["usage_med"].get((m, th, mt, size))
        return u if u else size

    def axis_lbl(m, th, mt, size):
        """档位标签：实测与配置偏差 >5% 时并列标出配置值（构造系数只影响近似程度，
        判读必须锚在服务端真实看到的规模上）。"""
        u = A["usage_med"].get((m, th, mt, size))
        if not u or not size:
            return "{:,}".format(size)
        if abs(u - size) / float(size) > 0.05:
            return "{:,}<span class='rng'>（配置 {:,}）</span>".format(u, size)
        return "{:,}".format(size)

    def mk_unit(scene, model, th, bkt, lbl, ttfts, tpots, tpss, n):
        ttfts = [v for v in ttfts if v]
        tpots = [v for v in tpots if v]
        tpss = [v for v in tpss if v]
        pp = pct9599(ttfts)
        return {
            "scene": scene, "model": model, "thinking": th, "bucket": bkt,
            "prompt": lbl, "n": n,
            "ttft_med": round(st.median(ttfts), 2) if ttfts else None,
            "ttft_p99": pp[1] if pp else None,  # 样本 < MIN_PCT_SAMPLE 时退回中位数判级
            # TPOT 取真实 per-token 解码间隔（tpot_ms = (E2E-TTFT)/(tokens-1)）。
            # ⚠️ 不要用 ITL 分位替代：ITL 按 **chunk** 计，投机解码（MTP）把多个 token
            # 合进一个 SSE chunk，于是 chunk 间隔 ≈ N × token 间隔（本实例实测 N≈2.66），
            # TPOT 会被系统性高估约 2.6 倍，使并发各档整片误判「未达标」。
            "tpot_med": round(st.median(tpots), 1) if tpots else None,
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
                        # 分箱按**实测 usage**（构造系数偏差不会把档位归错桶）
                        if bucket_of(axis_size(m, th, mt, size)) != bkt:
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
                    "单发·单轮", m, th, bkt,
                    prompt_lbl([axis_size(m, th, mt, size) for mt, size, _ in groups]),
                    ttfts, [r.get("tpot_ms") for r in top_rs],
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
                    [t.get("tpot_ms") for t in ts],
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
                                [t.get("tpot_ms") for t in ts],
                                [t.get("tokens_per_sec") for t in ts], len(ts)))
    A["baseline"] = base if A["baseline_enabled"] else []

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
    def meas_label(size, th):
        """横轴标签用服务端实测 usage 中位（跨模型取中位，横轴是共享的）；
        实测缺失才退回配置标称档位。构造系数只决定"近似到什么程度"，判读锚实测（10.3）。"""
        us = [u for (m2, t, _mt, sz), u in (A.get("usage_med") or {}).items()
              if t == th and sz == size and u]
        return "{:,}".format(int(round(st.median(us)))) if us else "{:,}".format(size)

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
        scales = {"x": spread({"title": {"display": True, "text": "prompt tokens（服务端 usage 实测中位）"}}),
                  "y": spread({"title": {"display": True, "text": "TTFT ms"}})}
        stmts.append(chart_js("c_s_ttft_" + th, "line", [meas_label(s, th) for s in sizes], ds,
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

    # 9.2 速率扫描（开环到达率）：每象限两张图 —— 吞吐/goodput vs 到达率、TTFT 中位/p99 vs 到达率。
    # 开环轮次 level=0，并发表那两张「vs 并发」图对它们整片不可见，容量曲线只能靠这一组。
    for quad, cid_tps, cid_ttft, qname in (("conc_single", "c_rs_tps", "c_rs_ttft", "并发·单轮"),
                                           ("conc_multi", "c_rsm_tps", "c_rsm_ttft", "并发·多轮")):
        items = [e for e in A.get(quad, []) if e.get("request_rate") and not e.get("shapes")]
        if not items:
            continue
        rates = sorted({e["request_rate"] for e in items})
        if len(rates) < 2:
            continue  # 单档到达率画不出曲线（该图的价值在趋势）
        groups = sorted({(e["model"], e["thinking"], e["mt"]) for e in items})
        multi_mt = len({g[2] for g in groups}) > 1
        ds_load, ds_ttft = [], []
        for m, th, mt in groups:
            ys_tps, ys_gp, ys_med, ys_p99 = [], [], [], []
            for r in rates:
                es = [e for e in items if e["model"] == m and e["thinking"] == th
                      and e["mt"] == mt and e["request_rate"] == r]
                if not es:
                    ys_tps.append(None); ys_gp.append(None)
                    ys_med.append(None); ys_p99.append(None)
                    continue
                ys_tps.append(round(sum(x["tps"] or 0 for x in es) / len(es), 1))
                gp = [x["goodput_rps"] for x in es if x.get("goodput_rps")]
                ys_gp.append(round(sum(gp) / len(gp), 2) if gp else None)
                meds = [x["ttft"][0] for x in es if x["ttft"]]
                ys_med.append(round(st.median(meds), 2) if meds else None)
                p99s = [x["ttft_p"][1] for x in es if x["ttft_p"]]
                ys_p99.append(round(max(p99s), 2) if p99s else None)
            if not any(v is not None for v in ys_tps):
                continue
            lbl = "{} (thinking={})".format(short(m), th) + (" out={}".format(mt) if multi_mt else "")
            col = color_of(models, m, 1 if th == "on" else 0)
            ds_load.append(line_ds(lbl, ys_tps, col))
            ds_ttft.append(line_ds(lbl + " TTFT 中位", ys_med, col))
            ds_ttft.append(line_ds(lbl + " TTFT p99", ys_p99, col, dash=[5, 4]))
            if any(v is not None for v in ys_gp):
                ds_load.append(line_ds(lbl + " goodput", ys_gp, col, dash=[5, 4]))
        labels = ["{:g}".format(r) for r in rates]
        if ds_load:
            scales = {"x": spread({"title": {"display": True, "text": "到达率 req/s（Poisson）"}}),
                      "y": spread({"title": {"display": True, "text": "tok/s（吞吐 / goodput）"}})}
            stmts.append(chart_js(cid_tps, "line", labels, ds_load,
                                  "{}：吞吐 / goodput vs 到达率（走平=容量边界）".format(qname), scales))
            add(cid_tps, "{} 吞吐 vs 到达率".format(qname))
        if ds_ttft:
            scales = {"x": spread({"title": {"display": True, "text": "到达率 req/s（Poisson）"}}),
                      "y": spread({"title": {"display": True, "text": "TTFT 秒"}})}
            stmts.append(chart_js(cid_ttft, "line", labels, ds_ttft,
                                  "{}：TTFT 中位 / p99 vs 到达率（上翘=排队开始）".format(qname), scales))
            add(cid_ttft, "{} TTFT vs 到达率".format(qname))

    return canvases, stmts


def short(model):
    return model.split("/")[-1] if "/" in model else model


# ────────────────────────── 表格 ──────────────────────────

def axis_cell(l):
    """单发档位列：以服务端实测 usage 为主，与配置标称偏差 >5% 时并列标出配置值（10.3）。
    构造 filler 的 chars/token 是近似系数（bench probe 的 filler_fidelity 实测），trace 模式更是估算——
    「档位 tk」列若只写配置目标值，读者会把构造偏差误当服务端行为。"""
    size, usage = l.get("size"), l.get("usage")
    if not usage or not size:
        return "{:,}".format(size or 0)
    if abs(usage - size) / float(size) > 0.05:
        return "{:,}<span class='rng'>（配置 {:,}）</span>".format(usage, size)
    return "{:,}".format(size)


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
            row = [esc(short(m)), str(l["mt"]), axis_cell(l), str(l["n"]), fail_cell,
                   f3(l["ttft"]), f3(l["ttft_content"])]
            if th == "on":
                # 思考占比 = 思考总时长 / E2E（思考与正文输出交错，占比为口径近似）
                # 整档全失败时 mmm() 返回 None（中位数不存在）——不能直接下标，否则中断产物无法出报告
                e2e0 = (l["e2e"] or [None])[0]
                think0 = (l["think"] or [None])[0]
                pct = "{:.0%}".format(think0 / e2e0) if e2e0 and think0 is not None else "—"
                row += [f1(l["think"]), pct]
            row += [f1(l["e2e"]), f1(l["decode"]),
                    f1(l["itl_p50"], "ms") if has_itl else "—",
                    f1(l["itl_p99"], "ms") if has_itl else "—",
                    f0(l["tokps"]), f0(l["comp"]),
                    f0(l["rc"]) if th == "on" else "—",
                    " / ".join(l["finish"])]
            rows.append(row)
    head = ["模型", "输出 tk", "输入 tk（实测）", "runs", "失败", "TTFT s", "首内容 s"] + \
           (["思考 s", "思考占比"] if th == "on" else []) + \
           ["E2E s", "decode s", "ITL p50 ms", "ITL p99 ms", "tok/s", "输出 tok"] + \
           (["思考字符"] if th == "on" else []) + ["finish"]
    return table(head, rows) if rows else "<p>无数据</p>"


def load_cell(e):
    """负载单元格：闭环 L{n}；开环 rate=X/s（开环档 level 恒为 0，只显示 level 会把整片读数变成 0）。"""
    if e.get("request_rate"):
        return "rate={:g}/s".format(e["request_rate"])
    return "L{}".format(e["level"])


def concurrent_table(A, quad):
    qname = "并发·多轮" if quad == "conc_multi" else "并发·单轮"
    items = A[quad]
    has_think = any(e["thinking"] != "off" or (e["think"] and e["think"][0] > 0) for e in items)
    has_slo = any(e.get("slo_total") for e in items)      # 未配置 slo.goodput 时不出这两列
    has_wait = any(e.get("waiting_max") for e in items)   # 观测层不可用时不出
    has_ramp = any(e.get("ramp") for e in items)
    head = ["模型", "thinking", "输出 tk", "负载", "单元数", "请求总数", "失败", "墙钟 s", "吞吐 tok/s"]
    if has_slo:
        head += ["SLO 达标", "goodput req/s"]
    if has_wait:
        head += ["waiting 峰值"]
    if has_ramp:
        head += ["发车"]
    head += ["TTFT s", "TTFT p95/p99 s", "E2E s", "E2E p95/p99 s"] + \
            (["思考 s"] if quad == "conc_multi" else []) + ["单请求 tok/s", "finish"]
    rows = []
    for e in sorted(items, key=lambda x: (x["model"], x["thinking"], x["mt"],
                                          x.get("request_rate", 0), x["level"])):
        mt_cell = "mix({})".format("/".join(s["label"] for s in e["shapes"])) if e["shapes"] else str(e["mt"])
        fail_cell = str(e.get("fails") or 0)
        if e.get("fails"):
            fail_cell = '<span style="color:#e5484d;font-weight:600">{}</span>'.format(e["fails"])
        lcell = esc(load_cell(e))
        if e.get("aborted"):
            # drain 语义：已发出的请求数据完整保留，只是样本量少于配置值——必须标出来，
            # 否则读者会把「被止损截断的档位」当成完整档位去比较吞吐
            lcell += ' <span style="color:#b45309;font-weight:600" title="{}">⚠️ 提前终止</span>'.format(
                esc(e["aborted"]))
        row = [esc(short(e["model"])), e["thinking"], mt_cell, lcell,
               "{:,}".format(e["n_units"]), "{:,}".format(e["n_turns"]), fail_cell,
               "{:.1f}".format(e["wall"]) if e["wall"] else "—",
               "{:.0f}".format(e["tps"]) if e["tps"] else "—"]
        if has_slo:
            if e.get("slo_total"):
                row += ["{}/{}({:.0%})".format(e["slo_meet"], e["slo_total"],
                                               e["slo_meet"] / e["slo_total"]),
                        "{:.2f}".format(e.get("goodput_rps") or 0)]
            else:
                row += ["—", "—"]
        if has_wait:
            row += ["{:.0f}".format(e["waiting_max"]) if e.get("waiting_max") else "—"]
        if has_ramp:
            row += ["爬坡 {} 批 / {:.0f}s".format(e["ramp"]["batches"], e["ramp"]["span_s"])
                    if e.get("ramp") else "齐射"]
        row += [f3(e["ttft"]), fpct(e["ttft_p"], 2), f1(e["e2e"]), fpct(e["e2e_p"], 1)]
        if quad == "conc_multi":
            row.append(f1(e["think"]))
        row += [f0(e["tokps"]), " / ".join(e["finish"])]
        rows.append(row)
    note = '<div class="note">单元数：{}。TTFT/E2E 为该并发等级下成功请求的中位数（min–max）；' \
           'p95/p99 仅在成功请求数 ≥ {} 时计算——长短混跑时中位数可能几乎不动而 p99 数倍膨胀，' \
           '请对照 p95/p99 列判断尾部时延风险。</div>'.format(
        "独立多轮会话（每用户各自跑完整会话）" if quad == "conc_multi" else "独立单轮请求", MIN_PCT_SAMPLE)
    if has_slo:
        note += '<div class="note">SLO 达标 = 满足 <code>slo.goodput</code>（TTFT/TPOT 阈值）的请求数 / 总请求数；' \
                'goodput req/s = 达标请求 ÷ 墙钟（配置阈值见第 2 节配置存档）。未达标不等于故障，' \
                '它表示该负载下已超出业务可接受时延。</div>'
    if has_wait:
        note += '<div class="note">waiting 峰值 = 服务端 <code>num_requests_waiting</code> 排队深度峰值' \
                '（<code>saturation_guard.max_waiting</code> 的标定依据，建议阈值 ≈ 峰值 × 4；' \
                '具体建议值见「结论与建议」）。</div>'
    if has_ramp:
        note += '<div class="note">发车 = 5.7 闭环错峰发车（指数爬坡：首批 1 路，等该批全部完成首轮再放大一倍）。' \
                '「爬坡 N 批 / Xs」表示本档位的会话在 X 秒窗口内陆续启动——' \
                '该窗口内尚处暂态，读数请以窗口后为准；齐射（<code>ramp: false</code>）无此问题。</div>'
    if any(e.get("aborted") for e in items):
        note += '<div class="note" style="color:#b45309;font-weight:600">⚠️ 存在提前终止的档位（hover 见原因）：' \
                '饱和止损/爬坡止损触发后关闭发射闸门，已发出的请求全部保留完整数据（drain 语义），' \
                '但样本量少于配置值，档位间吞吐对比需扣除该因素。</div>'
    if any(e.get("fails") for e in items):
        note += '<div class="note" style="color:#b45309;font-weight:600">⚠️ 该象限存在失败请求（已从延迟统计剔除）——' \
                '对应档位的中位数/分位数仅基于幸存请求，真实体验比表中更差；失败明细见数据质量章。</div>'
    return table(head, rows) + note if rows else "<p>无数据</p>"


def rate_sweep_table(A):
    """9.2 速率扫描（开环到达率）：GuideLLM sweep 的容量交付物口径。

    逐档吞吐 / goodput / SLO 达标率 / TTFT 中位与 p99，并给出两个容量结论：
      - 吞吐拐点：吞吐 ≥ 0.9×峰值 的最大到达率（再加压只会排队）
      - goodput 上限：SLO 达标率 ≥95% 的最大到达率（业务可接受的最大负载）
    """
    parts = []
    for quad, qname in (("conc_single", "并发·单轮"), ("conc_multi", "并发·多轮")):
        items = [e for e in A.get(quad, []) if e.get("request_rate")]
        if not items:
            continue
        groups = defaultdict(list)
        for e in items:
            groups[(e["model"], e["thinking"], e["mt"])].append(e)
        rows, notes = [], []
        for (m, th, mt), es in sorted(groups.items()):
            es.sort(key=lambda x: x["request_rate"])
            peak = max((e["tps"] or 0) for e in es)
            knee = max((e["request_rate"] for e in es if e["tps"] and e["tps"] >= 0.9 * peak),
                       default=None)
            slo_ok = [e for e in es if e.get("slo_total")]
            gp_max = None
            if slo_ok:
                cands = [e["request_rate"] for e in slo_ok
                         if e["slo_meet"] / e["slo_total"] >= 0.95]
                gp_max = max(cands) if cands else None
            for e in es:
                ttft_p99 = e["ttft_p"][1] if e["ttft_p"] else None
                rows.append([
                    esc(short(m)), th + ("·out={}tk".format(mt) if mt else ""),
                    "{:g}".format(e["request_rate"]),
                    "{:.0f}".format(e["tps"] or 0),
                    "{}/{}（{:.0%}）".format(e["slo_meet"], e["slo_total"],
                                            e["slo_meet"] / e["slo_total"]) if e.get("slo_total") else "—",
                    "{:.2f}".format(e.get("goodput_rps") or 0) if e.get("slo_total") else "—",
                    f3(e["ttft"]),
                    # p99 样本不足时明示（fpct 口径），而不是留空或硬算
                    "{:.2f}".format(ttft_p99) if ttft_p99 is not None
                    else "n&lt;{}".format(MIN_PCT_SAMPLE),
                    "{:.1f}".format(e["wall"]) if e["wall"] else "—",
                    "{:,}".format(e["n_turns"]),
                    '<span style="color:#b45309">⚠️ 末尾</span>' if e.get("aborted") else "",
                ])
            if knee:
                notes.append("{} {}：吞吐拐点 ≈ rate={:g}/s（0.9×峰值 {:.0f} tok/s）。".format(
                    esc(short(m)), th, knee, peak))
            if gp_max:
                notes.append("{} {}：goodput 达标上限 ≈ rate={:g}/s（≥95% 请求满足 SLO 的最大到达率）。".format(
                    esc(short(m)), th, gp_max))
            elif slo_ok:
                notes.append("{} {}：全部到达率档位均未达 95% SLO 达标率——即便最低档也已超出业务时延要求。".format(
                    esc(short(m)), th))
        if not rows:
            continue
        note = '<div class="note">开环到达率扫描：请求按 Poisson 过程到达（<code>rate_sweep</code>），' \
               '每档一轮；吞吐为整档墙钟口径（含排队等待）。' \
               '<b>拐点</b>之后吞吐不再增长、延迟快速上翘，即为该部署的容量边界。</div>'
        if notes:
            note += '<div class="finding">' + "".join("<p>{}</p>".format(n) for n in notes) + "</div>"
        parts.append("<h3>{}</h3>".format(qname) + table(
            ["模型", "变体", "到达率 req/s", "吞吐 tok/s", "SLO 达标", "goodput req/s",
             "TTFT s", "TTFT p99 s", "墙钟 s", "请求数", "备注"], rows) + note)
    return "".join(parts)


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
    tiers = A.get("tiers") or SLO_TIERS  # 9.1 合流：以 JSON 透出的 slo.baseline 为准

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
            note_html.append(note_once("输入 {}–{} 区间两档之间无权威锚点，只报数值不打徽章。".format(
                "{:,}".format(tiers["short_max_tokens"] + 1), "{:,}".format(tiers["long_min_tokens"] - 1))))
        else:
            g = tiers["short_good_ttft"] if bkt == "short" else tiers["long_good_ttft"]
            p = tiers["short_pass_ttft"] if bkt == "short" else tiers["long_pass_ttft"]
            t_b = badge(t_v, g, p)
        tp_b = badge(u["tpot_med"], tiers["good_tpot"], tiers["pass_tpot"])
        if u["thinking"] == "on" and u["tps"] is not None:
            note_html.append(note_once(
                "tok/s 为整响应吞吐（thinking=on 时含思考 token），速度判级仅供参考。"))
        ts_b = badge_hi(u["tps"], tiers["good_tps"], tiers["pass_tps"])
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
        rows.append([u["scene"], esc(short(u["model"])), u["thinking"], bucket_label(bkt, tiers),
                     u["prompt"], "{:,}".format(u["n"]), t_txt, t_b,
                     "{:.0f}ms".format(u["tpot_med"]) if u["tpot_med"] is not None else "—", tp_b,
                     "{:.0f}".format(u["tps"]) if u["tps"] is not None else "—", ts_b, verdict])

    ps = []
    for bkt, name in (("long", "agent 大上下文（{}）".format(bucket_label("long", tiers))),
                      ("short", "短输入（{}）".format(bucket_label("short", tiers)))):
        vs = summary.get(bkt) or []
        if not vs:
            continue
        n_g, n_p, n_b = vs.count("✅ 优"), vs.count("⚠️ 及格"), vs.count("❌ 未达标")
        ps.append("<b>{}</b>：{} 项配置中 {} 优 / {} 及格 / {} 未达标。".format(name, len(vs), n_g, n_p, n_b))
    if any(u["bucket"] == "long" for u in base):
        ps.append("现代 agent 产品基线上下文即约 35K（系统提示 + 工具定义 + RAG 注入，用户发一句「你好」请求就已带 35K），"
                  "<b>agent 场景的 TTFT 体验主判据是 {} 档</b>，短输入档徽章代表不了 agent 体验。".format(
                      bucket_label("long", tiers)))
        ps.append("暖路径提示：prefix cache 命中时大输入的 TTFT 只由新增 token 决定，会显著好于档位数字——"
                  "{} 档判的是冷 prefill 最坏角落；命中率的实测见第 7 节缓存判定。".format(bucket_label("long", tiers)))
    note_html.append("<li>判级口径：TTFT 优先 p99，样本不足 {} 时退回中位数（括号内标注）；"
                     "TPOT 取<b>真实 per-token 解码间隔</b>（<code>tpot_ms</code>，即 (E2E−TTFT)/(输出 token−1)）"
                     "的中位数，<b>不用 ITL 分位</b>——ITL 按 chunk 计，投机解码（MTP）会把多个 token 合进"
                     "同一个 SSE chunk，chunk 间隔约为 token 间隔的 N 倍（本实例实测 N≈2.66），"
                     "拿它当 TPOT 会高估约 2.6 倍并把并发各档成片误判为未达标。"
                     "基线不进退出码、不影响原始数据。</li>"
                     .format(MIN_PCT_SAMPLE))
    note_html.append("<li>判据出处：<code>docs/latency-baselines.md</code> §7 —— "
                     "MLPerf Server（TTFT p99 ≤2s / TPOT ≤200ms，锚定 ~240 wpm 阅读速度）、"
                     "MLPerf Interactive（450ms / 40ms，基于 ChatGPT/Perplexity 实测修订）、"
                     "{} 档为推导值（particula 10K 实测 0.75–1.6s 线性外推 + MLPerf 405B 档 6s 上限佐证）。</li>".format(
                         bucket_label("long", tiers)))

    head = ["场景", "模型", "thinking", "输入档", "prompt tk", "样本", "TTFT", "TTFT 判级",
            "TPOT ms", "TPOT 判级", "tok/s", "速度判级", "综合"]
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
    # 9.2 速率扫描（开环）：吞吐拐点与 goodput 达标上限——容量交付物的两个数
    for quad, qname in (("conc_single", "并发·单轮"), ("conc_multi", "并发·多轮")):
        by = defaultdict(list)
        for e in A.get(quad, []):
            if e.get("request_rate"):
                by[(e["model"], e["thinking"], e["mt"])].append(e)
        for (m, th, mt), es in sorted(by.items()):
            if len(es) < 2:
                continue
            es.sort(key=lambda x: x["request_rate"])
            peak = max((e["tps"] or 0) for e in es)
            if peak <= 0:
                continue
            knee = max((e["request_rate"] for e in es if e["tps"] and e["tps"] >= 0.9 * peak),
                       default=None)
            p99l = [e["ttft_p"][1] for e in es if e["ttft_p"]]
            ttft_txt = ""
            if len(p99l) >= 2:
                ttft_txt = "，TTFT p99 由 {:.2f}s 升到 {:.2f}s".format(p99l[0], p99l[-1])
            cs.append("<b>{}</b>（{}·thinking={}）：开环速率扫描峰值吞吐 {:.0f} tok/s"
                      "（到达率 {:g}/s 起进入 0.9×峰值平台{}）——再提高到达率只会排队，"
                      "该点即容量边界。".format(
                          esc(short(m)), qname, th, peak, knee if knee else es[-1]["request_rate"],
                          ttft_txt))
            slo_ok = [e for e in es if e.get("slo_total")]
            if slo_ok:
                hit = [e["request_rate"] for e in slo_ok if e["slo_meet"] / e["slo_total"] >= 0.95]
                if hit:
                    cs.append("<b>{}（{}·thinking={}）</b>：SLO 达标率 ≥95% 的最大到达率 ≈ {:g}/s"
                              "（goodput 上限）——业务可接受的最大负载，超出后虽仍出吞吐但时延已不达标。".format(
                                  esc(short(m)), qname, th, max(hit)))
                else:
                    cs.append("<b>{}（{}·thinking={}）</b>：所有到达率档位的 SLO 达标率均低于 95%"
                              "——当前 <code>slo.goodput</code> 阈值下，最低档位也已超出业务时延要求。".format(
                                  esc(short(m)), qname, th))
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
    # 9.4 waiting 峰值标定：阈值只能按实例标定（max_num_seqs 越小，正常负载下水位越高）
    waits = [(e["model"], e["waiting_max"]) for quad in ("conc_single", "conc_multi")
             for e in A.get(quad, []) if e.get("waiting_max")]
    if waits:
        wm, wv = max(waits, key=lambda x: x[1])
        rs.append(("中", "<b>saturation_guard.max_waiting</b> 建议从 <b>{:.0f}</b> 起"
                        "（本轮实测排队峰值 {:.0f} × 4，模型 {}）——阈值低于正常水位会把稳态排队误判为饱和、"
                        "提前关闭发射闸门。标定流程：先关着 guard 跑一轮 → 读本报告的 waiting 峰值 → 启用重跑。".format(
                            wv * 4, wv, esc(short(wm)))))
    # 10.1 两源一致性告警（诊断层守卫，不是新指标）
    sc = A.get("source_check") or {}
    if sc.get("deviation") is not None and sc["deviation"] < -0.15:
        rs.append(("中", "两源一致性异常：客户端实测聚合吞吐比服务端生成吞吐低 <b>{:.0%}</b>——"
                        "客户端侧可能在数百并发流 + 高 chunk 率下自身成瓶颈（数值偏低，而非服务端更快）。"
                        "复核方向：换更近的客户端/更少并发复测、检查 CPU 与网络读数、或降低 chunk 解析开销。".format(
                            abs(sc["deviation"]))))
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
    collected, no_data, _ = metrics_provenance(A.get("server") or {})
    if collected:
        lim.append("服务端观测（/metrics）已随报告输出（见数据质量章），仅作<b>辅助与交叉验证</b>"
                   "——全部结论仍以客户端实测为准，两者口径不同不可逐数对齐。")
    else:
        extra = "；本次已启用但未取到窗口数据" if no_data else ""
        lim.append("未使用服务端 /metrics（非标准端点，网关/代理部署普遍不提供{}）"
                   "——<b>不影响任何结论</b>：延迟、吞吐、缓存判定均基于客户端实测。".format(extra))
    return lim


# ────────────────────────── 10.2 一页纸（顶层 = 四个数 + 徽章 + 三分归因）──────────────────────────
#
# 表达层减法、能力不砍：顶层只留冻结的四个数（TTFT / decode 速度 / goodput@SLO /
# 正确性 canary）与三分归因（服务端推理 / 客户端与网络 / 负载层测试设计），
# 原 1–11 章与逐 run 明细整体降级为折叠附录——数字一个不少，只是不再挤在第一屏。
# 判级复用 5.8 三档制阈值（随 JSON slo.baseline 透出），不另设一套。

def _level_of(v, good, pas, lower_better=True):
    """体验判级：0=优 / 1=及格 / 2=未达标 / None=不可判。"""
    if v is None:
        return None
    if lower_better:
        return 0 if v <= good else (1 if v <= pas else 2)
    return 0 if v >= good else (1 if v >= pas else 2)


def _badge_txt(lv):
    return {0: "✅ 优", 1: "⚠️ 及格", 2: "❌ 未达标"}.get(lv, "—")


def _ttft_unit(base, m):
    """TTFT 判据单元：优先 agent 大上下文档（3 档制的 long，即体验判据档），
    其次最大输入档；只取 thinking=off（thinking=on 的 TTFT 含思考，不套 TTFT 判据）。"""
    cand = [x for x in base if x["model"] == m and x["thinking"] == "off"] or \
           [x for x in base if x["model"] == m]
    if not cand:
        return None
    singles = [x for x in cand if x["scene"] == "单发·单轮"] or cand
    order = {"long": 0, "mid": 1, "short": 2}
    return sorted(singles, key=lambda x: order.get(x["bucket"], 3))[0]


def onepager(data, A, meta):
    tiers = A.get("tiers") or SLO_TIERS
    base = A.get("baseline") or []
    models = A["models"]
    corpus = meta.get("correctness") or {}
    cov = A["coverage"]

    rows, per_model = [], {}
    for m in models:
        u = _ttft_unit(base, m)
        # ① TTFT：样本不足退回中位（与 5.8 同口径），标出实际输入与口径
        t_val = t_lv = None
        t_note = "本轮未测单发（无 TTFT 判据单元）"
        if u:
            t_val = u["ttft_p99"] if u["ttft_p99"] is not None else u["ttft_med"]
            t_kind = "p99" if u["ttft_p99"] is not None else "中位"
            if u["bucket"] == "long":
                t_lv = _level_of(t_val, tiers["long_good_ttft"], tiers["long_pass_ttft"])
            elif u["bucket"] == "short":
                t_lv = _level_of(t_val, tiers["short_good_ttft"], tiers["short_pass_ttft"])
            # mid 档两档之间无权威锚点 → 只报数不打徽章（与第 8 节同规则）
            t_note = "{} 输入 {}（{}·n={}）{}".format(
                u["scene"], u["prompt"], t_kind, u["n"],
                "" if t_lv is not None else "；区间无权威锚点，不判级")
        # ② decode 速度：TPOT（真实 per-token 间隔）+ 输出吞吐，同一单元取值
        tp_val = u["tpot_med"] if u else None
        ts_val = u["tps"] if u else None
        tp_lv = _level_of(tp_val, tiers["good_tpot"], tiers["pass_tpot"])
        ts_lv = _level_of(ts_val, tiers["good_tps"], tiers["pass_tps"], lower_better=False)
        # ③ goodput@SLO：跨档位汇总达标率 + 峰值 goodput（只统计配了 slo.goodput 的档位）
        es = [e for quad in ("conc_single", "conc_multi") for e in A.get(quad, [])
              if e["model"] == m and e.get("slo_total")]
        gp = None
        if es:
            meet = sum(e["slo_meet"] for e in es)
            tot = sum(e["slo_total"] for e in es)
            gpk = max(e["goodput_rps"] for e in es)
            rate = meet / tot if tot else None
            gp = (meet, tot, gpk)
            gp_lv = _level_of(1 - rate if rate is not None else None, 0.05, 0.20)
        else:
            gp_lv = None
        # ④ 正确性 canary
        c = corpus.get(m)
        c_lv = None if not c else (0 if c[0] == c[1] else 2)
        lvs = [x for x in (t_lv, tp_lv, ts_lv, gp_lv, c_lv) if x is not None]
        verdict = max(lvs) if lvs else None
        per_model[m] = {"ttft": t_val, "ttft_lv": t_lv, "unit": u, "gp": gp,
                        "gp_lv": gp_lv, "tp_lv": tp_lv, "ts_lv": ts_lv,
                        "verdict": verdict, "correctness": c}
        rows.append([
            esc(short(m)),
            "—" if t_val is None else "{:.2f}s {}".format(t_val, _badge_txt(t_lv)),
            "—" if ts_val is None else "{:.0f} tok/s {}".format(ts_val, _badge_txt(ts_lv)),
            "—" if not gp else "{} / {}（{:.0%}）".format(gp[0], gp[1], gp[0] / gp[1]) +
            " {}".format(_badge_txt(gp_lv)),
            "—" if not c else "{}/{} {}".format(c[0], c[1], "✅" if c[0] == c[1] else "❌"),
            _badge_txt(verdict),
        ])

    # ── 三分归因：慢在哪一层，逐层给可核实判据；缺证据写 NA，不猜 ──
    # ① 服务端推理
    sv = []
    verified = [(m, p) for m, p in per_model.items() if p["verdict"] is not None]
    if not verified:
        sv.append("本轮未取到可判级的单发指标（检查输入档位覆盖），服务端性能无从判定。")
    else:
        good = [m for m, p in verified if p["verdict"] == 0]
        if good:
            sv.append("{}：四个数全部达优——本次覆盖的负载区间内，服务端推理未见瓶颈。"
                      .format("、".join(esc(short(m)) for m in good)))
        for m, p in verified:
            if p["verdict"] == 0:
                continue
            c = p["correctness"]
            c_lv = None if not c else (0 if c[0] == c[1] else 2)
            items = (("TTFT", p["ttft_lv"]), ("TPOT", p["tp_lv"]), ("输出速度", p["ts_lv"]),
                     ("goodput@SLO", p["gp_lv"]), ("正确性 canary", c_lv))
            bad = [nm for nm, lv in items if lv == 2]
            mid = [nm for nm, lv in items if lv == 1]
            seg = []
            if bad:
                seg.append("未达标 = " + "、".join(bad))
            if mid:
                seg.append("仅及格 = " + "、".join(mid))
            sv.append("{}：{}。".format(esc(short(m)), "；".join(seg) or "—"))
    aborts = [e for quad in ("conc_single", "conc_multi") for e in A.get(quad, []) if e.get("aborted")]
    if aborts:
        sv.append("{} 个档位提前终止（饱和止损 / 爬坡止损 / 降速熔断），已越容量边界——"
                  "这些档位的样本量少于配置值，档间对比需扣除该因素。".format(len(aborts)))
    stalls = [n for _, n in meta.get("notes", []) if "熔断" in (n or "")]
    if stalls:
        sv.append("存在降速熔断留痕：{}。".format(esc(stalls[0][:90] + ("…" if len(stalls[0]) > 90 else ""))))

    # ② 客户端与网络（客户端计时是本工具的量测点，不是误差项）
    cl = []
    sc = A.get("source_check")
    if sc and sc.get("deviation") is not None:
        dev = sc["deviation"]
        if dev < -0.15:
            cl.append("⚠️ 两源偏差 {:+.0%}（客户端聚合吞吐低于服务端生成吞吐）——"
                      "客户端侧可能有损或网络路径异常，真实服务吞吐应更高。".format(dev))
        else:
            cl.append("两源一致（偏差 {:+.0%}）：客户端聚合吞吐与服务端生成吞吐吻合，"
                      "客户端计时链路未见异常。".format(dev))
    else:
        cl.append("无可比服务端观测 → 两源一致性记 <b>NA</b>；客户端实测即基线口径，不影响结论。")
    cl.append("客户端逐 chunk 计时开销 µs 级，与被测间隔（数十 ms）差 3~4 个数量级。")

    # ③ 负载层（测试设计是否够格下结论）
    ld = []
    miss = [nm for key, nm in (("has_single", "单发·单轮"), ("has_multiturn", "单发·多轮"),
                               ("has_conc_single", "并发·单轮"), ("has_conc_multi", "并发·多轮"))
            if not cov.get(key)]
    if miss:
        ld.append("未覆盖：{}——结论不外推到这些形状。".format("、".join(miss)))
    if not cov.get("has_concurrent"):
        ld.append("本轮未测并发：单发结论不外推服务吞吐。")
    else:
        rates = sorted({e["request_rate"] for e in A.get("conc_single", []) + A.get("conc_multi", [])
                        if e.get("request_rate")})
        if len(rates) < 2:
            ld.append("并发只跑了 1 个负载档，无法给出容量拐点（建议 ≥2 档到达率扫描）。")
        else:
            ld.append("并发覆盖 {} 个负载档{}。".format(
                len(rates), "（开环到达率扫描，可给容量拐点）" if rates else ""))
    ev = A.get("events") or {}
    if ev.get("thinking_trunc") or ev.get("no_content"):
        ld.append("存在思考截断 {} 次 / 正文 0 token {} 次，相关内容类指标缺失。".format(
            ev.get("thinking_trunc", 0), len(ev.get("no_content") or [])))
    ld.append("受控变量测试（filler 合成负载）：tool-call 不进压测路径，"
              "agent 主导业务下纯推理吞吐偏乐观。")

    def blk(title, items):
        return '<div class="att-i"><b>{}</b><br>{}</div>'.format(
            title, "<br>".join(items) if items else "NA")

    hrow = ["模型", "① TTFT（首字）", "② decode 速度", "③ goodput@SLO", "④ 正确性", "判定"]
    th = "<tr>{}</tr>".format("".join("<th>{}</th>".format(esc(x)) for x in hrow))
    body = "".join("<tr>{}</tr>".format("".join("<td>{}</td>".format(c) for c in r)) for r in rows)
    gp_rule = "未配置 <code>slo.goodput</code>（goodput@SLO 不可判）"
    _slo = A.get("slo") or {}
    if _slo.get("ttft_ms") or _slo.get("tpot_ms"):
        gp_rule = "达标 = 同时满足 TTFT ≤ {:,.0f}ms 且 TPOT ≤ {:,.0f}ms 的请求占比".format(
            _slo.get("ttft_ms") or 0, _slo.get("tpot_ms") or 0)
    return (
        '<div id="onepager">'
        '<div class="op-h">一页纸结论<span class="op-tag">'
        '顶层只有四个数（TTFT / decode 速度 / goodput@SLO / 正确性 canary）+ 三分归因；'
        '详细数据全部下沉附录，数字一个不少。</span></div>'
        '<table class="opt"><thead>{th}</thead><tbody>{body}</tbody></table>'
        '<div class="note">① TTFT 取 agent 大上下文档（无该档则取最大输入档），thinking=on 含思考不套 TTFT 判据；'
        '② decode = 最大输出档组中位；③ {gp_rule}；④ 金丝雀为「按序转写数字」抽查。'
        '判级阈值随 JSON <code>slo.baseline</code> 透出，改配置即改判据。</div>'
        '<div class="att">{a}{b}{c}</div>'
        '</div>'
    ).format(th=th, body=body, gp_rule=gp_rule,
             a=blk("① 服务端推理", sv), b=blk("② 客户端与网络", cl), c=blk("③ 负载层（测试设计）", ld))


# ── 服务端观测（/metrics）：可选的第二数据源 ──
# 原则：/metrics 是引擎实现细节、非标准端点。客户端实测是本工具唯一的标准口径，
# 服务端观测只做增强与交叉验证。本函数只负责「如实描述来源」，任何分支都不削弱结论。
def _window_ok(s):
    """窗口差值（counter/hist）是否真的可用。

    新口径直接看 available；同时兼容旧产物——旧版本在结束快照失败时仍写 available=true，
    只把原因放进 note。全仓只有「结束快照抓取失败」会设置 note，故 note 非空即视为没取到。
    """
    return bool(s.get("available")) and not s.get("note")


def metrics_provenance(server):
    """按场景归集服务端观测状态，返回 (collected, no_data, absent)。

    入参是 merge() 产出的 `meta["server"]`（{场景: 窗口汇总}）。
    collected / no_data 为 [(场景, 摘要)]；absent 为没有任何服务端观测数据的场景。
    判定依据是**窗口差值是否真取到**而非「字段是否存在」：结束快照失败时会留下 note，
    报告必须如实说「未取到」，而不是把全零计数器渲染成一个空面板。
    """
    collected, no_data, absent = [], [], []
    for k in SCENARIOS:
        sm = (server or {}).get(k)
        if not sm:
            absent.append(k)
            continue
        (collected if _window_ok(sm) else no_data).append((k, sm))
    return collected, no_data, absent


def hists_block(server):
    """服务端直方图窗口差值（9.1 数据面 → 报告面）：服务端口径的延迟分解。

    数据一直在 JSON 里（`server_metrics.histograms`）却无人消费——README 承诺渲染、实际零读取。
    这里补一张 P50/P99 小表：服务端排队/排队外分解与客户端口径互为交叉验证。
    口径保守标注：桶边界估算 + 多 label 合并（同一 family 的 model_name/finished_reason 各系列
    被累加）+ 窗口内混入非本次压测流量——只做量级对照，不与客户端数字逐位对齐。
    """
    rows = []
    for k in SCENARIOS:
        for name, h in sorted(((server or {}).get(k) or {}).get("histograms", {}).items()):
            if not h.get("count"):
                continue
            rows.append([k, esc(hist_label(name)), "{:,.0f}".format(h["count"]),
                         "{:.3f}".format(h.get("mean") or 0),
                         "{:.3f}".format(h.get("p50") or 0),
                         "{:.3f}".format(h.get("p99") or 0)])
    if not rows:
        return ""
    return "<h3>服务端延迟分解（/metrics 直方图窗口差值）</h3>" + table(
        ["场景", "指标（服务端口径）", "观测数", "均值 s", "P50 s", "P99 s"], rows) + \
        '<div class="note">口径：Prometheus 直方图窗口差值，分位为<b>桶边界估算</b>（非插值），' \
        '且同一 family 的多 label 系列被累加、窗口内可能混入其他流量——只作服务端归因参考，' \
        '与客户端实测（基线）不逐数对齐；不一致时以客户端为准。</div>'


def hist_label(name):
    """引擎直方图名 → 人读标签（认不出的原样显示，不猜）。"""
    for key, lbl in (("time_to_first_token", "TTFT"), ("inter_token_latency", "ITL（token 间隔）"),
                     ("e2e_request_latency", "E2E 请求延迟"), ("request_queue_time", "排队等待"),
                     ("request_prefill_time", "prefill 耗时"), ("request_decode_time", "decode 耗时"),
                     ("request_prefill_kv_computed", "prefill KV token 数")):
        if key in name:
            return lbl
    return name


def source_check_block(A):
    """10.1 两源一致性守卫：客户端聚合吞吐 vs 服务端生成吞吐（smetrics counter 差值）。

    定位：这是**诊断层**的守卫，不产生新的评测指标。危险形态是「数百并发流 + 高 chunk 率下
    客户端自身成瓶颈」——此时客户端测到的速度会系统性低于服务端真实产出。偏差超阈值只**标注**，
    不判失败：观测层缺失或不可比一律记 NA（绝不因为可选数据源缺失而少给结论）。
    """
    sc = A.get("source_check")
    if not sc:
        return ""
    if not sc.get("server_tps"):
        return '<p>两源一致性（客户端 vs 服务端生成吞吐）：<b>NA</b>——{}。</p>'.format(
            esc(sc.get("note") or "本次无可比的服务端观测（未启用 /metrics 或未取到窗口差值）"))
    dev = sc.get("deviation")
    if dev is None:
        return '<p>两源一致性：<b>NA</b>——{}。</p>'.format(
            esc(sc.get("note") or "缺少可比口径"))
    bad = dev < -0.15  # 客户端显著低于服务端 = 客户端侧可能有损/成瓶颈
    tag = '<span style="color:#b45309;font-weight:600">⚠️ 客户端可能有损</span>' if bad else "✅ 一致"
    extra = "；" + esc(sc["note"]) if sc.get("note") else ""
    return '<p>两源一致性（客户端 vs 服务端生成吞吐）：客户端 <b>{:.0f}</b> tok/s，服务端 ' \
           '<b>{:.0f}</b> tok/s（{} tokens / {:.0f}s 窗口），偏差 <b>{:+.0%}</b>——{}{}。</p>' \
           '<p class="note">阈值 ±15%：客户端在数百并发流 + 高 chunk 率下可能自身成为瓶颈，' \
           '此时客户端读数偏低、真实服务吞吐更高；服务端 counter 为本窗口差值，' \
           '含窗口内的其他流量属已知近似。本项<b>只标注不判失败</b>。</p>'.format(
        sc.get("client_tps") or 0, sc.get("server_tps") or 0,
        "{:,.0f}".format(sc.get("server_tokens") or 0), sc.get("window_seconds") or 0, dev, tag, extra)


def quality_block(data, A):
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

    # 服务端观测（可选数据源）：有则展示，失败/缺失则如实说明——绝不留空面板
    collected, no_data, _ = metrics_provenance(A.get("server") or {})
    if collected or no_data:
        parts.append("<p><b>数据源</b>：本报告全部指标为客户端实测口径（基线）；"
                     "下列服务端 /metrics 观测为<b>可选增强</b>，采集失败或端点不存在均不影响任何结论。</p>")
    for k, s in collected:
        bits = []
        hit_q = s.get("cache_query_tokens") or 0
        if hit_q > 0:
            bits.append("前缀缓存命中率 {:.1%}".format((s.get("cache_hit_tokens") or 0) / hit_q))
        # 0 要显式渲染出来：省略会让「窗口内没有抢占」看起来像「没采到这一项」。
        # 旧产物没有该键（is None）时自然不显示，兼容不变。
        if s.get("preemptions") is not None:
            bits.append("preemptions {:,.0f}".format(s["preemptions"]))
        if s.get("spec_drafts"):
            # 语义是「每个 draft 平均被接受多少 token」（0–N 之间的数，N = 每步投机 token 数，
            # 本实例 3），**不是百分比**。按 % 渲染会给出 148% 这种不可能值，读者会当成接受率超高。
            bits.append("MTP 平均每 draft 接受 {:.2f} token（{:.0f}/{:.0f}）".format(
                (s.get("spec_accepted_tokens") or 0) / s["spec_drafts"],
                s.get("spec_accepted_tokens") or 0, s["spec_drafts"]))
        for gname, gv in sorted((s.get("gauges") or {}).items()):
            mx = (gv or {}).get("max") or 0
            if not mx:
                continue
            bits.append("{} 峰值 {:.1%}".format(gname, mx) if gname == "kv_usage"
                        else "{} 峰值 {:.0f}".format(gname, mx))
        if s.get("observation_degraded"):
            bits.append("⚠️ " + esc(s.get("observation_note") or "gauge 轮询降级"))
        parts.append("<p>服务端观测（{}）：{}。</p>".format(
            k, "；".join(bits) if bits else "已采集，但本场景该项无有效数据"))
    for k, s in no_data:
        note = "（{}）".format(esc(s["note"])) if s.get("note") else ""
        g = sorted((s.get("gauges") or {}).keys())
        gtxt = "；窗口内已轮询到 {}".format("、".join(g)) if g else ""
        parts.append("<p>服务端观测（{}）：已启用但<b>未取到窗口差值</b>{}{}"
                     "——该场景全部结论仍按客户端实测口径给出。</p>".format(k, note, gtxt))
    # 正确性抽查
    for k in SCENARIOS:
        if data[k] and (data[k][0].get("correctness") or []):
            corr = data[k][0]["correctness"]
            passed = sum(1 for r in corr if r.get("match"))
            extra = "" if passed == len(corr) else "——⚠️ 存在回复与要求不符，警惕缓存污染/网关伪响应/截断"
            parts.append("<p>正确性抽查（{}）：{}/{} 通过{}。</p>".format(k, passed, len(corr), extra))
            break
    # 两源一致性（10.1）：只在可比时给判定，不可比记 NA
    sc_html = source_check_block(A)
    if sc_html:
        parts.append(sc_html)
    # 服务端直方图窗口差值（有则出表，无则绝不空面板）
    hb = hists_block(A.get("server") or {})
    if hb:
        parts.append(hb)
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
        "source_check": A.get("source_check"),  # 10.1 两源一致性（客户端 vs 服务端吞吐）
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
    A["server"] = meta["server"]  # 服务端观测（可选数据源）按场景挂到分析结果，供各章节如实标注
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
            top = max(es, key=lambda x: (x["request_rate"], x["level"]))
            if top["tps"]:
                lbl_q = qname + ("·out={}tk".format(mt) if multi_mt else "")
                # 开环档 level=0：标成「吞吐@L0」会把到达率维度整片藏起来（读者以为是并发 0）
                load_lbl = "rate={:g}/s".format(top["request_rate"]) if top["request_rate"] \
                    else "L{}".format(top["level"])
                kpis.append(("吞吐@{}·{} ({})".format(load_lbl, lbl_q, short(m)),
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
    _collected, _no_data, _ = metrics_provenance(meta["server"])
    if _collected:
        src = "客户端实测（基线）＋ 服务端 /metrics 辅助（{}）".format("、".join(k for k, _ in _collected))
    elif _no_data:
        src = "客户端实测（基线）；服务端 /metrics 已启用但未取到窗口数据（见数据质量章）"
    else:
        src = "客户端实测（基线）；未启用或端点未提供 /metrics（非标准端点，不影响结论）"
    sec2_body = table_kv([("端点", meta["endpoint"]), ("工具版本", meta["tool"]),
                          ("场景覆盖", "；".join(cov)),
                          ("请求总数", str(n_req)),
                          ("数据来源", src)])
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
    if meta.get("plan"):
        sec2_body += plan_table(meta["plan"])
    sec.append(("<h2>2 · 测试配置与方法</h2>", sec2_body))
    sec.append(("<h2>3 · 指标口径</h2>",
                '<p class="note">下表全部为<b>客户端实测</b>口径（基线）——与服务端是否存在 /metrics 无关。</p>'
                + table(
        ["指标", "定义"],
        [["TTFT", "请求发出 → 首个流式 chunk（空首包不计）；本报告单位秒"],
         ["首内容", "请求发出 → 首个 content chunk（TTFT_content）"],
         ["思考时长", "模型思考输出的总时长（think_ms）；TTFT 在 thinking=on 时即思考首包"],
         ["思考占比", "思考时长 ÷ E2E（思考与正文输出可能交错，口径近似）"],
         ["E2E", "请求发出 → 流结束（含思考全程）"],
         ["decode", "content 首包 → 流结束；全程无正文（思考吃光预算）时不可测，记「—」"],
         ["ITL p50/p99", "chunk 间间隔分位（不含 TTFT）。注意是 chunk 间隔、不是 token 间隔，"
                         "投机解码会把多个 token 合进一个 chunk"],
         ["tok/s", "completion_tokens ÷（E2E−TTFT），含思考段——不用 decode 时长（decode 只覆盖正文段，"
                   "思考 token 计入分子而思考耗时不在分母，会显著虚高）"],
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
        if body:
            body += ('<div class="note">「输入 tk」列为服务端 <code>usage.prompt_tokens</code> 实测中位'
                     '（构造 filler 的 chars/token 只是近似系数，trace 模式更只是估算）；'
                     '与配置标称档位偏差 &gt;5% 时并列标出配置值。'
                     '档位分箱与基线判级同样按实测走——判读锚在服务端真实看到的规模上。'
                     '偏离超过 25% 时 <code>bench probe</code> 的 filler_fidelity 检查会告警，'
                     '此时建议改用 <code>filler_corpus</code> 真实语料（比调系数更贴近真实 tokenization）。</div>')
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
        # 9.2 速率扫描（开环到达率）：容量边界交付物
        sweep = rate_sweep_table(A)
        if sweep:
            body += "<h3>6.3 速率扫描（开环到达率）</h3>"
            for cid in ("c_rs_tps", "c_rs_ttft", "c_rsm_tps", "c_rsm_ttft"):
                if any(c == cid for c, _ in canvases):
                    body += '<div class="chart"><canvas id="{}" height="110"></canvas></div>'.format(cid)
            body += sweep
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
    sec.append(("<h2>11 · 数据质量</h2>", quality_block(data, A)))
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

    # 10.2 表达层减法：原 1–11 章 + 逐 run 明细 + 图表 + KPI 卡整体降级为折叠附录，
    # 顶层只留一页纸（四个数 + 徽章 + 三分归因）。数字一个不少，只是不再挤第一屏。
    one_html = onepager(data, A, meta)
    appendix = ""
    if body or chart_block:
        appendix = (
            '<details class="appendix" id="appendix"><summary>详细数据（附录）：'
            '关键指标卡 · 摘要 · 配置与方法 · 指标口径 · 各场景结果 · 体验基线 · 数据质量 · 逐 run 明细 · 图表'
            '</summary><p class="note">以下是完整明细与口径说明——数字一个都不少，'
            '只是不再占用第一屏。排障时从一页纸的判定出发，在这里往下查证据。</p>'
            '<div class="kpis">{kpi}</div>{body}{charts}</details>').format(
                kpi=kpi_html, body=body,
                charts=('<h2>图表</h2>' + chart_block) if chart_block else "")

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
#onepager{background:#fff;border:1px solid var(--line);border-radius:12px;padding:16px 20px;margin:20px 0}
.op-h{font-size:17px;font-weight:700;color:#111827}
.op-h .op-tag{font-size:12px;font-weight:400;color:#6b7280;margin-left:10px}
table.opt{margin:12px 0 6px}
table.opt td:first-child{font-weight:600}
.att{display:grid;grid-template-columns:repeat(auto-fit,minmax(320px,1fr));gap:12px;margin:14px 0 4px}
.att-i{background:#f8fafc;border:1px solid var(--line);border-left:3px solid #1652f0;border-radius:8px;padding:10px 13px;font-size:13px;line-height:1.65}
.att-i b{color:#1652f0}
details.appendix{background:#fff;border:1px solid var(--line);border-radius:12px;padding:12px 18px;margin:26px 0}
details.appendix>summary{font-size:15px;font-weight:600;color:#374151}
</style></head><body>
<h1>__TITLE__</h1>
<p class="sub">__SUB__</p>
__ONEPAGER__
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
                .replace("__ONEPAGER__", one_html)
                .replace("__CARDS__", cards_html)
                .replace("__BODY__", appendix)
                .replace("__SUMMARY__", summary_block)
                .replace("__CHARTS__", "")
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
