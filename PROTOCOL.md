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

**Anomaly alert** (ADR-024): a raise/clear transition from the streaming detector.
`state` is `firing` or `resolved`; `severity` is `warn` or `crit`; `baseline` is the
tracked EWMA level and `score` is `|value - baseline| / dispersion`; `seq` is a
monotonic id for de-duplication.
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
returns them most-recent-first as `{"anomalies": [...], "total": N, "active": M}`
so a late-joining client can seed its view.
