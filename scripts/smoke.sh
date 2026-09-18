#!/usr/bin/env bash
# llm-perf 核心数据采集冒烟：只覆盖多轮 agent、RPS/并发、多模型、思考变体、probe 与完整辅助请求落盘。
# 单发单轮已从公共执行入口移除；所有 benchmark 请求均为多轮会话。
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

for f in smoke smoke-all smoke-openloop; do
  sed -e "s#^output_dir:.*#output_dir: $TMP/$f#" \
      -e "s#\([[:space:]]*path:[[:space:]]*[\"']\{0,1\}\)fixtures/#\1$PWD/configs/fixtures/#" \
      "configs/$f.yaml" >"$TMP/$f.yaml"
done

if [ -n "${BENCH:-}" ]; then
  echo "==> 使用指定二进制: $BENCH"
else
  GO_BIN="${GO_BIN:-go}"
  "$GO_BIN" build -o "$TMP/bench" ./cmd/bench
  BENCH="$TMP/bench"
fi

echo "==> 启动 mock_server (127.0.0.1:$PORT)"
MOCK_PORT="$PORT" python3 scripts/mock_server.py &
MOCK_PID=$!
for _ in $(seq 1 25); do
  if curl -sf "http://127.0.0.1:$PORT/metrics" >/dev/null 2>&1; then break; fi
  sleep 0.2
done
curl -sf "http://127.0.0.1:$PORT/metrics" >/dev/null

run() {
  local label="$1" out="$2"
  shift 2
  echo "==> $label"
  "$BENCH" -o "$TMP/$out" "$@" >"$TMP/$out.log" 2>&1 \
    || { echo "❌ $label 失败"; tail -40 "$TMP/$out.log"; exit 1; }
}

# 多模型单发多轮：验证模型独立分区、thinking 变体、warmup/correctness 完整采集。
run "多模型单发多轮" out-multi -c "$TMP/smoke.yaml" --turns multi --concurrency 1
# 多模型多用户多轮：验证闭环并发数据仍以 session/turn 为主。
run "多模型闭环多轮" out-concurrent -c "$TMP/smoke.yaml" --turns multi --concurrency cfg --thinking off
# 开环 RPS：request_rate/rate_sweep 是容量采集主路径。
run "RPS 多轮" out-rps -c "$TMP/smoke-openloop.yaml" --turns multi --concurrency cfg --thinking off
# 全能力：server_metrics、trace、多轮、goodput、correctness。
run "全能力多轮" out-all -c "$TMP/smoke-all.yaml" --turns multi --concurrency 1 --thinking off

# probe 必须独立落盘，支持配置中的多个模型；这里验证默认选择与模型列表采集。
echo "==> probe"
"$BENCH" probe -c "$TMP/smoke.yaml" -o "$TMP/probe.json" >"$TMP/probe.log" 2>&1 \
  || { echo "❌ probe 失败"; tail -40 "$TMP/probe.log"; exit 1; }

# 公共入口拒绝 single，防止产品范围回退。
if "$BENCH" -c "$TMP/smoke.yaml" --turns single --concurrency 1 >"$TMP/single.log" 2>&1; then
  echo "❌ --turns single 不应继续执行"; cat "$TMP/single.log"; exit 1
fi

python3 - "$TMP" <<'PY'
import json
import os
import sys

root = sys.argv[1]
failures = []

def check(cond, msg):
    print(("  ✅ " if cond else "  ❌ ") + msg)
    if not cond:
        failures.append(msg)

def load_all(name):
    out = []
    base = os.path.join(root, name)
    for current, _, files in os.walk(base):
        for fn in files:
            if fn.endswith(".json"):
                with open(os.path.join(current, fn), encoding="utf-8") as f:
                    out.append(json.load(f))
    return out

def metrics(rep):
    out = []
    for row in rep.get("multiturn", []):
        out.extend(row.get("turns") or [])
    for level in rep.get("concurrent", []):
        out.extend(level.get("requests") or [])
        for session in level.get("sessions") or []:
            out.extend(session.get("turns") or [])
    return out

multi = load_all("out-multi")
check(len(multi) == 2, f"多模型单发多轮按模型落盘（{len(multi)} 份）")
check(all(not rep.get("single") for rep in multi), "多轮产物不再包含 single 场景")
check(all(rep.get("schema_version") == 4 for rep in multi), "数据契约版本为 4")
check(all(rep.get("multiturn") for rep in multi), "单发多轮产物含 multiturn")
aux = [a for rep in multi for a in (rep.get("auxiliary_requests") or [])]
check(sum(a.get("phase") == "warmup" for a in aux) >= 2, "warmup 完整 TurnMetrics 落盘")
check(sum(a.get("phase") == "correctness" for a in aux) >= 2, "correctness 完整 TurnMetrics 落盘")
check(all((a.get("metrics") or {}).get("phase") == a.get("phase") for a in aux), "辅助请求 phase 与 metrics 一致")

cm = load_all("out-concurrent")
levels = [lv for rep in cm for lv in rep.get("concurrent", [])]
check(levels and all(lv.get("sessions") for lv in levels), "闭环并发产物以多轮 sessions 落盘")
check(levels and all(lv.get("completed_requests", 0) >= 0 and lv.get("failed_requests", 0) >= 0 for lv in levels), "并发档位落盘 completed/failed/cancelled 分类")

rps = load_all("out-rps")
rps_levels = [lv for rep in rps for lv in rep.get("concurrent", [])]
check(rps_levels and all(lv.get("request_rate", 0) > 0 for lv in rps_levels), "RPS 档位 request_rate 落盘")
check(rps_levels and all(lv.get("sessions") or lv.get("requests") for lv in rps_levels), "RPS 档位请求数据落盘")

all_reps = load_all("out-all")
all_metrics = [m for rep in all_reps for m in metrics(rep)]
check(all_metrics and all(m.get("phase") == "benchmark" for m in all_metrics), "主压测 TurnMetrics 标记 phase=benchmark")
probe = json.load(open(os.path.join(root, "probe.json"), encoding="utf-8"))
check(set(probe.get("models") or []) >= {"mock-model-a", "mock-model-b"}, "probe 采集多模型列表")
check(bool(probe.get("checks")), "probe checks 落盘")

if failures:
    print(f"\n❌ 冒烟断言失败 {len(failures)} 项")
    sys.exit(1)
print("\n✅ 核心数据采集冒烟全部通过")
PY

echo "==> 冒烟完成"
