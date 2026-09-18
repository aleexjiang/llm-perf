#!/usr/bin/env python3
"""profile-build：从外部 raw trace 提取会话形状特征，产出 user 模式的 profile JSON。

职责边界（见 docs/workload-refactor-plan.md）：
  - 只提取统计特征（轮次分布、上下文增量分布），不提取任何消息文本；
  - 连续 user 消息视为异常段：跳过多余 user，只记录清洗计数；
  - 权重默认 6:3:1（light:medium:heavy），可用 --weights 覆盖；
  - 输出 profile 供 `bench user` 运行时消费；raw trace 不进入运行时。

用法：
  python3 scripts/profile_build.py \
    --trace /path/to/trace-real-128.json \
    --out /path/to/profile.json \
    [--weights 6,3,1] [--chars-per-token-en 4.0] [--chars-per-token-zh 1.4]

trace 格式：ShareGPT（[{"conversations":[{"from","value"},...]}]）或
兼容 role/content 变体；与 internal/engine/trace.go 的解析口径一致。
"""

from __future__ import annotations

import argparse
import json
import re
import statistics
import sys
from datetime import datetime, timezone
from pathlib import Path

# 档位定义：按有效 user 轮次划分（与 plan 13.2 一致）
PROFILE_TURN_RANGES = {
    "light": (2, 4),
    "medium": (5, 8),
    "heavy": (9, 10**9),
}
PROFILE_ORDER = ["light", "medium", "heavy"]

# agent 形状硬约束（plan 13.2）：首轮 prompt ≥ 35K token（基座+首轮 user/context）
FIRST_TURN_MIN_TOKENS = 35000
FIRST_TURN_MAX_TOKENS = 40000

DEFAULT_WEIGHTS = "6,3,1"


def norm_role(msg: dict) -> str:
    role = str(msg.get("from") or msg.get("role") or "").lower()
    if role in ("human", "user"):
        return "user"
    if role in ("gpt", "bot", "chatgpt", "assistant"):
        return "assistant"
    if role == "system":
        return "system"
    if role in ("function", "tool", "observation"):
        return "tool"
    return role


def msg_text(msg: dict) -> str:
    return msg.get("value") or msg.get("content") or ""


def quantile(values: list[float], p: float) -> float:
    if not values:
        return 0.0
    xs = sorted(values)
    idx = p / 100 * (len(xs) - 1)
    lo, hi = int(idx // 1), int(-(-idx // 1))
    if lo == hi:
        return xs[lo]
    return xs[lo] + (xs[hi] - xs[lo]) * (idx - lo)


def range_of(values: list[float], lo_p=10, hi_p=90) -> list[int]:
    if not values:
        return [0, 0]
    return [int(quantile(values, lo_p)), int(quantile(values, hi_p))]


def parse_trace(path: Path):
    data = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(data, list) or not data:
        sys.exit(f"trace {path} 不是非空数组（ShareGPT 格式）")
    return data


def extract_sessions(data: list) -> tuple[list[dict], dict]:
    """按合法 user 轮切块；连续 user 记为异常段并清洗。

    返回 (sessions, cleaning)；每个 session：
      turns: 有效 user 轮数
      blocks: 每个有效轮的 [user_chars, follow_chars]
        user_chars  = 该 user 消息字符数（进入本轮 prompt 的新增输入）
        follow_chars = 本轮 assistant/tool/system 的字符总量（进入下一轮历史的新增上下文）
    """
    sessions = []
    cleaning = {
        "raw_sessions": len(data),
        "kept_sessions": 0,
        "consecutive_user_runs": 0,
        "consecutive_user_messages_dropped": 0,
        "sessions_affected": 0,
    }
    for conv in data:
        msgs = conv.get("conversations") or conv.get("messages") or []
        blocks: list[list[int]] = []
        i = 0
        n = len(msgs)
        run_dropped = 0
        while i < n:
            if norm_role(msgs[i]) != "user":
                i += 1
                continue
            # 连续 user：保留第一条，其余记为异常丢弃
            j = i + 1
            while j < n and norm_role(msgs[j]) == "user":
                j += 1
            run = j - i
            if run > 1:
                cleaning["consecutive_user_runs"] += 1
                cleaning["consecutive_user_messages_dropped"] += run - 1
                run_dropped += run - 1
            user_chars = len(msg_text(msgs[i]))
            follow_chars = 0
            k = j
            while k < n and norm_role(msgs[k]) != "user":
                follow_chars += len(msg_text(msgs[k]))
                k += 1
            blocks.append([user_chars, follow_chars])
            i = k
        if run_dropped:
            cleaning["sessions_affected"] += 1
        if blocks:
            sessions.append({"turns": len(blocks), "blocks": blocks})
            cleaning["kept_sessions"] += 1
    return sessions, cleaning


def classify(turns: int) -> str | None:
    for name in PROFILE_ORDER:
        lo, hi = PROFILE_TURN_RANGES[name]
        if lo <= turns <= hi:
            return name
    return None  # turns < 2 的单轮会话不进入多轮 profile


def cpr_for(path_hint: str, en: float, zh: float) -> float:
    # WorkBuddy/CodeBuddy 类 trace 为中英混合；按两者均值近似（只做形状估算，最终以 usage 为准）
    _ = path_hint
    return (en + zh) / 2


def main() -> None:
    ap = argparse.ArgumentParser(description="trace 会话形状特征 → user 模式 profile")
    ap.add_argument("--trace", required=True, help="raw trace 路径（ShareGPT JSON）")
    ap.add_argument("--out", required=True, help="输出 profile JSON 路径")
    ap.add_argument("--weights", default=DEFAULT_WEIGHTS,
                    help=f"light,medium,heavy 权重（默认 {DEFAULT_WEIGHTS}）")
    ap.add_argument("--chars-per-token-en", type=float, default=4.0)
    ap.add_argument("--chars-per-token-zh", type=float, default=1.4)
    ap.add_argument("--min-turns", type=int, default=2, help="低于该轮数的会话不入 profile")
    args = ap.parse_args()

    weights = [float(x) for x in args.weights.split(",")]
    if len(weights) != len(PROFILE_ORDER) or any(w < 0 for w in weights) or sum(weights) <= 0:
        sys.exit(f"--weights 非法: {args.weights}（需要 {len(PROFILE_ORDER)} 个非负数且总和 > 0）")

    raw = parse_trace(Path(args.trace))
    sessions, cleaning = extract_sessions(raw)
    cpr = cpr_for(args.trace, args.chars_per_token_en, args.chars_per_token_zh)

    buckets: dict[str, dict] = {name: {"sessions": 0, "user_inputs": [], "follows": []} for name in PROFILE_ORDER}
    skipped_short = 0
    for s in sessions:
        if s["turns"] < args.min_turns:
            skipped_short += 1
            continue
        name = classify(s["turns"])
        if name is None:
            skipped_short += 1
            continue
        b = buckets[name]
        b["sessions"] += 1
        for user_chars, follow_chars in s["blocks"]:
            b["user_inputs"].append(user_chars / cpr)
            b["follows"].append(follow_chars / cpr)

    profiles = {}
    for name, weight in zip(PROFILE_ORDER, weights):
        b = buckets[name]
        lo, hi = PROFILE_TURN_RANGES[name]
        if name == "heavy":
            hi = None  # 上限由运行时 max turns 保护，profile 不写死
        profiles[name] = {
            "weight": round(weight / sum(weights), 4),
            "turns_range": [lo, hi] if hi else [lo],
            # user 输入长度分布（token）：每轮新增的 user 文本
            "user_input_tokens": range_of(b["user_inputs"]),
            # 每轮进入下一轮历史的上下文增量（token）：trace 中 assistant+tool 体积的代理
            "context_tokens": range_of(b["follows"]),
            "trace_sessions": b["sessions"],
        }

    profile = {
        "version": 1,
        "generated_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "source": str(Path(args.trace).name),
        "first_turn_tokens": [FIRST_TURN_MIN_TOKENS, FIRST_TURN_MAX_TOKENS],
        "profiles": profiles,
        "cleaning": {**cleaning, "skipped_short_turns": skipped_short},
        "notes": [
            "weights 为人工设定的运行比例（默认 6:3:1），非 trace 实测占比",
            "turns_range 来自 trace 轮次分布，仅作形状参考",
            "user_input_tokens/context_tokens 为 trace 字符量按混合 chars-per-token 折算的代理分布",
            "运行时文本一律由经典书语料按 seed 生成，profile 不携带任何 trace 消息文本",
        ],
    }
    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(profile, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")

    print(f"profile → {out}")
    for name, p in profiles.items():
        print(f"  {name:7} weight={p['weight']:<6} turns={p['turns_range']} "
              f"user_input={p['user_input_tokens']}tk context/turn={p['context_tokens']}tk "
              f"(trace sessions={p['trace_sessions']})")
    c = cleaning
    print(f"cleaning: raw={c['raw_sessions']} kept={c['kept_sessions']} "
          f"consecutive_user_runs={c['consecutive_user_runs']} dropped_msgs={c['consecutive_user_messages_dropped']}")


if __name__ == "__main__":
    main()
