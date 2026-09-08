#!/usr/bin/env bash
# llm-perf 自动化冒烟：mock 服务 + 多组合运行 + 输出断言。
#
# 背景（2026-09-08 现场踩坑）：-thinking off 被 model_overrides.thinking.mode=both 反超，
# on 变体照跑。当时冒烟只"跑通看输出"没有断言，且 smoke.yaml 没有 model_overrides，
# 踩坑代码路径根本没被执行——本脚本把这类"数据形状错误"变成硬断言。
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

# ── mock OpenAI 兼容服务（stream + enable_thinking + /metrics）──
echo "==> 启动 mock_server (127.0.0.1:$PORT)"
python3 scripts/mock_server.py &
MOCK_PID=$!
ready=0
for _ in $(seq 1 25); do
  if curl -sf "http://127.0.0.1:$PORT/metrics" >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.2
done
[ "$ready" = 1 ] || { echo "❌ mock_server 未就绪"; exit 1; }

run() { # run <label> <outdir> [bench 参数...]
  local label="$1" out="$2"
  shift 2
  echo "==> bench: $label"
  "$BENCH" -o "$TMP/$out" "$@" >"$TMP/$out.log" 2>&1 \
    || { echo "❌ bench 失败: $label（日志: $TMP/$out.log）"; tail -30 "$TMP/$out.log"; exit 1; }
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

# ── 断言：校验输出 JSON 的模型×变体分布，不再靠目测 ──
echo "==> 断言输出数据形状"
python3 - "$TMP" <<'PYEOF'
import json, os, sys

tmp = sys.argv[1]
failures = []

def rows(outdir):
    """递归收集输出 JSON 的 (model, thinking, single_prompt_tokens) 行。"""
    out = []
    for root, _, files in os.walk(os.path.join(tmp, outdir)):
        for f in files:
            if not f.endswith(".json"):
                continue
            with open(os.path.join(root, f), encoding="utf-8") as fp:
                rep = json.load(fp)
            for r in rep.get("single", []):
                out.append({"model": r.get("model"), "thinking": r.get("thinking"),
                            "pt": r.get("prompt_tokens")})
            for r in rep.get("multiturn", []):
                out.append({"model": r.get("model"), "thinking": r.get("thinking"), "pt": None})
            for lv in rep.get("concurrent", []):
                out.append({"model": lv.get("model"), "thinking": lv.get("thinking"), "pt": None})
    return out

def check(cond, msg):
    print(("  ✅ " if cond else "  ❌ ") + msg)
    if not cond:
        failures.append(msg)

# 1) 基础冒烟：两模型都在，on/off 变体都有
basic = rows("out-basic")
models = {r["model"] for r in basic}
think = {r["thinking"] for r in basic}
check(len(basic) > 0, f"基础冒烟有数据（{len(basic)} 行）")
check({"mock-model-a", "mock-model-b"} <= models, f"两模型都有数据: {sorted(models)}")
check({"on", "off"} <= think, f"both 模式 on/off 变体都出现: {sorted(think)}")

# 2) -thinking off：所有行必须是 off；两模型都有 off 数据（回归点：旧 bug 下 b 混入 on）
off = rows("out-off")
check(len(off) > 0, f"off 过滤有数据（{len(off)} 行）")
check(all(r["thinking"] == "off" for r in off), "off 过滤后不存在任何 on 行")
check({r["model"] for r in off} == {"mock-model-a", "mock-model-b"},
      "off 过滤后两模型都有 off 数据（overrides 未反超 CLI）")

# 3) -thinking on：off-only 模型整体跳过；有数据的行全是 on
on = rows("out-on")
check(len(on) > 0, f"on 过滤有数据（{len(on)} 行）")
check({r["model"] for r in on} == {"mock-model-b"}, "on 过滤后只剩 overrides 模型 b（a 整模型跳过）")
check(all(r["thinking"] == "on" for r in on), "on 过滤后所有行都是 on")

# 4) --max-ctx 300：配置档位 500 应全部截断到 ≤300
ctx = [r for r in rows("out-ctx") if r["pt"]]
check(len(ctx) > 0, "max-ctx 运行有 single 数据")
check(all(r["pt"] <= 300 for r in ctx), f"prompt_tokens 全部 ≤ 300（实际: {sorted({r['pt'] for r in ctx})}）")

if failures:
    print(f"\n❌ 冒烟断言失败 {len(failures)} 项")
    sys.exit(1)
print("\n✅ 全部断言通过")
PYEOF

echo "==> 冒烟完成（运行目录已清理；详细日志见上方各步输出）"
