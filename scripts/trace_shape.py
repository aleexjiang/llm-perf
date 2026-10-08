#!/usr/bin/env python3
"""trace_shape：离线分析 trace 请求形状，只输出统计摘要，不生成可回放数据。

用法：
  python3 scripts/trace_shape.py --trace trace.json [--out shape.json]

支持 ShareGPT JSON 数组、每行一个会话的 JSONL，以及每行一个请求的 JSONL
（`session` / `turn` / `prompt`）。消息可用 from/value 或 role/content。
输出的是 token 近似值，用于人工设定 user profile 和 YAML 首轮参数；运行时文本仍由内置语料生成。
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path


def role(msg: dict) -> str:
    value = str(msg.get("from") or msg.get("role") or "").lower()
    if value in ("human", "user"):
        return "user"
    if value in ("gpt", "bot", "chatgpt", "assistant"):
        return "assistant"
    if value in ("function", "tool", "observation"):
        return "tool"
    return value


def text(msg: dict) -> str:
    value = msg.get("value", msg.get("content", ""))
    return value if isinstance(value, str) else json.dumps(value, ensure_ascii=False)


def load(path: Path) -> list[dict]:
    raw = path.read_text(encoding="utf-8")
    if path.suffix == ".jsonl":
        return [json.loads(line) for line in raw.splitlines() if line.strip()]
    data = json.loads(raw)
    if not isinstance(data, list):
        raise SystemExit("trace 顶层必须是数组，或使用 .jsonl")
    return data


def request_rows(rows: list[dict]) -> bool:
    return bool(rows) and all("prompt" in row and "session" in row for row in rows[:20])


def prompt_chars(value) -> int:
    if isinstance(value, str):
        return len(value)
    if isinstance(value, list):
        return sum(len(text(msg)) for msg in value if isinstance(msg, dict))
    return len(json.dumps(value, ensure_ascii=False))


def pct(values: list[float], p: float) -> int:
    if not values:
        return 0
    xs = sorted(values)
    pos = (len(xs) - 1) * p
    lo, hi = int(pos), min(int(pos) + 1, len(xs) - 1)
    return round(xs[lo] + (xs[hi] - xs[lo]) * (pos - lo))


def dist(values: list[float]) -> dict:
    return {
        "count": len(values),
        "p10": pct(values, 0.10),
        "p50": pct(values, 0.50),
        "p90": pct(values, 0.90),
        "p95": pct(values, 0.95),
        "max": round(max(values)) if values else 0,
    }


def main() -> None:
    ap = argparse.ArgumentParser(description="trace 请求形状统计")
    ap.add_argument("--trace", required=True)
    ap.add_argument("--out")
    ap.add_argument("--chars-per-token", type=float, default=2.7, help="中英混合近似换算，默认 2.7")
    ap.add_argument("--burst-threshold", type=int, default=3000, help="单轮新增超过该 token 数记为上下文突增")
    args = ap.parse_args()

    first, user_inputs, increments, turns = [], [], [], []
    bursts = 0
    rows = load(Path(args.trace))
    if request_rows(rows):
        sessions: dict[str, list[dict]] = {}
        for row in rows:
            sessions.setdefault(str(row["session"]), []).append(row)
        for items in sessions.values():
            items.sort(key=lambda row: row.get("turn", 0))
            sizes = [prompt_chars(row["prompt"]) / args.chars_per_token for row in items]
            first.append(sizes[0])
            for prev, cur in zip(sizes, sizes[1:]):
                inc = max(0, cur - prev)
                increments.append(inc)
                bursts += inc >= args.burst_threshold
            turns.append(len(items))
    else:
        for conv in rows:
            msgs = conv.get("conversations") or conv.get("messages") or []
            prompt_chars_total = 0
            increment_chars = 0
            turn_count = 0
            for msg in msgs:
                chars = len(text(msg))
                if role(msg) == "user":
                    prompt_tokens = (prompt_chars_total + chars) / args.chars_per_token
                    increment_tokens = (increment_chars + chars) / args.chars_per_token
                    if turn_count == 0:
                        first.append(prompt_tokens)
                    else:
                        increments.append(increment_tokens)
                        bursts += increment_tokens >= args.burst_threshold
                    user_inputs.append(chars / args.chars_per_token)
                    turn_count += 1
                    increment_chars = 0
                else:
                    increment_chars += chars
                prompt_chars_total += chars
            if turn_count:
                turns.append(turn_count)

    shape = {
        "source": Path(args.trace).name,
        "format": "request_jsonl" if request_rows(rows) else "conversations",
        "chars_per_token": args.chars_per_token,
        "sessions": len(turns),
        "turns": dist(turns),
        "first_turn_prompt_tokens": dist(first),
        "user_input_tokens": dist(user_inputs),
        "followup_increment_tokens": dist(increments),
        "context_burst": {
            "threshold_tokens": args.burst_threshold,
            "followup_turns": len(increments),
            "burst_turns": bursts,
            "probability": round(bursts / len(increments), 4) if increments else 0,
        },
    }
    output = json.dumps(shape, ensure_ascii=False, indent=2) + "\n"
    if args.out:
        Path(args.out).write_text(output, encoding="utf-8")
    print(output, end="")


if __name__ == "__main__":
    main()
