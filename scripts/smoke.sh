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
#   SIGHUP 优雅中断 / 降速熔断（两场景相继熔断续跑）+ stall_trace 落盘
#   8.2 熔断恢复探针（未恢复停整轮 / 恢复后续跑）+ 5.7 闭环爬坡发车批次落盘
#   参数错误路径（非法变体名 / levels 下用 on / 多场景 -o 单文件）
#   报告管线（gen_html_report.py + validate_report.js）
#
# 用法:
#   scripts/smoke.sh                          # 自动 go build 临时二进制（GO_BIN 可指定 go 路径）
#   BENCH=/path/bench scripts/smoke.sh        # 用已构建好的二进制
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=18099
PORT_STALL=18100
PORT_STALL_B=18101
PORT_STALL_C=18102
TMP="$(mktemp -d /tmp/llm-perf-smoke.XXXXXX)"
MOCK_PID=""
MOCK_STALL_PID=""
MOCK_STALL_B_PID=""
MOCK_STALL_C_PID=""
cleanup() {
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  [ -n "$MOCK_STALL_PID" ] && kill "$MOCK_STALL_PID" 2>/dev/null || true
  [ -n "$MOCK_STALL_B_PID" ] && kill "$MOCK_STALL_B_PID" 2>/dev/null || true
  [ -n "$MOCK_STALL_C_PID" ] && kill "$MOCK_STALL_C_PID" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

# 冒烟配置改写后落 TMP：output_dir 全部指向 $TMP/runlog。
# bench 的 run.log/raw 恒写配置的 output_dir（-o 只重定向 JSON 落点，
# 见 cmd/bench/main.go run.log 段），不改写的话每轮冒烟都会在项目根
# 漏 output-smoke*/run.log。
# dataset.path 同理：它相对配置文件所在目录解析，配置搬到 TMP 后会指向
# $TMP/fixtures/ 落空，需一并改写为仓库内的绝对路径。
for f in smoke smoke-all smoke-overrides smoke-openloop smoke-sweep smoke-levels smoke-stall; do
  sed -e "s#^output_dir:.*#output_dir: $TMP/runlog#" \
      -e "s#\([[:space:]]*path:[[:space:]]*[\"']\{0,1\}\)fixtures/#\1$PWD/configs/fixtures/#" \
      "configs/$f.yaml" >"$TMP/$f.yaml"
done

# ── bench 二进制：外部传入或临时构建 ──
if [ -n "${BENCH:-}" ]; then
  echo "==> 使用指定二进制: $BENCH"
else
  GO_BIN="${GO_BIN:-go}"
  echo "==> go build 临时二进制（GO_BIN=${GO_BIN}）"
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
    || { echo "❌ bench 失败: ${label}（日志: $TMP/$out.log）"; tail -30 "$TMP/$out.log"; exit 1; }
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
run "基础冒烟 smoke.yaml" out-basic -c "$TMP/smoke.yaml"

# 2) CLI 优先级回归：全局 off + overrides both（踩坑形状）
#    -thinking off：a/b 都应只剩 off；旧 bug 下 b 会混入 on 行
run "thinking off 过滤" out-off -c "$TMP/smoke-overrides.yaml" --thinking off --turns both --concurrency 1
#    -thinking on：a 无 on 变体应整模型跳过，只有 b 出数据且全是 on
run "thinking on 过滤" out-on -c "$TMP/smoke-overrides.yaml" --thinking on --turns both --concurrency 1

# 3) --max-ctx 截断：配置档位 500，CLI 截到 300
run "max-ctx 截断" out-ctx -c "$TMP/smoke-overrides.yaml" --turns single --concurrency 1 --max-ctx 300

# 4) 全能力冒烟：trace 回放 + 观测层 + 预热 + goodput + correctness + max_tokens 列表
run "全能力 smoke-all.yaml" out-all -c "$TMP/smoke-all.yaml" --turns both --concurrency 1,cfg

# 5) probe 兼容性探测（mock 已提供 /models + tool-call 好路径）
echo "==> bench: probe 探测"
"$BENCH" probe -c "$TMP/smoke-overrides.yaml" -o "$TMP/probe.json" >"$TMP/probe.log" 2>&1 \
  || { echo "❌ probe 失败（日志: $TMP/probe.log）"; tail -30 "$TMP/probe.log"; exit 1; }

# 6) 非流式路径（原 smoke-nostream 变体）：stream 关 + thinking on
sed -e 's/^stream: true/stream: false/' -e 's/^  mode: both/  mode: on/' \
  "$TMP/smoke.yaml" >"$TMP/nostream.yaml"
run "非流式 thinking on" out-nostream -c "$TMP/nostream.yaml" --turns single --concurrency 1

# 7) 开环到达率（原 smoke-open 变体）
run "开环到达率" out-openloop -c "$TMP/smoke-openloop.yaml" --turns single --concurrency cfg

# 7b) 速率扫描（rate_sweep 多档）+ 观测层 + goodput + slo.baseline：
#     报告侧「速率扫描」表与图、拐点/goodput 上限、测试画像、服务端延迟分解、两源一致性
#     都以这份产物为数据源（报告断言见文末报告管线段）
run "速率扫描（rate_sweep）" out-sweep -c "$TMP/smoke-sweep.yaml" --turns both --concurrency cfg

# 8) 思考档位 levels：全档 + 档位名过滤
run "levels 全档" out-levels-all -c "$TMP/smoke-levels.yaml" --turns single --concurrency 1
run "levels 过滤 low" out-levels-low -c "$TMP/smoke-levels.yaml" --thinking low --turns single --concurrency 1

# 9) -m 模型过滤
run "-m 模型过滤" out-mfilter -c "$TMP/smoke.yaml" -m mock-model-b --turns single --concurrency 1

# 10) 错误路径：非法变体名 / levels 下用 on / 多场景输出到单 .json
expect_fail "非法变体名 --thinking badname" -c "$TMP/smoke-overrides.yaml" --thinking badname
expect_fail "levels 配置下 --thinking on 应报错" -c "$TMP/smoke-levels.yaml" --thinking on
expect_fail "多场景 -o 指到单 .json 应报错" -c "$TMP/smoke.yaml" --turns both --concurrency 1 -o "$TMP/bad.json"

# 11) SIGHUP 优雅中断（2026-09-09 现场事故回归：SSH 断开发 SIGHUP，进程被杀丢整个场景）
#     把多轮拉长到 8 轮，跑 2 秒后发 SIGHUP：应优雅退出（退出码 0，非强退 130）且部分数据落盘
sed -e 's/^  turns: 2$/  turns: 8/' "$TMP/smoke.yaml" >"$TMP/hup.yaml"
echo "==> bench(SIGHUP 中断): 优雅保存验证"
"$BENCH" -c "$TMP/hup.yaml" --turns multi --concurrency 1 -o "$TMP/out-hup" >"$TMP/out-hup.log" 2>&1 &
HUP_PID=$!
sleep 2
kill -HUP "$HUP_PID" 2>/dev/null || true
HUP_RC=0
wait "$HUP_PID" || HUP_RC=$?
if [ "$HUP_RC" != 0 ]; then
  echo "❌ SIGHUP 应优雅退出（退出码 ${HUP_RC}，130=强退）；日志:"; tail -20 "$TMP/out-hup.log"; exit 1
fi
if [ -z "$(find "$TMP/out-hup" -name '*.json' 2>/dev/null)" ]; then
  echo "❌ SIGHUP 中断后没有任何 JSON 落盘；日志:"; tail -20 "$TMP/out-hup.log"; exit 1
fi
echo "  ✅ SIGHUP 优雅退出（rc=0）且已完成数据落盘"

# 12) 降速熔断（stall_guard）+ 采样序列落盘（stall_trace）：
#     单独起一个 stall-e2e 假慢速服务端（主 mock 不调速，互不影响）。它按 max_tokens
#     全量吐 token、5 tok/s → 每个流式场景都在窗口后熔断：应「中止当前场景 → 冷却 →
#     续跑下一场景」，各场景 JSON 带 note、旁落 .stall.csv（正常段也记录 + # tripped 原因行）
echo "==> 启动慢速 mock (127.0.0.1:$PORT_STALL, 5 tok/s)"
python3 scripts/stall-e2e/mockserver.py "$PORT_STALL" 5 &
MOCK_STALL_PID=$!
ready=0
for _ in $(seq 1 25); do
  if curl -sf "http://127.0.0.1:$PORT_STALL/v1/models" >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.2
done
[ "$ready" = 1 ] || { echo "❌ 慢速 mock 未就绪"; exit 1; }
run "降速熔断（两场景相继熔断续跑）" out-stall -c "$TMP/smoke-stall.yaml" --turns both
# --no-stall-trace：JSON 不带 stall_trace、不落 .stall.csv（熔断行为本身不变）
run "no-stall-trace 关闭落盘" out-stall-notrace -c "$TMP/smoke-stall.yaml" --turns single --no-stall-trace

# 12b) 8.2 探针：冷却后探针实测未恢复 → 停止整轮（只剩首个场景的输出）
#      40 tok/s < min_tps=45（触发熔断）且 < 2×45=90（探针必不达标）。
#      max_tokens 抬到 160：请求时长 4s > 2s 窗口才熔断得起来（32tk @40tok/s=0.8s 跑不完窗口）；
#      探针 3×256tk @40tok/s ≈ 19s，是"未恢复路径"的固定实测成本
sed 's/^  max_tokens: 32/  max_tokens: 160/' "$TMP/smoke-stall.yaml" \
  | sed "s#127.0.0.1:$PORT_STALL#127.0.0.1:$PORT_STALL_B#" >"$TMP/smoke-stall-b.yaml"
echo "==> 启动慢速 mock (127.0.0.1:$PORT_STALL_B, 40 tok/s)"
python3 scripts/stall-e2e/mockserver.py "$PORT_STALL_B" 40 &
MOCK_STALL_B_PID=$!
ready=0
for _ in $(seq 1 25); do
  if curl -sf "http://127.0.0.1:$PORT_STALL_B/v1/models" >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.2
done
[ "$ready" = 1 ] || { echo "❌ 慢速 mock(B) 未就绪"; exit 1; }
run "熔断探针未恢复停整轮" out-stall-probe -c "$TMP/smoke-stall-b.yaml" --turns both \
  --stall-tps 45 --stall-cooldown 1 --stall-probe-factor 2
kill "$MOCK_STALL_B_PID" 2>/dev/null || true
MOCK_STALL_B_PID=""

# 12c) 8.2 探针：服务端中途恢复 → 探针通过 → 继续下一个场景。
#      mock 恢复时钟锚定「首个慢速请求到达」+4s（对 bench 启动方差免疫）：
#      熔断窗口 1s → 首请求后 ~1.2-2.5s 熔断；恢复在 +4s；冷却 3s → 探针最早 +4.5s。
#      熔断 << 恢复 < 探针，两边都有裕量；探针撞上边界由 3 条取中位兜底。
#      独立端口（18102）：避免同端口 kill/重绑竞态让 readiness 探到垂死的旧实例
echo "==> 启动可恢复 mock (127.0.0.1:$PORT_STALL_C, 5 tok/s → 首个慢速请求 4s 后恢复)"
python3 scripts/stall-e2e/mockserver.py "$PORT_STALL_C" 5 4 &
MOCK_STALL_C_PID=$!
ready=0
for _ in $(seq 1 25); do
  if curl -sf "http://127.0.0.1:$PORT_STALL_C/v1/models" >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.2
done
[ "$ready" = 1 ] || { echo "❌ 可恢复 mock 未就绪"; exit 1; }
# smoke-stall-b.yaml 的端点已是 PORT_STALL_B（12b 改写过），从它换成 PORT_STALL_C
sed "s#127.0.0.1:$PORT_STALL_B#127.0.0.1:$PORT_STALL_C#" "$TMP/smoke-stall-b.yaml" >"$TMP/smoke-stall-c.yaml"
run "熔断探针恢复后续跑" out-stall-recover -c "$TMP/smoke-stall-c.yaml" --turns both \
  --stall-tps 50 --stall-window 1 --stall-cooldown 3 --stall-probe-factor 2
kill "$MOCK_STALL_C_PID" 2>/dev/null || true
MOCK_STALL_C_PID=""

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

# 1b) 5.7 爬坡发车（闭环默认启用）：批次号与启动偏移落盘，首批 1 路、批次递增。
#     注意挂 out-all：基础冒烟默认 concurrency=[1]，1 不组成并发场景，闭环档只在
#     --concurrency 1,cfg 的 smoke-all 里跑（levels=[2] → 批次 [1,1]）；
#     且只看多轮闭环档（单轮闭环没有 sessions 字段）
basic_conc = [lv for rep in load_all("out-all") for lv in rep.get("concurrent", [])
              if not lv.get("request_rate") and lv.get("sessions")]
check(len(basic_conc) > 0, "smoke-all 含闭环多轮并发档位")
sess_all = [s for lv in basic_conc for s in lv.get("sessions", [])]
check(sess_all and all(s.get("batch", 0) >= 1 for s in sess_all),
      f"爬坡发车批次号落盘（{len(sess_all)} 个会话）")
check(all(sum(1 for s in lv.get("sessions", []) if s.get("batch") == 1) == 1 for lv in basic_conc),
      "每个并发档位首批只发 1 个会话")
check(all({s.get("batch") for s in lv.get("sessions", [])} == {1, 2} for lv in basic_conc),
      "level=2 的批次号为 {{1,2}}（首批 1 + 第二批 1）")
check(all([s.get("start_offset_s", -1) for s in lv.get("sessions", [])] ==
          sorted(s.get("start_offset_s", -1) for s in lv.get("sessions", [])) for lv in basic_conc),
      "启动偏移随批次单调不减")

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

# 填充保真度自举校准（10.3）：发已知标称样本、用 usage 反推实测 chars/token。
# mock 的 usage 按 4 chars/token 模拟，与构造系数（corpus.CharsPerToken en=4.0）一致
# → 必须给出"可信"结论并落盘实测值。真机上偏离 >25% 才告警，这里验证好路径不误报。
ff = next((c for c in probe.get("checks", []) if c["name"] == "filler_fidelity"), None)
check(ff is not None and ff.get("ok") and not ff.get("na"),
      f"probe 填充保真度检查通过（{(ff or {}).get('detail', '缺 filler_fidelity 检查项')}）")
cpt = probe.get("filler_cpt") or 0
check(abs(cpt - 4.0) < 0.2, f"probe 实测 chars/token ≈4.0（实际 {cpt}）")

# probe 证据分级：扩展面（/metrics、max_model_len 等）服务端未提供时记 na，绝不判失败；
# 汇总行必须区分标准面与扩展面，避免可选端点的缺失拉低通过率
bad_na = [c["name"] for c in probe.get("checks", []) if c.get("na") and not c.get("ok")]
check(not bad_na, f"probe 扩展面 NA 项不得同时标记为失败（{bad_na or '无'}）")
check(bool(probe.get("summary")) and "标准面" in probe.get("summary", ""),
      f"probe 分级汇总已落盘（实际: {probe.get('summary')!r}）")
check(bool(probe.get("suggested_config")), "probe 已输出可粘贴的配置片段")

# 配置片段约定：凡由**扩展面**推导来的项（/models 的 max_model_len、/metrics 可达性）
# 一律以注释态给出，让人显式确认后再启用。扩展面不是标准 OpenAI 兼容面、网关代理后
# 普遍缺失，默认替用户打开等于替他做了个没依据的决定。
# （回归：此前 server_metrics 被写成生效态，与 README 自述直接矛盾。）
sug_lines = (probe.get("suggested_config") or "").splitlines()
eff_on = [l for l in sug_lines
          if not l.lstrip().startswith("#")
          and (l.startswith("server_metrics:") or l.startswith("max_prompt_tokens:"))]
check(not eff_on, f"probe 配置片段的扩展面推导项须为注释态（实际生效: {eff_on}）")

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

# 10b) 速率扫描（rate_sweep）+ 两源一致性 + slo.baseline：JSON 侧的数据形状
reps = load_all("out-sweep")
cl = [lv for rep in reps for lv in rep.get("concurrent", [])]
check(cl and {lv.get("request_rate") for lv in cl} == {2, 8},
      f"速率扫描两档到达率落盘（实际 {sorted({lv.get('request_rate') for lv in cl})}）")
check(cl and all(lv.get("goodput_rps", 0) > 0 and lv.get("slo_total", 0) > 0 for lv in cl),
      "开环档位 goodput 落盘（slo.goodput 生效）")
# waiting_max 是「本档窗口内新采到的样本峰值」口径（omitempty：0 落键消失 → 取 0 兜底）
wm = {lv.get("request_rate"): (lv.get("waiting_max") or 0) for lv in cl}
check(any(v > 0 for v in wm.values()),
      f"waiting 排队峰值落盘且有非零观测（{wm}）")
# 档位峰值不得超过本场景窗口的服务端观测峰值（越界 = 串了别的窗口的样本）
gw = []
for rep in reps:
    gauges = (rep.get("server_metrics") or {}).get("gauges") or {}
    mx = (gauges.get("waiting") or {}).get("max")
    if isinstance(mx, (int, float)):
        gw.append(mx)
check(gw and all(w <= max(gw) + 0.5 for w in wm.values()),
      f"档位 waiting 峰值不超过场景窗口峰值（档位 {wm}，窗口 {gw}）")
scs = [rep.get("source_check") for rep in reps if rep.get("source_check")]
check(len(scs) == 2 and all(s.get("client_tps", 0) > 0 and s.get("server_tps", 0) > 0 for s in scs),
      f"两源一致性判定落盘（{len(scs)} 份并发产物）")
check(all(abs(s.get("deviation", 9)) <= 0.15 for s in scs),
      f"mock 两源一致（偏差 {[round(s.get('deviation', 9), 3) for s in scs]}，应 ≤15%）")
check(all("deviation" in s for s in scs),
      "deviation 键恒在（0 = 完全一致，不能因 omitempty 消失——否则报告把最好情况渲染成 NA）")
sb = next((rep.get("slo_baseline") for rep in reps if rep.get("slo_baseline")), None)
check(sb is not None and sb.get("long_min_tokens") == 300,
      f"slo.baseline 阈值随 JSON 透出（实际 {sb}）")
plans = [rep.get("plan") for rep in reps if rep.get("plan")]
concs = [s for p in plans for m in (p.get("models") or []) for s in (m.get("scenarios") or [])
         if s.get("name") == "concurrent"]
check(concs and all("开环到达率" in s.get("detail", "") for s in concs),
      f"测试画像按开环口径估算（不是 levels 矩阵；实际 {[s.get('detail') for s in concs][:1]}）")
reqs = sorted({s.get("requests") for s in concs})
check(reqs == [8, 16],
      f"开环画像请求估算：单轮 2 档 × 4 请求 = 8，多轮 2 档 × 4 会话 × 2 轮 = 16（实际取值集 {reqs}）")

# 11) 降速熔断：两场景相继熔断续跑，note 与 stall_trace 双留痕
import glob
stall_reps = load_all("out-stall")
check(len(stall_reps) == 2,
      f"熔断场景两场景都落盘（实际 {len(stall_reps)} 份 JSON）")
check(all("降速熔断" in (r.get("note") or "") for r in stall_reps),
      "每个熔断场景 JSON 的 note 都含熔断原因")
for rep in stall_reps:
    st = rep.get("stall_trace")
    ok_st = bool(st) and os.path.exists(os.path.join(tmp, "out-stall", st))
    check(ok_st, f"{rep.get('scenario')}: stall_trace={st!r} 存在")
csvs = sorted(glob.glob(os.path.join(tmp, "out-stall", "*.stall.csv")))
check(len(csvs) == 2, f"两场景各落一个 .stall.csv（实际 {len(csvs)} 个）")
for csvp in csvs:
    base = os.path.basename(csvp)
    lines = open(csvp, encoding="utf-8").read().splitlines()
    check(lines[0] == "t_s,agg_tps,in_flight,emitting,phase", f"{base}: 表头正确")
    data = [l for l in lines[1:] if l and not l.startswith("#")]
    check(len(data) >= 2, f"{base}: 采样行 {len(data)} 行")
    ts = [float(l.split(",")[0]) for l in data]
    check(all(b > a for a, b in zip(ts, ts[1:])), f"{base}: t_s 单调递增")
    check(any(l.startswith("# tripped:") for l in lines), f"{base}: 熔断原因已留痕")
    phases = {l.split(",")[4] for l in data}
    check("emit" in phases, f"{base}: 有 emit 相位记录（正常段也落盘）")
    check(all(("-1" not in l.split(',')[2]) and l.split(',')[3] != "-1" for l in data),
          f"{base}: in_flight/emitting 无负值")
# --no-stall-trace：字段与文件都不落
nt = load_all("out-stall-notrace")
check(len(nt) == 1 and not nt[0].get("stall_trace"),
      "--no-stall-trace 下 JSON 无 stall_trace 字段")
check(not glob.glob(os.path.join(tmp, "out-stall-notrace", "*.stall.csv")),
      "--no-stall-trace 不落 .stall.csv")

# 11b) 8.2 探针未恢复：冷却后探针实测 < 阈值 → 停止整轮，只剩首个场景输出且 note 留痕
pb = load_all("out-stall-probe")
check(len(pb) == 1 and pb[0].get("scenario") == "single",
      f"探针未恢复：整轮在首个场景后停止（实际 {len(pb)} 份: {[r.get('scenario') for r in pb]}）")
check(any("探针" in (r.get("note") or "") and "停止后续场景" in (r.get("note") or "") for r in pb),
      f"探针未恢复留痕进 note（实际: {[r.get('note') for r in pb]}）")
with open(os.path.join(tmp, "out-stall-probe.log"), encoding="utf-8") as fp:
    plog = fp.read()
check("服务端未恢复" in plog, "探针未恢复日志留痕")

# 11c) 8.2 探针恢复：服务端 3s 后转快 → 探针通过 → 第二个场景正常跑完
rc_reps = load_all("out-stall-recover")
check(len(rc_reps) == 2, f"探针恢复后两场景都跑完（实际 {len(rc_reps)} 份）")
mt_reps = [r for r in rc_reps if r.get("scenario") == "multiturn"]
check(mt_reps and "降速熔断" not in (mt_reps[0].get("note") or ""),
      "恢复后第二个场景正常完成（note 无熔断标记）")
with open(os.path.join(tmp, "out-stall-recover.log"), encoding="utf-8") as fp:
    rlog = fp.read()
check("服务端已恢复" in rlog, "探针通过日志留痕")

if failures:
    print(f"\n❌ 冒烟断言失败 {len(failures)} 项")
    sys.exit(1)
print("\n✅ 全部断言通过")
PYEOF

# ── 报告管线：合并 → HTML 生成 → JS 校验（node 缺失时降级为提示）──
echo "==> 报告管线冒烟（gen_html_report + validate_report）"
# 保留模型分区子目录：多模型产物的文件名相同（scenario-时间戳），平铺 cp 会互相覆盖，
# 只留一个模型 → 多模型落地页/切换器路径根本进不到。
flat_copy() { # flat_copy <源目录> <目标目录>
  local src="$1" dst="$2"
  mkdir -p "$dst"
  for f in $(find "$src" -name '*.json'); do
    local out="$dst/${f#$src/}"
    mkdir -p "$(dirname "$out")"
    cp "$f" "$out"
  done
}
flat_copy "$TMP/out-basic" "$TMP/flat"
python3 scripts/gen_html_report.py "$TMP/flat" >"$TMP/report.log" 2>&1 \
  || { echo "❌ gen_html_report 失败（日志: $TMP/report.log）"; tail -20 "$TMP/report.log"; exit 1; }
HTML="$TMP/flat/llm-perf-报告.html"
[ -f "$HTML" ] || { echo "❌ 报告 HTML 未生成"; exit 1; }
echo "  ✅ 报告 HTML 已生成（$(wc -c <"$HTML" | tr -d ' ') bytes）"
python3 - "$HTML" <<'PYEOF'
import sys, re, html
raw = open(sys.argv[1], encoding="utf-8").read()
t = html.unescape(re.sub(r"<[^>]+>", " ", raw))
assert "数据来源" in t, "报告缺少『数据来源』声明（/metrics 是可选数据源，口径必须在报告里标注）"
bad = re.findall(r"服务端观测（\w+）：。", t)
assert not bad, "报告出现空的服务端观测面板: {}".format(bad)
# 10.2 一页纸：顶层 = 四个数 + 三分归因；详细区降级为折叠附录（能力不砍）
assert 'id="onepager"' in raw, "报告缺少一页纸区块"
assert "一页纸结论" in t and "三分归因" in t, "一页纸未渲染标题/三分归因说明"
for nm in ("① TTFT", "② decode 速度", "③ goodput@SLO", "④ 正确性"):
    assert nm in t, "一页纸缺少四个数之一: {}".format(nm)
for nm in ("服务端推理", "客户端与网络", "负载层（测试设计）"):
    assert nm in t, "三分归因缺少: {}".format(nm)
assert 'id="appendix"' in raw, "详细数据未降级为附录"
assert 'id="appendix" open' not in raw, "附录默认不应展开（顶层只留一页纸）"
# 多模型：一页纸表格必须两个模型都在（平铺 cp 覆盖文件曾导致只剩一个模型）
assert "mock-model-a" in t and "mock-model-b" in t, "多模型报告缺模型"
# 原章节仍在附录里（能力不砍）：抽查几个只可能来自详细区的字样
for nm in ("指标口径", "缓存判定", "数据质量"):
    assert nm in t, "附录缺少原章节内容: {}".format(nm)
assert raw.find("const grid=") < raw.find('class="foot"'), "图表应随详细数据一起收进附录"
print("  ✅ 数据来源已标注；无空的服务端观测面板")
print("  ✅ 一页纸（四个数 + 三分归因）就位；详细区降级为折叠附录")
PYEOF

# 10.4 测试类别（test: benchmark|performance|soak）：结论区按类别切换。
# 复用上面那份 flat 产物改 test 键重渲染——类别只改表达层（Go 侧测量路径与类别无关，
# 配置校验由 go test 覆盖），所以这里零额外压测即可端到端锁住三套口径。
for K in benchmark soak; do
  rm -rf "$TMP/kind-$K"
  cp -r "$TMP/flat" "$TMP/kind-$K"
  python3 - "$TMP/kind-$K" "$K" <<'KINDEOF'
import json, os, sys, glob
d, k = sys.argv[1], sys.argv[2]
for f in glob.glob(os.path.join(d, "**", "*.json"), recursive=True):
    j = json.load(open(f, encoding="utf-8"))
    j["test"] = k
    json.dump(j, open(f, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
KINDEOF
  python3 scripts/gen_html_report.py "$TMP/kind-$K" >"$TMP/kind-$K.log" 2>&1 \
    || { echo "❌ 类别 $K 渲染失败（日志: $TMP/kind-$K.log）"; tail -20 "$TMP/kind-$K.log"; exit 1; }
done
python3 - "$TMP/flat/llm-perf-报告.html" "$TMP/kind-benchmark/llm-perf-报告.html" "$TMP/kind-soak/llm-perf-报告.html" <<'PYEOF'
import sys
perf, bench, soak = (open(p, encoding="utf-8").read() for p in sys.argv[1:4])
# performance 是默认类别：其 HTML 必须与历史产物逐字同形（无类别块），否则老产物重渲染会漂移
assert "class=\"kn\"" not in perf, "performance（默认）不该有类别块"
assert "顶层只有四个数" in perf, "performance 一页纸口径说明缺失"
assert "基准口径" in bench and "class=\"kn\"" in bench, "benchmark 未渲染基准口径块"
assert "稳定性口径" in soak and "是否随时间退化" in soak, "soak 未渲染稳定性三问块"
for nm, h in (("benchmark", bench), ("soak", soak)):
    assert "id=\"onepager\"" in h and "id=\"appendix\"" in h, "{} 的一页纸/折叠附录缺失".format(nm)
# 类别只换首屏口径，四个数一个不少
for nm, h in (("benchmark", bench), ("soak", soak)):
    for k in ("① TTFT", "② decode 速度", "③ goodput@SLO", "④ 正确性"):
        assert k in h, "{} 缺少四个数之一: {}".format(nm, k)
print("  ✅ 类别切换：performance（默认）无类别块 / benchmark 基准口径 / soak 稳定性三问，四数俱在")
PYEOF

if command -v node >/dev/null 2>&1; then
  node scripts/validate_report.js "$HTML" >"$TMP/validate.log" 2>&1 \
    || { echo "❌ validate_report 校验失败（日志: $TMP/validate.log）"; tail -20 "$TMP/validate.log"; exit 1; }
  echo "  ✅ validate_report.js 校验通过"
else
  echo "  ⚠️ 无 node，跳过 validate_report.js（报告 JS 校验未执行）"
fi

# ── 报告侧：速率扫描（9.2）+ 测试画像（5.10）+ 服务端延迟分解（11.2）+ 两源一致性（10.1）──
echo "==> 报告管线冒烟（速率扫描 / 画像 / 直方图 / 两源一致性）"
mkdir -p "$TMP/sweepflat"
find "$TMP/out-sweep" -name '*.json' -exec cp {} "$TMP/sweepflat/" \;
python3 scripts/gen_html_report.py "$TMP/sweepflat" >"$TMP/report-sweep.log" 2>&1 \
  || { echo "❌ gen_html_report 失败（速率扫描；日志: $TMP/report-sweep.log）"; tail -20 "$TMP/report-sweep.log"; exit 1; }
HTML2="$TMP/sweepflat/llm-perf-报告.html"
[ -f "$HTML2" ] || { echo "❌ 速率扫描报告 HTML 未生成"; exit 1; }
python3 - "$HTML2" <<'PYEOF'
import sys, re, html
t = html.unescape(re.sub(r"<[^>]+>", " ", open(sys.argv[1], encoding="utf-8").read()))
need = {
    "速率扫描（开环到达率）": "9.2 速率扫描表",
    "吞吐拐点": "吞吐拐点结论",
    "goodput 达标上限": "goodput 上限结论",
    "测试画像": "5.10 测试画像表渲染（此前落盘无人读）",
    "服务端延迟分解": "11.2 直方图窗口差值表",
    "两源一致性（客户端": "10.1 两源一致性判定",
    "waiting 峰值": "9.4 waiting 排队峰值列",
    "max_waiting": "9.4 阈值标定建议",
    "≥300": "slo.baseline 阈值覆盖生效（档位标签跟随 JSON 阈值）",
}
missing = [why for key, why in need.items() if key not in t]
assert not missing, "报告缺少: {}".format("、".join(missing))
# 画像表必须列出开环到达率（而不是被忽略的 levels 矩阵）
assert "开环到达率" in t, "测试画像未按开环口径渲染"
print("  ✅ 速率扫描 / 画像 / 直方图 / 两源一致性 全部渲染")
PYEOF

echo "==> 冒烟完成（运行目录已清理；详细日志见上方各步输出）"
