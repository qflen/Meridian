# Meridian Architecture

## High-Level Overview

Meridian is a distributed time-series database in Go, drawing on Prometheus TSDB
internals and Facebook's Gorilla paper. It runs as a **single binary** (ingestion,
compression, storage, query, clustering, and visualization in one process), or
splits the same code across a **microservices cluster**.

```
┌─────────────────────────────────────────────────────────┐
│                    HTTP / WebSocket                      │
│   Dashboard (React)  │  REST API  │  WebSocket Hub      │
├──────────────────────┼────────────┼─────────────────────┤
│                   Query Engine                           │
│   Lexer → Parser → Planner → Executor                   │
├─────────────────────────────────────────────────────────┤
│                      TSDB                                │
│   ┌──────────┐  ┌──────────┐  ┌────────────────────┐   │
│   │ Head     │  │ WAL      │  │ Persistent Blocks  │   │
│   │ (in-mem) │  │ (CRC32)  │  │ (Gorilla-encoded)  │   │
│   └──────────┘  └──────────┘  └────────────────────┘   │
├─────────────────────────────────────────────────────────┤
│                  Cluster Layer                           │
│  Hash Ring  │  Quorum Replication  │  Read-Repair        │
├─────────────────────────────────────────────────────────┤
│               Retention & Downsampling                   │
│   TTL Enforcer  │  raw → 1m → 1h Rollups                │
└─────────────────────────────────────────────────────────┘
```

## Component Details

### Storage Engine

**Head block** (`internal/storage/head.go`): in-memory store for the most recent data.
- Inverted index maps label name/value pairs to sorted series-ID slices.
- Monotonic per-series order (ADR-015): a sample older than the series' last is
  rejected (counted in `meridian_out_of_order_samples_total`), an exact duplicate is
  deduped, a same-timestamp conflicting value is rejected. Series stay sorted; time
  bounds never invert.
- `ts=0` is a valid datum, not an "unset" sentinel.
- Periodically flushed to a persistent block.

**Write-ahead log** (`internal/storage/wal.go`): CRC32-framed, 8-byte-aligned entries,
auto-rotated at 128 MB. Every write is fsynced before acknowledgment.
- **Group commit** (default on, ADR-026): one committer goroutine coalesces concurrent
  frames behind a single fsync. A write still returns only after the fsync covering its
  frame, so durability and the on-disk format are unchanged, but concurrent writers no
  longer serialize.
- Corruption-resilient recovery: a bad length field or CRC re-anchors at the next
  8-byte frame boundary and keeps scanning, so one corrupt frame no longer discards the
  rest of a segment.
- Replay can start past a given segment, so data already in a durable block is not
  replayed twice.

**Persistent blocks** (`internal/storage/block.go`): Gorilla-compressed, ULID-named
directories.
- Each block holds a binary index (series ID to byte offset in the compressed chunks)
  plus a recorded WAL low-water-mark.
- Written crash-safely: temp dir → fsync files+dirs → atomic rename → fsync parent. The
  rename is the single durable commit point.

**Crash-consistent flush** (`internal/storage/tsdb.go`, ADR-016):
1. Under a write lock, capture the old head, install a fresh head, and rotate the WAL so
   in-flight writes land in a new segment. The rotation point is the block's
   low-water-mark.
2. Write the old head to a durable block outside the lock.
3. Only then delete the now-covered WAL segments (best-effort).

On open, replay skips segments at or below the maximum low-water-mark across all blocks,
so a crash that leaves both a block and its source segments recovers exactly once: no
loss, no double-count.

### Compression

**Gorilla encoding** (`internal/compress/gorilla.go`): Facebook's Gorilla codec.
- Delta-of-delta encoding for timestamps, XOR encoding for float64 values.
- 4-byte count header for decoder bootstrapping.
- ~20-30x compression on regular metric data.

### Query Engine

**Lexer** (`internal/query/lexer.go`): tokenizes the PromQL subset (durations like
`5m`/`1h`, label matchers, operators, aggregations).

**Parser** (`internal/query/parser.go`): recursive descent producing an AST: vector/range
selectors, function calls, aggregations (sum, avg, min, max, count, topk, bottomk), binary
expressions, sub-expressions.

**Planner** (`internal/query/planner.go`): extracts label matchers for predicate pushdown,
adjusts time ranges for range selectors, selects a rollup resolution from the span/step, and
annotates each selector with the rollup aggregate its wrapping function needs (ADR-025).

**Executor** (`internal/query/executor.go`): evaluates the AST against the TSDB (`rate()`,
the `*_over_time` range aggregations, `histogram_quantile()`, all aggregations). At a coarse
resolution it reads the column matching the operation and serves `rate()` from the stored
counter-increase column.

### Ingestion

**TCP server** (`internal/ingestion/server.go`): JSON-over-TCP. Accepts `WriteRequest`
batches under a per-message size cap, read deadline, and concurrent-connection bound. A
write the bounded queue sheds is NACKed (`Shed`/`Throttled`).

**Batch writer** (`internal/ingestion/batch.go`, ADR-023): coalesces samples and drains
them to the TSDB through a **bounded queue** with block-then-shed flow control.
- Producers enqueue full batches; a single drain goroutine writes them FIFO.
- A full queue blocks the producer up to a deadline (backpressure), then sheds: it drops
  and counts the batch instead of growing without bound.
- The single drain preserves FIFO, so an in-order producer is not reordered into
  out-of-order rejections.

**Backpressure primitive** (`internal/backpressure/queue.go`): the shared cost-bounded,
block-then-shed FIFO behind every ingest path. Cost is a sample count, so depth ≤ capacity
is a memory bound; `Enqueue` blocks up to a deadline when full, then sheds.

**Admission shaper** (`internal/backpressure/admission.go`, ADR-027): optional, off by
default; consulted before the queue to make shedding selective instead of uniform.
- **Priority class**: a label or `__name__` match sets a capacity ceiling, so low priority
  sheds before high.
- **Per-series fair share**: a token bucket throttles a hot or high-cardinality series
  rather than starving well-behaved ones.
- Both gates engage only under contention. Per-series buckets live in a fixed-size shard
  array, so a cardinality flood cannot grow the tracking state.
- Holds no samples (the queue capacity is still the hard memory bound). Drops fold into
  `meridian_dropped_samples_total`, attributed by class, reason, and series-hash bucket.
- Applied per-series by both the monolith `BatchWriter` and the service `WritePool`; order
  within a series is preserved.

**Service write pool** (`internal/service/pool.go`): the ingestor and storage node bound
in-flight writes with a fixed worker pool draining a bounded queue. `Submit` blocks while
the queue is full and sheds past the deadline (`ErrShed`, mapped to HTTP 429 + `Retry-After`
or TCP NACK), so a stalled quorum write or slow WAL fsync caps concurrency rather than
piling up goroutines. Quorum semantics are unchanged; only the submission rate is bounded.

### HTTP & WebSocket

**HTTP server** (`internal/server/http.go`, ADR-018): REST API for queries, label browsing,
and health checks, plus the dashboard SPA. Hardened at the boundary:
- A traversal guard rejects any `..` path with 400 before `http.ServeMux` can clean and
  redirect it; static reads are confined to the dashboard directory.
- `/api/v1/query` runs under a configurable deadline (`server.query_timeout`) with
  `start ≤ end` validation, strict `start`/`end`/`step` parsing, and panic recovery.
- CORS echoes only configured origins (default localhost, never `*`).
- `/api/v1/cluster` probes peers concurrently under a request-scoped deadline, reporting
  each reachable peer's real series/samples (or, single-node, just the one node).

**WebSocket hub** (`internal/server/websocket.go`): broadcasts live metrics and stats to
dashboard clients. Each tick's payload is marshaled once and the same bytes sent to every
client. A client whose send buffer stays full across `maxClientDrops` consecutive
broadcasts is force-disconnected, so a stalled reader cannot leak its goroutines.

**Metrics** (`internal/server/metrics.go`): shared Prometheus exposition helpers; the
monolith and all five microservices serve `/metrics`.
- Storage nodes expose the full storage set; the cumulative `meridian_samples_ingested_total`
  counter stays distinct from the windowed rate on `/api/v1/stats` (ADR-017, ADR-019).
- Ingest-bounding nodes export the flow-control families (`WriteQueueMetrics`, ADR-023):
  dropped/shed/backpressure counters plus queue depth/capacity/high-watermark gauges.
- With admission on, `WriteAdmissionMetrics` adds admitted/dropped/series-bucket breakdowns
  by class and reason (ADR-027); omitted when off.
- The monolith and gateway (where the detector runs) export `meridian_anomalies_total` and
  `meridian_active_anomalies` (`WriteAnomalyMetrics`, ADR-024).

### Streaming Anomaly Detection

**Detector** (`internal/anomaly/detector.go`, ADR-024): a pure, per-series online detector
fed from the per-series tick stream the broadcaster emits, so it runs uniformly in the
monolith (`Head().SeriesInfos()`) and the cluster gateway (aggregated `FetchSeries`).
Scoring sits behind a small **model interface** selected by `Config.Mode`; the detector owns
the shared machinery (dedup, debounce/hysteresis, scale floor, event emission, eviction) and
a model supplies only a `(baseline, score)`.

- **EWMA** (default): O(1) state per series, an EWMA **level** (baseline) and EWMA
  **variance** (dispersion). The score `|value - level| / dispersion` is a *local* z-score,
  so slow diurnal swing and drift are tracked (no alert) while a spike departs sharply
  (alert), where a global z-score would flag the diurnal peak. A Welford warmup seeds the
  baseline.
- **Holt-Winters** (`holtwinters.go`, opt-in `mode: holt_winters`, ADR-028): additive
  level+trend+seasonal. It derives the seasonal phase from the sample timestamp
  (`ts mod season_period`), warms up over one full season, and scores each value against the
  band for its own time of day, so it flags a value normal globally but abnormal for its
  phase, which EWMA cannot. State is O(season).

Shared machinery: a Huber clamp bounds a spike's pull on the baseline/dispersion; a relative
scale floor stops a flat series collapsing the dispersion; debounce, a hysteresis clear band,
and timestamp dedup (on `SeriesInfo.LastTS`) avoid alert storms; stale series are evicted so
memory follows live cardinality. Raise/clear transitions broadcast as a distinct `anomaly`
WebSocket frame and into a bounded recent-events ring at `/api/v1/anomalies` (which reports
the active model); the dashboard's Anomalies strip lists them most-recent-first.

### Cluster (Microservices Tier)

**Hash ring** (`internal/cluster/ring.go`): a 64-bit SHA-256 consistent hash ring with
configurable virtual nodes and a deterministic nodeID tie-break, so a key's replica set is a
function of membership rather than join order.
- `GetNodes` returns the first N distinct **routable** replicas, skipping Dead/Leaving/
  Joining nodes.
- `PreferenceList` returns the natural N owners *including* the skipped ones, so hinted
  handoff can tell which owner a write missed.
- `SetState`/`State`/`LiveNodes` are the hooks the health monitor and replay loop drive.

**Replicated storage client** (`internal/service/client.go`, ADR-022): the live routing
source; seeds a ring from the configured storage addresses and applies the quorum model.
- **Write**: `replicas = ring.GetNodes(key, N)`, sent to all live replicas (batched per
  node), succeeding only at ≥W acks. Too few is a quorum error, not a partial write. With
  handoff on, a missed natural owner has the series buffered as a durable hint.
- **Read**: scatters to all live nodes, merges and dedupes by (series, timestamp), enforces
  read quorum R globally and per series, and asynchronously **read-repairs** any replica
  missing points relative to the merged truth.
- **Health**: `StartHealthMonitor` sets ring node state so routing excludes dead nodes and
  re-includes recovered ones. The same refresh discovers each node's rollup-tier
  availability (intersected by the resolution planner); a node returning with a hint backlog
  routes through `joining` before `active`.

**Hinted handoff** (`internal/service/handoff.go`, `hints.go`, ADR-029): the ingestor
buffers writes a replica misses while down and replays them on its return, closing the
interior-gap case read-repair cannot reach.
- **Buffer**: `Write` diffs `PreferenceList` against the live replicas it reached and, once
  every series met quorum, records a durable hint per missed owner in a bounded, per-target
  `HintStore` (one crash-safe file per hint, FIFO, capped per target with drop-oldest,
  rebuilt from disk on restart).
- **Replay**: a background loop drains each reachable target's hints in order through the
  out-of-order-tolerant `/api/internal/backfill` (`TSDB.Backfill`), deleting each on ack.
  Backfill inserts historical samples in sorted position (gap-fill only) under a distinct WAL
  frame, so they survive a crash while the live in-order policy (ADR-015) is untouched.
- **Catch-up**: a replica returning from Dead with a backlog enters `joining` (out of
  routing) and is promoted to `active` only once its hints drain, so it is made whole before
  it can strand an interior gap. An already-Active node is never demoted by a transient hint.

**Proactive anti-entropy** (`internal/service/antientropy.go`, `internal/storage/merkle.go`,
`internal/cluster/antientropy.go`, ADR-030): a rate-limited, jittered background sweep on the
ingestor that converges co-replicas neither read-repair nor hinted handoff reaches (cold
data, an unobserved partial write, a hint dropped past the cap, a series no longer written).
- **Digest**: each storage node summarises the series whose ring position falls in a set of
  hash arcs, bucketed by time window, as a Merkle tree (`/api/internal/antientropy/digest`).
  Equal roots mean agreement (nothing transfers); a differing root localises divergence to
  specific windows. The ring hash is injected (`cluster.HashKey`), so storage stays
  ring-agnostic.
- **Sweep**: `ring.ReplicaGroups` partitions the ring into the arc sets sharing a replica
  set; the loop round-robins them (`groups_per_round` per round). For a divergent window it
  reads that window from every replica (`/api/internal/antientropy/range`) and pushes each
  the points it lacks through `/api/internal/backfill`, a bidirectional gap-fill to the
  window union.
- **Scoped to shared arcs**: comparison runs per replica group, so a node's data outside an
  arc it shares with the peer is never mistaken for divergence and re-shipped.

**Rebalancing on membership change** (`internal/cluster/rebalance.go`,
`internal/service/rebalance.go`, `internal/storage/rebalance.go`, ADR-031): adding or
removing a storage node re-derives placement *and moves the data*. Read-repair, hinted
handoff, and anti-entropy all converge nodes that *share* an owner set; rebalancing supplies
the piece they assume but never provide, moving the owner set itself.
- **Diff**: `cluster.PlacementDelta` diffs two ring snapshots at the union of their
  virtual-node boundaries and groups the arcs whose owner set changed into
  `OwnershipChange{Before, After, Added, Removed, Arcs}`. A joining node is a target owner; a
  leaving/dead node is excluded as a target but still a source.
- **Migrate**: the coordinator reads the moved arcs from a current owner
  (`/api/internal/antientropy/range`) and pushes them to each new owner through
  `/api/internal/backfill`; a new owner is confirmed only on its ack.
- **GC**: once data has moved and the ring is flipped to the target placement (so read-repair
  cannot re-add it), the old owner drops the arcs it no longer owns (`TSDB.DropSeriesInRanges`
  via `/api/internal/rebalance/drop`): head flushed, then each block left, deleted whole, or
  rewritten to keep only its owned series. GC runs only after new owners are confirmed at
  quorum and never drops the last copy.
- **Lifecycle**: a join catches up out of routing (`joining`), promotes to `active`, then GCs
  the displaced owner; a leave migrates while still `active` (reads stay complete), then goes
  `leaving`, then removed. A node returning from `dead` has the over-replication its absence
  created reclaimed off the fallbacks. Driven by `POST /api/internal/cluster/{join,leave}`.

Wiring: the ingestor and querier build this client from `REPLICATION_FACTOR`/`WRITE_QUORUM`/
`READ_QUORUM` and run the health monitor; only the ingestor wires the hint store and the
rebalance coordinator (the write owner). The `Coordinator` (`internal/cluster/coordinator.go`)
wraps the same ring with `RouteWrite`/`RouteRead` helpers. The monolith `serve` path is
single-node and does not use the ring.

### Retention & Downsampling

**Downsampler** (`internal/retention/downsampler.go`, ADR-011): runs the live raw → 1m → 1h
rollup cascade.
- Each pass advances a tier as far as its source is durably closed, rolling sealed raw blocks
  into resolution-tagged **rollup blocks** (`storage.RollupBlock`, format v2). Each stores six
  Gorilla-compressed aggregate columns (min/max/sum/count/avg plus a reset-aware counter
  increase) per series under `<dataDir>/rollups/<resolution>/`.
- The 1h tier is *chained* from 1m (count-weighted average, additive increase), so it equals a
  1h rollup built directly from raw. A per-resolution `covered_through` watermark makes passes
  idempotent and crash-recoverable (rollups are regenerable).
- **Query-time selection**: the planner picks a resolution from the span/step; the executor
  reads the column matching the operation (`TSDB.QueryResolution` with a `RollupAggregate`):
  avg for a bare value, max/min/sum/count for the matching `*_over_time`, the increase column
  for `rate()` (ADR-025). It rolls up the freshest not-yet-closed tail on the fly so the coarse
  series stays current.
- Runs in both deployments: the single-binary `serve` (TSDB implements
  `ResolutionDataSource`) and the cluster, where `service.StorageClient` implements the same
  capability, so engine and planner are reused unchanged. The cluster querier picks a
  resolution against the intersection of live nodes' advertised tiers
  (`/api/internal/resolutions`), requests it (`QueryAtMostResolution` serves the coarsest
  tier at or below), and merges by (series, window-timestamp).
- The coarse merge skips read-repair (rollups are node-local derivations, so raw replication +
  read-repair is the convergence layer, ADR-022). A series whose replicas served different
  resolutions falls back to a raw read, so heterogeneity never breaks a query. Older v1 blocks
  (no increase column) serve their five columns, with `rate()` falling back to raw.

**Enforcer** (`internal/retention/enforcer.go`): per-resolution TTL cleanup. Raw blocks expire
first (and only once the finest rollup tier has captured them); each rollup tier is kept
longer, so a long-range query is still answerable from the 1h tier after the raw behind it is
gone.

## Data Flow

1. **Ingest**: TCP → optional admission shaper (ADR-027) → BatchWriter's bounded
   block-then-shed queue (ADR-023) → drain → WAL (group-commit fsync, ADR-026) → head
   (in-order policy, ADR-015). A full queue sheds and NACKs the producer rather than growing
   memory; with admission on, the shed victims are the lowest-priority or most-over-budget
   series.
2. **Flush**: head swap + WAL rotate → Gorilla block via temp-dir + fsync + atomic rename →
   covered WAL segments reclaimed.
3. **Query**: Parser → Planner (also selects a rollup resolution from the span/step) →
   merge(head, blocks) or read the chosen rollup tier → Executor → result.
4. **Stream**: the WebSocket hub broadcasts metrics + stats at 60fps; the same per-tick
   per-series stream feeds the anomaly detector, whose raise/clear transitions broadcast as
   `anomaly` frames (ADR-024).
5. **Downsample & retain**: the downsampler rolls sealed raw blocks into the 1m/1h tiers (1h
   chained count-weighted from 1m); the enforcer expires each tier on its own TTL, raw first
   (only once it has been rolled up).
6. **Recover**: on open, load blocks, then replay only the WAL beyond the blocks' maximum
   low-water-mark → exactly-once reconstruction of the head.
