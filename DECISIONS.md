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
