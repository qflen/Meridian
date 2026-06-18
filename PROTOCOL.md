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
  "walSegments": 1,
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
