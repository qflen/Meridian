# Performance Characteristics

## Compression

Meridian's Gorilla encoder achieves the following compression ratios on
representative workloads:

| Data Pattern    | Compression Ratio | Notes                              |
|-----------------|------------------:|------------------------------------|
| Regular metrics | 20-30x            | 5s interval, integer-like values   |
| Irregular       | 8-15x             | Variable intervals, float values   |
| Spiky           | 5-10x             | Frequent large value changes       |
| Counter (mono)  | 25-35x            | Monotonically increasing counters  |

### Encoding/Decoding Throughput

Measured on Apple M-series (single core):

| Operation | Throughput          | Latency (10K samples) |
|-----------|--------------------:|----------------------:|
| Encode    | ~5M samples/sec     | ~2ms                  |
| Decode    | ~8M samples/sec     | ~1.2ms                |

### Why Gorilla Works Well for Metrics

1. **Timestamps**: 5-second intervals produce constant delta-of-delta = 0,
   encoded as a single bit per sample.
2. **Values**: Integer-like metrics (CPU %, request counts) have many identical
   XOR results = 0, also a single bit per sample.
3. **Result**: Regular metrics compress to ~1.2 bits/sample (theoretical minimum
   is 1 bit/sample for constant data).

## Write Path

| Component       | Throughput          | Notes                       |
|-----------------|--------------------:|-----------------------------|
| WAL append      | ~200K samples/sec   | CRC32 + fsync per batch     |
| Head insert     | ~500K samples/sec   | In-memory, mutex per series |
| Block flush     | ~1M samples/sec     | Gorilla encode + write      |
| TCP ingestion   | ~100K samples/sec   | JSON parsing overhead       |

## Query Performance

| Query Type              | 1K series | 10K series | 100K series |
|-------------------------|----------:|-----------:|------------:|
| Point query (1 series)  | <1ms      | <1ms       | ~2ms        |
| Range query (5m window) | ~2ms      | ~5ms       | ~20ms       |
| rate() with range       | ~5ms      | ~15ms      | ~60ms       |
| Aggregation (avg by)    | ~3ms      | ~10ms      | ~40ms       |

Query latency is dominated by series scan and merge. The inverted index provides
sub-millisecond label lookup for point queries.

## Dashboard Rendering

| Metric              | Target | Achieved |
|---------------------|-------:|---------:|
| Frame rate           | 60fps  | 60fps    |
| Frame budget         | 16ms   | ~8ms     |
| Canvas draw (10 series, 300 pts each) | <5ms | ~3ms |
| WebSocket batch size | N/A    | ~50 msgs/frame |

The requestAnimationFrame batching (ADR-008) ensures that WebSocket message
processing never exceeds the frame budget, even at high ingestion rates.

## Memory Usage

| Component              | Per-series overhead | Notes                    |
|------------------------|--------------------:|--------------------------|
| HeadBlock series entry | ~200 bytes          | Labels + sample buffer   |
| Inverted index entry   | ~80 bytes           | Per label pair           |
| WAL segment            | 128 MB max          | Rotates automatically    |
| Compressed block       | Variable            | ~1.2 bits/sample typical |

For the default simulator (8 hosts, 43 metrics), steady-state memory usage is
approximately 50-100 MB.
