#!/usr/bin/env bash
# llm-perf 冒烟：覆盖四个子命令（probe / user / rps / concurrency）与数据契约。
# 2026-09-18 新架构：filler/旧 multiturn+concurrent 入口已下线，请求/会话形状
# 来自 request_set.sharegpt_path（冻结快照）与 user.profile_path（profile 特征）。
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

# output_dir 重定向到临时目录；fixtures 相对路径重写到仓库绝对路径（冒烟从任意 cwd 运行）
for f in smoke-probe smoke-user smoke-rps smoke-conc; do
  sed -e "s#^output_dir:.*#output_dir: $TMP/$f#" \
      -e "s#\([[:space:]]*profile_path:[[:space:]]*\"\{0,1\}\)fixtures/#\1$PWD/configs/fixtures/#" \
      -e "s#\([[:space:]]*sharegpt_path:[[:space:]]*\"\{0,1\}\)fixtures/#\1$PWD/configs/fixtures/#" \
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
  local label="$1" out="$2" cmd="$3"
  shift 3
  echo "==> $label"
  "$BENCH" "$cmd" -o "$TMP/$out" "$@" >"$TMP/$out.log" 2>&1 \
    || { echo "❌ $label 失败"; tail -40 "$TMP/$out.log"; exit 1; }
}

# probe：独立 JSON，采集多模型列表与能力检查项。
echo "==> probe"
"$BENCH" probe -c "$TMP/smoke-probe.yaml" -o "$TMP/probe.json" >"$TMP/probe.log" 2>&1 \
  || { echo "❌ probe 失败"; tail -40 "$TMP/probe.log"; exit 1; }

# user：生成式多轮用户会话（profile 分派 + 语料生成 + 真实 assistant 进 history）。
run "user 生成式多轮" out-user user -c "$TMP/smoke-user.yaml" --seed-salt 1

# rps：冻结请求快照开环到达（高到达率近齐射，冒烟只验证调度与落盘形态）。
run "rps 开环到达" out-rps rps -c "$TMP/smoke-rps.yaml" --seed-salt 2

# concurrency：固定在飞齐射，多档位。
run "concurrency 在飞齐射" out-conc concurrency -c "$TMP/smoke-conc.yaml" --seed-salt 3

# 旧入口必须拒绝：子命令化后裸 bench / --turns / --concurrency 不再是合法入口。
if "$BENCH" -c "$TMP/smoke-user.yaml" >"$TMP/legacy.log" 2>&1; then
  echo "❌ 裸 bench（无子命令）不应继续执行"; cat "$TMP/legacy.log"; exit 1
fi
if "$BENCH" -c "$TMP/smoke-user.yaml" --concurrency 2,4 >/dev/null 2>&1; then
  echo "❌ --concurrency 旧入口不应继续执行"; exit 1
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

# ── user：profile 分派、轮次、真实 assistant 进 history（prompt 逐轮增长）──
user = load_all("out-user")
check(len(user) == 1, f"user 产物按模型落盘（{len(user)} 份）")
rep = user[0]
check(rep.get("scenario") == "user", "报告 scenario=user")
check(rep.get("schema_version") == 7, "数据契约版本为 7")
sessions = rep.get("multiturn") or []
check(len(sessions) == 2, f"users=2 应有 2 条会话（{len(sessions)}）")
check(all(s.get("profile") in ("light", "medium", "heavy") for s in sessions), "会话带 profile 档位标签")
check(all(len(s.get("turns") or []) >= 2 for s in sessions), "user 会话均为多轮（≥2 轮）")
ok_growth = True
for s in sessions:
    ts = s.get("turns") or []
    if len(ts) > 1 and ts[-1].get("prompt_tokens", 0) <= ts[0].get("prompt_tokens", 0):
        ok_growth = False
check(ok_growth, "assistant 回复进 history：prompt 逐轮增长（动态 prefix cache）")
first_ok = all((s.get("turns") or [{}])[0].get("prompt_tokens", 0) >= 35000 for s in sessions)
check(first_ok, "首轮 prompt ≥35K token（agent 形状硬约束）")
metrics_user = [t for s in sessions for t in (s.get("turns") or [])]
check(metrics_user and all(m.get("phase") == "benchmark" for m in metrics_user), "主压测 TurnMetrics 标记 phase=benchmark")

# ── rps：开环到达 + 冻结快照 ──
rps = load_all("out-rps")
rps_levels = [lv for rep2 in rps for lv in rep2.get("concurrent", [])]
check(rps_levels and all(lv.get("request_rate", 0) > 0 for lv in rps_levels), "RPS 档位 request_rate 落盘")
check(rps_levels and all(len(lv.get("requests") or []) == 6 for lv in rps_levels), "RPS 档位 6 条冻结请求全部落盘")
check(rps_levels and all(lv.get("completed_requests") == 6 for lv in rps_levels), "RPS 档位 completed 计数正确")

# ── concurrency：固定在飞齐射，多档位 ──
conc = load_all("out-conc")
conc_levels = [lv for rep2 in conc for lv in rep2.get("concurrent", [])]
check([lv.get("level") for lv in conc_levels] == [1, 2], "concurrency 两档位按序落盘（level 1,2）")
check(all(len(lv.get("requests") or []) == 6 for lv in conc_levels), "每档位 6 条请求全部落盘")
check(all(lv.get("completed_requests") == 6 and lv.get("failed_requests") == 0 for lv in conc_levels), "concurrency completed/failed 计数正确")

# ── probe ──
probe = json.load(open(os.path.join(root, "probe.json"), encoding="utf-8"))
check(bool(probe.get("checks")), "probe checks 落盘")

if failures:
    print(f"\n❌ 冒烟断言失败 {len(failures)} 项")
    sys.exit(1)
print("\n✅ 核心数据采集冒烟全部通过")
PY

echo "==> 冒烟完成"
