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

### Live Stream

```
ws://<host>/ws/live
```

Subscribes to raw sample batches:
```json
{
  "type": "live",
  "series": [
    {
      "labels": {"__name__": "cpu_usage_percent", "host": "web-1"},
      "samples": [{"timestamp": 1700000000000, "value": 42.5}]
    }
  ]
}
```
