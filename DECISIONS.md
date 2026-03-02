# Architecture Decision Records

## ADR-001: Go as Implementation Language

**Status**: Accepted  
**Context**: Need a systems language with good concurrency, fast compilation, and
single-binary deployment.  
**Decision**: Go 1.25 with zero CGO dependencies. CI installs the toolchain from
`go.mod` (`go-version-file`) so the build version never drifts from the module.  
**Consequences**: Simple cross-compilation, no shared library issues in containers,
goroutine-per-connection model fits well.

## ADR-002: Gorilla Compression for Time-Series Data

**Status**: Accepted  
**Context**: Time-series data exhibits high temporal locality and value similarity
that generic compression (gzip, lz4) cannot exploit effectively.  
**Decision**: Implement Facebook's Gorilla encoding with delta-of-delta timestamps
and XOR float encoding. Extended to 64-bit millisecond timestamps (paper uses
32-bit seconds). 4-byte count header for decoder bootstrapping.  
**Consequences**: Achieves 20-30x compression on regular metrics vs. 3-5x with
generic algorithms. Small decoder that can stream without seeking.

## ADR-003: CRC32-Framed WAL with Segment Rotation

**Status**: Accepted  
**Context**: Must survive process crashes without losing acknowledged writes.  
**Decision**: Write-ahead log with CRC32 checksums, 8-byte alignment for
efficient reads, and automatic rotation at 128 MB segments.  
**Consequences**: Crash recovery by replaying WAL. Segment rotation keeps
individual files manageable and enables garbage collection.

## ADR-004: Inverted Index with Sorted Slices, Not Roaring Bitmaps

**Status**: Accepted  
**Context**: Need an inverted index for label-based series lookup. Roaring bitmaps
are the standard choice but add a dependency.  
**Decision**: Use `map[string]map[string][]uint64` with sorted slices and
set intersection/union via merge-join.  
**Consequences**: Zero external dependencies. Performance is adequate for the
expected scale (< 100K series). Not optimal for millions of series.

## ADR-005: JSON-over-TCP Ingestion Protocol

**Status**: Accepted  
**Context**: Protobuf would be the natural choice for ingestion, but protoc is not
available in the build environment.  
**Decision**: JSON-over-TCP with newline framing. Same message structure as the
proto definition for future migration.  
**Consequences**: ~3x larger on the wire than protobuf. Simpler debugging with
netcat/telnet. Easy to switch to protobuf later since struct shapes match.

## ADR-006: PromQL Subset via Recursive Descent Parser

**Status**: Accepted  
**Context**: Users expect a familiar query language for time-series databases.  
**Decision**: Implement a PromQL subset with recursive descent parsing. Supports
vector/range selectors, label matchers (=, !=, =~, !~), aggregations (sum, avg,
min, max, count, topk, bottomk), functions (rate, histogram_quantile), binary
operators, and group-by clauses.  
**Consequences**: No parser generator dependency. Easy to extend. Covers the
most common monitoring use cases.

## ADR-007: Consistent Hash Ring for Data Distribution

**Status**: Accepted  
**Context**: Need to distribute series across cluster nodes with even load
balance and minimal disruption during scaling.  
**Decision**: SHA256-based consistent hash ring with configurable virtual nodes
(default 64 per node). Series assigned by MetricKey = hash(sorted labels).  
**Consequences**: Adding/removing a node only redistributes ~1/N of data.
Virtual nodes smooth out hash distribution.

## ADR-008: requestAnimationFrame Batching for WebSocket Messages

**Status**: Accepted  
**Context**: WebSocket messages arrive faster than the display refresh rate.
Processing each message individually causes excessive React re-renders and
dropped frames.  
**Decision**: Buffer incoming WebSocket messages and flush them in a single batch
on each requestAnimationFrame callback.  
**Consequences**: Dashboard maintains 60fps even at high ingestion rates. Slight
increase in perceived latency (up to 16ms) which is imperceptible.

## ADR-009: Canvas-Based Chart Rendering, No Chart Library

**Status**: Accepted  
**Context**: The spec requires zero chart dependencies (no D3, Chart.js,
Recharts). Charts must render at 60fps for live streaming data.  
**Decision**: Direct Canvas 2D API rendering with custom TimeSeriesChart
component. Features: glow effects, area fills, animated transitions, auto-scaling
axes, multi-series support.  
**Consequences**: Full control over rendering pipeline. No dependency bloat.
Requires manual hit-testing for interactivity (tooltips, zoom).

## ADR-010: React Context + useReducer for State Management

**Status**: Accepted  
**Context**: Dashboard state (theme, time range, query results, live metrics,
cluster nodes) needs to be shared across many components.  
**Decision**: Single DashboardContext with useReducer pattern. No external state
library (Redux, Zustand, etc.).  
**Consequences**: Zero dependencies for state management. Action-based updates
are predictable and debuggable. Adequate for the component count.

## ADR-011: Three-Tier Downsampling Cascade

**Status**: Accepted  
**Context**: Long-term storage of 5-second resolution data is prohibitively
expensive. Users querying older data don't need high resolution.  
**Decision**: Automatic downsampling: 5s → 1m (after 24h) → 1h (after 7d).
Each rollup stores min, max, avg, sum, count per window.  
**Consequences**: Storage savings of ~12x for 1m and ~720x for 1h rollups.
Query engine transparently selects appropriate resolution.

## ADR-012: Single-Binary Architecture

**Status**: Accepted  
**Context**: Deployment simplicity is a core design goal. Users should be able to
run `./meridian serve` and have a complete system.  
**Decision**: Single Go binary bundles server, ingestion, query engine, simulator,
CLI tools, and dashboard static files.  
**Consequences**: No orchestration required for single-node deployment. Dashboard
assets are embedded or served from a directory. Trade-off: binary size is larger.

## ADR-013: Diurnal Simulation with Spike Injection

**Status**: Accepted  
**Context**: Testing and demos require realistic-looking metric data, not random
noise. Real infrastructure exhibits predictable daily patterns.  
**Decision**: Simulator generates diurnal curves (peak at 14:00 local time) with
random spike injection (10% probability per host per cycle) and memory drift
(slow monotonic increase with periodic resets).  
**Consequences**: Dashboard screenshots and demos look realistic. Compression
benchmarks reflect real-world data patterns.

## ADR-014: PromQL Evaluation Semantics

**Status**: Accepted
**Context**: The query engine advertised a PromQL subset, but several pieces were
incorrect rather than merely incomplete: a range window was subtracted twice,
`rate()` divided by the sample span, `histogram_quantile()` ignored `le` buckets,
vector÷vector returned the left operand, and `/0` returned `0`. Making the engine
*correct* required committing to specific semantics.
**Decision**:
- **Stepped range evaluation → matrix.** `Execute(start, end, step)` evaluates the
  expression as an instant query at each `t` in `{start, start+step, …, end}` and
  assembles a matrix: one point list per series, keyed by label set, in time order.
  `start == end` is a single instant.
- **Range windows are half-open `(t-d, t]`, applied once.** A range vector `m[d]`
  at instant `t` covers `(t-d, t]` (lower-exclusive, upper-inclusive, per
  Prometheus). The duration is subtracted in exactly one place — the per-step slice
  — never twice. Anchoring each window at its step `t` makes `rate(m[5m])` read one
  selector-width of samples per step regardless of the `[start,end]` span.
- **Instant vectors use a 5-minute look-back.** At `t`, a selector takes each
  series' most recent sample with timestamp in `[t-5m, t]`, stamped at `t`. 5m is
  Prometheus's staleness delta; with no sample in that window the series yields no
  point — a gap, never a zero, so "no data" stays distinct from "zero".
- **`rate()` follows Prometheus.** Divide the increase by the *range* (threaded in
  from the selector), correct counter resets by adding the post-reset value, and
  extrapolate to the window edges. `rate()` takes the window end explicitly, so it
  runs unchanged per step and `rate(x[5m])` becomes a multi-point series.
- **`histogram_quantile()` interpolates cumulative buckets.** Group `_bucket`
  series by their non-`le` labels, sort by numeric `le` (`+Inf` parsed), and
  linearly interpolate within the bucket holding rank `φ·total`.
- **Vector matching ignores `__name__`; division is IEEE-754.** Two vectors pair on
  their full label set excluding the metric name, drop unmatched series, and drop
  `__name__` from the result. `x/0` yields `±Inf` and `0/0` yields `NaN` rather
  than a silent `0`.
- **One duration grammar.** The query layer parses durations via
  `config.ParseDuration`, so compound/decimal forms (`1h30m`, `1.5h`) are accepted
  consistently and the two layers cannot drift.
- **Fetch once, slice per step.** Each leaf selector's full needed window —
  `[start - maxRange - lookback, end]`, the planner's block-pruning span widened by
  the look-back — is fetched from storage once and sliced in memory for every step,
  avoiding an N+1 over steps. Matchers are pushed down per selector.
- **Step defaults and guards.** `step ≤ 0` derives a step so the range yields ~250
  points (floored at 1s); `start > end` is rejected; the step count is capped at
  11000. The cap bounds output size and pre-empts a denial-of-service via
  attacker-controlled `start`/`end`/`step`.
**Consequences**: Evaluation is stepped/range — a query returns a matrix with one
point per step per series — so `rate(x[5m])`, `sum(...) by (...)`, and `a/b` render
as smooth multi-point lines rather than single values. The planner's `TimeRange`
now feeds the single per-step fetch, as the prior revision anticipated. Output
matches Prometheus for the supported subset, backed by regression tests for matrix
shape, look-back/staleness, step alignment, per-step aggregation and vector
matching, counter resets across windows, and the step-count guard.

## ADR-015: Reject Out-of-Order Samples

**Status**: Accepted
**Context**: Samples were appended to a series in arrival order without any ordering
check, and `WriteBlock` took `Timestamps[0]`/`Timestamps[last]` as a block's min/max
assuming sorted input. Ingesting `100, 50, 200, 10` therefore produced an *inverted*
block (`minTime=200, maxTime=10`), which silently dropped overlapping queries
(`Overlaps` compares against these bounds) and poisoned retention (the enforcer
deletes by `MaxTime`). A policy was required; the options were reject, sort-on-flush,
or full out-of-order support.
**Decision**: **Reject**, the Prometheus-classic model.
- A sample with a timestamp strictly older than the series' last is dropped and
  counted in `meridian_out_of_order_samples_total`.
- A sample whose timestamp equals the series' last is **deduplicated** if its value
  is identical (a harmless retransmit) and **rejected** (counted) if the value
  conflicts.
- Otherwise the sample is appended. This keeps every series' timestamps monotonic.
- Belt-and-braces: `WriteBlock` computes each series' min/max by **scanning** its
  timestamps rather than trusting position, so block bounds can never be inverted
  even if unsorted data reaches it by another path.
- Ordering is enforced at apply time, not before the WAL write: ingest logs the
  sample to the WAL first and then applies the policy, and replay applies the *same*
  policy, so the recovered head is identical to the live head. The counter is a
  process-lifetime gauge incremented only on the live path (not during replay).
- The policy is scoped to the **active head**. After a flush the head is empty, so a
  series' "last" resets; a post-flush sample older than already-flushed data is
  accepted into the new head. Blocks may then overlap in time, which the read path
  already handles (it merges and sorts). Cross-flush ordering is intentionally not
  enforced, matching the head-relative out-of-order window of mainstream TSDBs.
**Consequences**: Series stay sorted, so `Timestamps[0]`/`[last]` range checks and
non-inverted block bounds hold; retention expires correctly. Out-of-order data is
visibly dropped and counted rather than silently corrupting bounds. Full
out-of-order ingestion (an out-of-order head plus m-block overlap resolution) is
future work.

## ADR-016: Crash-Consistent Flush with a Per-Block WAL Low-Water-Mark

**Status**: Accepted
**Context**: `Flush()` ran `WriteBlock(head)` → `head.Reset()` → `wal.Truncate()`
with no lock spanning the three steps. A sample ingested after the block snapshot but
before `Reset()` was discarded from the head *and* erased from the WAL by `Truncate()`
— silent loss. A crash after the block was persisted but before `Truncate()` left the
next `Open()` to load the block *and* replay the whole WAL — a double-count. Block
writes were also non-atomic (no temp/rename, nothing fsynced, several ignored I/O
errors).
**Decision**: A three-phase flush with an atomic in-memory cut and a durable
low-water-mark.
1. **Cut (under `db.mu` as writer).** Capture the old head, install a fresh head, and
   `Rotate()` the WAL so in-flight and future writes land in a new segment. The
   rotation returns the sealed segment sequence — the **low-water-mark**: every
   sample the old head holds is in WAL segments `<= mark`; every later sample is
   beyond it. Ingest holds `db.mu` as a *reader* across its WAL-append + head-append
   pair, so the cut (a brief writer section) never splits a sample between the old and
   new generation.
2. **Persist (outside the lock).** Write the old head to a block in a temp dir, fsync
   the files and their directories, atomically `rename` into place, then fsync the
   parent directory. The **rename is the single durable commit point**. The block
   records the low-water-mark in its metadata.
3. **Reclaim (best-effort).** Only after the block is durable are the covered WAL
   segments deleted. Deletion is pure space reclamation, never a correctness
   dependency.

   On `Open`, replay skips every WAL segment at or below the maximum low-water-mark
   across all loaded blocks. Blocks written before the field carries a `0` mark,
   which conservatively replays the whole WAL (`data/` is disposable local state).
   A failed block write disables further flushes until restart, so a later flush
   cannot record a higher mark that would skip the failed flush's still-uncovered
   segments; that data remains in the WAL and is recovered on the next open. Leftover
   temp block dirs from an interrupted write are removed on open.
**Consequences**: Exactly-once recovery. A crash **before the block is durable**
leaves no committed block and an un-truncated WAL, so replay rebuilds the data once
from the WAL (the in-memory cut is lost with the crash, so there is no persistent
gap). A crash **after the block is durable but before WAL cleanup** leaves both on
disk, but the low-water-mark makes replay skip the covered segments — no
double-count. Concurrent ingestion during a flush loses nothing. Every I/O error in
the write path is checked, and durability is confirmed (rename + parent fsync) before
any WAL is reclaimed. The trade-off is a brief ingest stall during the cut (one
segment sync while the writer lock is held) and that a (rare) block-write failure
makes the in-flight generation queryable only after a restart. Group-commit of WAL
frames (one fsync for coalesced writes) is noted as future work; today each frame
still fsyncs individually.

## ADR-017: Ingestion Rate as a Windowed Rate, Cumulative Count for the Counter

**Status**: Accepted
**Context**: `TSDB.IngestionRate()` returned the cumulative `ingested` counter. The
dashboard samples that value once per second and charts it as a rate, so it drew a
monotonically rising line instead of throughput. Separately, the Prometheus
exposition needs a *cumulative* `meridian_samples_ingested_total` — a `..._total`
counter is correct by Prometheus convention (the scraper computes `rate()`). One
method could not honestly serve both roles.
**Decision**: Split the two concerns.
- `IngestionRate()` returns a **windowed** samples/sec rate: a moving average over
  `RateWindow` (default 5s), fed by a background sampler that records the cumulative
  count every `RateSampleInterval` (default 1s). Idle intervals contribute the same
  total as the previous one, so the rate decays smoothly to 0 when ingestion stops. A
  counter reset (process restart, total decreases) is clamped to a non-negative rate.
- `IngestedTotal()` returns the cumulative count and backs
  `meridian_samples_ingested_total`, which stays a monotonic counter.
- `/api/v1/stats` `ingestion_rate` and the WebSocket `ingestionRate` carry the
  windowed rate. Wire types stay `int64` (the rate is rounded to whole samples/sec),
  so neither the dashboard nor the inter-service protocol changes. Per-node rates are
  additive, so the gateway sums them into the cluster rate.
**Consequences**: The dashboard charts a true rate that tracks load and falls back to
~0 when idle, with no dashboard change. The Prometheus counter remains a proper
cumulative counter. The reported rate lags a step change by up to the window, and
each TSDB runs one extra lightweight sampler goroutine. Verified live: under load the
rate read ~140/s and fell to 0 within a window of going idle, while
`samples_ingested_total` held at its cumulative value.

## ADR-018: CORS Restricted to Configured Origins (Default Localhost)

**Status**: Accepted
**Context**: Both the monolith and the gateway returned
`Access-Control-Allow-Origin: *` for every method, POST included. Any web page the
operator visited could therefore script cross-origin reads and writes against a
Meridian instance reachable from that browser (e.g. on a private network).
**Decision**: Replace the blanket wildcard with an origin-checked middleware that
echoes only permitted origins.
- Default (unconfigured): allow only `localhost` / `127.0.0.1` / `[::1]` origins —
  the local dashboard and its Vite dev server — and nothing else.
- An explicit allow-list is exact-matched (`server.allowed_origins` for the monolith,
  `GATEWAY_ALLOWED_ORIGINS` for the gateway). A single `"*"` entry re-enables
  allow-all for deliberately trusted networks.
- The matched origin is reflected back (with `Vary: Origin`) rather than `"*"`, so the
  policy is also correct for credentialed requests. A disallowed cross-origin request
  receives no CORS headers and is blocked by the browser.
**Consequences**: Same-origin dashboard use is unchanged (a same-origin request sends
no `Origin` header and proceeds normally). Cross-origin browser access now requires
explicit configuration. The internal service APIs (storage/querier/ingestor) are not
browser-facing surfaces and keep their permissive CORS; the public entry points
(monolith `serve`, gateway) enforce the policy.

## ADR-019: Prometheus /metrics on Every Service

**Status**: Accepted
**Context**: Only the monolith exposed `/metrics`. In the docker-compose topology the
gateway, querier, storage, ingestor, and compactor had no scrape endpoint, so the
"cluster" the README advertises could not actually be observed by a
Prometheus-compatible collector.
**Decision**: Register `/metrics` on every service through shared
`WriteStorageMetrics`/`WriteServiceMetrics` helpers. Storage nodes expose the full
storage metrics — the cumulative `meridian_samples_ingested_total`,
`meridian_out_of_order_samples_total`, head samples, active series, block count,
storage bytes by layer, and compression ratio. Every service additionally exposes
`meridian_up` and `meridian_uptime_seconds`, and the gateway reports connected
WebSocket clients. The monolith handler reuses the same storage helper so a storage
node emits identical metrics whether run as the monolith or as the storage service.
**Consequences**: The cluster is scrapeable end-to-end and metric names/labels are
consistent across deployment modes. The endpoints are unauthenticated and intended
for an internal scrape network (the same trust boundary as the internal RPC APIs).
