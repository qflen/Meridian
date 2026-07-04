# Changelog

## Unreleased

### Replication & clustering
- **Rebalancing on membership change** (ADR-031): adding or removing a storage node now
  re-derives placement **and moves the data**. `cluster.PlacementDelta` diffs the owner
  sets before/after the change (at the union of the rings' virtual-node boundaries) into
  the arcs that moved; the coordinator migrates each to its new owners — reusing the
  anti-entropy range read and the handoff backfill apply — then GCs the data a node no
  longer owns via the new `TSDB.DropSeriesInRanges` (head flushed, then blocks left,
  deleted whole, or rewritten to keep only owned series). GC runs only after the new owners
  confirm at quorum and never drops the last copy; a join catches up out of routing before
  serving, a leave re-homes its ranges before removal, and a return from `dead` reclaims
  over-replication off the fallbacks — reads stay complete throughout. Driven by `POST
  /api/internal/cluster/{join,leave}`; `meridian_rebalance_*` exposes migrations/bytes/GC/
  joins/leaves. On by default (`cluster.rebalance`); off, routing re-derives without moving
  data.
- **Proactive anti-entropy** (ADR-030): a rate-limited, jittered background sweep on the
  ingestor converges co-replicas that read-repair and hinted handoff never reach — cold
  data, an unobserved partial write, a hint dropped past the cap, a series no longer
  written. Each storage node summarises its data as **Merkle range digests** over
  `(ring-range × time-window)` buckets; the sweep round-robins the ring's replica groups,
  and where two replicas' roots differ it reads only the divergent window and pushes each
  the points it lacks through the same out-of-order-tolerant backfill apply (ADR-029) —
  bidirectional gap-fill to the window union. Agreement transfers nothing; the ring hash is
  injected so storage stays ring-agnostic. `meridian_anti_entropy_*` exposes
  rounds/divergence/repairs/bytes. On by default (`cluster.anti_entropy`); disabled it is
  exactly ADR-029.
- **Hinted handoff** (ADR-029): a write that can't reach a natural replica (down or
  catching up) buffers a durable, bounded hint while quorum still succeeds on the survivors,
  and replays it on the replica's return so it **fully converges** — including an *interior*
  gap read-repair can't fill (read-repair only converges forward; storage rejects
  out-of-order, ADR-015). Replay applies through a new out-of-order-tolerant backfill path
  (`TSDB.Backfill` + a distinct WAL frame), and a returning replica catches up in the
  reserved `joining` state — excluded from live routing — before promotion to `active`, so
  it is made whole before it can strand a gap. The hint store is per-target, capped in
  samples (drop-oldest past the cap), and rebuilt from disk on restart; `meridian_handoff_*`
  exposes pending/replayed/dropped. On by default (`cluster.handoff`); disabled it is
  exactly ADR-022.

### Write path & flow control
- **Per-series fair-share / priority-class load shedding** (ADR-027): an opt-in admission
  shaper in front of the bounded ingest queue makes overload shedding selective instead of
  uniform. Priority bands (a label or `__name__` match → a capacity ceiling) shed low
  priority before high; per-series token buckets throttle a hot or high-cardinality series
  so it can't starve well-behaved ones. Both engage only under contention, per-series state
  is bounded (sharded, so a cardinality flood can't grow it), and order within a series is
  preserved. Wired into the monolith BatchWriter and the service WritePool; drops fold into
  `meridian_dropped_samples_total` and break down by class/reason/series-bucket via
  `meridian_admission_*`. Off by default (`ingestion.admission`), leaving ADR-023's uniform
  shedding as the fallback.

### Storage & durability
- **WAL group commit** (ADR-026): a single committer goroutine coalesces
  concurrently-submitted frames behind one fsync, so a write still returns only after its
  own frame is durable while concurrent writers stop serializing one fsync at a time —
  measured ~30–37× WAL write throughput at 64 concurrent writers, ~4× at 8. Default on with
  zero linger; the on-disk frame format is byte-for-byte unchanged and the synchronous path
  remains available (`storage.wal_group_commit`).

### Anomaly detection
- **Seasonal Holt-Winters model** (ADR-028): per-series scoring now sits behind a model
  interface, with an additive level+trend+seasonal **Holt-Winters** model selectable via
  `anomaly.mode: holt_winters` alongside the default EWMA. It derives the diurnal phase from
  the sample timestamp, warms up over one full season, and scores each value against the
  band for its own time of day — so it flags a value normal globally but abnormal for that
  phase (a midday level in the small hours, a scheduled dip that fails to happen), which
  EWMA cannot. The Huber clamp, scale floor, debounce/hysteresis and event emission are
  shared by both models; EWMA's math and defaults are unchanged. The active model appears on
  `/api/v1/stats`, `/api/v1/anomalies`, `meridian_anomaly_model_info`, and the dashboard.

## v0.2.0 — 2026-06-18

Meridian as it stands at this release: a distributed time-series database in Go with a
canvas-rendered React dashboard. See [DECISIONS.md](DECISIONS.md) for the full ADR set and
[PERFORMANCE.md](PERFORMANCE.md) for measured numbers.

### Storage & durability
- **Gorilla compression** (delta-of-delta timestamps + XOR floats): ~28× on regular
  integer-like gauges, ~2× on continuously varying floats (ADR-002).
- **CRC32-framed WAL** with 128 MB segment rotation; a corrupt frame resyncs to the next
  8-byte boundary instead of discarding the rest of the segment (ADR-003).
- **Crash-consistent flush**: atomic head-swap + WAL-rotate cut, temp-dir → fsync → rename
  block writes, and a per-block WAL low-water-mark for exactly-once replay (ADR-016).
- **Out-of-order handling**: out-of-order samples are rejected and counted; exact duplicates
  are deduped (ADR-015).

### Query engine
- **PromQL subset** with stepped range/matrix evaluation, unary `+`/`-`, bare selectors
  (`{job="x"}`), `=`/`!=`/`=~`/`!~`, and compound durations (`1h30m`).
- **Correct semantics**: `rate()` (per selector-range, counter-reset corrected and
  extrapolated), `histogram_quantile()` (cumulative-`le` interpolation), vector↔vector ops
  with label matching and IEEE-754 division, `topk`/`bottomk`, and both `by`/`without`
  grouping (ADR-014).

### Cluster (docker-compose tier)
- **Quorum replication** over a 64-bit consistent-hash ring (defaults N=3/W=2/R=2):
  write-to-all at W acks, scatter reads at R with merge + async read-repair, and
  health-driven membership (ADR-022).
- **Prometheus `/metrics`** on every service — monolith, gateway, ingestor, storage,
  querier, compactor (ADR-019).

### Flow control & detection
- **Write-path backpressure**: bounded block-then-shed ingest queues with HTTP 429 +
  `Retry-After` / TCP NACK and cumulative drop/shed/backpressure metrics (ADR-023).
- **Streaming anomaly detection**: per-series EWMA baseline + dispersion robust to a moving
  diurnal baseline, with alerts over the WebSocket hub (ADR-024).

### Operations
- **Downsampling cascade** raw → 1m → 1h with count-weighted chaining and query-time
  resolution selection in the single binary; per-resolution retention (ADR-011). Cluster
  query-time resolution selection is documented future work — storage nodes generate
  rollups, but the querier still reads raw.
- **Configurable storage timings**: storage-service TSDB timings (`STORAGE_BLOCK_DURATION` /
  `_FLUSH_INTERVAL` / `_RETENTION`) are set via the environment.

### Dashboard
- **"Precision Instrument" design language** (ADR-020, ADR-021): one accent, self-hosted
  fonts, tabular-mono numerics, a signature strip-chart with a cursor crosshair readout,
  dark/light themes, an accessibility floor, and reconnect handling. WAL size is reported in
  bytes (previously mislabeled as a segment count).
