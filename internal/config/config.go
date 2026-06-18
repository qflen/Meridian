// Package config provides YAML-based configuration for Meridian nodes.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/meridiandb/meridian/internal/anomaly"
	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration with YAML parsing that accepts "d" (days) and "w" (weeks)
// suffixes in addition to the units Go's time.ParseDuration supports.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	dur, err := ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", n.Value, err)
	}
	*d = Duration(dur)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (interface{}, error) {
	return time.Duration(d).String(), nil
}

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// ParseDuration parses a duration string. In addition to the units accepted by
// time.ParseDuration (ns, us, ms, s, m, h) it also supports "d" (days) and "w" (weeks),
// and compound forms like "1d12h".
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}

	// Fast path: no d/w suffix anywhere — delegate to stdlib.
	if !strings.ContainsAny(s, "dw") {
		return time.ParseDuration(s)
	}

	// Walk the string splitting on "d" and "w" boundaries, converting each
	// prefix into hours, and delegate the remainder to time.ParseDuration.
	var total time.Duration
	i := 0
	for i < len(s) {
		// Scan an integer.
		j := i
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i || (j == i+1 && (s[i] == '+' || s[i] == '-')) {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		n, err := strconv.Atoi(s[i:j])
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", s, err)
		}
		if j >= len(s) {
			return 0, fmt.Errorf("invalid duration %q: missing unit", s)
		}
		switch s[j] {
		case 'd':
			total += time.Duration(n) * 24 * time.Hour
			i = j + 1
		case 'w':
			total += time.Duration(n) * 7 * 24 * time.Hour
			i = j + 1
		default:
			// Hit a stdlib-supported unit — hand the rest (including this number) to ParseDuration.
			rest := s[i:]
			d, err := time.ParseDuration(rest)
			if err != nil {
				return 0, err
			}
			return total + d, nil
		}
	}
	return total, nil
}

// Config represents the top-level Meridian configuration.
type Config struct {
	Server       ServerConfig       `yaml:"server"`
	Storage      StorageConfig      `yaml:"storage"`
	Cluster      ClusterConfig      `yaml:"cluster"`
	Downsampling DownsamplingConfig `yaml:"downsampling"`
	Ingestion    IngestionConfig    `yaml:"ingestion"`
	Anomaly      AnomalyConfig      `yaml:"anomaly"`
	Log          LogConfig          `yaml:"log"`
}

// ServerConfig holds HTTP and gRPC listen addresses plus HTTP API limits.
type ServerConfig struct {
	HTTPAddr string `yaml:"http_addr"`
	GRPCAddr string `yaml:"grpc_addr"`
	// QueryTimeout bounds how long a single /api/v1/query may run. Zero leaves the
	// server default (30s) in place.
	QueryTimeout Duration `yaml:"query_timeout"`
	// AllowedOrigins is the CORS allow-list for the HTTP API. Empty (the default)
	// permits only localhost origins; a single "*" entry permits all origins.
	AllowedOrigins []string `yaml:"allowed_origins"`
}

// StorageConfig holds storage engine parameters.
type StorageConfig struct {
	DataDir       string   `yaml:"data_dir"`
	WALDir        string   `yaml:"wal_dir"`
	BlockDuration Duration `yaml:"block_duration"`
	Retention     Duration `yaml:"retention"`
	FlushInterval Duration `yaml:"flush_interval"`
}

// ClusterConfig holds cluster membership and replication settings.
//
// Replication follows a quorum model: a series is written to the N
// (ReplicationFactor) ring replicas and succeeds once WriteQuorum acks; reads take
// ReadQuorum responses and merge. Choosing WriteQuorum+ReadQuorum > ReplicationFactor
// guarantees read-your-writes (the write and read sets always overlap). See ADR-022.
type ClusterConfig struct {
	Enabled           bool     `yaml:"enabled"`
	NodeID            string   `yaml:"node_id"`
	BindAddr          string   `yaml:"bind_addr"`
	Join              []string `yaml:"join"`
	ReplicationFactor int      `yaml:"replication_factor"`
	// WriteQuorum (W) is the number of replica acks required for a write to succeed.
	WriteQuorum int `yaml:"write_quorum"`
	// ReadQuorum (R) is the number of replicas a read must hear from.
	ReadQuorum   int `yaml:"read_quorum"`
	VirtualNodes int `yaml:"virtual_nodes"`
}

// Validate checks the replication parameters are internally consistent. It enforces
// 1 <= W,R <= N and the read-your-writes condition W+R > N, so a misconfiguration is
// caught at load time rather than silently weakening consistency at runtime.
func (c ClusterConfig) Validate() error {
	n, w, r := c.ReplicationFactor, c.WriteQuorum, c.ReadQuorum
	if n < 1 {
		return fmt.Errorf("cluster.replication_factor must be >= 1, got %d", n)
	}
	if w < 1 || w > n {
		return fmt.Errorf("cluster.write_quorum must be in [1,%d], got %d", n, w)
	}
	if r < 1 || r > n {
		return fmt.Errorf("cluster.read_quorum must be in [1,%d], got %d", n, r)
	}
	if w+r <= n {
		return fmt.Errorf("cluster.write_quorum + cluster.read_quorum must exceed replication_factor for read-your-writes (W=%d + R=%d <= N=%d)", w, r, n)
	}
	if c.VirtualNodes < 1 {
		return fmt.Errorf("cluster.virtual_nodes must be >= 1, got %d", c.VirtualNodes)
	}
	return nil
}

// DownsamplingConfig holds automatic rollup rules.
type DownsamplingConfig struct {
	Rules []DownsamplingRule `yaml:"rules"`
}

// DownsamplingRule defines a single rollup rule.
type DownsamplingRule struct {
	SourceInterval Duration `yaml:"source_interval"`
	TargetInterval Duration `yaml:"target_interval"`
	Retention      Duration `yaml:"retention"`
}

// IngestionConfig holds batch writer and write-path flow-control parameters.
//
// The ingest path is a bounded queue between accept and the drain-to-storage
// worker (ADR-023). QueueCapacity bounds the samples that may sit in the queue,
// so it caps resident memory under overload. When the queue is full a producer
// blocks up to BlockDeadline (the backpressure); past it, the batch is shed —
// dropped, counted, and the producer NACKed (HTTP 429 / TCP NACK) — rather than
// buffered without bound. QueueHighWatermark is the depth at which producers are
// flagged to throttle before shedding begins.
type IngestionConfig struct {
	BatchSize     int      `yaml:"batch_size"`
	FlushInterval Duration `yaml:"flush_interval"`
	// MaxConcurrentWrites bounds the worker pool that drains the service ingest
	// queue to storage (ingestor) or the local TSDB (storage node).
	MaxConcurrentWrites int `yaml:"max_concurrent_writes"`
	// QueueCapacity is the bounded ingest queue size in samples (the hard memory
	// cap). It must be at least BatchSize so a single batch can be enqueued.
	QueueCapacity int `yaml:"queue_capacity"`
	// QueueHighWatermark is the queue depth in samples at or above which producers
	// are flagged to throttle (early backpressure). It must be in [1, QueueCapacity].
	QueueHighWatermark int `yaml:"queue_high_watermark"`
	// BlockDeadline is how long an enqueue blocks against a full queue before the
	// batch is shed. Zero makes enqueue non-blocking (shed immediately when full).
	BlockDeadline Duration `yaml:"block_deadline"`
}

// Validate checks the ingestion flow-control parameters are internally consistent
// so a misconfiguration is caught at load time rather than producing a queue that
// can never accept a batch or a watermark above the cap.
func (c IngestionConfig) Validate() error {
	if c.BatchSize < 1 {
		return fmt.Errorf("ingestion.batch_size must be >= 1, got %d", c.BatchSize)
	}
	if c.MaxConcurrentWrites < 1 {
		return fmt.Errorf("ingestion.max_concurrent_writes must be >= 1, got %d", c.MaxConcurrentWrites)
	}
	if c.QueueCapacity < c.BatchSize {
		return fmt.Errorf("ingestion.queue_capacity (%d) must be >= batch_size (%d) so a batch can enqueue", c.QueueCapacity, c.BatchSize)
	}
	if c.QueueHighWatermark < 1 || c.QueueHighWatermark > c.QueueCapacity {
		return fmt.Errorf("ingestion.queue_high_watermark must be in [1,%d], got %d", c.QueueCapacity, c.QueueHighWatermark)
	}
	if c.BlockDeadline < 0 {
		return fmt.Errorf("ingestion.block_deadline must be >= 0, got %s", time.Duration(c.BlockDeadline))
	}
	return nil
}

// AnomalyConfig tunes the streaming anomaly detector that runs over the live
// telemetry path (ADR-024). The detector tracks an EWMA baseline + dispersion per
// series and flags points whose local z-score |value-baseline|/dispersion exceeds
// Threshold; the remaining robustness knobs (Huber clamp, scale floor, hysteresis)
// take internal defaults.
type AnomalyConfig struct {
	// Enabled turns the detector on. When false the broadcast loop feeds it nothing
	// and no anomaly frames or metrics are produced.
	Enabled bool `yaml:"enabled"`
	// Threshold is the local z-score above which a sample is out-of-band (~3–4).
	Threshold float64 `yaml:"threshold"`
	// Alpha is the EWMA smoothing factor in (0,1] for the baseline and dispersion.
	Alpha float64 `yaml:"alpha"`
	// Warmup is the number of samples used to seed a per-series baseline before any
	// alert may fire (>= 2).
	Warmup int `yaml:"warmup"`
	// DebounceK is the consecutive out-of-band samples required to raise an alert (>= 1).
	DebounceK int `yaml:"debounce_k"`
}

// Validate checks the anomaly tunables when the detector is enabled, so a
// misconfiguration is caught at load time rather than producing a detector that
// never warms up or alerts on every point.
func (c AnomalyConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Threshold <= 0 {
		return fmt.Errorf("anomaly.threshold must be > 0, got %g", c.Threshold)
	}
	if c.Alpha <= 0 || c.Alpha > 1 {
		return fmt.Errorf("anomaly.alpha must be in (0,1], got %g", c.Alpha)
	}
	if c.Warmup < 2 {
		return fmt.Errorf("anomaly.warmup must be >= 2, got %d", c.Warmup)
	}
	if c.DebounceK < 1 {
		return fmt.Errorf("anomaly.debounce_k must be >= 1, got %d", c.DebounceK)
	}
	return nil
}

// Detector maps the YAML-exposed tunables onto an anomaly.Config, leaving the
// detector's internal robustness defaults (clip/clear/crit/floor/buffer) in place.
func (c AnomalyConfig) Detector() anomaly.Config {
	base := anomaly.DefaultConfig()
	base.Enabled = c.Enabled
	base.Threshold = c.Threshold
	base.Alpha = c.Alpha
	base.Warmup = c.Warmup
	base.DebounceK = c.DebounceK
	return base
}

// LogConfig holds logging parameters.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			HTTPAddr:     "0.0.0.0:8080",
			GRPCAddr:     "0.0.0.0:9090",
			QueryTimeout: Duration(30 * time.Second),
		},
		Storage: StorageConfig{
			DataDir:       "./data",
			WALDir:        "./data/wal",
			BlockDuration: Duration(15 * time.Minute),
			Retention:     Duration(15 * 24 * time.Hour),
			FlushInterval: Duration(30 * time.Second),
		},
		Cluster: ClusterConfig{
			Enabled:           false,
			BindAddr:          "0.0.0.0:7946",
			ReplicationFactor: 3,
			WriteQuorum:       2,
			ReadQuorum:        2,
			VirtualNodes:      256,
		},
		Downsampling: DownsamplingConfig{
			Rules: []DownsamplingRule{
				{SourceInterval: Duration(5 * time.Second), TargetInterval: Duration(1 * time.Minute), Retention: Duration(7 * 24 * time.Hour)},
				{SourceInterval: Duration(1 * time.Minute), TargetInterval: Duration(1 * time.Hour), Retention: Duration(30 * 24 * time.Hour)},
			},
		},
		Ingestion: IngestionConfig{
			BatchSize:           1000,
			FlushInterval:       Duration(100 * time.Millisecond),
			MaxConcurrentWrites: 64,
			QueueCapacity:       50000,
			QueueHighWatermark:  40000,
			BlockDeadline:       Duration(250 * time.Millisecond),
		},
		Anomaly: AnomalyConfig{
			Enabled:   true,
			Threshold: 3.5,
			Alpha:     0.1,
			Warmup:    20,
			DebounceK: 3,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
		},
	}
}

// Load reads a YAML configuration file and returns the parsed Config.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	if err := cfg.Cluster.Validate(); err != nil {
		return nil, err
	}
	if err := cfg.Ingestion.Validate(); err != nil {
		return nil, err
	}
	if err := cfg.Anomaly.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
