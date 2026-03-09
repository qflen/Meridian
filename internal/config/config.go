// Package config provides YAML-based configuration for Meridian nodes.
package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/meridiandb/meridian/internal/anomaly"
	"github.com/meridiandb/meridian/internal/backpressure"
	"github.com/meridiandb/meridian/internal/retention"
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
	// WALGroupCommit coalesces concurrently-submitted WAL frames into a single fsync,
	// raising ingest throughput under concurrent writers without weakening durability
	// (a write still returns only after the fsync covering its frame). Default on; set
	// false to force the legacy one-fsync-per-frame path. See ADR-026.
	WALGroupCommit bool `yaml:"wal_group_commit"`
	// WALCommitLinger optionally delays each group-commit fsync to coalesce more
	// frames per batch (Nagle-style). Default 0 — no added latency, while still
	// coalescing frames that arrive during an in-flight fsync.
	WALCommitLinger Duration `yaml:"wal_commit_linger"`
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
	// Handoff configures hinted handoff (ADR-029), which buffers writes for a replica
	// that is unreachable at write time and replays them on its return.
	Handoff HandoffConfig `yaml:"handoff"`
	// AntiEntropy configures proactive Merkle anti-entropy (ADR-030), the background
	// sweep that converges co-replicas read-repair and handoff cannot reach.
	AntiEntropy AntiEntropyConfig `yaml:"anti_entropy"`
	// Rebalance configures data migration on membership change (ADR-031): when a node joins
	// or leaves, the ranges that changed owners are migrated to their new owners and the data
	// a node no longer owns is GC'd.
	Rebalance RebalanceConfig `yaml:"rebalance"`
}

// RebalanceConfig configures rebalancing on membership change (ADR-031). When a node joins or
// leaves the ring, placement is re-derived and the affected ranges are migrated to their new
// owners (reusing the backfill transfer); the old owners then drop the data they no longer own
// once the new owners confirm receipt at quorum (never the last copy). Disabled, a membership
// change re-derives routing but leaves existing data where it was (the pre-ADR-031 behaviour).
type RebalanceConfig struct {
	// Enabled turns rebalancing on for the coordinator (the ingestor).
	Enabled bool `yaml:"enabled"`
	// Lookback bounds how far back a migration reads/GCs. 0 migrates all history; a finite
	// value bounds the per-pass read cost on large datasets.
	Lookback Duration `yaml:"lookback"`
	// MaxBytesPerRound soft-caps the bytes a single migrate pass transfers (the throughput
	// rate limit); 0 is unlimited. A node is not promoted/removed until a pass completes, so
	// capping just spreads a large move across more passes.
	MaxBytesPerRound int64 `yaml:"max_bytes_per_round"`
}

// Validate checks the rebalance tunables when enabled, so a misconfiguration is caught at
// load time.
func (c RebalanceConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Lookback < 0 {
		return fmt.Errorf("cluster.rebalance.lookback must be >= 0, got %s", time.Duration(c.Lookback))
	}
	if c.MaxBytesPerRound < 0 {
		return fmt.Errorf("cluster.rebalance.max_bytes_per_round must be >= 0, got %d", c.MaxBytesPerRound)
	}
	return nil
}

// HandoffConfig configures hinted handoff (ADR-029): durable, bounded buffering of
// writes destined for a replica that was unreachable at write time, replayed on its
// return so it fully converges — including an interior gap read-repair cannot fix.
// Disabled, a missed write simply waits for read-repair (the ADR-022 behaviour).
type HandoffConfig struct {
	// Enabled turns hinted handoff on for the write path (the ingestor).
	Enabled bool `yaml:"enabled"`
	// Dir is where buffered hints are persisted (one crash-safe file per hint). The
	// call site defaults it to "<data_dir>/hints" when empty.
	Dir string `yaml:"dir"`
	// MaxSamplesPerNode bounds the samples buffered per target replica; past the cap the
	// oldest hints are dropped (counted), so a long outage cannot grow the buffer without
	// bound while the most recent hint is always retained.
	MaxSamplesPerNode int `yaml:"max_samples_per_node"`
	// ReplayInterval is how often the replay loop scans for reachable targets to catch
	// up. Zero uses the built-in default (5s).
	ReplayInterval Duration `yaml:"replay_interval"`
}

// Validate checks the hinted-handoff tunables when enabled, so a misconfiguration is
// caught at load time rather than producing a buffer that can never hold a hint.
func (c HandoffConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.MaxSamplesPerNode < 1 {
		return fmt.Errorf("cluster.handoff.max_samples_per_node must be >= 1, got %d", c.MaxSamplesPerNode)
	}
	if c.ReplayInterval < 0 {
		return fmt.Errorf("cluster.handoff.replay_interval must be >= 0, got %s", time.Duration(c.ReplayInterval))
	}
	return nil
}

// AntiEntropyConfig configures proactive Merkle anti-entropy (ADR-030): a rate-limited,
// jittered background sweep that compares co-replicas' range digests and repairs the
// divergence neither read-repair nor hinted handoff reaches — cold data, a series no
// longer written, or a window dropped past the hint cap. Disabled, convergence stays
// write- and read-triggered (the ADR-029 behaviour).
type AntiEntropyConfig struct {
	// Enabled turns the background sweep on for the coordinator (the ingestor).
	Enabled bool `yaml:"enabled"`
	// Interval is the time between sweep rounds.
	Interval Duration `yaml:"interval"`
	// Window is the time-bucket size for digests; a divergent bucket is the unit
	// transferred, so a smaller window re-transfers less at the cost of a larger digest.
	Window Duration `yaml:"window"`
	// Lookback bounds how far back from now a round reconciles. 0 reconciles all history;
	// a finite value bounds the per-round read cost on large datasets.
	Lookback Duration `yaml:"lookback"`
	// Jitter is a random [0, Jitter) delay added to each interval so coordinators do not
	// sweep in lockstep.
	Jitter Duration `yaml:"jitter"`
	// GroupsPerRound caps the replica groups reconciled per round (the spatial rate
	// limit); a round-robin cursor covers the rest over subsequent rounds.
	GroupsPerRound int `yaml:"groups_per_round"`
}

// Validate checks the anti-entropy tunables when enabled, so a misconfiguration is
// caught at load time rather than producing a sweep that never makes progress.
func (c AntiEntropyConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Interval <= 0 {
		return fmt.Errorf("cluster.anti_entropy.interval must be > 0, got %s", time.Duration(c.Interval))
	}
	if c.Window <= 0 {
		return fmt.Errorf("cluster.anti_entropy.window must be > 0, got %s", time.Duration(c.Window))
	}
	if c.Lookback < 0 {
		return fmt.Errorf("cluster.anti_entropy.lookback must be >= 0, got %s", time.Duration(c.Lookback))
	}
	if c.Jitter < 0 {
		return fmt.Errorf("cluster.anti_entropy.jitter must be >= 0, got %s", time.Duration(c.Jitter))
	}
	if c.GroupsPerRound < 1 {
		return fmt.Errorf("cluster.anti_entropy.groups_per_round must be >= 1, got %d", c.GroupsPerRound)
	}
	return nil
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
	if err := c.Handoff.Validate(); err != nil {
		return err
	}
	if err := c.AntiEntropy.Validate(); err != nil {
		return err
	}
	if err := c.Rebalance.Validate(); err != nil {
		return err
	}
	return nil
}

// DownsamplingConfig holds the automatic rollup cascade: whether it runs, how often a
// pass runs, and the per-tier rules (source→target interval and the tier's retention).
// The first rule reads raw blocks; a later rule whose source matches an earlier rule's
// target chains that finer rollup tier. Each tier keeps its own retention, longer for
// coarser tiers, so a long-range query is still served after raw expires.
type DownsamplingConfig struct {
	// Enabled turns the cascade on. When false no rollup blocks are generated and every
	// query reads raw.
	Enabled bool `yaml:"enabled"`
	// Interval is how often a downsampling pass runs (default 1m when unset).
	Interval Duration           `yaml:"interval"`
	Rules    []DownsamplingRule `yaml:"rules"`
}

// DownsamplingRule defines a single rollup rule.
type DownsamplingRule struct {
	SourceInterval Duration `yaml:"source_interval"`
	TargetInterval Duration `yaml:"target_interval"`
	Retention      Duration `yaml:"retention"`
}

// Validate checks the cascade is internally consistent when enabled: each target
// exceeds and is an exact multiple of its source (required for weighted chaining), each
// retention is positive, and coarser tiers are retained at least as long as finer ones
// (so the cascade degrades resolution over age, never the reverse). The raw retention
// (storage.retention) should be the shortest of all; it is enforced separately since it
// lives in StorageConfig.
func (c DownsamplingConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if len(c.Rules) == 0 {
		return fmt.Errorf("downsampling.enabled is true but no rules are configured")
	}
	if c.Interval < 0 {
		return fmt.Errorf("downsampling.interval must be >= 0, got %s", time.Duration(c.Interval))
	}
	rules := append([]DownsamplingRule(nil), c.Rules...)
	sort.Slice(rules, func(i, j int) bool { return rules[i].TargetInterval < rules[j].TargetInterval })
	var prevRet time.Duration
	for i, r := range rules {
		s, tg, ret := r.SourceInterval.Std(), r.TargetInterval.Std(), r.Retention.Std()
		if s <= 0 {
			return fmt.Errorf("downsampling rule %d: source_interval must be > 0", i)
		}
		if tg <= s {
			return fmt.Errorf("downsampling rule %d: target_interval (%s) must exceed source_interval (%s)", i, tg, s)
		}
		if tg%s != 0 {
			return fmt.Errorf("downsampling rule %d: target_interval (%s) must be a multiple of source_interval (%s) for weighted chaining", i, tg, s)
		}
		if ret <= 0 {
			return fmt.Errorf("downsampling rule %d: retention must be > 0", i)
		}
		if i > 0 && ret < prevRet {
			return fmt.Errorf("downsampling: coarser tier %s retention (%s) must be >= finer tier retention (%s)", tg, ret, prevRet)
		}
		prevRet = ret
	}
	return nil
}

// DownsampleRules converts the configured rules into retention.DownsampleRule values
// for the live downsampler.
func (c DownsamplingConfig) DownsampleRules() []retention.DownsampleRule {
	out := make([]retention.DownsampleRule, 0, len(c.Rules))
	for _, r := range c.Rules {
		out = append(out, retention.DownsampleRule{
			SourceInterval: r.SourceInterval.Std(),
			TargetInterval: r.TargetInterval.Std(),
			Retention:      r.Retention.Std(),
		})
	}
	return out
}

// RollupRetentions maps each rollup resolution (ms) to its TTL, for the tiered
// retention enforcer.
func (c DownsamplingConfig) RollupRetentions() map[int64]time.Duration {
	out := make(map[int64]time.Duration, len(c.Rules))
	for _, r := range c.Rules {
		out[r.TargetInterval.Std().Milliseconds()] = r.Retention.Std()
	}
	return out
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
	// Admission optionally layers per-series fair-share / priority-class shedding on
	// top of the uniform bounded-queue shedding (ADR-027). Disabled by default.
	Admission AdmissionConfig `yaml:"admission"`
}

// AdmissionConfig tunes per-series fair-share / priority-class load shedding (ADR-027),
// layered in front of the bounded ingest queue. When disabled (the default) the queue's
// uniform block-then-shed is the only policy. When enabled, overload sheds the lowest
// priority and most over-budget series first instead of the next arrival regardless of
// series or importance, while preserving FIFO order within a single series.
type AdmissionConfig struct {
	// Enabled turns the layer on. At least one priority class or a positive
	// fair_share_rate must be configured for it to have any effect.
	Enabled bool `yaml:"enabled"`
	// ContentionFraction is the queue depth/capacity at or above which per-series
	// fair-share metering engages; below it there is room, so every series is admitted.
	// In [0,1]; 0 meters as soon as anything is resident.
	ContentionFraction float64 `yaml:"contention_fraction"`
	// FairShareRate is the per-series token-bucket refill in samples/sec. Zero disables
	// the fair-share gate, leaving only the priority bands.
	FairShareRate float64 `yaml:"fair_share_rate"`
	// FairShareBurst is the per-series token-bucket depth in samples (the burst a quiet
	// series may spend at once). Defaults to FairShareRate when unset.
	FairShareBurst float64 `yaml:"fair_share_burst"`
	// Shards is the number of fair-share token buckets (bounded memory regardless of
	// cardinality). Defaults to 4096 when unset.
	Shards int `yaml:"shards"`
	// MetricBuckets is the number of per-series shed counters exposed as metrics; the
	// series space is hashed into this many buckets to bound cardinality. Defaults to 16.
	MetricBuckets int `yaml:"metric_buckets"`
	// Classes are the priority classes in descending priority order: the first whose
	// matcher matches a series wins. Exactly one catch-all (empty label) is the default.
	Classes []ClassConfig `yaml:"classes"`
}

// ClassConfig defines one priority class for AdmissionConfig.
type ClassConfig struct {
	// Name labels the class in metrics; it must be unique and non-empty.
	Name string `yaml:"name"`
	// Label selects the class: a series matches when its label Label equals Value. The
	// special label "__name__" matches the metric name. An empty Label marks the
	// catch-all default class.
	Label string `yaml:"label"`
	Value string `yaml:"value"`
	// Ceiling is the fraction (0,1] of the queue capacity this class may occupy before
	// it is shed. Higher-priority classes take a higher ceiling so the top band is held
	// for them.
	Ceiling float64 `yaml:"ceiling"`
}

// Validate checks the admission tunables when enabled, so a misconfiguration is caught
// at load time rather than producing a layer that silently does nothing or a class set
// that can never match.
func (c AdmissionConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.ContentionFraction < 0 || c.ContentionFraction > 1 {
		return fmt.Errorf("ingestion.admission.contention_fraction must be in [0,1], got %g", c.ContentionFraction)
	}
	if c.FairShareRate < 0 {
		return fmt.Errorf("ingestion.admission.fair_share_rate must be >= 0, got %g", c.FairShareRate)
	}
	if c.FairShareBurst < 0 {
		return fmt.Errorf("ingestion.admission.fair_share_burst must be >= 0, got %g", c.FairShareBurst)
	}
	if c.Shards < 0 {
		return fmt.Errorf("ingestion.admission.shards must be >= 0, got %d", c.Shards)
	}
	if c.MetricBuckets < 0 {
		return fmt.Errorf("ingestion.admission.metric_buckets must be >= 0, got %d", c.MetricBuckets)
	}
	if len(c.Classes) == 0 && c.FairShareRate <= 0 {
		return fmt.Errorf("ingestion.admission.enabled is true but neither classes nor a positive fair_share_rate is configured")
	}
	seen := make(map[string]bool, len(c.Classes))
	defaults := 0
	for i, cl := range c.Classes {
		if cl.Name == "" {
			return fmt.Errorf("ingestion.admission.classes[%d]: name must not be empty", i)
		}
		if seen[cl.Name] {
			return fmt.Errorf("ingestion.admission.classes: duplicate class name %q", cl.Name)
		}
		seen[cl.Name] = true
		if cl.Ceiling <= 0 || cl.Ceiling > 1 {
			return fmt.Errorf("ingestion.admission.classes[%q]: ceiling must be in (0,1], got %g", cl.Name, cl.Ceiling)
		}
		if cl.Label == "" {
			defaults++
		}
	}
	if defaults > 1 {
		return fmt.Errorf("ingestion.admission.classes: at most one catch-all (empty label) class is allowed, got %d", defaults)
	}
	return nil
}

// Shaper maps the validated config onto a backpressure.ShaperConfig, or nil when the
// layer is disabled (so the caller leaves the queue's uniform shedding in place).
func (c AdmissionConfig) Shaper() *backpressure.ShaperConfig {
	if !c.Enabled {
		return nil
	}
	classes := make([]backpressure.ClassRule, len(c.Classes))
	for i, cl := range c.Classes {
		classes[i] = backpressure.ClassRule{Name: cl.Name, Label: cl.Label, Value: cl.Value, Ceiling: cl.Ceiling}
	}
	return &backpressure.ShaperConfig{
		Classes:            classes,
		ContentionFraction: c.ContentionFraction,
		FairShareRate:      c.FairShareRate,
		FairShareBurst:     c.FairShareBurst,
		Shards:             c.Shards,
		MetricBuckets:      c.MetricBuckets,
	}
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
	if err := c.Admission.Validate(); err != nil {
		return err
	}
	return nil
}

// AnomalyConfig tunes the streaming anomaly detector that runs over the live
// telemetry path (ADR-024, ADR-028). The detector scores each series' latest value
// against a per-series model and flags points whose score |value-baseline|/dispersion
// exceeds Threshold; the remaining robustness knobs (Huber clamp, scale floor,
// hysteresis) take internal defaults.
//
// Mode selects the model. The default "ewma" tracks an EWMA baseline + dispersion,
// robust to a moving baseline. "holt_winters" (ADR-028) additionally learns the
// diurnal shape and scores against the band for each time of day, so it flags a value
// that is normal globally but abnormal for that phase — at the cost of warming up over
// a full season. Threshold/Alpha/DebounceK apply to both models; SeasonLength and
// SeasonPeriod apply only to Holt-Winters (its trend/seasonal smoothing take internal
// defaults).
type AnomalyConfig struct {
	// Enabled turns the detector on. When false the broadcast loop feeds it nothing
	// and no anomaly frames or metrics are produced.
	Enabled bool `yaml:"enabled"`
	// Threshold is the local z-score above which a sample is out-of-band (~3–4).
	Threshold float64 `yaml:"threshold"`
	// Alpha is the smoothing factor in (0,1] for the level and dispersion (and the
	// Holt-Winters level).
	Alpha float64 `yaml:"alpha"`
	// Warmup is the number of samples used to seed a per-series baseline before any
	// alert may fire (>= 2). Used by EWMA; Holt-Winters instead warms over one season.
	Warmup int `yaml:"warmup"`
	// DebounceK is the consecutive out-of-band samples required to raise an alert (>= 1).
	DebounceK int `yaml:"debounce_k"`

	// Mode selects the scoring model: "ewma" (default, also the empty value) or
	// "holt_winters".
	Mode string `yaml:"mode"`
	// SeasonLength is the number of seasonal buckets the season is divided into
	// (holt_winters only, >= 2); a sample is scored against the band learned for its
	// bucket. Unset falls back to the detector's internal default.
	SeasonLength int `yaml:"season_length"`
	// SeasonPeriod is the wall-clock span of one full season (holt_winters only, > 0),
	// e.g. 24h for a diurnal cycle. A sample's bucket is derived from its timestamp
	// modulo this period. Unset falls back to the detector's internal default.
	SeasonPeriod Duration `yaml:"season_period"`
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
	switch c.Mode {
	case "", string(anomaly.ModeEWMA):
		// EWMA needs no seasonal parameters.
	case string(anomaly.ModeHoltWinters):
		// Validate the seasonal parameters only when set; unset falls back to the
		// detector's internal defaults, which are themselves valid.
		if c.SeasonLength != 0 && c.SeasonLength < 2 {
			return fmt.Errorf("anomaly.season_length must be >= 2 for holt_winters, got %d", c.SeasonLength)
		}
		if c.SeasonPeriod.Std() < 0 {
			return fmt.Errorf("anomaly.season_period must be > 0 for holt_winters, got %s", c.SeasonPeriod.Std())
		}
	default:
		return fmt.Errorf("anomaly.mode must be %q or %q, got %q", anomaly.ModeEWMA, anomaly.ModeHoltWinters, c.Mode)
	}
	return nil
}

// Detector maps the YAML-exposed tunables onto an anomaly.Config, leaving the
// detector's internal robustness defaults (clip/clear/crit/floor/buffer and the
// Holt-Winters trend/seasonal smoothing) in place. An unset Mode/SeasonLength/
// SeasonPeriod falls through to the anomaly package defaults.
func (c AnomalyConfig) Detector() anomaly.Config {
	base := anomaly.DefaultConfig()
	base.Enabled = c.Enabled
	base.Threshold = c.Threshold
	base.Alpha = c.Alpha
	base.Warmup = c.Warmup
	base.DebounceK = c.DebounceK
	base.Mode = anomaly.Mode(c.Mode) // "" → withDefaults normalises to ModeEWMA
	if c.SeasonLength > 0 {
		base.SeasonLength = c.SeasonLength
	}
	if c.SeasonPeriod.Std() > 0 {
		base.SeasonPeriodMs = c.SeasonPeriod.Std().Milliseconds()
	}
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
			DataDir:         "./data",
			WALDir:          "./data/wal",
			BlockDuration:   Duration(15 * time.Minute),
			Retention:       Duration(15 * 24 * time.Hour),
			FlushInterval:   Duration(30 * time.Second),
			WALGroupCommit:  true,
			WALCommitLinger: 0,
		},
		Cluster: ClusterConfig{
			Enabled:           false,
			BindAddr:          "0.0.0.0:7946",
			ReplicationFactor: 3,
			WriteQuorum:       2,
			ReadQuorum:        2,
			VirtualNodes:      256,
			Handoff: HandoffConfig{
				Enabled:           true,
				MaxSamplesPerNode: 1_000_000,
				ReplayInterval:    Duration(5 * time.Second),
			},
			AntiEntropy: AntiEntropyConfig{
				Enabled:        true,
				Interval:       Duration(30 * time.Second),
				Window:         Duration(1 * time.Hour),
				Lookback:       0, // all history
				Jitter:         Duration(10 * time.Second),
				GroupsPerRound: 16,
			},
			Rebalance: RebalanceConfig{
				Enabled:          true,
				Lookback:         0, // all history
				MaxBytesPerRound: 0, // unlimited; migrate a membership change in one pass
			},
		},
		Downsampling: DownsamplingConfig{
			Enabled:  true,
			Interval: Duration(1 * time.Minute),
			// Retentions are coarser-is-longer and exceed the raw retention (15d), so the
			// cascade trades resolution for age: raw 15d → 1m 30d → 1h 365d.
			Rules: []DownsamplingRule{
				{SourceInterval: Duration(5 * time.Second), TargetInterval: Duration(1 * time.Minute), Retention: Duration(30 * 24 * time.Hour)},
				{SourceInterval: Duration(1 * time.Minute), TargetInterval: Duration(1 * time.Hour), Retention: Duration(365 * 24 * time.Hour)},
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
			DebounceK: 2,
			Mode:      string(anomaly.ModeEWMA),
			// SeasonLength/SeasonPeriod are unset: they apply only to holt_winters and
			// fall back to the detector's internal defaults (48 buckets over 24h).
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
	if err := cfg.Downsampling.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
