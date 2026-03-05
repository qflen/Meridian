package config

import (
	"os"
	"strconv"
)

// AdmissionFromEnv builds an AdmissionConfig from environment variables prefixed with
// prefix, for the services configured by env rather than YAML (the ingestor and storage
// binaries). It supports the fair-share knobs and a single optional high-priority class
// (by label or the "__name__" metric-name pseudo-label) over a catch-all default — the
// common "protect one class, fair-share the rest" shape; richer class sets are
// configured via YAML on the monolith.
//
// Recognised vars (PREFIX is e.g. "INGEST" or "STORAGE"):
//
//	<PREFIX>_ADMISSION_ENABLED         bool    turn the layer on
//	<PREFIX>_CONTENTION_FRACTION       float   depth/capacity at which fair share engages
//	<PREFIX>_FAIR_SHARE_RATE           float   per-series refill, samples/sec
//	<PREFIX>_FAIR_SHARE_BURST          float   per-series token-bucket depth
//	<PREFIX>_ADMISSION_SHARDS          int     fair-share token buckets (bounded memory)
//	<PREFIX>_ADMISSION_METRIC_BUCKETS  int     per-series shed metric buckets
//	<PREFIX>_PRIORITY_LABEL            string  label (or "__name__") selecting the high class
//	<PREFIX>_PRIORITY_VALUE            string  required value of the priority label
//	<PREFIX>_PRIORITY_CEILING          float   high-class capacity ceiling (default 1.0)
//	<PREFIX>_DEFAULT_CEILING           float   catch-all ceiling (default 1.0)
//
// The result is not yet validated; callers run Validate (and skip the layer when
// disabled). When ADMISSION_ENABLED is unset/false the zero value is returned.
func AdmissionFromEnv(prefix string) AdmissionConfig {
	c := AdmissionConfig{
		Enabled:            envBool(prefix+"_ADMISSION_ENABLED", false),
		ContentionFraction: envFloat(prefix+"_CONTENTION_FRACTION", 0.8),
		FairShareRate:      envFloat(prefix+"_FAIR_SHARE_RATE", 0),
		FairShareBurst:     envFloat(prefix+"_FAIR_SHARE_BURST", 0),
		Shards:             envIntCfg(prefix+"_ADMISSION_SHARDS", 0),
		MetricBuckets:      envIntCfg(prefix+"_ADMISSION_METRIC_BUCKETS", 0),
	}
	if !c.Enabled {
		return AdmissionConfig{}
	}
	// An optional high-priority class over a catch-all default. Without a priority label,
	// the layer runs fair-share only (a single synthesised full-capacity default).
	if label := os.Getenv(prefix + "_PRIORITY_LABEL"); label != "" {
		c.Classes = []ClassConfig{
			{Name: "high", Label: label, Value: os.Getenv(prefix + "_PRIORITY_VALUE"), Ceiling: envFloat(prefix+"_PRIORITY_CEILING", 1.0)},
			{Name: "default", Ceiling: envFloat(prefix+"_DEFAULT_CEILING", 1.0)},
		}
	}
	return c
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envIntCfg(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
