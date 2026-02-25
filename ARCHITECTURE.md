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
│   Consistent Hash Ring  │  Coordinator  │  Gossip       │
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
before acknowledgment. Recovery is resilient to corruption: a bad length field or
CRC mismatch re-anchors at the next 8-byte frame boundary and keeps scanning, so
one corrupt frame no longer discards the rest of a segment. Replay can start past
a given segment so that data already captured in a durable block is not replayed
again (see below).

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
pushdown and adjusts time ranges for range selectors.

**Executor** (`internal/query/executor.go`): Evaluates the AST against the TSDB.
Implements rate(), histogram_quantile(), and all aggregation functions.

### Ingestion

**TCP Server** (`internal/ingestion/server.go`): JSON-over-TCP ingestion
protocol. Accepts WriteRequest messages containing batched time-series samples.

**Batch Writer** (`internal/ingestion/batch.go`): Buffers incoming samples and
flushes on either a size threshold or timeout, whichever comes first.

### HTTP & WebSocket

**HTTP Server** (`internal/server/http.go`): REST API for queries, label
browsing, and health checks. Serves the dashboard SPA with embedded static
files.

**WebSocket Hub** (`internal/server/websocket.go`): Broadcasts live metrics and
system stats to connected dashboard clients. Supports metric subscription and
server stats streams.

### Cluster

**Hash Ring** (`internal/cluster/ring.go`): SHA256-based consistent hash ring
with configurable virtual nodes for even distribution.

**Coordinator** (`internal/cluster/coordinator.go`): Routes writes and reads
based on ring ownership with configurable replication factor.

### Retention & Downsampling

**Enforcer** (`internal/retention/enforcer.go`): Periodic TTL-based cleanup
that deletes blocks older than the configured retention period.

**Downsampler** (`internal/retention/downsampler.go`): Computes rollup
aggregates (min, max, avg, sum, count) per time window, enabling the
5s → 1m → 1h downsampling cascade.

## Data Flow

1. **Ingest**: Samples arrive via TCP → BatchWriter → WAL (fsync) → HeadBlock
   (in-order policy). The TCP server bounds message size, applies a per-message
   read deadline, and caps concurrent connections.
2. **Flush**: Head swap + WAL rotate (cut) → Gorilla-compressed block written via
   temp-dir + fsync + atomic rename → covered WAL segments reclaimed.
3. **Query**: Parser → Planner → merge(HeadBlock, Blocks) → Executor → Result
4. **Stream**: WebSocket hub broadcasts metrics + stats to dashboard at 60fps
5. **Retain**: Enforcer deletes expired blocks; downsampler creates rollups
6. **Recover**: On open, load blocks, then replay only the WAL beyond the blocks'
   max low-water-mark → exactly-once reconstruction of the head.
