# Meridian Architecture

## High-Level Overview

Meridian is a distributed time-series database written in Go, inspired by
Prometheus TSDB internals and Facebook's Gorilla paper. It is designed as a
single-binary system that handles ingestion, compression, storage, querying,
clustering, and visualization.

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
│   TTL Enforcer  │  5s→1m→1h Rollups                     │
└─────────────────────────────────────────────────────────┘
```

## Component Details

### Storage Engine

**Head Block** (`internal/storage/head.go`): In-memory storage for the most
recent data. Maintains an inverted index mapping label name/value pairs to
sorted series ID slices. Ingestion enforces a monotonic-per-series order:
samples older than a series' last are rejected (counted in
`meridian_out_of_order_samples_total`), an exact duplicate of the last sample is
deduplicated, and a conflicting value at the same timestamp is rejected — so each
series stays sorted and time bounds never invert (see ADR-015). A real `ts=0`
sample is a valid datum, not an "unset" sentinel. The head is periodically
flushed to a persistent block.

**Write-Ahead Log** (`internal/storage/wal.go`): CRC32-framed WAL with 8-byte
aligned entries and automatic segment rotation at 128 MB. Every write is fsynced
before acknowledgment. Under **group commit** (default on, ADR-026) a single
committer goroutine coalesces concurrently-submitted frames behind one fsync — a
write still returns only after the fsync covering its frame, so durability and the
on-disk format are unchanged, but concurrent writers no longer serialize one fsync
at a time. Recovery is resilient to corruption: a bad length field or CRC mismatch
re-anchors at the next 8-byte frame boundary and keeps scanning, so one corrupt
frame no longer discards the rest of a segment. Replay can start past a given
segment so that data already captured in a durable block is not replayed again
(see below).

**Persistent Blocks** (`internal/storage/block.go`): Gorilla-compressed blocks
with ULID-named directories. Each block contains a binary index file mapping
series IDs to byte offsets in the compressed chunks file, plus a recorded WAL
low-water-mark. Blocks are written crash-safely: into a temp directory, fsynced
(files and dirs), atomically renamed into place, then the parent directory is
fsynced — the rename is the single durable commit point.

**Crash-consistent flush** (`internal/storage/tsdb.go`): A flush (1) under a
write lock captures the old head, installs a fresh head, and rotates the WAL so
in-flight writes land in a new segment — the rotation point is the block's
low-water-mark; (2) writes the old head to a durable block outside the lock; (3)
only then deletes the now-covered WAL segments (best-effort). On open, replay
skips WAL segments at or below the maximum low-water-mark across all blocks, so a
crash that leaves both a block and its source segments present recovers exactly
once — no loss, no double-count (see ADR-016).

### Compression

**Gorilla Encoding** (`internal/compress/gorilla.go`): Implements Facebook's
Gorilla compression for time-series data:
- Delta-of-delta encoding for timestamps
- XOR-based encoding for float64 values
- 4-byte count header for decoder bootstrapping
- Achieves 20-30x compression on regular metric data

### Query Engine

**Lexer** (`internal/query/lexer.go`): Tokenizes PromQL-subset expressions
including durations (5m, 1h), label matchers, operators, and aggregations.

**Parser** (`internal/query/parser.go`): Recursive descent parser producing an
AST. Supports vector selectors, range selectors, function calls, aggregations
(sum, avg, min, max, count, topk, bottomk), binary expressions, and
sub-expressions.

**Planner** (`internal/query/planner.go`): Extracts label matchers for predicate
pushdown, adjusts time ranges for range selectors, selects a rollup resolution from the
span/step, and annotates each selector with the rollup aggregate its wrapping function
needs for function-aware coarse reads (ADR-025).

**Executor** (`internal/query/executor.go`): Evaluates the AST against the TSDB.
Implements rate(), the `*_over_time` range-aggregation functions, histogram_quantile(),
and all aggregation functions. At a coarse resolution it reads the column that matches
the operation and serves rate() from the stored counter-increase column.

### Ingestion

**TCP Server** (`internal/ingestion/server.go`): JSON-over-TCP ingestion
protocol. Accepts WriteRequest messages containing batched time-series samples,
under a per-message size cap, a per-message read deadline, and a concurrent-
connection bound. A write that the bounded queue sheds is NACKed in the response
(`Shed`/`Throttled`).

**Batch Writer** (`internal/ingestion/batch.go`): Coalesces incoming samples into
batches and drains them to the TSDB through a **bounded queue** with block-then-shed
flow control (ADR-023). Producers enqueue full batches; a single drain goroutine
writes them in FIFO order; a full queue blocks the producer up to a deadline (the
backpressure) and then sheds — dropping and counting the batch — instead of growing
without bound. Queue bounds come from ingestion config; a single drain preserves
FIFO so an in-order producer is not reordered into out-of-order rejections.

**Backpressure primitive** (`internal/backpressure/queue.go`): the shared
cost-bounded, block-then-shed FIFO behind every ingest path. The cost is a sample
count, so queue depth is a memory bound (depth ≤ capacity); `Enqueue` blocks up to a
deadline when full, then sheds.

**Admission shaper** (`internal/backpressure/admission.go`): an optional, off-by-default
layer consulted *before* the queue that makes shedding selective instead of uniform
(ADR-027). It sheds by **priority class** (a label or `__name__` match → a capacity
ceiling, so low priority sheds before high) and **per-series token-bucket fair share**
(a hot/high-cardinality series is throttled rather than starving well-behaved ones).
Both gates engage only under contention; per-series token buckets live in a fixed-size
shard array, so a cardinality flood cannot grow the tracking state. It holds no samples
— the queue capacity is still the hard memory bound — and an admission drop is folded
into `meridian_dropped_samples_total` while also being attributed by class, reason, and
series-hash bucket. The monolith `BatchWriter` and the service `WritePool` both apply it
per-series (a multi-series batch/request is filtered, not classified as a whole), and
order within a series is preserved.

**Service write pool** (`internal/service/pool.go`): the ingestor and storage node
bound in-flight writes with a fixed worker pool draining a bounded queue. `Submit`
blocks while the queue is full and sheds past the deadline (`ErrShed` → HTTP 429 +
`Retry-After` / TCP NACK), so a stalled quorum write or a slow WAL fsync caps
concurrency rather than piling up unbounded goroutines — quorum semantics are
unchanged, only the submission rate is bounded.

### HTTP & WebSocket

**HTTP Server** (`internal/server/http.go`): REST API for queries, label
browsing, and health checks, plus the dashboard SPA. Hardened at the boundary: a
traversal guard rejects any `..` path with 400 before `http.ServeMux` can clean and
redirect it, and static reads are confined to the dashboard directory;
`/api/v1/query` runs under a configurable deadline (`server.query_timeout`) with
`start ≤ end` validation, strict `start`/`end`/`step` parsing, and a panic-recovery
guard; and CORS echoes only configured origins — default localhost, never `*`
(ADR-018). `/api/v1/cluster` probes peers concurrently under a request-scoped
deadline and reports each reachable peer's real series/samples, or — in single-node
mode — just the one real node.

**WebSocket Hub** (`internal/server/websocket.go`): Broadcasts live metrics and
system stats to connected dashboard clients. Each tick's payload is marshaled once
and the same bytes are sent to every client; a client whose send buffer stays full
across `maxClientDrops` consecutive broadcasts is force-disconnected so a stalled
reader cannot linger and leak its read/write goroutines.

**Metrics** (`internal/server/metrics.go`): shared Prometheus exposition helpers.
The monolith and all five microservices serve `/metrics`; storage nodes expose the
full storage metric set, and the cumulative `meridian_samples_ingested_total` counter
is kept distinct from the windowed ingestion rate reported on `/api/v1/stats`
(ADR-017, ADR-019). Every ingest-bounding node (monolith, ingestor, storage) also
exports the write-path flow-control families via `WriteQueueMetrics`:
`meridian_dropped_samples_total`, `meridian_ingest_shed_events_total`, and
`meridian_ingest_backpressure_events_total` (cumulative counters), plus
`meridian_ingest_queue_depth`/`_capacity`/`_high_watermark` (gauges) — ADR-023. When
per-series/priority admission is enabled, `WriteAdmissionMetrics` adds the
`meridian_admission_admitted_samples_total{class}`,
`meridian_admission_dropped_samples_total{class,reason}`, and
`meridian_admission_series_bucket_dropped_total{bucket}` families (ADR-027); they are
omitted entirely when it is off, so the default scrape is unchanged. The
monolith and the gateway, where the anomaly detector runs, additionally export
`meridian_anomalies_total` (counter) and `meridian_active_anomalies` (gauge) via
`WriteAnomalyMetrics` (ADR-024).

### Streaming anomaly detection

**Detector** (`internal/anomaly/detector.go`): a pure, per-series online detector
fed from the same per-series stream the broadcaster emits each tick, so it runs
uniformly in the monolith (`cmd/meridian/serve.go`, from `Head().SeriesInfos()`) and
the cluster gateway (`cmd/gateway`, from the aggregated `FetchSeries`). Each series
keeps O(1) state: an **EWMA level** (baseline) and **EWMA variance** (dispersion);
the score `|value − level| / dispersion` is a *local* z-score, so the slow diurnal
swing and memory drift are tracked (small residual → no alert) while a spike departs
sharply (large score → alert) — where a naive global z-score would flag the diurnal
peak (ADR-024). A Welford warmup seeds the baseline; a Huber clamp bounds a spike's
pull on the level/variance; a relative scale floor stops a flat series from collapsing
the dispersion; debounce + a hysteresis clear band and timestamp dedup (on
`SeriesInfo.LastTS`) avoid alert storms; stale series are evicted so memory follows
live cardinality. Raise/clear transitions broadcast as a distinct `anomaly` WebSocket
frame and into a bounded recent-events ring exposed at `/api/v1/anomalies` for
late-joining clients; the dashboard's Anomalies strip lists them most-recent-first.

### Cluster (microservices tier)

**Hash Ring** (`internal/cluster/ring.go`): a 64-bit SHA-256 consistent hash ring
with configurable virtual nodes and a deterministic nodeID tie-break, so a key's
replica set is a function of membership rather than of node-join order. `GetNodes`
returns the first N distinct replicas walking the ring, skipping nodes in the Dead or
Leaving state; `SetState`/`LiveNodes` are the hooks the health monitor drives.

**Replicated storage client** (`internal/service/client.go`): the live routing
source. It seeds a ring from the configured storage addresses and applies the
quorum model (ADR-022):

- **Write** — for each series, `replicas = ring.GetNodes(key, N)`; the series is sent
  to all live replicas (batched per node) and the write succeeds only at ≥W acks.
  Too few live replicas or acks is a quorum error, not a partial write.
- **Read** — a label-matcher query scatters to all live nodes (a superset of any
  matched series' replicas), merges and dedupes by (series, timestamp), enforces read
  quorum R globally and per series, and asynchronously **read-repairs** any responding
  replica missing points relative to the merged truth.
- **Health** — a background `/health` monitor (`StartHealthMonitor`) sets ring node
  state Active/Dead, so routing excludes dead nodes and re-includes recovered ones; a
  revived node is caught up by read-repair. The same refresh discovers each live node's
  rollup tier availability, which the resolution planner intersects across live nodes.

The ingestor and querier each build this client from `REPLICATION_FACTOR`/
`WRITE_QUORUM`/`READ_QUORUM` and run the health monitor. The `Coordinator`
(`internal/cluster/coordinator.go`) wraps the same ring with `RouteWrite`/`RouteRead`
helpers. Hinted handoff and rebalancing are future work; the monolith `serve` path is
single-node and does not use the ring.

### Retention & Downsampling

**Downsampler** (`internal/retention/downsampler.go`): Runs the live raw → 1m → 1h
cascade (ADR-011). Each background pass advances a tier as far as its source is
durably closed, rolling sealed raw blocks into resolution-tagged **rollup blocks**
(`storage.RollupBlock`, format v2) that store six Gorilla-compressed aggregate columns
(min/max/sum/count/avg + a reset-aware counter increase) per series under
`<dataDir>/rollups/<resolution>/`. The 1h tier is *chained* from the 1m tier — a
count-weighted average and an additive increase — so it equals a 1h rollup built
directly from raw. A per-resolution `covered_through` watermark makes passes idempotent
and crash-recoverable (rollups are regenerable). At query time the planner
(`internal/query`) selects a resolution from the span and step and the executor reads
the column that matches the operation (`TSDB.QueryResolution` with a `RollupAggregate`):
avg for a bare value, max/min/sum/count for the matching `*_over_time` function, and the
increase column for `rate()` (ADR-025) — rolling up the freshest not-yet-closed tail on
the fly so the coarse series stays current. This query-time selection runs in both
deployments: the single-binary `serve`, whose TSDB implements the `ResolutionDataSource`
capability, **and** the cluster, where `service.StorageClient` implements the same
capability — the engine and planner are reused unchanged. In the cluster the querier picks
a resolution from the span/step against the intersection of the live nodes' advertised tiers
(`/api/internal/resolutions`), asks the replicas for it (`QueryAtMostResolution` on each node
serves the coarsest tier it holds at or below the request, reporting what it served), and
merges the coarse results by (series, window-timestamp). The coarse merge skips read-repair —
rollups are node-local derivations, so raw replication + read-repair is the convergence layer
(ADR-022) — and a series whose replicas served different resolutions falls back to a raw read,
so heterogeneity never breaks a query (ADR-011). Older v1 blocks (no increase column) load and
serve their five columns, with `rate()` falling back to raw for the spans they cover.

**Enforcer** (`internal/retention/enforcer.go`): Per-resolution TTL cleanup. Raw
blocks expire first (and only once the finest rollup tier has captured them); each
rollup tier is kept longer, so a long-range query is still answerable from the 1h
tier after the raw behind it is gone.

## Data Flow

1. **Ingest**: Samples arrive via TCP → (optional per-series/priority admission shaper,
   ADR-027) → BatchWriter's bounded queue (block-then-shed backpressure) → drain → WAL
   (group-commit fsync, ADR-026) → HeadBlock (in-order policy). The TCP server
   bounds message size, applies a per-message read deadline, and caps concurrent
   connections; a full queue sheds and NACKs the producer rather than growing memory
   (ADR-023), and when admission is enabled the shed victims are chosen by lowest
   priority / most over-budget series rather than uniformly.
2. **Flush**: Head swap + WAL rotate (cut) → Gorilla-compressed block written via
   temp-dir + fsync + atomic rename → covered WAL segments reclaimed.
3. **Query**: Parser → Planner (also selects a rollup resolution from the span/step)
   → merge(HeadBlock, Blocks) or read the chosen rollup tier → Executor → Result
4. **Stream**: WebSocket hub broadcasts metrics + stats to dashboard at 60fps. The
   same per-tick per-series stream feeds the anomaly detector, whose raise/clear
   transitions broadcast as `anomaly` frames (ADR-024).
5. **Downsample & retain**: the downsampler rolls sealed raw blocks into the 1m/1h
   tiers (1h chained count-weighted from 1m); the enforcer expires each tier on its
   own TTL, raw first (only once it has been rolled up)
6. **Recover**: On open, load blocks, then replay only the WAL beyond the blocks'
   max low-water-mark → exactly-once reconstruction of the head.
