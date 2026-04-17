# Meridian

A distributed time-series database written in Go with a real-time React dashboard.

Meridian implements Facebook's Gorilla compression, a PromQL-subset query engine,
consistent-hash clustering, automatic downsampling, and a canvas-rendered 60 fps
monitoring dashboard. It ships as a single binary with minimal dependencies.

![Meridian dashboard demo](docs/demo.gif)

## Quick Start

```bash
# Build everything, start server + simulator, open the dashboard
./run.sh demo
```

Step by step:

```bash
make build dashboard

./bin/meridian serve &
./bin/meridian simulate &          # 8 hosts × 43 metrics, diurnal patterns

open http://localhost:8080

./bin/meridian query "rate(http_requests_total[5m])"
./bin/meridian query "avg by (host)(cpu_usage_percent)"
```

## Measured Performance

Apple M5, Go 1.22, `./bin/meridian bench` (1 M samples, regular-interval):

| Metric              | Value           |
| ------------------- | --------------- |
| Compression ratio   | **28.3×**       |
| Space savings       | 96.5%           |
| Encode throughput   | 45.2 M points/s |
| Decode throughput   | 66.0 M points/s |
| Encode latency      | 22 ns/point     |
| Decode latency      | 15 ns/point     |

Live compression figures (blocks + in-memory head) are exposed on `/api/v1/stats`,
`/metrics`, and the dashboard's compression gauge.

## Features

### Storage Engine
- **Gorilla compression**: delta-of-delta timestamps + XOR float encoding
- **Write-ahead log**: CRC32-framed, 128 MB segment rotation
- **Inverted index**: sorted-slice intersection, no external bitmap dependencies
- **Block storage**: ULID-named immutable blocks with a binary index

### Query Engine
- **PromQL subset**: recursive-descent parser
- **Selectors**: vector, range, label matchers (`=`, `!=`, `=~`, `!~`)
- **Functions**: `rate()`, `histogram_quantile()`
- **Aggregations**: `sum`, `avg`, `min`, `max`, `count`, `topk`, `bottomk` with `by`/`without`
- **Binary ops**: `+`, `-`, `*`, `/` with operator precedence

### Cluster
- **Consistent hash ring**: SHA256 with virtual nodes
- **Configurable replication**: writes fan out to N nodes
- **Node lifecycle**: joining → active → leaving → dead

### Dashboard
- **Canvas-rendered**: 60 fps charts, no chart library
- **10 components**: query editor, time-series chart, metric explorer, cluster topology, ingestion monitor, compression gauge, latency histogram, retention timeline, live stream, theme toggle
- **Real-time**: WebSocket streaming batched through `requestAnimationFrame`
- **Themes**: dark, light, high-contrast

### Observability
- **`/metrics`**: Prometheus exposition format (head/block stats, query-latency histogram, compression ratio, WS client count, uptime)
- **`/health`**: liveness probe for orchestrators
- **`/api/v1/stats`**: JSON snapshot of storage, WAL, ingestion, compression

### Operations
- **Retention enforcement**: TTL-based block deletion
- **Downsampling**: 5s → 1m → 1h cascade (min/max/avg/sum/count)
- **Simulator**: diurnal patterns, spike injection, memory drift across 8 hosts

## Architecture

```
HTTP/WS ─→ Query Engine ─→ TSDB (Head + Blocks)
  │                            │
  ├── Dashboard (React)        ├── WAL (CRC32)
  ├── REST API                 ├── Gorilla Compression
  ├── /metrics (Prometheus)    └── Inverted Index
  └── WebSocket Hub                │
                                   │
TCP Ingestion ─→ BatchWriter ──────┘
                                   │
Cluster Ring ──→ Coordinator ──────┘
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for detailed component documentation.

## API

```bash
# Query (rolling 15-min window by default)
curl "http://localhost:8080/api/v1/query?q=cpu_usage_percent"

# Series & labels
curl "http://localhost:8080/api/v1/series"
curl "http://localhost:8080/api/v1/labels"
curl "http://localhost:8080/api/v1/label/__name__/values"

# Storage / cluster / blocks
curl "http://localhost:8080/api/v1/stats"
curl "http://localhost:8080/api/v1/cluster"
curl "http://localhost:8080/api/v1/blocks"

# Prometheus-scrapeable self-metrics
curl "http://localhost:8080/metrics"

# Live WebSocket stream
websocat "ws://localhost:8080/ws/metrics"
```

See [PROTOCOL.md](PROTOCOL.md) for the full wire protocol.

## Configuration

Meridian reads `meridian.yaml` if present; unknown fields fall back to defaults.
Durations accept `ns`, `us`, `ms`, `s`, `m`, `h`, plus `d` (days) and `w` (weeks):

```yaml
storage:
  block_duration: "15m"   # flush head to a compressed block this often
  retention:      "15d"   # drop blocks older than this
```

## Docker

```bash
# Single node
docker build -t meridian .
docker run -p 8080:8080 -p 9090:9090 meridian

# 3-node microservices cluster (gateway + 2 ingestors + 3 storage + querier + compactor)
docker compose up --build
```

## Project Structure

```
cmd/meridian/       Monolith CLI (serve, simulate, query, bench)
cmd/{gateway,ingestor,storage,querier,compactor}/  Per-service binaries
internal/
  compress/         Gorilla encoder/decoder + benchmarks
  storage/          WAL, head block, persistent blocks, TSDB
  query/            Lexer, parser, planner, executor
  ingestion/        TCP server, batch writer
  server/           HTTP API, WebSocket hub, /metrics exporter
  cluster/          Hash ring, coordinator, node lifecycle
  retention/        TTL enforcer, downsampler
  config/           YAML configuration (with d/w duration suffixes)
  service/          Shared service-to-service RPC
simulator/          Metric generation with diurnal patterns
dashboard/          React + TypeScript + Tailwind + Canvas
```

## Design Decisions

See [DECISIONS.md](DECISIONS.md) for 13 ADRs covering key trade-offs:
Gorilla vs generic compression, sorted slices vs roaring bitmaps, JSON vs protobuf
ingestion, rAF batching for WebSocket, and more.

## Development

```bash
make test       # all tests with the race detector
make bench      # compression + query benchmarks
make vet        # static analysis
make dashboard  # build the React dashboard
make clean      # remove artifacts
```

## License

MIT
