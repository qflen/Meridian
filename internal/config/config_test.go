package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"0s", 0},
		{"500ms", 500 * time.Millisecond},
		{"30s", 30 * time.Second},
		{"15m", 15 * time.Minute},
		{"2h", 2 * time.Hour},
		{"1d", 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"30d", 30 * 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
		{"1d12h", 36 * time.Hour},
		{"1w1d", 8 * 24 * time.Hour},
		{"2h30m", 2*time.Hour + 30*time.Minute},
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if err != nil {
			t.Errorf("ParseDuration(%q) returned error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseDurationErrors(t *testing.T) {
	bad := []string{"", "abc", "10", "d", "-", "1.5d"}
	for _, s := range bad {
		if _, err := ParseDuration(s); err == nil {
			t.Errorf("ParseDuration(%q) should have errored", s)
		}
	}
}

func TestClusterConfigValidate(t *testing.T) {
	valid := []ClusterConfig{
		{ReplicationFactor: 3, WriteQuorum: 2, ReadQuorum: 2, VirtualNodes: 256}, // default
		{ReplicationFactor: 1, WriteQuorum: 1, ReadQuorum: 1, VirtualNodes: 64},   // single node
		{ReplicationFactor: 3, WriteQuorum: 3, ReadQuorum: 1, VirtualNodes: 8},    // write-all/read-one
		{ReplicationFactor: 5, WriteQuorum: 3, ReadQuorum: 3, VirtualNodes: 256},
	}
	for _, c := range valid {
		if err := c.Validate(); err != nil {
			t.Errorf("expected %+v to be valid, got %v", c, err)
		}
	}

	invalid := []ClusterConfig{
		{ReplicationFactor: 0, WriteQuorum: 1, ReadQuorum: 1, VirtualNodes: 1},  // N<1
		{ReplicationFactor: 3, WriteQuorum: 0, ReadQuorum: 2, VirtualNodes: 1},  // W<1
		{ReplicationFactor: 3, WriteQuorum: 4, ReadQuorum: 2, VirtualNodes: 1},  // W>N
		{ReplicationFactor: 3, WriteQuorum: 2, ReadQuorum: 4, VirtualNodes: 1},  // R>N
		{ReplicationFactor: 3, WriteQuorum: 1, ReadQuorum: 2, VirtualNodes: 1},  // W+R == N (no read-your-writes)
		{ReplicationFactor: 4, WriteQuorum: 2, ReadQuorum: 2, VirtualNodes: 1},  // W+R == N
		{ReplicationFactor: 3, WriteQuorum: 2, ReadQuorum: 2, VirtualNodes: 0},  // virtual nodes < 1
	}
	for _, c := range invalid {
		if err := c.Validate(); err == nil {
			t.Errorf("expected %+v to be invalid", c)
		}
	}
}

func TestDefaultConfigClusterValid(t *testing.T) {
	if err := DefaultConfig().Cluster.Validate(); err != nil {
		t.Fatalf("default cluster config must validate: %v", err)
	}
}

func TestLoadRejectsBadQuorum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meridian.yaml")
	yaml := `
cluster:
  replication_factor: 3
  write_quorum: 1
  read_quorum: 1
  virtual_nodes: 256
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load should reject W+R <= N (1+1 <= 3)")
	}
}

func TestLoadYAMLWithDayWeekSuffix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meridian.yaml")
	yaml := `
storage:
  data_dir: "./data"
  wal_dir: "./data/wal"
  block_duration: "15m"
  retention: "30d"
  flush_interval: "30s"

downsampling:
  rules:
    - source_interval: "5s"
      target_interval: "1m"
      retention: "1w"
    - source_interval: "1m"
      target_interval: "1h"
      retention: "30d"
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Storage.Retention.Std() != 30*24*time.Hour {
		t.Errorf("retention: got %v, want 30d", cfg.Storage.Retention.Std())
	}
	if cfg.Storage.BlockDuration.Std() != 15*time.Minute {
		t.Errorf("block_duration: got %v, want 15m", cfg.Storage.BlockDuration.Std())
	}
	if len(cfg.Downsampling.Rules) != 2 {
		t.Fatalf("downsampling rules: got %d, want 2", len(cfg.Downsampling.Rules))
	}
	if cfg.Downsampling.Rules[0].Retention.Std() != 7*24*time.Hour {
		t.Errorf("rule[0].retention: got %v, want 1w", cfg.Downsampling.Rules[0].Retention.Std())
	}
}
