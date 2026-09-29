---
name: llm-perf-report
description: Use when generating or reviewing llm-perf performance reports from scenario JSON, including user/rps/concurrency comparisons, requested-shape summaries, metric tables, SVG charts, and saturation conclusions. Do not use for data collection or service tuning.
---

# llm-perf Report Generation

## Inputs

Read only final scenario JSON files. Checkpoints are not authoritative.

```bash
find <data-dir> -maxdepth 1 -name '*.json' ! -name '*checkpoint*'
```

Identify the scene from `scenario`: `user`, `rps`, or `concurrency`. Read `schema_version`
first. Do not invent fields for an older schema; state the schema and continue with available
fields.

Primary metric locations:

- user rows: `user_levels[].metrics`, `user_levels[].server_metrics`, workload shape in
  `user_levels[].workload`.
- rps/concurrency rows: `concurrent[].metrics`, `concurrent[].server_metrics`. Use
  `request_rate` for rps and `level` for concurrency as the ladder axis.
- raw samples: `user_levels[].sessions[].turns[]` or `concurrent[].requests[]`.

Metric semantics are in `docs/metrics-semantics.md`; field definitions are in
`docs/data-contract.md`. Read them when a field is unclear.

## Required report structure

Generate one Markdown report with inline SVG. Use this order and do not omit sections.

1. **元信息**
   - endpoint, model ID, schema version, thinking mode, max_tokens, generated_at.
   - service notes such as max_num_seqs if known from the test record.
   - exclude endpoints, API keys, hostnames, or customer-identifying paths.

2. **请求形状**
   - One row per ladder point or profile combination. For user, include workload weights and
     attachment settings from `workload`.
   - Required fields: `request_shape.count`, prompt total/mean/P50/P95/min/max,
     completion total/mean/P50/P95/min/max, cached total.
   - State prompt semantics explicitly: user prompt is cumulative multi-turn context;
     rps/concurrency prompt is the full frozen request.
   - Never mix profiles, attachment probabilities, or token shapes in one average without
     labeling them.

3. **性能摘要**
   - One row per ladder point or workload combination.
   - Required: `bucket_tps_mean`, `bucket_tps_p95`, `bucket_tps_peak`, TPS P5/P50/P95/P99,
     TTFT P50/P95, TPOT P50/P95, E2E P50/P95, thinking P50/P95,
     completed/failed/cancelled/invalid, running/waiting, preemptions, cache hit rate.
   - Show "每秒总吞吐" as the display label for `bucket_tps`; keep the raw JSON field names
     in code paths and appendices.
   - `thinking` must appear even when all values are 0; label it "思考关闭（0ms）".
   - Show sample counts beside every percentile.

4. **图表**
   - Inline SVG, no external dependencies.
   - Minimum set: bucket TPS time axis, throughput by ladder axis, TTFT P95 by ladder axis,
     TPS P5 by ladder axis, request-shape P50/P95 prompt and completion by ladder axis.
   - For comparisons, overlay one line per tested variant and label it. Keep colors stable
     across comparable reports.
   - Do not put more than eight overlaid lines in one chart; split by workload if needed.

5. **结论**
   - State saturation status and stop rule. Common rules are `TTFT P95 > 10s` or
     `TPS P5 < 30`; report the actual rule used in the run.
   - Explain the shape effect: prompt P95, completion P95, heavy share, and attachment
     probability usually explain throughput and latency changes better than ladder value alone.
   - Verify token conservation: `sum(bucket_tps[].tps) ≈ sum(completion_tokens)` for valid
     requests. Report numeric delta.
   - For rps/concurrency, report `source_check.deviation`.
   - List excluded samples and why. Never hide failures.

## Comparison discipline

Compare within a single mode only: user vs user, rps vs rps, concurrency vs concurrency.
Never compare user to rps or concurrency directly.

For a valid comparison, require identical: model, endpoint, thinking, max_tokens, sampling,
seed salt policy, request-set shape, attachment probability, profile weights, and schema
version. If one differs, place the rows under a separate "control changed" heading and mark
the comparison directional, not exact.

For user runs, group by `(weights, attachment_probability, users)`. For rps/concurrency
shape tests, group by `(request-set label, shape metric, ladder axis)`. If the source JSON
does not identify request-set variants, ask for the missing metadata before publishing a
comparison.

## Saturation interpretation

Total TPS plateaus are not enough. A plateau with rising TTFT P95 or falling TPS P5 is a
saturation signal; a total TPS increase obtained by queuing may still violate the latency or
low-tail rule. Separate:

- service capacity limit: total TPS no longer increases;
- experience limit: TTFT P95 or TPS P5 crosses the stop rule;
- scheduling limit: running reaches `max_num_seqs` and waiting rises.

Do not report a peak-only number as sustainable capacity. Show mean, P95, and peak together.

## Chart output

Use deterministic SVG with:

```html
<svg viewBox="0 0 860 260" class="chart" role="img">...</svg>
```

Bars are suitable for per-ladder summaries; line charts are suitable for `bucket_tps` time
axes. Mark the ladder point next to each bar or line. Include grid lines and axis labels.
The report must remain readable as plain text if SVG is stripped.

## Validation before delivering

Run:

```bash
python3 -m py_compile <report-script>
python3 <report-script> <data-dir>
```

Check generated Markdown contains:

- all five sections;
- one table row per final JSON ladder point;
- no `None`, `NaN`, `undefined`, or empty summary cells;
- no checkpoint paths as authoritative sources;
- no customer endpoints or secrets.

If a required field is missing, write `NA（原因）` instead of omitting the row. Do not
estimate values that can be derived from JSON unless you label them as derived and show the
formula.
