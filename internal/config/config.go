// Package config provides YAML-based configuration for Meridian nodes.
package config

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

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
	DataDir       string        `yaml:"data_dir"`
	WALDir        string        `yaml:"wal_dir"`
	BlockDuration time.Duration `yaml:"block_duration"`
	Retention     time.Duration `yaml:"retention"`
	FlushInterval time.Duration `yaml:"flush_interval"`
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
	SourceInterval time.Duration `yaml:"source_interval"`
	TargetInterval time.Duration `yaml:"target_interval"`
	Retention      time.Duration `yaml:"retention"`
}

// IngestionConfig holds batch writer parameters.
type IngestionConfig struct {
	BatchSize           int           `yaml:"batch_size"`
	FlushInterval       time.Duration `yaml:"flush_interval"`
	MaxConcurrentWrites int           `yaml:"max_concurrent_writes"`
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
			BlockDuration: 2 * time.Hour,
			Retention:     15 * 24 * time.Hour,
			FlushInterval: 1 * time.Minute,
		},
		Cluster: ClusterConfig{
			Enabled:           false,
			BindAddr:          "0.0.0.0:7946",
			ReplicationFactor: 3,
			VirtualNodes:      256,
		},
		Downsampling: DownsamplingConfig{
			Rules: []DownsamplingRule{
				{SourceInterval: 5 * time.Second, TargetInterval: 1 * time.Minute, Retention: 7 * 24 * time.Hour},
				{SourceInterval: 1 * time.Minute, TargetInterval: 1 * time.Hour, Retention: 30 * 24 * time.Hour},
			},
		},
		Ingestion: IngestionConfig{
			BatchSize:           1000,
			FlushInterval:       100 * time.Millisecond,
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
