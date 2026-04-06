# Architecture Decision Records

## ADR-001: Go as Implementation Language

**Status**: Accepted  
**Context**: Need a systems language with good concurrency, fast compilation, and
single-binary deployment.  
**Decision**: Go 1.23 with zero CGO dependencies.  
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
