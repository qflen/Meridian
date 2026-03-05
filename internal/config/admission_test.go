package config

import "testing"

func TestAdmissionFromEnv(t *testing.T) {
	if c := AdmissionFromEnv("ING"); c.Enabled {
		t.Fatal("admission should be disabled when ING_ADMISSION_ENABLED is unset")
	}

	t.Setenv("ING_ADMISSION_ENABLED", "true")
	t.Setenv("ING_FAIR_SHARE_RATE", "250")
	t.Setenv("ING_FAIR_SHARE_BURST", "500")
	t.Setenv("ING_CONTENTION_FRACTION", "0.7")
	t.Setenv("ING_ADMISSION_SHARDS", "2048")
	t.Setenv("ING_PRIORITY_LABEL", "tier")
	t.Setenv("ING_PRIORITY_VALUE", "gold")
	t.Setenv("ING_PRIORITY_CEILING", "1.0")
	t.Setenv("ING_DEFAULT_CEILING", "0.4")

	c := AdmissionFromEnv("ING")
	if !c.Enabled || c.FairShareRate != 250 || c.FairShareBurst != 500 || c.ContentionFraction != 0.7 || c.Shards != 2048 {
		t.Fatalf("scalars not parsed from env: %+v", c)
	}
	if len(c.Classes) != 2 || c.Classes[0].Label != "tier" || c.Classes[0].Value != "gold" || c.Classes[1].Ceiling != 0.4 {
		t.Fatalf("priority classes not built from env: %+v", c.Classes)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("env-built config should validate: %v", err)
	}
}

func TestAdmissionConfigValidate(t *testing.T) {
	valid := []AdmissionConfig{
		{Enabled: false}, // disabled is always valid, even if otherwise empty
		{Enabled: true, FairShareRate: 100},
		{Enabled: true, ContentionFraction: 0.8, Classes: []ClassConfig{
			{Name: "high", Label: "tier", Value: "gold", Ceiling: 1.0},
			{Name: "default", Ceiling: 0.5},
		}},
		{Enabled: true, FairShareRate: 50, FairShareBurst: 100, Shards: 1024, MetricBuckets: 8},
	}
	for _, c := range valid {
		if err := c.Validate(); err != nil {
			t.Errorf("expected %+v to be valid, got %v", c, err)
		}
	}

	invalid := []AdmissionConfig{
		{Enabled: true},                    // neither classes nor fair-share
		{Enabled: true, FairShareRate: -1}, // negative rate
		{Enabled: true, ContentionFraction: 1.5, FairShareRate: 1},         // fraction out of range
		{Enabled: true, FairShareRate: 1, Shards: -1},                      // negative shards
		{Enabled: true, Classes: []ClassConfig{{Name: "x", Ceiling: 0}}},   // ceiling out of range
		{Enabled: true, Classes: []ClassConfig{{Name: "x", Ceiling: 1.1}}}, // ceiling > 1
		{Enabled: true, Classes: []ClassConfig{{Name: "", Ceiling: 0.5}}},  // empty class name
		{Enabled: true, Classes: []ClassConfig{ // duplicate names
			{Name: "dup", Label: "a", Value: "1", Ceiling: 0.5},
			{Name: "dup", Ceiling: 0.5},
		}},
		{Enabled: true, Classes: []ClassConfig{ // two catch-all defaults
			{Name: "d1", Ceiling: 0.5},
			{Name: "d2", Ceiling: 0.6},
		}},
	}
	for _, c := range invalid {
		if err := c.Validate(); err == nil {
			t.Errorf("expected %+v to be invalid", c)
		}
	}
}

// TestAdmissionConfigShaperMapping checks the config→backpressure mapping and that a
// disabled config maps to nil (so callers leave uniform shedding in place).
func TestAdmissionConfigShaperMapping(t *testing.T) {
	if (AdmissionConfig{Enabled: false, FairShareRate: 10}).Shaper() != nil {
		t.Fatal("a disabled admission config must map to a nil shaper config")
	}

	c := AdmissionConfig{
		Enabled:            true,
		ContentionFraction: 0.75,
		FairShareRate:      100,
		FairShareBurst:     200,
		Shards:             2048,
		MetricBuckets:      32,
		Classes: []ClassConfig{
			{Name: "high", Label: "tier", Value: "gold", Ceiling: 1.0},
			{Name: "default", Ceiling: 0.5},
		},
	}
	sc := c.Shaper()
	if sc == nil {
		t.Fatal("an enabled admission config must map to a non-nil shaper config")
	}
	if sc.ContentionFraction != 0.75 || sc.FairShareRate != 100 || sc.FairShareBurst != 200 || sc.Shards != 2048 || sc.MetricBuckets != 32 {
		t.Fatalf("scalar fields not mapped: %+v", sc)
	}
	if len(sc.Classes) != 2 || sc.Classes[0].Name != "high" || sc.Classes[0].Label != "tier" || sc.Classes[0].Value != "gold" || sc.Classes[0].Ceiling != 1.0 {
		t.Fatalf("classes not mapped: %+v", sc.Classes)
	}
}

// TestIngestionConfigValidateRejectsBadAdmission ensures admission errors surface through
// the ingestion config's own Validate (and therefore Load).
func TestIngestionConfigValidateRejectsBadAdmission(t *testing.T) {
	c := DefaultConfig().Ingestion
	c.Admission = AdmissionConfig{Enabled: true, FairShareRate: -5}
	if err := c.Validate(); err == nil {
		t.Fatal("ingestion.Validate must reject an invalid admission config")
	}
}
