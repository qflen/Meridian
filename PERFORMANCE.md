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

## WAL Group Commit — fsync Coalescing Under Concurrency

The WAL coalesces concurrently-submitted frames behind a single fsync (ADR-026).
Measured by `BenchmarkWALConcurrentWrite` (`internal/storage`), which drives a fixed
number of concurrent writers through `LogSamples` against a real on-disk segment and
reports `frames/fsync` alongside `ns/op`:

| Concurrent writers | Mode | frames/fsync | Throughput vs synchronous |
|--------------------|------|-------------:|--------------------------:|
| 8  | synchronous (off)        | 1.0   | 1× (baseline) |
| 8  | group commit             | ~4.0  | ~4× |
| 8  | group commit, 200 µs linger | ~8.0  | ~7× |
| 64 | synchronous (off)        | 1.0   | 1× (baseline) |
| 64 | group commit             | ~32   | ~30–37× |
| 64 | group commit, 200 µs linger | ~64   | ~60–65× |

The synchronous path is **flat in concurrency** — every frame pays its own fsync under
the global lock, so 8 and 64 writers reach the same ceiling (~1 fsync per frame, bounded
by `1 / fsync_latency`). Group commit instead coalesces every frame submitted while the
prior fsync is in flight, so the batch size — and the speedup — **scale with the number
of concurrent writers**: ~N frames share each fsync at N writers. A small `linger`
(default 0) trades a little latency to push the batch size to the full writer count. The
absolute fsync latency on this machine's APFS is ~3–4 ms (the synchronous ceiling); the
multiples above are the structural result and are stable run-to-run even as that latency
drifts with the filesystem and thermal state. Durability is unchanged — a write still
returns only after the fsync covering its frame.

## Write Path, Query, Memory (characteristics, not micro-benchmarked)

These paths have no repeatable benchmark in this repo, so rather than quote invented
numbers, here are the cost characteristics and where to read the real signal live:

- **Ingestion**: TCP JSON decode → (optional per-series/priority admission, ADR-027) →
  bounded block-then-shed queue (ADR-023) → WAL
  append (group-commit fsync, ADR-026 — see below) → in-memory head append (with an
  inverted-index update). The live
  rate is on `/api/v1/stats` (`ingestion_rate`, a windowed samples/sec — ADR-017) and
  the cumulative `meridian_samples_ingested_total` counter; `ingest_queue_depth`/
  `_capacity` and `meridian_dropped_samples_total` expose backpressure. The default
  simulator (8 hosts × ~43 series at a 5 s cadence) is a light, steady load.
- **Admission shaping** (when enabled, ADR-027): one classify (a short scan over the
  configured classes) plus an allocation-free order-independent hash of the series
  identity per offered series, and — only above the contention threshold — one
  sharded token-bucket consult. State is O(shards + metric-buckets), fixed at
  construction, so cost and memory are independent of series cardinality. Off by
  default, it adds nothing to the hot path; the selectivity it buys is visible in the
  `meridian_admission_*` counters.
- **Query**: cost scales with the number of series matched (inverted-index
  intersection) × steps — each leaf selector is fetched once over the whole range and
  sliced per step (ADR-014) — plus the block scan/merge. Per-query latency is recorded
  in the `meridian_query_latency_seconds` histogram and shown on the dashboard's latency
  panel.
- **Memory**: dominated by the in-memory head (per-series label set + sample buffer) and
  the inverted index. It is bounded over time by flush-to-block plus retention, and
  bounded under overload by the ingest queue capacity (depth ≤ capacity — ADR-023).
  Persisted blocks store ~4.5 bits/sample for regular gauges (see above).
- **Hinted handoff** (ADR-029): off the live write path. A normal write adds, per missed
  replica, one ring `PreferenceList` walk plus one durable hint file (fsync + rename) on
  the ingestor — and only while a replica is actually down. Replay and backfill are a
  background recovery path (one target at a time, FIFO), not the hot path. The buffer is
  bounded per target by `max_samples_per_node` (drop-oldest past it), so a long outage
  caps hint disk/memory rather than growing without bound; `meridian_handoff_pending_*`
  shows the live backlog and `_replayed_/_dropped_samples_total` the catch-up progress.
- **Anti-entropy** (ADR-030): a background sweep, never on the read or write path, and
  bounded on both axes. *Spatially*, `groups_per_round` caps how many replica groups a
  round touches (the round-robin cursor covers the rest over later rounds) and the number
  of groups tracks the cluster's distinct replica sets, not the virtual-node count.
  *Temporally*, a round's per-group cost is one match-all read over `[now-lookback, now]`
  per replica to compute the digest; the agreement case ends at a single root comparison
  and transfers nothing, and only a divergent `window` is re-read and gap-filled. Smaller
  `window` re-transfers less per divergence but enlarges the digest; `lookback` bounds the
  per-round read on large datasets (`0` re-digests all history). `interval`+`jitter` set
  the cadence and de-sync coordinators. Progress shows in
  `meridian_anti_entropy_repairs_total` / `_transferred_samples_total`; `_divergent_windows_total`
  climbing while `_repairs_total` does not flags an unresolvable difference (a same-timestamp
  value conflict, which gap-fill does not overwrite).
- **Rebalancing on membership change** (ADR-031): off the live read and write path — it runs
  only when a node is explicitly joined or left, and the work is proportional to the data that
  actually moved, not the dataset. The owner-set diff is `O(vnodes × nodes)` over two ring
  snapshots (no I/O); migration cost is one range read from a current owner plus one backfill
  push per new owner, per moved arc-group, processed **sequentially** (no thundering herd) with
  an optional `max_bytes_per_round` to spread a large move across passes. GC is a per-node drop
  of the shed arcs: the head is flushed once, then only the blocks holding un-owned series are
  rewritten (decode + re-encode of the kept series) — fully-owned blocks are untouched and
  fully-un-owned blocks are deleted, so a node that loses a fraction of the keyspace pays for
  rewriting only the blocks that mix owned and un-owned series. A joining node stays out of
  routing until its data has arrived, so the migration adds no read-path latency; reads stay
  complete throughout because the old owners keep their copy until the new owners are confirmed
  at quorum. `meridian_rebalance_*` shows migrations/bytes moved and GC series/samples
  reclaimed; `_skipped_total` climbing without `_migrations_total` flags a move that cannot
  reach a source or a quorum.
- **Anomaly detection** (ADR-024, ADR-028): on the broadcast tick, not the read or write
  path — one map lookup and a handful of float ops per live series per tick (~1 Hz), under
  a single lock. **EWMA** (default) keeps O(1) state per series (a few `float64`s).
  **Holt-Winters** (`mode: holt_winters`) keeps O(`season_length`) per series (the seasonal
  array, plus a one-season warmup accumulator that is released once seeded); its per-tick
  work is still a constant handful of ops (one bucket index from the timestamp, a forecast,
  three smoothing updates). Memory follows live cardinality because unseen series are
  evicted. Activity shows in `meridian_anomalies_total` / `meridian_active_anomalies`, and
  `meridian_anomaly_model_info` names the active model.
