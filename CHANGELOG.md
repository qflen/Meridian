# Changelog

## Unreleased

### Storage & durability
- WAL group commit: a single committer goroutine coalesces concurrently-submitted
  frames behind one fsync, so a write still returns only after its own frame is
  durable while concurrent writers stop serializing one fsync at a time — measured
  ~30–37× WAL write throughput at 64 concurrent writers and ~4× at 8 (ADR-026).
  Default on with zero linger; the on-disk frame format is byte-for-byte unchanged
  and the synchronous path remains available (`storage.wal_group_commit`).

## v0.2.0 — 2026-06-18

Meridian as it stands at this release: a distributed time-series database in Go
with a canvas-rendered React dashboard. See [DECISIONS.md](DECISIONS.md) for the
full ADR set and [PERFORMANCE.md](PERFORMANCE.md) for measured numbers.

### Storage & durability
- Gorilla compression (delta-of-delta timestamps + XOR floats): ~28× on regular
  integer-like gauges, ~2× on continuously varying floats (ADR-002).
- CRC32-framed WAL with 128 MB segment rotation; a corrupt frame resyncs to the
  next 8-byte boundary instead of discarding the rest of the segment (ADR-003).
- Crash-consistent flush: atomic head-swap + WAL-rotate cut, temp-dir → fsync →
  rename block writes, and a per-block WAL low-water-mark for exactly-once replay
  (ADR-016).
- Out-of-order samples are rejected and counted; exact duplicates are deduped
  (ADR-015).

### Query engine
- PromQL subset with stepped range/matrix evaluation, unary `+`/`-`, bare
  selectors (`{job="x"}`), `=`/`!=`/`=~`/`!~`, and compound durations (`1h30m`).
- Correct `rate()` (per selector-range, counter-reset corrected and
  extrapolated), `histogram_quantile()` (cumulative-`le` interpolation),
  vector↔vector ops with label matching and IEEE-754 division, `topk`/`bottomk`,
  and both `by`/`without` grouping (ADR-014).

### Cluster (docker-compose tier)
- Quorum replication over a 64-bit consistent-hash ring (defaults N=3/W=2/R=2):
  write-to-all at W acks, scatter reads at R with merge + async read-repair, and
  health-driven membership (ADR-022).
- Prometheus `/metrics` on every service — monolith, gateway, ingestor, storage,
  querier, compactor (ADR-019).

### Flow control & detection
- Write-path backpressure: bounded block-then-shed ingest queues with HTTP 429 +
  `Retry-After` / TCP NACK and cumulative drop/shed/backpressure metrics
  (ADR-023).
- Streaming anomaly detection: per-series EWMA baseline + dispersion robust to a
  moving diurnal baseline, with alerts over the WebSocket hub (ADR-024).

### Operations
- Downsampling cascade raw → 1m → 1h with count-weighted chaining and query-time
  resolution selection in the single binary; per-resolution retention (ADR-011).
  Cluster query-time resolution selection is documented future work — storage
  nodes generate rollups, but the querier still reads raw.
- Storage-service TSDB timings (`STORAGE_BLOCK_DURATION` / `_FLUSH_INTERVAL` /
  `_RETENTION`) are configurable via the environment.

### Dashboard
- "Precision Instrument" design language (ADR-020, ADR-021): one accent,
  self-hosted fonts, tabular-mono numerics, a signature strip-chart with a cursor
  crosshair readout, dark/light themes, an accessibility floor, and reconnect
  handling. WAL size is reported in bytes (previously mislabeled as a segment
  count).
