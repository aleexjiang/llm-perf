#!/usr/bin/env python3
"""profile-build：生成 user 模式的受控 light/medium/heavy profile。

trace 只用于离线分析请求形状（见 scripts/trace_shape.py），不作为回放或 profile 数据源。
本脚本输出固定的单调 workload 梯度；需要贴近 trace 时，先看 trace_shape 摘要，再手工调整 profile。

用法：
  python3 scripts/profile_build.py --out /path/to/profile.json [--weights 6,3,1]
"""

from __future__ import annotations

import argparse
import json
import sys
from datetime import datetime, timezone
from pathlib import Path

PROFILE_ORDER = ["light", "medium", "heavy"]
PROFILE_TURN_RANGES = {
    "light": [2, 4],
    "medium": [5, 8],
    "heavy": [9],
}
FIRST_TURN_TOKENS = [35000, 40000]
DEFAULT_WEIGHTS = "6,3,1"

# light=少轮次+低突增，medium 居中，heavy=多轮次+高突增。
CONTROLLED_AGENT_RANGES = {
    "light": {"user_input_tokens": [20, 120], "context_tokens": [0, 0],
              "context_burst_probability": 0.01, "context_burst_tokens": [8000, 30000]},
    "medium": {"user_input_tokens": [30, 200], "context_tokens": [0, 0],
               "context_burst_probability": 0.05, "context_burst_tokens": [3000, 8000]},
    "heavy": {"user_input_tokens": [30, 200], "context_tokens": [0, 0],
              "context_burst_probability": 0.10, "context_burst_tokens": [10000, 25000]},
}


def main() -> None:
    ap = argparse.ArgumentParser(description="生成受控 user workload profile")
    ap.add_argument("--out", required=True, help="输出 profile JSON 路径")
    ap.add_argument("--weights", default=DEFAULT_WEIGHTS,
                    help=f"light,medium,heavy 权重（默认 {DEFAULT_WEIGHTS}）")
    args = ap.parse_args()

    weights = [float(x) for x in args.weights.split(",")]
    if len(weights) != len(PROFILE_ORDER) or any(w < 0 for w in weights) or sum(weights) <= 0:
        sys.exit(f"--weights 非法: {args.weights}（需要 {len(PROFILE_ORDER)} 个非负数且总和 > 0）")

    profiles = {}
    for name, weight in zip(PROFILE_ORDER, weights):
        profiles[name] = {
            "weight": round(weight / sum(weights), 4),
            "turns_range": PROFILE_TURN_RANGES[name],
            **CONTROLLED_AGENT_RANGES[name],
        }

    profile = {
        "version": 1,
        "generated_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "source": "controlled-agent-workload",
        "first_turn_tokens": FIRST_TURN_TOKENS,
        "profiles": profiles,
        "notes": [
            "weights 为运行比例，不来自 trace 实测",
            "普通轮只有短输入，按 context_burst_probability 选择少数上下文突增轮",
            "user_input_tokens/context_tokens/context_burst_tokens 均为 token 数",
            "运行时文本由内置语料按 seed 生成",
        ],
    }
    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(profile, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")

    print(f"profile -> {out}")
    for name, p in profiles.items():
        print(f"  {name:7} weight={p['weight']:<6} turns={p['turns_range']} "
              f"user_input={p['user_input_tokens']}tk "
              f"context_burst={p['context_burst_probability']:.0%}x{p['context_burst_tokens']}tk")


if __name__ == "__main__":
    main()
