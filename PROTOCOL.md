# Meridian Wire Protocols

## TCP Ingestion Protocol

Meridian uses a JSON-over-TCP protocol for sample ingestion (port 9090 by
default). Each message is a JSON object terminated by a newline.

### WriteRequest

```json
{
  "timeseries": [
    {
      "labels": [
        {"name": "__name__", "value": "cpu_usage_percent"},
        {"name": "host", "value": "web-1"}
      ],
      "samples": [
        {"timestamp": 1700000000000, "value": 42.5},
        {"timestamp": 1700000005000, "value": 43.1}
      ]
    }
  ]
}
```

- `labels`: Array of name/value pairs. `__name__` is the metric name.
- `samples`: Array of timestamp (Unix ms) / value (float64) pairs.
- Samples must be in chronological order within each series.
- Maximum batch size is configured via `ingestion.batch_size`.
- Under overload the server may **shed** samples (ADR-023). The wire format is
  unchanged, but when per-series/priority admission is enabled (ADR-027) the shed
  decision is per-series: a series' `__name__` or a label value can place it in a
  priority class, and a single series flooding faster than its fair-share budget is
  shed before well-behaved ones. Order within an admitted series is preserved.

### Response

The server does not send responses on the TCP connection. The connection remains
open for streaming writes. Close the connection to stop.

## HTTP API

### Query

```
GET /api/query?q=<promql>&start=<ms>&end=<ms>&format=<json|csv|table>
```

**Response** (JSON format):
```json
{
  "status": "success",
  "data": [
    {
      "labels": {"__name__": "cpu_usage_percent", "host": "web-1"},
      "samples": [
        {"timestamp": 1700000000000, "value": 42.5}
      ]
    }
  ],
  "stats": {
    "seriesFetched": 8,
    "samplesFetched": 1200,
    "executionMs": 3.2
  }
}
```

**Resolution selection** (ADR-011, ADR-025): in the single-binary `serve`, the planner
transparently serves wide spans from a coarse rollup tier instead of raw, picking the
resolution from the query span and step. The response carries `resolution_ms` (the
rollup window served, in ms; `0` = raw) and `points_read` (points fetched from storage)
so a caller can see that a wide query read far fewer points from a coarse tier. A wide
range query is also served coarse when its function maps to a stored column — the
`*_over_time` family reads the matching aggregate (`max_over_time`→max, …) and `rate()`
reads the counter-increase column as `Σincrease / range` (window-averaged). Short ranges,
`last_over_time`, and a bare range selector still read raw. This selection runs in the
docker-compose **cluster** too: the querier runs the same planner and requests the chosen
resolution + aggregate column from the storage nodes (see Internal Cluster API below), so a
wide cluster query reports a non-zero `resolution_ms` exactly like the single binary
(ADR-011).

### Labels

```
GET /api/labels
```

Returns an array of known metric names: `["cpu_usage_percent", "memory_used_bytes", ...]`

### Health

```
GET /health
```

Returns `{"status":"ok"}` with 200 if the server is healthy.

## Internal Cluster API

These endpoints are used between services (querier/ingestor → storage). They are JSON over
HTTP on the storage node's address.

### Storage query

```
POST /api/internal/query
```

**Request:**
```json
{
  "matchers": [{"name": "__name__", "value": "cpu", "type": "="}],
  "start": 1700000000000,
  "end": 1700003600000,
  "resolution": 3600000,
  "aggregate": "max"
}
```

- `resolution` (ms, optional): the requested rollup window. `0`/absent reads raw. The node
  serves the **coarsest tier it holds at or below** this value, falling back to a finer tier
  or raw when the requested tier is absent (so a node just restarted or mid-downsample never
  fails the query).
- `aggregate` (optional, ignored when `resolution` is 0): the rollup column to read —
  `avg` (default), `min`, `max`, `sum`, `count`, or `increase` (for `rate()`). `increase` is
  only served from tiers whose counter-increase column is complete.

**Response:**
```json
{
  "status": "success",
  "resolution_ms": 3600000,
  "data": [
    {"name": "cpu", "labels": {"host": "web-1"}, "points": [{"t": 1700001800000, "v": 91.0}]}
  ]
}
```

- `resolution_ms`: the resolution the node **actually** served (`0` = raw). It can be finer
  than requested when the node lacks the requested tier; the querier reconciles a merge
  across replicas that served different resolutions (and falls back to a raw read for any
  series whose replicas disagree).

### Rollup tier availability

```
GET /api/internal/resolutions
```

Advertises a node's rollup tiers so the querier can plan a resolution (it requests only a
resolution every live node can serve — the intersection):
```json
{
  "resolutions": [60000, 3600000],
  "increase_resolutions": [60000, 3600000]
}
```

- `resolutions`: every tier (ms) that currently has data.
- `increase_resolutions`: the subset whose counter-increase column is complete, i.e. the
  tiers from which `rate()` can be served (ADR-025).

### Hinted-handoff backfill

```
POST /api/internal/backfill
```

The catch-up path for hinted handoff (ADR-029). The ingestor replays the hints it
buffered for a replica that was down through this endpoint when the replica returns. The
request body is identical to a storage write (`{"time_series":[...]}`); the difference is
the apply semantics: backfill accepts samples **older** than a series' last (inserting
them in sorted position, filling only gaps) where `/api/internal/write` would reject them
as out-of-order (ADR-015). This is what fills an interior gap read-repair cannot. The
response reports how many samples were applied (an exact duplicate of an existing point is
skipped):

```json
{ "samples_ingested": 128 }
```

Backfilled samples are logged under a distinct WAL frame, so they survive a storage
restart and replay through the same out-of-order-tolerant path; only a recovering replica
uses this endpoint, so the live in-order write path is unaffected.

### Anti-entropy digest & range

```
POST /api/internal/antientropy/digest
POST /api/internal/antientropy/range
```

The proactive convergence path (ADR-030). The coordinator (the ingestor) holds the ring,
so it sends the hash arcs to compare; the storage node stays ring-agnostic and classifies
its series with the same ring hash writes were routed by. Both requests carry a list of
`[lo, hi]` hash arcs — half-open `(lo, hi]`; `lo > hi` wraps the ring; `lo == hi` is the
whole ring — and a `[start, end]` millisecond span.

`digest` buckets the in-arc series into `window`-ms leaves and returns a Merkle tree over
them — each leaf a content hash over its `(series, sample)` data, plus a sample count:

```json
{ "ranges": [[1024, 4096]], "start": 0, "end": 1750000000000, "window": 3600000 }
```
```json
{
  "root": "9f86d0…",
  "window": 3600000,
  "leaves": [ { "start": 1749996000000, "hash": "a1b2…", "count": 240 } ]
}
```

Equal roots between two replicas mean they agree over the whole span — nothing transfers;
a differing root localises divergence to the leaves whose `hash` differs.

`range` exports the raw samples for the same arcs and span as a write body
(`{"time_series":[...]}`, with the synthetic `__name__` label dropped), so the coordinator
can read a divergent window from each replica and push whatever a peer is missing straight
back through `/api/internal/backfill`. Both the read and the push are scoped to the
differing window, so a converged cluster moves no data.

### Rebalance drop (storage)

```
POST /api/internal/rebalance/drop
```

The GC half of rebalancing on membership change (ADR-031). After the coordinator has
migrated a range to its new owners (the range read above + a backfill push) and confirmed
receipt at quorum, it tells the old owner to drop the data it no longer owns. The body is a
list of `[lo, hi]` hash arcs (same arc shape as anti-entropy); the storage node drops every
series whose ring position falls in any arc, across the head, raw blocks, and rollup tiers,
and reports what it removed:

```json
{ "ranges": [[1024, 4096]] }
```
```json
{ "series_dropped": 12, "samples_dropped": 4096, "rollup_windows_dropped": 340,
  "blocks_rewritten": 2, "blocks_deleted": 1 }
```

An empty `ranges` is a no-op — the drop is always expressed as the arcs that moved away,
never "keep only these," so it cannot erase a node. The cluster-level safety (drop only
after the new owners confirm at quorum, never the last copy) is enforced by the coordinator
before it issues the drop.

### Cluster membership (ingestor)

```
POST /api/internal/cluster/join     { "addr": "storage-4:8080" }
POST /api/internal/cluster/leave    { "addr": "storage-4:8080" }
```

Drives a membership change through the rebalance coordinator (ADR-031). `join` adds the node
(catching up out of routing), migrates the ranges it now owns onto it, promotes it, and GCs
the displaced owners; `leave` re-homes the node's ranges to the survivors before removing it.
The migration runs synchronously and the response reports what moved:

```json
{ "Migrations": 18, "SamplesMoved": 7200, "BytesMoved": 245760, "GCRuns": 18,
  "SeriesDropped": 18, "SamplesGCed": 7200, "NodesJoined": 1, "NodesLeft": 0, "Skipped": 0 }
```

Returns `503` when rebalancing is disabled (`REBALANCE_ENABLED=false`).

## WebSocket Streams

### Metrics Stream

```
ws://<host>/ws/metrics
```

Server sends JSON messages of type:

**Metric update:**
```json
{
  "type": "metric",
  "series": "cpu_usage_percent{host=\"web-1\"}",
  "labels": {"__name__": "cpu_usage_percent", "host": "web-1"},
  "timestamp": 1700000000000,
  "value": 42.5
}
```

**Server stats:**
```json
{
  "type": "stats",
  "ingestionRate": 8600,
  "activeSeries": 43,
  "memoryBytes": 52428800,
  "compressedBytes": 1048576,
  "rawBytes": 31457280,
  "walBytes": 262144,
  "blockCount": 4,
  "uptimeSeconds": 3600
}
```

Per-series samples are also pushed on the same channel:
```json
{
  "type": "metric",
  "series": "cpu_usage_percent{host=\"web-01\",role=\"web\"}",
  "labels": {"host": "web-01", "role": "web"},
  "timestamp": 1700000000000,
  "value": 42.5
}
```

**Anomaly alert** (ADR-024, ADR-028): a raise/clear transition from the streaming
detector. `state` is `firing` or `resolved`; `severity` is `warn` or `crit`;
`baseline` is the value the point was scored against — the EWMA level, or the
Holt-Winters forecast (level+trend+seasonal) under `mode: holt_winters` — and `score`
is `|value - baseline| / dispersion`; `seq` is a monotonic id for de-duplication.
```json
{
  "type": "anomaly",
  "seq": 12,
  "series": "cpu_usage_percent{host=\"web-01\",role=\"web\"}",
  "metric": "cpu_usage_percent",
  "labels": {"host": "web-01", "role": "web"},
  "value": 95.2,
  "baseline": 41.3,
  "score": 8.4,
  "severity": "crit",
  "state": "firing",
  "timestamp": 1700000000000
}
```

The recent transitions are also retained server-side; `GET /api/v1/anomalies`
returns them most-recent-first as
`{"anomalies": [...], "total": N, "active": M, "model": "ewma"}` — `model` is the
active scoring model (`ewma` or `holt_winters`) — so a late-joining client can seed its
view and label the detector.
