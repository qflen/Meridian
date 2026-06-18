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
component. Features: area fills, a one-shot load transition, auto-scaling axes,
and multi-series support. (The original revision also drew decorative glow; that
was removed under ADR-020.)  
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

**Status**: Accepted (implemented)
**Context**: High-resolution data is expensive to store and slow to scan over long
ranges: a 30-day view at 5-second resolution is ~518k points per series, far more
than a screen can show or a user needs. The rollup math (`Rollup`) existed but was
dead code — never scheduled, never persisted, never queried — so the advertised
"5s→1m→1h cascade with transparent resolution selection" was a claim with no code
path behind it. Making it real means three things that must agree: generating
resolution-tagged rollups, selecting a resolution at query time, and expiring each
tier on its own schedule.

**Decision**: A live cascade — raw → 1m → 1h — with rollups stored in their own
resolution-tagged blocks, a planner that picks the resolution from the query span,
and per-resolution retention.

- **Rollup blocks** (`storage.RollupBlock`). Rollups are stored separately from raw
  blocks, one directory per resolution (`<dataDir>/rollups/<label>/<ulid>`). Each
  series carries **five aggregate columns** — min, max, sum, count, and the
  count-weighted avg — each a Gorilla-compressed stream over the shared window-centre
  timestamps (the Gorilla codec is reused unchanged). Writing reuses the raw block's
  crash-safe path (temp dir → fsync files+dir → atomic rename → fsync parent). A
  rollup is **derived data**, so it carries no WAL low-water-mark; a block instead
  records its `resolution` and a `covered_through` watermark (the window-aligned,
  exclusive source-time bound the tier is complete to).

- **Generation** (`retention.Downsampler`). A background pass advances each tier as
  far as its source is *closed*: `covered_through = ⌊source_frontier / window⌋ ×
  window`, where the source frontier is the max time of the immutable raw blocks (for
  1m) or the finer tier's `covered_through` (for 1h). It rolls only windows whose end
  has passed that frontier, reading from **sealed raw blocks** (merged across block
  boundaries) so a rollup is deterministic and regenerable; the still-open head tail
  is left to the query path. Idempotency is the on-disk watermark — a tier resumes
  from `max(covered_through)`, so passes never duplicate or re-roll a window and a
  crash mid-pass is recovered by regeneration.

- **Weighted chaining** (the A16 correctness point). 1h is built by chaining the 1m
  tier, **not** by re-reading raw. For this to equal a direct raw→1h rollup the
  average must be count-weighted: `sum = Σ sub.sum`, `count = Σ sub.count`, `min/max =
  min/max of sub.min/max`, and `avg = sum / count`. A plain mean of the sixty 1m
  averages is wrong whenever the minutes hold unequal sample counts. Because every
  raw point falls in exactly one 1m window and every 1m window in exactly one 1h
  window (global alignment), the chained result is *identical*, for all five
  aggregates, to the direct one — proven by a test on deliberately uneven counts.

- **Query-time selection** (`query.Plan`/`executor`). The planner chooses the
  coarsest resolution that (a) is no coarser than the step (no upsampling) and (b)
  yields enough windows over the span, considering only resolutions that actually
  have data. The executor reads the chosen tier's **avg** column as the series value
  (min/max/sum/count are kept for future function-aware selection), widening the
  staleness window to one rollup interval so coarse points are not dropped as gaps.
  `TSDB.QueryResolution` serves persisted rollups for the range and rolls up the
  freshest, not-yet-closed tail on the fly (seamed at the window-aligned
  `covered_through`), so the coarse series stays complete to *now*. Selection is
  transparent — the result shape is unchanged; the HTTP API reports the chosen
  `resolution_ms` and `points_read` for observability.

- **Per-resolution retention** (`retention.Enforcer`). Raw expires first; each rollup
  tier is kept longer (defaults: raw 15d → 1m 30d → 1h 365d). A raw block is only
  eligible for deletion once the finest rollup tier has captured it (its max time is
  below that tier's `covered_through`), so shortening the raw TTL trades resolution
  for space without losing history. In the cluster the compactor enforces the same
  per-resolution TTLs, keyed by the resolution now carried on each block's metadata.

**Forced raw / future work**: range selectors and `rate()` force raw, because a rate
over a downsampled counter is not generally correct. Two follow-ups are deliberately
out of scope here and documented as future work: **function-aware aggregate
selection** (e.g. serving `max_over_time` from the stored max column rather than avg)
and **rate-on-rollup**. In the **cluster**, rollups are generated on each storage
node but the querier still reads raw (the remote client does not yet push a
resolution to storage); cluster query-time selection is the remaining piece, so
cluster raw retention should stay ≥ the longest query span until it lands.

**Consequences**: Verified on a 4-series, 8-hour backfill: a wide query (8h span, 1h
step) was served from the 1h tier reading **36 points**, versus **3844** for the same
span at raw resolution — a ~107× reduction — while a narrow query (5m span) read raw.
The 1h tier reported a 105× point-reduction (raw samples represented per stored
window); over 5s raw data the figures are ~12× (1m) and ~720× (1h). The cost is five
stored aggregates per window and a background pass; rollups are regenerable, so the
extra on-disk state is never authoritative.

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

## ADR-020: Visual Design Language — "Precision Instrument"

**Status**: Accepted
**Context**: The dashboard was functional but read as templated / AI-generated.
The concrete tells: the brand palette was verbatim Mantine indigo (`#4c6ef5`);
every surface used the same glassmorphism (backdrop-blur + lifted shadow +
`rounded-xl`); charts carried decorative glow and a rainbow compression gradient
that encoded no scale; `Inter` was named ~30 times but never actually loaded (a
silent system-font fallback, with the DOM and canvas free to disagree); two
styling systems coexisted (semantic CSS-var inline styles next to hardcoded
Tailwind grays monkey-patched per theme); three different `formatBytes` rendered
the same quantity differently; and the chrome carried a self-congratulatory,
partly false "60fps" boast backed by an always-on `requestAnimationFrame` meter.
A single committed point of view was needed — and consistency, not novelty, is
what removes the generated feel.
**Decision**: Adopt one design language — *Precision Instrument*
(mission-console-meets-chronometer: calm, engraved, exact), derived from the name
*Meridian* (navigation / transit instruments). Deliberately avoid the other two
AI-default looks as well (near-black + acid accent; cream + serif + terracotta).

- **Color — one accent; status means status.** Semantic CSS custom properties,
  re-valued per theme and exposed as Tailwind colors via
  `rgb(var(--x) / <alpha-value>)`. Exactly one accent (a calibrated phosphor teal)
  is reserved for live data and focus; ok/warn/crit carry status meaning only.
  - Dark (default): bg `#0E1320`, surface `#131A29`, border `#26303F`, text
    `#E6EAF0`, muted `#8A97A8`, accent `#3AA8A0` (hover `#4AC0B7`, active
    `#2D8B84`), ok `#57A773`, warn `#C9993A`, crit `#C7564C`.
  - Light (warm paper): bg `#F6F4EE`, surface `#FCFBF7`, border `#D8D3C6`, text
    `#1A2230`, muted `#5A6675`, accent `#1E7A72` (hover `#17635C`, active
    `#12524D`), ok `#2F7A54`, warn `#A6741A`, crit `#B04036`.
  - Multi-series, retention lanes, and node roles draw from a shared muted
    categorical palette (brass, slate-blue, sage, orchid, clay, dim-teal), never
    neon. The canvas graticule is derived from the border token, so DOM hairlines
    and chart grids are one system.
- **Type — three roles, mono for all figures.** Self-hosted via `@fontsource`:
  Inter Tight (display grotesque, headings), Inter (body), IBM Plex Mono (every
  numeric, with `tabular-nums`, so columns of figures align like a readout). One
  `canvasFont()` constant mirrors the loaded families into every canvas `ctx.font`
  — numeric axes/codes in the mono, word labels in the sans — so the DOM and
  canvas can never disagree. A small scale replaces ad-hoc sizes (`text-[10px]` →
  `text-2xs`).
- **Panel language.** One coherent base panel: a solid surface, a single hairline
  border in the border token, a tight radius (`rounded-md`), no glass blur and no
  lifted shadow. The header is solid, not blurred. (Panel tiers / hierarchy are
  deferred.)
- **Decoration removed (it encoded nothing).** All canvas glow (`shadowBlur`) on
  lines, bars, and nodes; the rainbow compression-gauge gradient (replaced by a
  single-accent arc whose sweep length honestly encodes the ratio) and the indigo
  bar gradient; the meaningless zebra stripe behind retention lanes; the header
  FPS/frameTime/dropped meter and the `useFrameMetrics` rAF loop it drove (the
  last always-on animation — an idle tab now does ~zero rAF); and the boastful
  footer line.
- **One styling system; one formatter.** Components use the semantic Tailwind
  classes only — the inline `style={{ rgb(var(--color-*)) }}` sprawl, the
  per-theme `.light/.dark .text-gray-*` override blocks, and the inline
  onMouseEnter/Leave hover hacks are gone. `utils/format.ts` is the single source
  for byte / number / duration / time formatting, so identical quantities render
  identically everywhere.
**Consequences**: One coherent identity per theme, verified in both dark and
light: fonts load over the network (woff2 under `/assets`) and the canvas shares
the DOM family; there is no Mantine indigo, no glow, and no glassmorphism;
numerics are tabular mono and aligned. This ADR supersedes the decorative choices
of ADR-009 (canvas rendering stays; its "glow effects" do not). The follow-up work
it deferred — the signature instrument chart, panel tiers / hierarchy, unified
empty/loading/error states, the logo and node iconography, and the accessibility
pass — is delivered in **ADR-021**.

## ADR-021: Visual Hierarchy, the Signature Instrument Chart, and the A11y Floor

**Status**: Accepted
**Context**: ADR-020 committed the "Precision Instrument" language but left the
identity work for a follow-up: every panel was a uniform `.card` in a flat grid
(the primary query action and a tertiary stat tile carried identical weight),
heights were eyeballed magic pixels (`h-[294px]`, canvases pinned to 140/160/180),
there was no memorable element, the four empty states were worded four different
ways and drawn at canvas center, the logo was a generic circle-and-rising-line,
cluster nodes were two-letter codes, and there was no keyboard/focus/reduced-motion
story. A consistent instrument *needs* a hierarchy and one bold focal point.
**Decision**:
- **Hierarchy through size, not decoration.** A `Panel` component renders one
  surface in three tiers — `primary` (the query bar and result), `secondary`
  (live monitors), `tertiary` (dense readouts) — differentiated only by padding,
  heading scale, and an accent eyebrow on the primary. No tier adds shadow or
  glow. A single `PANEL_BODY` scale on an 8px module replaces the per-panel magic
  heights; charts and lists size intrinsically (`flex-1`) inside a fixed body, so
  panels in a row align without anyone hard-coding a height.
- **One signature, the chart.** An `instrument` variant of `TimeSeriesChart` is
  used by the *single* primary chart and nowhere else (boldness spent once). It
  draws a fine graticule (faint minor subdivisions under major lines), instrument
  tick marks with tabular-mono labels, and a calm trace (only the primary accent
  series is area-filled, so multi-series stays legible). A cursor crosshair snaps
  a vertical guide to the nearest sample of the primary series — an O(log n)
  binary search (`utils/nearestSample`, unit-tested) — marks each series' nearest
  sample, and shows a corner tabular-mono readout of the timestamp and every
  series' value under the cursor. The hover pass is coalesced to one frame and the
  load sweep is skipped under `prefers-reduced-motion`, so an idle chart schedules
  no work.
- **One voice for empty/loading/error.** A shared `Placeholder` (a flat
  instrument-baseline mark plus plain, active copy) replaces the four canvas
  captions. The primary result surface is always present and resolves to
  loading / error / no-match / no-query states; a good chart is never blanked on a
  later failure — the last result stays with a small "running" chip — and a
  dropped stream surfaces as a reconnect banner driven by `connectionStatus`.
- **A subject-grounded mark.** The logo is a transit reticle — a graduated setting
  circle, the meridian line running clear through it, an inset horizon chord, and
  a star fixed on the meridian — deliberately echoing the chart's crosshair and
  sample dot; the favicon matches. Cluster nodes read as distinct shapes (gateway
  diamond, ingestor down-triangle, storage square, querier circle, compactor
  hexagon) instead of two-letter codes, mirrored in the legend.
- **Accessibility as a floor, not a polish.** Query suggestions are a real
  combobox/listbox (`aria-expanded`/`controls`/`activedescendant`, arrow/Enter/
  Escape, type-to-filter); icon-only controls carry `aria-label`, the theme
  control `aria-pressed`, Execute `aria-busy`; the connection state is conveyed by
  text, not colour alone. One global `:focus-visible` accent ring replaces the
  per-component focus styles, a global `prefers-reduced-motion` reset backs the
  `motion-safe:` utilities, and the layout is responsive to a phone width (header
  wraps, grids stack, stat tiles reflow).
**Consequences**: One coherent, hierarchical identity per theme, verified in dark,
light, and at a 390px width by driving the live dashboard in a headless browser:
the crosshair readout tracks the cursor, the empty/loading/error/reconnect states
render in one voice, and there are no console errors. The boldness lives in exactly
one place; everything else stays quiet, which is what makes it land.

## ADR-022: Replication — Quorum Writes, Quorum Reads, and Read-Repair

**Status**: Accepted
**Context**: The project claimed "consistent-hash clustering with configurable
replication," but `internal/cluster` (the ring and coordinator) was dead code with
zero non-test importers: the live write path sharded each series to a single node
with `fnv(name) % len(nodes)` — replication factor 1 — and `replication_factor: 3`
in config was inert. A single storage node's loss therefore lost data, and reads
returned silent partial results. The ring also truncated its hash to 32 bits and
ignored node state, so it could route to dead nodes. The goal: make replication real
over the existing docker-compose tier (ingestor → storage ← querier) while leaving
the single-binary monolith single-node.
**Decision**: Adopt a Dynamo-style quorum model with N, W, R configured in
`cluster` (defaults N=3, W=2, R=2; `Validate` enforces 1≤W,R≤N and W+R>N for
read-your-writes). The consistent-hash ring (`internal/cluster`, now widened to a
64-bit hash with a deterministic nodeID tie-break and Dead/Leaving filtering) is the
single routing source, built by `service.StorageClient` from the configured storage
addresses so the ingestor and querier agree on placement.

- **Replica selection**: a series' key is `MetricKey(name, labels)` (the synthetic
  `__name__` label excluded so write-side `[]Label` and read-side label maps hash
  identically); `replicas = ring.GetNodes(key, N)`, skipping nodes whose health
  state is Dead/Leaving.
- **Write — write-to-all, require W**: send each series to all of its live replicas
  (batched per node), succeed only when ≥W acknowledge. Fewer than W live replicas,
  or fewer than W acks, is a quorum failure returned as an error — never a silent
  partial write. The request is all-or-nothing. A replica that was down simply
  misses the write and is reconciled later by read-repair.
- **Read — scatter, quorum, merge, repair**: a PromQL query carries label matchers,
  not a single series key, so it can match many series each with its own replica
  set. The client therefore scatters to all live nodes (a superset of any matched
  series' replicas) and merges responses, deduping by (series, timestamp) via the
  existing point-merge. Read quorum R is enforced globally (≥R live nodes must
  respond) and per returned series (≥R of that series' replicas must have
  responded); too few is a quorum error, not partial data. It then asynchronously
  read-repairs: any responding replica missing points relative to the merged truth
  is sent exactly those points in the background.
- **Health**: a background monitor probes each node's `/health` and sets ring state
  Active/Dead, so routing excludes dead nodes and a node returning Dead→Active
  resumes receiving writes and is repaired on the next read.
- **Degraded mode**: with one node down, N=3/W=2/R=2 still satisfies both quorums,
  so writes and reads keep succeeding; the surviving replicas hold a complete copy.

Why quorum over async primary/replica: W+R>N gives read-your-writes without a leader
or failover election, and read-repair makes the system self-healing on the read path
— a better fit for a single-process-per-node tier than primary hand-off.

**Consequences**: A storage node can be lost without losing data or failing reads,
and a recovered node converges via read-repair — proven by in-process integration
tests (exactly-N placement, write-at-quorum-with-one-down, complete-read-with-one-
down, read-repair convergence, read-your-writes across a membership shift, and
quorum-failure errors). `internal/cluster` is now on the live path. The model is
eventually consistent: a read during a failure may briefly observe a replica that
has not yet been repaired, but the merge always returns the union, so no acknowledged
write is lost. Effective N/W/R are clamped to the live node count, so a cluster
smaller than the configured N degrades gracefully rather than rejecting every write.

**Deferred (honest scope)**:
- **Hinted handoff**: writes are not buffered for a dead replica; it stays stale
  until a read repairs it. Read-repair only converges points *newer* than a
  replica's last sample (storage rejects out-of-order — ADR-015), which covers the
  usual contiguous down-window; filling an interior gap needs hinted handoff.
- **Rebalancing on membership change**: the ring re-derives placement when state
  changes, but data already written is not migrated, and over-replication left by a
  degraded-mode write to a non-owner is not garbage-collected.
- **Cross-node anti-entropy** (e.g. Merkle repair) beyond read-triggered repair.
- **Topology/ownership in the UI**: `/api/v1/cluster` still reports per-service
  health, not per-series replica ownership; surfacing the ring there and in the
  dashboard is left as-is.

## ADR-023: Write-Path Backpressure — Bounded Queues with Block-Then-Shed Load Shedding

**Status**: Accepted
**Context**: The ingest buffers were effectively unbounded. The monolith
`BatchWriter` appended into a slice that grew without limit; the ingestor fanned
out an unbounded number of concurrent quorum writes (one goroutine per HTTP/TCP
request); the storage node ingested inline, one goroutine per request. Under
sustained overload — and replication makes this sharper, because a quorum write
can stall on a single slow replica, and a storage node's WAL fsync can become the
bottleneck — the arrival rate exceeds the service rate, the backlog grows, and the
process simply consumes memory until it OOMs. There was no flow control: no signal
to the producer to slow down, and no bound on resident work. A policy was required:
how much to buffer, when to push back, and what to do past the limit.
**Decision**: Put a **bounded queue between accept and the drain-to-storage
worker** on every ingest path, with **block-then-shed** enqueue. This is flow
control by a bounded queue: queue depth ≈ arrival_rate × service_time (Little's
Law), so bounding the depth bounds both resident memory and tail latency, and
overload is shed as a counted, observable event rather than a silent OOM.

- **Shared primitive** (`internal/backpressure.Queue[T]`). A FIFO bounded by an
  additive *cost* — for ingest the cost is a sample count, so the queue's depth is
  a sample budget and **depth ≤ capacity always**, which is the memory bound.
  `Enqueue(val, cost, deadline)` adds the item if there is room; if the queue is
  full it **blocks up to the deadline** waiting for the drain to free room — this
  is the backpressure — and if the deadline elapses it **sheds**: drops the item,
  counts it, and returns `Accepted=false` so the caller can NACK the producer. An
  item larger than the whole capacity can never fit and is shed at once rather than
  blocking forever.
- **Three zones via a high-water mark.** Below the high-water mark the queue
  accepts freely; at or above it the producer is flagged to **throttle** (an early
  hint, before any drop); when the queue is full the enqueue blocks then sheds.
  Capacity is the hard cap; the high-water mark is the soft warning.
- **Monolith** (`ingestion.BatchWriter`). Producers coalesce samples and enqueue
  full batches; a **single drain goroutine** pulls in FIFO order and calls
  `IngestBatch`. A slow TSDB backpressures the producer instead of growing a
  buffer; past the deadline batches are shed. `Server.Write` returns the shed count
  so the TCP producer is NACKed (`WriteResponse.Shed`/`Throttled`); the round-trip
  stall itself slows a synchronous producer like the simulator. A single drain
  keeps the queue FIFO, so an in-order producer's per-series timestamps are not
  reordered into out-of-order rejections (ADR-015).
- **Ingestor** (`service.WritePool`). A bounded queue plus a fixed **worker pool**
  draining to the quorum `Write`. `Submit` blocks while the queue is full and sheds
  past the deadline, returning `ErrShed`, which the HTTP handler maps to **429 +
  `Retry-After`** and the TCP handler to a NACK. The workers call `Write`
  unchanged, so **replication/quorum semantics are untouched** — only the
  submission rate is bounded, capping the concurrent in-flight writes that a stalled
  replica would otherwise let pile up.
- **Storage node**. The same pool drains into the local TSDB. A node whose WAL
  fsync is the bottleneck backpressures and, past the deadline, sheds with **429**;
  that non-200 makes the ingestor's quorum write observe the overload, so
  backpressure **propagates end-to-end** (producer → ingestor → storage) under one
  model.
- **Why block-then-shed** (not block-forever, not drop-immediately). Blocking
  forever turns a slow downstream into a stuck producer with unbounded queued
  goroutines — the OOM moves, it does not go away — and couples every producer to
  the slowest replica. Dropping the moment the queue is full wastes the burst
  absorption a small queue provides and sheds on transient spikes that a short block
  would have ridden out. A bounded block absorbs bursts (true backpressure) while a
  hard deadline guarantees the producer is never blocked unboundedly and memory is
  capped; **shedding with an accounted drop is strictly better than a silent OOM**,
  and propagating a 429/NACK lets a cooperative producer back off.
- **Config & metrics.** `queue_capacity`, `queue_high_watermark`, and
  `block_deadline` are configurable (with `max_concurrent_writes` sizing the
  service worker pools), validated at load. Every node exposes
  `meridian_dropped_samples_total`, `meridian_ingest_shed_events_total`, and
  `meridian_ingest_backpressure_events_total` (counters) plus
  `meridian_ingest_queue_depth`/`_capacity`/`_high_watermark` (gauges) on
  `/metrics`; queue depth/capacity and cumulative drops are on `/api/v1/stats` and
  the WebSocket stats frame. The drop counter stays **cumulative**
  (Prometheus-correct); the dashboard derives a per-second shed rate from successive
  samples, mirroring the windowed-rate / cumulative-counter split of ADR-017.

**Consequences**: Resident ingest memory is bounded by the queue capacity on every
path — the monolith, the ingestor, and the storage node — so overload can no longer
grow the process without limit. A slow consumer is felt by the producer as a
stalled round-trip (TCP) or a 429 (HTTP), and once the block deadline is exceeded
load is shed with an exact, counted drop; when the consumer recovers, throughput
resumes and shedding stops. The per-response shed count is best-effort (batches
coalesce samples across calls), but `meridian_dropped_samples_total` is the
authoritative count. Verified by tests on **both** paths: backpressure while full,
deadline shedding with exact drop counts, depth never exceeding capacity under a
concurrent flood, and recovery — all under `-race`, plus the HTTP 429 / TCP NACK
wiring.

**Deferred (honest scope)**:
- **Per-series / priority shedding**: past the cap all series are shed equally;
  there is no per-series fairness, priority class, or token-bucket rate limit
  (cardinality control is noted as future work in the roadmap).
- **Adaptive control**: the block deadline and capacity are static, not an
  AIMD/aware-of-latency controller; there is no spill-to-disk overflow.
- **Storage parallelism**: the storage pool's workers serialize on the TSDB write
  lock, so its worker count is an admission bound rather than true write
  parallelism; group-commit (ADR-016) would raise the service rate it bounds.

## ADR-024: Streaming Anomaly Detection — EWMA Baseline + Dispersion, Robust to a Moving Baseline

**Status**: Accepted
**Context**: The telemetry on the live path is not stationary. The simulator (and
real infrastructure) produces a slow **diurnal swing** (a 24-hour load cycle) and
slow **drift** (a leaking memory gauge that climbs for minutes then resets), with
**spikes** injected on top (CPU jumping 2–3× for tens of seconds). We want to flag
the spikes in real time and surface them on the dashboard — *without* flagging the
diurnal swing or the drift, which are normal. The naive approach — score each point
against a global mean and standard deviation (a fixed-baseline z-score) — is exactly
wrong here: the baseline it compares against moves, so it would flag the entire
afternoon peak of a diurnal series and the whole tail of a drifting one (a point
many σ above the *seed* mean even though nothing is wrong). A streaming detector
also has to be cheap: single-pass, O(1) state per series, no history buffer, run
inline in the per-second broadcast over hundreds of series.

**Decision**: A per-series **exponentially-weighted moving baseline + dispersion**
in a new pure package `internal/anomaly`, fed from the broadcast loop and emitting
alerts over the existing WebSocket hub.

- **Local z-score, not global.** Each series tracks an EWMA **level** (the
  baseline) and an EWMA **variance** of the residual (the dispersion). The score is
  `|value − level| / dispersion` — a z-score against the *recently tracked*
  baseline. Because both terms decay old data geometrically, the level follows the
  slow diurnal/drift movement (so its residual stays small → no alert) while a spike
  is a fast departure from the value the series was just holding (residual ≫
  dispersion → large score → alert). This is the whole point: the moving baseline is
  what makes diurnal data safe, where a global mean/σ is not.
- **Welford warmup.** The first `warmup` samples seed the level/variance with an
  exact Welford mean and sample variance and raise nothing, so the detector never
  alerts before a baseline exists.
- **Robustness — Huber winsorization + a scale floor.** The classic EWMA-z-score
  failure is self-blinding: once the spike arrives it is folded into the level (which
  lurches toward it, so the next point looks normal) and the dispersion (which
  inflates, so nothing looks anomalous for a while after). Before a residual updates
  the level and variance it is **clamped to ±(clip · dispersion)** (a Huber step,
  clip defaults to the threshold). A spike therefore moves the baseline only
  slightly, so its score stays high for the whole spike and the alert holds until the
  value genuinely recovers; a slow ramp's residual is well inside the clamp and
  passes through unchanged, so the baseline still tracks it. The dispersion is also
  **floored** at `floorFrac · |level| + floorAbs` (relative, so it adapts to each
  series' magnitude) so a momentarily-flat series cannot collapse the scale toward
  zero and make ordinary noise look anomalous — important because the 1 Hz broadcast
  resamples a slower (5 s) stream, so a series often repeats a value.
- **Debounce + hysteresis (no alert storms).** An alert is raised only after
  `debounce_k` consecutive out-of-band samples and cleared only once the score falls
  back through a lower hysteresis band (½·threshold), so single-sample noise and
  boundary flapping do not flap the alert. To make "consecutive" mean genuine
  samples rather than re-reads, the detector **dedups on the sample timestamp**
  (`SeriesInfo.LastTS`, plumbed through storage and the service layer): a sample
  whose timestamp does not advance the series is ignored. On the gateway this also
  collapses the same series fetched from multiple replicas at one timestamp.
- **Bounded memory.** State is O(1) per series (a handful of float64s). Series not
  seen within a TTL are **evicted**, so memory follows live cardinality (series leave
  the head on flush/retention) rather than every series ever observed.
- **Where it runs — tick-based, uniform across modes.** The detector is fed from the
  same per-series stream the broadcaster already emits each tick: `SeriesInfos()` in
  the monolith (`cmd/meridian/serve.go`) and the aggregated `FetchSeries` in the
  cluster gateway (`cmd/gateway`). One code path, identical in both deployment modes.
  We chose the tick feed over hooking raw ingest because it is simpler, naturally
  cross-mode (the gateway never sees raw samples), and the 1 Hz cadence is the right
  resolution for alerting on a live dashboard; the timestamp dedup recovers
  per-sample debounce semantics. The trade-off (honest): detection runs at the
  broadcast resolution and observes each series' latest value per tick, not every
  raw sample.
- **Events, buffer, transport.** A raise/clear transition is an `Event` (series,
  metric, labels, value, baseline, score, severity warn/crit by score vs
  2·threshold, state firing/resolved, a monotonic seq, timestamp) broadcast as a
  distinct **`anomaly`** WebSocket frame. A bounded server-side ring keeps the recent
  events; `/api/v1/anomalies` returns them most-recent-first so a late-joining
  dashboard seeds its strip before live frames arrive. The dashboard merges by series
  on the seq (one row per series, firing → resolved updates the same row), seeded
  once then kept live.
- **Config & metrics.** `anomaly.{enabled,threshold,alpha,warmup,debounce_k}` are
  configurable and validated at load; the robustness knobs take internal defaults.
  `meridian_anomalies_total` (cumulative counter) and `meridian_active_anomalies`
  (gauge) are on `/metrics` and `/api/v1/stats`. The detector runs only where the
  broadcast loop runs — the monolith and the gateway — so only those expose the
  anomaly metrics; the storage/querier/ingestor/compactor services do no detection.

**Consequences**: The diurnal swing and the slow memory drift do **not** raise
alerts (the moving baseline tracks them and, for memory, the relative scale floor
treats a few-percent jump as in-band), while the injected CPU spikes raise an alert
within `debounce_k` samples that clears on recovery — proven by unit tests on
synthetic stationary / diurnal / drift / spike sequences (the drift test also checks
that a naive frozen-baseline z-score *would* have fired, so the moving baseline is
demonstrably doing the work), plus warmup, debounce, threshold, exact Welford+EWMA
math, dedup, eviction, and the robustness check that a second identical spike still
fires after the first. The cost on the hot path is one map lookup and a few float
ops per series per tick, with bounded memory.

**Deferred (honest scope)**:
- **Tick resolution, not per-sample.** Detection observes the latest value per
  series per broadcast tick; a spike shorter than a tick that never lands on a
  broadcast read is not seen. Hooking raw ingest would give per-sample fidelity at
  the cost of the cross-mode uniformity above.
- **No seasonal model.** The EWMA tracks *any* slow movement, diurnal or not; it
  does not learn the 24-hour period explicitly (e.g. Holt-Winters), so a step change
  in the *rate* of the baseline is briefly out-of-band until the level catches up.
- **Per-series tuning.** One global threshold/alpha applies to every series; there
  is no per-metric override or learned per-series sensitivity.
- **No alert persistence or routing.** Alerts live in a bounded in-memory ring and
  on the WebSocket; they are not persisted, deduplicated across restarts, or routed
  to an external alertmanager.
