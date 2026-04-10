// Package config provides YAML-based configuration for Meridian nodes.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

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
	Log          LogConfig          `yaml:"log"`
}

// ServerConfig holds HTTP and gRPC listen addresses.
type ServerConfig struct {
	HTTPAddr string `yaml:"http_addr"`
	GRPCAddr string `yaml:"grpc_addr"`
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
type ClusterConfig struct {
	Enabled           bool     `yaml:"enabled"`
	NodeID            string   `yaml:"node_id"`
	BindAddr          string   `yaml:"bind_addr"`
	Join              []string `yaml:"join"`
	ReplicationFactor int      `yaml:"replication_factor"`
	VirtualNodes      int      `yaml:"virtual_nodes"`
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

// IngestionConfig holds batch writer parameters.
type IngestionConfig struct {
	BatchSize           int      `yaml:"batch_size"`
	FlushInterval       Duration `yaml:"flush_interval"`
	MaxConcurrentWrites int      `yaml:"max_concurrent_writes"`
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
			HTTPAddr: "0.0.0.0:8080",
			GRPCAddr: "0.0.0.0:9090",
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

	return cfg, nil
}
