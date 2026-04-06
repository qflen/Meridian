# Meridian

A distributed time-series database written in Go with a real-time React dashboard.

Meridian implements Facebook's Gorilla compression, a PromQL-subset query engine,
consistent-hash clustering, automatic downsampling, and a canvas-rendered 60fps
monitoring dashboard — all in a single binary with minimal dependencies.

## Quick Start

```bash
# Clone and run the demo (builds everything, starts server + simulator, opens dashboard)
./run.sh demo
```

Or step by step:

```bash
# Build
make build dashboard

# Start server
./bin/meridian serve &

# Start simulator (8 hosts, 43 metrics)
./bin/meridian simulate &

# Open dashboard
open http://localhost:8080

# Query via CLI
./bin/meridian query "rate(http_requests_total[5m])"
./bin/meridian query "avg by (host)(cpu_usage_percent)"
```

## Features

### Storage Engine
- **Gorilla compression** — delta-of-delta timestamps + XOR float encoding, 20-30x compression
- **Write-ahead log** — CRC32-framed with 128MB segment rotation
- **Inverted index** — sorted-slice intersection, no external bitmap dependencies
- **Block storage** — ULID-named persistent blocks with binary index

### Query Engine
- **PromQL subset** — recursive descent parser
- **Selectors** — vector, range, with label matchers (=, !=, =~, !~)
- **Functions** — rate(), histogram_quantile()
- **Aggregations** — sum, avg, min, max, count, topk, bottomk with by/without
- **Binary ops** — +, -, *, /, with operator precedence

### Cluster
- **Consistent hash ring** — SHA256 with virtual nodes
- **Configurable replication** — writes to N nodes
- **Node lifecycle** — joining → active → leaving → dead

### Dashboard
- **Canvas-rendered** — 60fps charts with zero chart library dependencies
- **10 components** — query editor, time-series chart, metric explorer, cluster topology, ingestion monitor, compression stats, latency histogram, retention timeline, live stream, theme toggle
- **Real-time** — WebSocket streaming with requestAnimationFrame batching
- **Themes** — dark, light, high-contrast

### Operations
- **Retention enforcement** — TTL-based block deletion
- **Downsampling** — 5s → 1m → 1h cascade with min/max/avg/sum/count
- **Simulator** — diurnal patterns, spike injection, memory drift across 8 hosts

## Architecture

```
HTTP/WS ─→ Query Engine ─→ TSDB (Head + Blocks)
  │                            │
  ├── Dashboard (React)        ├── WAL (CRC32)
  ├── REST API                 ├── Gorilla Compression
  └── WebSocket Hub            └── Inverted Index
                                   │
TCP Ingestion ─→ BatchWriter ──────┘
                                   │
Cluster Ring ──→ Coordinator ──────┘
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for detailed component documentation.

## API

```bash
# Query
curl "http://localhost:8080/api/query?q=cpu_usage_percent&format=json"

# Labels
curl "http://localhost:8080/api/labels"

# Health
curl "http://localhost:8080/health"
```

See [PROTOCOL.md](PROTOCOL.md) for full wire protocol documentation.

## Docker

```bash
# Single node
docker build -t meridian .
docker run -p 8080:8080 -p 9090:9090 meridian

# 3-node cluster
docker compose up --build
```

## Project Structure

```
cmd/meridian/       CLI entry points (serve, simulate, query, bench)
internal/
  compress/         Gorilla encoder/decoder
  storage/          WAL, head block, persistent blocks, TSDB
  query/            Lexer, parser, planner, executor
  ingestion/        TCP server, batch writer
  server/           HTTP API, WebSocket hub
  cluster/          Hash ring, coordinator, node lifecycle
  retention/        TTL enforcer, downsampler
  config/           YAML configuration
simulator/          Metric generation with diurnal patterns
dashboard/          React + TypeScript + Tailwind + Canvas
```

## Design Decisions

See [DECISIONS.md](DECISIONS.md) for 13 Architecture Decision Records covering
key trade-offs: Gorilla vs generic compression, sorted slices vs roaring bitmaps,
JSON vs protobuf ingestion, rAF batching for WebSocket, and more.

## Performance

See [PERFORMANCE.md](PERFORMANCE.md) for compression ratios, throughput
benchmarks, query latency, and memory usage characteristics.

## Development

```bash
make test       # Run all tests with race detector
make bench      # Run benchmarks
make vet        # Go vet
make clean      # Remove artifacts
make dashboard  # Build dashboard
```

## License

MIT
