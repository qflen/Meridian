package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/anomaly"
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
		{ReplicationFactor: 1, WriteQuorum: 1, ReadQuorum: 1, VirtualNodes: 64},  // single node
		{ReplicationFactor: 3, WriteQuorum: 3, ReadQuorum: 1, VirtualNodes: 8},   // write-all/read-one
		{ReplicationFactor: 5, WriteQuorum: 3, ReadQuorum: 3, VirtualNodes: 256},
	}
	for _, c := range valid {
		if err := c.Validate(); err != nil {
			t.Errorf("expected %+v to be valid, got %v", c, err)
		}
	}

	invalid := []ClusterConfig{
		{ReplicationFactor: 0, WriteQuorum: 1, ReadQuorum: 1, VirtualNodes: 1}, // N<1
		{ReplicationFactor: 3, WriteQuorum: 0, ReadQuorum: 2, VirtualNodes: 1}, // W<1
		{ReplicationFactor: 3, WriteQuorum: 4, ReadQuorum: 2, VirtualNodes: 1}, // W>N
		{ReplicationFactor: 3, WriteQuorum: 2, ReadQuorum: 4, VirtualNodes: 1}, // R>N
		{ReplicationFactor: 3, WriteQuorum: 1, ReadQuorum: 2, VirtualNodes: 1}, // W+R == N (no read-your-writes)
		{ReplicationFactor: 4, WriteQuorum: 2, ReadQuorum: 2, VirtualNodes: 1}, // W+R == N
		{ReplicationFactor: 3, WriteQuorum: 2, ReadQuorum: 2, VirtualNodes: 0}, // virtual nodes < 1
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

func TestHandoffConfigValidate(t *testing.T) {
	valid := []HandoffConfig{
		{Enabled: false}, // disabled needs no tunables
		{Enabled: true, MaxSamplesPerNode: 1, ReplayInterval: 0},
		{Enabled: true, MaxSamplesPerNode: 1_000_000, ReplayInterval: Duration(5 * time.Second)},
	}
	for _, c := range valid {
		if err := c.Validate(); err != nil {
			t.Errorf("expected %+v to be valid, got %v", c, err)
		}
	}

	invalid := []HandoffConfig{
		{Enabled: true, MaxSamplesPerNode: 0},                                          // cap < 1
		{Enabled: true, MaxSamplesPerNode: 10, ReplayInterval: Duration(-time.Second)}, // negative interval
	}
	for _, c := range invalid {
		if err := c.Validate(); err == nil {
			t.Errorf("expected %+v to be invalid", c)
		}
	}

	// A bad handoff block must fail cluster validation (it is nested).
	cc := ClusterConfig{ReplicationFactor: 3, WriteQuorum: 2, ReadQuorum: 2, VirtualNodes: 256,
		Handoff: HandoffConfig{Enabled: true, MaxSamplesPerNode: 0}}
	if err := cc.Validate(); err == nil {
		t.Error("cluster validate must reject an invalid nested handoff config")
	}
}

func TestAntiEntropyConfigValidate(t *testing.T) {
	valid := []AntiEntropyConfig{
		{Enabled: false}, // disabled needs no tunables
		{Enabled: true, Interval: Duration(30 * time.Second), Window: Duration(time.Hour), GroupsPerRound: 1},
		DefaultConfig().Cluster.AntiEntropy, // the shipped default must be valid
	}
	for _, c := range valid {
		if err := c.Validate(); err != nil {
			t.Errorf("expected %+v to be valid, got %v", c, err)
		}
	}

	base := DefaultConfig().Cluster.AntiEntropy
	invalid := []AntiEntropyConfig{
		{Enabled: true, Window: Duration(time.Hour), GroupsPerRound: 1},        // interval <= 0
		{Enabled: true, Interval: Duration(time.Second), GroupsPerRound: 1},    // window <= 0
		{Enabled: true, Interval: Duration(time.Second), Window: Duration(time.Hour)}, // groups < 1
		func() AntiEntropyConfig { c := base; c.Lookback = Duration(-time.Second); return c }(), // negative lookback
		func() AntiEntropyConfig { c := base; c.Jitter = Duration(-time.Second); return c }(),   // negative jitter
	}
	for _, c := range invalid {
		if err := c.Validate(); err == nil {
			t.Errorf("expected %+v to be invalid", c)
		}
	}

	// A bad anti-entropy block must fail cluster validation (it is nested).
	cc := ClusterConfig{ReplicationFactor: 3, WriteQuorum: 2, ReadQuorum: 2, VirtualNodes: 256,
		AntiEntropy: AntiEntropyConfig{Enabled: true, Interval: 0, Window: Duration(time.Hour), GroupsPerRound: 1}}
	if err := cc.Validate(); err == nil {
		t.Error("cluster validate must reject an invalid nested anti-entropy config")
	}
}

func TestRebalanceConfigValidate(t *testing.T) {
	valid := []RebalanceConfig{
		{Enabled: false},                                              // disabled needs no tunables
		{Enabled: false, Lookback: Duration(-time.Second)},            // disabled: tunables ignored
		{Enabled: true},                                               // zero lookback (all history) + unlimited bytes
		{Enabled: true, Lookback: Duration(time.Hour), MaxBytesPerRound: 1 << 20},
		DefaultConfig().Cluster.Rebalance, // the shipped default must be valid
	}
	for _, c := range valid {
		if err := c.Validate(); err != nil {
			t.Errorf("expected %+v to be valid, got %v", c, err)
		}
	}

	invalid := []RebalanceConfig{
		{Enabled: true, Lookback: Duration(-time.Second)}, // negative lookback
		{Enabled: true, MaxBytesPerRound: -1},             // negative byte cap
	}
	for _, c := range invalid {
		if err := c.Validate(); err == nil {
			t.Errorf("expected %+v to be invalid", c)
		}
	}

	// A bad rebalance block must fail cluster validation (it is nested).
	cc := ClusterConfig{ReplicationFactor: 3, WriteQuorum: 2, ReadQuorum: 2, VirtualNodes: 256,
		Rebalance: RebalanceConfig{Enabled: true, MaxBytesPerRound: -1}}
	if err := cc.Validate(); err == nil {
		t.Error("cluster validate must reject an invalid nested rebalance config")
	}
}

func TestIngestionConfigValidate(t *testing.T) {
	valid := []IngestionConfig{
		{BatchSize: 1000, MaxConcurrentWrites: 64, QueueCapacity: 50000, QueueHighWatermark: 40000, BlockDeadline: Duration(250 * time.Millisecond)},
		{BatchSize: 1, MaxConcurrentWrites: 1, QueueCapacity: 1, QueueHighWatermark: 1, BlockDeadline: 0}, // minimal, non-blocking
	}
	for _, c := range valid {
		if err := c.Validate(); err != nil {
			t.Errorf("expected %+v to be valid, got %v", c, err)
		}
	}

	invalid := []IngestionConfig{
		{BatchSize: 0, MaxConcurrentWrites: 1, QueueCapacity: 10, QueueHighWatermark: 5},                                              // batch_size < 1
		{BatchSize: 1000, MaxConcurrentWrites: 0, QueueCapacity: 10000, QueueHighWatermark: 5},                                        // workers < 1
		{BatchSize: 1000, MaxConcurrentWrites: 1, QueueCapacity: 500, QueueHighWatermark: 400},                                        // capacity < batch_size
		{BatchSize: 100, MaxConcurrentWrites: 1, QueueCapacity: 1000, QueueHighWatermark: 0},                                          // hw < 1
		{BatchSize: 100, MaxConcurrentWrites: 1, QueueCapacity: 1000, QueueHighWatermark: 2000},                                       // hw > capacity
		{BatchSize: 100, MaxConcurrentWrites: 1, QueueCapacity: 1000, QueueHighWatermark: 500, BlockDeadline: Duration(-time.Second)}, // negative deadline
	}
	for _, c := range invalid {
		if err := c.Validate(); err == nil {
			t.Errorf("expected %+v to be invalid", c)
		}
	}
}

func TestDefaultConfigIngestionValid(t *testing.T) {
	if err := DefaultConfig().Ingestion.Validate(); err != nil {
		t.Fatalf("default ingestion config must validate: %v", err)
	}
}

func TestAnomalyConfigValidate(t *testing.T) {
	valid := []AnomalyConfig{
		{Enabled: true, Threshold: 3.5, Alpha: 0.1, Warmup: 20, DebounceK: 3},
		{Enabled: true, Threshold: 0.5, Alpha: 1, Warmup: 2, DebounceK: 1}, // boundary values
		{Enabled: false}, // tunables irrelevant when disabled
		{Enabled: true, Threshold: 3.5, Alpha: 0.1, Warmup: 20, DebounceK: 2, Mode: "ewma"},
		// holt_winters with explicit and with defaulted season parameters.
		{Enabled: true, Threshold: 3.5, Alpha: 0.1, Warmup: 20, DebounceK: 2, Mode: "holt_winters", SeasonLength: 48, SeasonPeriod: Duration(24 * time.Hour)},
		{Enabled: true, Threshold: 3.5, Alpha: 0.1, Warmup: 20, DebounceK: 2, Mode: "holt_winters"},
	}
	for _, c := range valid {
		if err := c.Validate(); err != nil {
			t.Errorf("expected %+v to be valid, got %v", c, err)
		}
	}

	invalid := []AnomalyConfig{
		{Enabled: true, Threshold: 0, Alpha: 0.1, Warmup: 20, DebounceK: 3},   // threshold <= 0
		{Enabled: true, Threshold: 3.5, Alpha: 0, Warmup: 20, DebounceK: 3},   // alpha <= 0
		{Enabled: true, Threshold: 3.5, Alpha: 1.5, Warmup: 20, DebounceK: 3}, // alpha > 1
		{Enabled: true, Threshold: 3.5, Alpha: 0.1, Warmup: 1, DebounceK: 3},  // warmup < 2
		{Enabled: true, Threshold: 3.5, Alpha: 0.1, Warmup: 20, DebounceK: 0}, // debounce < 1
		{Enabled: true, Threshold: 3.5, Alpha: 0.1, Warmup: 20, DebounceK: 2, Mode: "bogus"},                              // unknown mode
		{Enabled: true, Threshold: 3.5, Alpha: 0.1, Warmup: 20, DebounceK: 2, Mode: "holt_winters", SeasonLength: 1},      // season_length < 2
		{Enabled: true, Threshold: 3.5, Alpha: 0.1, Warmup: 20, DebounceK: 2, Mode: "holt_winters", SeasonPeriod: Duration(-time.Second)}, // negative period
	}
	for _, c := range invalid {
		if err := c.Validate(); err == nil {
			t.Errorf("expected %+v to be invalid", c)
		}
	}
}

func TestDefaultConfigAnomalyValid(t *testing.T) {
	if err := DefaultConfig().Anomaly.Validate(); err != nil {
		t.Fatalf("default anomaly config must validate: %v", err)
	}
	// The mapping onto the detector config preserves the YAML-exposed tunables and
	// defaults to the EWMA model.
	d := DefaultConfig().Anomaly.Detector()
	if !d.Enabled || d.Threshold != 3.5 || d.Alpha != 0.1 || d.Warmup != 20 || d.DebounceK != 2 {
		t.Fatalf("detector mapping lost tunables: %+v", d)
	}
	if d.Mode != anomaly.ModeEWMA {
		t.Fatalf("default mode should be EWMA, got %q", d.Mode)
	}
}

func TestAnomalyHoltWintersMapping(t *testing.T) {
	c := AnomalyConfig{
		Enabled: true, Threshold: 3.5, Alpha: 0.1, Warmup: 20, DebounceK: 2,
		Mode: "holt_winters", SeasonLength: 24, SeasonPeriod: Duration(12 * time.Hour),
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("holt_winters config should validate: %v", err)
	}
	d := c.Detector()
	if d.Mode != anomaly.ModeHoltWinters {
		t.Errorf("mode not mapped onto the detector: got %q", d.Mode)
	}
	if d.SeasonLength != 24 {
		t.Errorf("season_length = %d, want 24", d.SeasonLength)
	}
	if want := (12 * time.Hour).Milliseconds(); d.SeasonPeriodMs != want {
		t.Errorf("season_period_ms = %d, want %d", d.SeasonPeriodMs, want)
	}

	// Unset season parameters fall back to the detector's internal defaults (non-zero),
	// so a minimal holt_winters config is still complete.
	bare := AnomalyConfig{Enabled: true, Threshold: 3.5, Alpha: 0.1, Warmup: 20, DebounceK: 2, Mode: "holt_winters"}.Detector()
	if bare.SeasonLength <= 0 || bare.SeasonPeriodMs <= 0 {
		t.Errorf("unset season params should default, got length=%d periodMs=%d", bare.SeasonLength, bare.SeasonPeriodMs)
	}
}

func TestLoadParsesHoltWintersAnomaly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meridian.yaml")
	yaml := `
anomaly:
  enabled: true
  threshold: 4
  alpha: 0.2
  warmup: 30
  debounce_k: 3
  mode: holt_winters
  season_length: 24
  season_period: 12h
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load should accept a valid holt_winters config: %v", err)
	}
	a := cfg.Anomaly
	if a.Mode != "holt_winters" || a.SeasonLength != 24 || a.SeasonPeriod.Std() != 12*time.Hour {
		t.Fatalf("holt_winters anomaly fields not parsed from YAML: %+v", a)
	}
	if d := a.Detector(); d.Mode != anomaly.ModeHoltWinters || d.SeasonPeriodMs != (12*time.Hour).Milliseconds() {
		t.Fatalf("detector mapping after load is wrong: mode=%q periodMs=%d", d.Mode, d.SeasonPeriodMs)
	}
}

func TestLoadRejectsBadIngestion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meridian.yaml")
	// queue_capacity below batch_size: a batch could never enqueue.
	yaml := `
ingestion:
  batch_size: 1000
  queue_capacity: 100
  queue_high_watermark: 50
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load should reject queue_capacity < batch_size")
	}
}

func TestLoadDefaultsIngestionQueue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meridian.yaml")
	// An ingestion block that sets only batch_size keeps the queue defaults.
	yaml := `
ingestion:
  batch_size: 500
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Ingestion.QueueCapacity != 50000 {
		t.Errorf("queue_capacity: got %d, want default 50000", cfg.Ingestion.QueueCapacity)
	}
	if cfg.Ingestion.BlockDeadline.Std() != 250*time.Millisecond {
		t.Errorf("block_deadline: got %v, want default 250ms", cfg.Ingestion.BlockDeadline.Std())
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
