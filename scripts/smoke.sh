#!/usr/bin/env bash
# llm-perf 自动化冒烟：mock 服务 + 多组合运行 + 输出断言。
#
# 背景（2026-09-08 现场踩坑）：-thinking off 被 model_overrides.thinking.mode=both 反超，
# on 变体照跑。当时冒烟只"跑通看输出"没有断言，且 smoke.yaml 没有 model_overrides，
# 踩坑代码路径根本没被执行——本脚本把这类"数据形状错误"变成硬断言。
#
# 覆盖面（每项都带数据形状断言，不再靠目测）：
#   基础三场景 × 两模型 × both 变体         configs/smoke.yaml
#   CLI 优先级回归（踩坑形状）              configs/smoke-overrides.yaml
#   全能力：trace 回放/观测层/warmup/goodput/correctness/max_tokens 列表  configs/smoke-all.yaml
#   probe 兼容性探测（mock 含 /models + tool-call 好路径）
#   非流式路径（临时配置 stream: false + thinking on）
#   开环到达率（request_rate/num_prompts/max_concurrency）configs/smoke-openloop.yaml
#   思考档位 levels 机制 + 档位名过滤       configs/smoke-levels.yaml
#   -m 模型过滤 / --max-ctx 截断
#   参数错误路径（非法变体名 / levels 下用 on / 多场景 -o 单文件）
#   报告管线（gen_html_report.py + validate_report.js）
#
# 用法:
#   scripts/smoke.sh                          # 自动 go build 临时二进制（GO_BIN 可指定 go 路径）
#   BENCH=/path/bench scripts/smoke.sh        # 用已构建好的二进制
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=18099
TMP="$(mktemp -d /tmp/llm-perf-smoke.XXXXXX)"
MOCK_PID=""
cleanup() {
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

# ── bench 二进制：外部传入或临时构建 ──
if [ -n "${BENCH:-}" ]; then
  echo "==> 使用指定二进制: $BENCH"
else
  GO_BIN="${GO_BIN:-go}"
  echo "==> go build 临时二进制（GO_BIN=$GO_BIN）"
  "$GO_BIN" build -o "$TMP/bench" ./cmd/bench
  BENCH="$TMP/bench"
fi

# ── mock OpenAI 兼容服务（stream + enable_thinking + /models + /metrics + tool-call）──
echo "==> 启动 mock_server (127.0.0.1:$PORT)"
python3 scripts/mock_server.py &
MOCK_PID=$!
ready=0
for _ in $(seq 1 25); do
  if curl -sf "http://127.0.0.1:$PORT/metrics" >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.2
done
[ "$ready" = 1 ] || { echo "❌ mock_server 未就绪"; exit 1; }

run() { # run <label> <outdir> [bench 参数...] —— 期望成功
  local label="$1" out="$2"
  shift 2
  echo "==> bench: $label"
  "$BENCH" -o "$TMP/$out" "$@" >"$TMP/$out.log" 2>&1 \
    || { echo "❌ bench 失败: $label（日志: $TMP/$out.log）"; tail -30 "$TMP/$out.log"; exit 1; }
}

expect_fail() { # expect_fail <label> [bench 参数...] —— 期望非零退出（参数校验/错误路径）
  local label="$1"
  shift
  echo "==> bench(应失败): $label"
  if "$BENCH" -o "$TMP/ef-tmp" "$@" >"$TMP/ef.log" 2>&1; then
    echo "❌ $label 本应报错退出却成功了"; exit 1
  fi
}

# 1) 基础冒烟：两模型 × both 变体 × 全场景（流式多轮并发闭环）
run "基础冒烟 smoke.yaml" out-basic -c configs/smoke.yaml

# 2) CLI 优先级回归：全局 off + overrides both（踩坑形状）
#    -thinking off：a/b 都应只剩 off；旧 bug 下 b 会混入 on 行
run "thinking off 过滤" out-off -c configs/smoke-overrides.yaml --thinking off --turns both --concurrency 1
#    -thinking on：a 无 on 变体应整模型跳过，只有 b 出数据且全是 on
run "thinking on 过滤" out-on -c configs/smoke-overrides.yaml --thinking on --turns both --concurrency 1

# 3) --max-ctx 截断：配置档位 500，CLI 截到 300
run "max-ctx 截断" out-ctx -c configs/smoke-overrides.yaml --turns single --concurrency 1 --max-ctx 300

# 4) 全能力冒烟：trace 回放 + 观测层 + 预热 + goodput + correctness + max_tokens 列表
run "全能力 smoke-all.yaml" out-all -c configs/smoke-all.yaml --turns both --concurrency 1,cfg

# 5) probe 兼容性探测（mock 已提供 /models + tool-call 好路径）
echo "==> bench: probe 探测"
"$BENCH" probe -c configs/smoke-overrides.yaml -o "$TMP/probe.json" >"$TMP/probe.log" 2>&1 \
  || { echo "❌ probe 失败（日志: $TMP/probe.log）"; tail -30 "$TMP/probe.log"; exit 1; }

# 6) 非流式路径（原 smoke-nostream 变体）：stream 关 + thinking on
sed -e 's/^stream: true/stream: false/' -e 's/^  mode: both/  mode: on/' \
  configs/smoke.yaml >"$TMP/nostream.yaml"
run "非流式 thinking on" out-nostream -c "$TMP/nostream.yaml" --turns single --concurrency 1

# 7) 开环到达率（原 smoke-open 变体）
run "开环到达率" out-openloop -c configs/smoke-openloop.yaml --turns single --concurrency cfg

# 8) 思考档位 levels：全档 + 档位名过滤
run "levels 全档" out-levels-all -c configs/smoke-levels.yaml --turns single --concurrency 1
run "levels 过滤 low" out-levels-low -c configs/smoke-levels.yaml --thinking low --turns single --concurrency 1

# 9) -m 模型过滤
run "-m 模型过滤" out-mfilter -c configs/smoke.yaml -m mock-model-b --turns single --concurrency 1

# 10) 错误路径：非法变体名 / levels 下用 on / 多场景输出到单 .json
expect_fail "非法变体名 --thinking badname" -c configs/smoke-overrides.yaml --thinking badname
expect_fail "levels 配置下 --thinking on 应报错" -c configs/smoke-levels.yaml --thinking on
expect_fail "多场景 -o 指到单 .json 应报错" -c configs/smoke.yaml --turns both --concurrency 1 -o "$TMP/bad.json"

# 11) SIGHUP 优雅中断（2026-09-09 现场事故回归：SSH 断开发 SIGHUP，进程被杀丢整个场景）
#     把多轮拉长到 8 轮，跑 2 秒后发 SIGHUP：应优雅退出（退出码 0，非强退 130）且部分数据落盘
sed -e 's/^  turns: 2$/  turns: 8/' configs/smoke.yaml >"$TMP/hup.yaml"
echo "==> bench(SIGHUP 中断): 优雅保存验证"
"$BENCH" -c "$TMP/hup.yaml" --turns multi --concurrency 1 -o "$TMP/out-hup" >"$TMP/out-hup.log" 2>&1 &
HUP_PID=$!
sleep 2
kill -HUP "$HUP_PID" 2>/dev/null || true
HUP_RC=0
wait "$HUP_PID" || HUP_RC=$?
if [ "$HUP_RC" != 0 ]; then
  echo "❌ SIGHUP 应优雅退出（退出码 $HUP_RC，130=强退）；日志:"; tail -20 "$TMP/out-hup.log"; exit 1
fi
if [ -z "$(find "$TMP/out-hup" -name '*.json' 2>/dev/null)" ]; then
  echo "❌ SIGHUP 中断后没有任何 JSON 落盘；日志:"; tail -20 "$TMP/out-hup.log"; exit 1
fi
echo "  ✅ SIGHUP 优雅退出（rc=0）且已完成数据落盘"

# ── 断言：校验输出 JSON 的模型×变体分布与指标完整性，不再靠目测 ──
echo "==> 断言输出数据形状"
python3 - "$TMP" <<'PYEOF'
import json, os, sys

tmp = sys.argv[1]
failures = []

def check(cond, msg):
    print(("  ✅ " if cond else "  ❌ ") + msg)
    if not cond:
        failures.append(msg)

def load_all(outdir):
    """递归收集目录下全部场景 JSON。"""
    reps = []
    root = os.path.join(tmp, outdir)
    for r, _, files in os.walk(root):
        for f in files:
            if f.endswith(".json"):
                with open(os.path.join(r, f), encoding="utf-8") as fp:
                    reps.append(json.load(fp))
    return reps

def rows(outdir):
    """(model, thinking, single prompt_tokens) 行 + 原始单条指标列表。"""
    out, m = [], []
    for rep in load_all(outdir):
        for r in rep.get("single", []):
            out.append({"model": r.get("model"), "thinking": r.get("thinking"),
                        "pt": r.get("prompt_tokens"), "mt": r.get("max_tokens")})
            m += r.get("runs") or []
        for r in rep.get("multiturn", []):
            out.append({"model": r.get("model"), "thinking": r.get("thinking"),
                        "pt": None, "mt": r.get("max_tokens")})
            m += r.get("turns") or []
        for lv in rep.get("concurrent", []):
            out.append({"model": lv.get("model"), "thinking": lv.get("thinking"),
                        "pt": None, "mt": lv.get("max_tokens")})
            m += lv.get("requests") or []
            for s in lv.get("sessions") or []:
                m += s.get("turns") or []
    return out, m

def models(rs): return {r["model"] for r in rs}
def thinks(rs): return {r["thinking"] for r in rs}

# 1) 基础冒烟：两模型都在，on/off 变体都有
basic, basic_m = rows("out-basic")
check(len(basic) > 0, f"基础冒烟有数据（{len(basic)} 行）")
check({"mock-model-a", "mock-model-b"} <= models(basic), f"两模型都有数据: {sorted(models(basic))}")
check({"on", "off"} <= thinks(basic), f"both 模式 on/off 变体都出现: {sorted(thinks(basic))}")
# 指标完整性：流式下 TTFT/E2E/tokens 必须齐全且无 error（防字段静默缺失类回归）
check(basic_m and all(x.get("ttft_ms", 0) > 0 and x.get("e2e_ms", 0) > 0
                      and x.get("prompt_tokens", 0) > 0 and x.get("completion_tokens", 0) > 0
                      and not x.get("error") for x in basic_m),
      f"基础冒烟指标完整（{len(basic_m)} 条: ttft/e2e/tokens>0 且无 error）")

# 2) -thinking off：所有行必须是 off；两模型都有 off 数据（回归点：旧 bug 下 b 混入 on）
off, _ = rows("out-off")
check(len(off) > 0, f"off 过滤有数据（{len(off)} 行）")
check(all(r["thinking"] == "off" for r in off), "off 过滤后不存在任何 on 行")
check(models(off) == {"mock-model-a", "mock-model-b"},
      "off 过滤后两模型都有 off 数据（overrides 未反超 CLI）")

# 3) -thinking on：off-only 模型整体跳过；有数据的行全是 on
on, _ = rows("out-on")
check(len(on) > 0, f"on 过滤有数据（{len(on)} 行）")
check(models(on) == {"mock-model-b"}, "on 过滤后只剩 overrides 模型 b（a 整模型跳过）")
check(all(r["thinking"] == "on" for r in on), "on 过滤后所有行都是 on")

# 4) --max-ctx 300：配置档位 500 应全部截断到 ≤300
ctx, _ = rows("out-ctx")
check(any(r["pt"] for r in ctx), "max-ctx 运行有 single 数据")
check(all((r["pt"] or 0) <= 300 for r in ctx),
      f"prompt_tokens 全部 ≤ 300（实际: {sorted({r['pt'] for r in ctx if r['pt']})}）")

# 5) 全能力：correctness 金丝雀、观测层真路径、trace 多轮轮数、输出长度列表、goodput
reps = load_all("out-all")
allrep = next((r for r in reps if r.get("correctness")), None)
check(allrep is not None and len(allrep["correctness"]) >= 1
      and all(c.get("e2e_ms", 0) > 0 for c in allrep["correctness"]),
      "smoke-all correctness 金丝雀有结果")
sm = next((r.get("server_metrics") for r in reps if r.get("server_metrics")), None)
check(sm is not None and sm.get("available") is True, "smoke-all 观测层走真路径（available=true）")
mt = [r for rep in reps for r in rep.get("multiturn", [])]
check(mt and all(len(r.get("turns") or []) == 3 for r in mt), "smoke-all trace 回放多轮 3 轮齐全")
sg = [r for rep in reps for r in rep.get("single", []) if r.get("thinking") == "off"]
check({r.get("max_tokens") for r in sg} >= {32, 64}, "smoke-all max_tokens 列表扫描出多档（off 档 32/64）")
cl = [lv for rep in reps for lv in rep.get("concurrent", [])]
check(cl and all(lv.get("slo_total", 0) > 0 and lv["slo_meet"] == lv["slo_total"] for lv in cl),
      "smoke-all goodput 统计落盘且宽松 SLO 全达标")

# 6) probe：模型列表两模型齐全；checks 除 thinking_levels（mock 无等级参数，预期 false）全绿
with open(os.path.join(tmp, "probe.json"), encoding="utf-8") as fp:
    probe = json.load(fp)
check(set(probe.get("models") or []) == {"mock-model-a", "mock-model-b"}, "probe 模型列表两模型齐全")
bad = [c["name"] for c in probe.get("checks", []) if not c.get("ok") and c["name"] != "thinking_levels"]
check("models_list" in {c["name"] for c in probe.get("checks", [])} and not bad,
      f"probe checks 全绿（thinking_levels 除外；失败项: {bad or '无'}）")
tc = [c for c in probe.get("checks", []) if c["name"].startswith("toolcall")]
check(tc and all(c.get("ok") for c in tc), "probe tool-call 检查通过（mock 结构化 tool_calls 好路径）")

# 7) 非流式：有数据、全 on、E2E 计时在（TTFT 允许缺失）
ns, ns_m = rows("out-nostream")
check(len(ns) > 0 and thinks(ns) == {"on"}, f"非流式有数据且全 on（{len(ns)} 行）")
check(ns_m and all(x.get("e2e_ms", 0) > 0 and not x.get("error") for x in ns_m), "非流式 E2E 计时正常")

# 8) 开环：RequestRate 落盘、请求数尊重 num_prompts
reps = load_all("out-openloop")
cl = [lv for rep in reps for lv in rep.get("concurrent", [])]
check(cl and all(lv.get("request_rate") == 4 for lv in cl), "开环到达率 request_rate=4 落盘")
nreq = sum(len(lv.get("requests") or []) for lv in cl)
check(1 <= nreq <= 6, f"开环总请求数尊重 num_prompts=6（实际 {nreq}）")

# 9) levels：全档三个变体；--thinking low 只剩 low
la, _ = rows("out-levels-all")
check(thinks(la) == {"off", "low", "high"}, f"levels 全档三变体都跑: {sorted(thinks(la))}")
ll, _ = rows("out-levels-low")
check(len(ll) > 0 and thinks(ll) == {"low"}, "levels 档位名过滤：--thinking low 只跑 low")

# 10) -m 过滤：只剩 mock-model-b
mf, _ = rows("out-mfilter")
check(len(mf) > 0 and models(mf) == {"mock-model-b"}, "-m 过滤后只剩目标模型")

if failures:
    print(f"\n❌ 冒烟断言失败 {len(failures)} 项")
    sys.exit(1)
print("\n✅ 全部断言通过")
PYEOF

# ── 报告管线：合并 → HTML 生成 → JS 校验（node 缺失时降级为提示）──
echo "==> 报告管线冒烟（gen_html_report + validate_report）"
mkdir -p "$TMP/flat"
find "$TMP/out-basic" -name '*.json' -exec cp {} "$TMP/flat/" \;
python3 scripts/gen_html_report.py "$TMP/flat" >"$TMP/report.log" 2>&1 \
  || { echo "❌ gen_html_report 失败（日志: $TMP/report.log）"; tail -20 "$TMP/report.log"; exit 1; }
HTML="$TMP/flat/llm-perf-报告.html"
[ -f "$HTML" ] || { echo "❌ 报告 HTML 未生成"; exit 1; }
echo "  ✅ 报告 HTML 已生成（$(wc -c <"$HTML" | tr -d ' ') bytes）"
if command -v node >/dev/null 2>&1; then
  node scripts/validate_report.js "$HTML" >"$TMP/validate.log" 2>&1 \
    || { echo "❌ validate_report 校验失败（日志: $TMP/validate.log）"; tail -20 "$TMP/validate.log"; exit 1; }
  echo "  ✅ validate_report.js 校验通过"
else
  echo "  ⚠️ 无 node，跳过 validate_report.js（报告 JS 校验未执行）"
fi

echo "==> 冒烟完成（运行目录已清理；详细日志见上方各步输出）"
