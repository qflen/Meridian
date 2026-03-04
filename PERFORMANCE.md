# Performance Characteristics

All figures below were measured on this machine — **Apple M5 (10-core), 32 GB,
Go 1.25.6, macOS (darwin/arm64)** — with the in-repo benchmarks: `make bench` for the
Go micro-benchmarks (`internal/compress`) and `./bin/meridian bench` for the CLI
compression bench. They are reproducible with those commands. Where a figure varies
run-to-run (CPU thermal/boost state) that is called out. Paths without a repeatable
benchmark are described qualitatively rather than with invented numbers — the live,
authoritative signals are on `/metrics` and `/api/v1/stats`.

## Compression

Gorilla's ratio is governed mainly by **value entropy**, not interval regularity:
delta-of-delta collapses a regular timestamp stream to ~1 bit/sample, but the XOR value
stream collapses to ~1 bit only when consecutive values are identical or integer-like.
Continuously varying floats keep most of their mantissa, so they compress far less.

Measured ratios (1 M samples for the CLI bench, 100 K for the Go bench; both use a
fixed seed, so ratios are deterministic):

| Pattern | Source | Ratio |
|---------|--------|------:|
| Regular, integer-like gauges | `meridian bench --pattern regular` | **28.3×** |
| Irregular intervals + random floats | `meridian bench --pattern irregular` | 2.1× |
| Spiky (occasional large jumps) | `meridian bench --pattern spiky` | 2.2× |
| Continuous-float random walk (CPU%-like) | `BenchmarkCompressionRatio/regular_cpu` | 2.0× |
| Monotonic counter (float increments) | `BenchmarkCompressionRatio/counter` | 2.2× |
| Sinusoidal | `BenchmarkCompressionRatio/sinusoidal` | 2.2× |

The headline **28.3×** is the best case: a regular interval with integer-like gauge
values that change only intermittently (common for infrastructure metrics — request
counts, rounded percentages), which produces long runs of zero XORs and lands at
**~4.5 bits/sample**. Real-world float telemetry with continuous noise compresses closer
to **~2×** with this codec. The timestamp stream alone (regular interval) always
compresses to ~1 bit/sample; the spread above is entirely the value stream.

## Encode / Decode Throughput

Single core. Throughput tracks the *output* size — emitting few bits (best-case data) is
fast, emitting full mantissas (high-entropy data) is slower.

| Workload | Encode | Decode | Source |
|----------|-------:|-------:|--------|
| Regular, integer-like (best case) | ~88 M pts/s (~11 ns/pt) | ~132 M pts/s (~8 ns/pt) | `meridian bench --pattern regular` |
| Continuous-float random walk (hard) | ~7 M pts/s | ~25 M pts/s | `BenchmarkEncode` / `BenchmarkDecode` |

The best-case throughput is a **warm** figure and **varies ~±30% with thermal/boost
state**: across repeated runs on this machine encode ranged 45–90 M pts/s and decode
65–135 M pts/s. Decode allocates nothing (`0 B/op`); encode allocates only the growing
bit-buffer.

## Downsampling — Query-Time Point Reduction

The rollup cascade (ADR-011) lets a wide query read coarse points instead of raw.
Measured by `TestEngineResolutionWideVsNarrow` (`internal/query`) on a 2-series, 8-hour
backfill at 15 s raw resolution:

| Query | Tier read | Points read |
|-------|-----------|------------:|
| 8 h span, 1 h step | 1 h rollup | 16 |
| same span if forced to raw | raw | 3840 (samples in span) |
| 5 m span, 15 s step | raw | ~82 |

The wide query reads **240× fewer points**. The per-window reduction is definitional — a
1 m window aggregates `60 s / raw_interval` samples and a 1 h window 60× that — i.e.
~12× (1 m) / ~720× (1 h) over a 5 s raw cadence, and ~4× / ~240× over the test's 15 s.
The live `meridian_downsampling_point_reduction` gauge reports this per resolution.
Query-time selection runs in the single-binary `serve`; the cluster querier currently
reads raw (ADR-011).

## Write Path, Query, Memory (characteristics, not micro-benchmarked)

These paths have no repeatable benchmark in this repo, so rather than quote invented
numbers, here are the cost characteristics and where to read the real signal live:

- **Ingestion**: TCP JSON decode → bounded block-then-shed queue (ADR-023) → one WAL
  fsync per frame → in-memory head append (with an inverted-index update). The live
  rate is on `/api/v1/stats` (`ingestion_rate`, a windowed samples/sec — ADR-017) and
  the cumulative `meridian_samples_ingested_total` counter; `ingest_queue_depth`/
  `_capacity` and `meridian_dropped_samples_total` expose backpressure. The default
  simulator (8 hosts × ~43 series at a 5 s cadence) is a light, steady load.
- **Query**: cost scales with the number of series matched (inverted-index
  intersection) × steps — each leaf selector is fetched once over the whole range and
  sliced per step (ADR-014) — plus the block scan/merge. Per-query latency is recorded
  in the `meridian_query_latency_seconds` histogram and shown on the dashboard's latency
  panel.
- **Memory**: dominated by the in-memory head (per-series label set + sample buffer) and
  the inverted index. It is bounded over time by flush-to-block plus retention, and
  bounded under overload by the ingest queue capacity (depth ≤ capacity — ADR-023).
  Persisted blocks store ~4.5 bits/sample for regular gauges (see above).
