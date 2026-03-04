package config

import (
	"testing"
	"time"
)

func dsRule(src, tgt, ret time.Duration) DownsamplingRule {
	return DownsamplingRule{
		SourceInterval: Duration(src),
		TargetInterval: Duration(tgt),
		Retention:      Duration(ret),
	}
}

func TestDownsamplingValidate(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		name    string
		cfg     DownsamplingConfig
		wantErr bool
	}{
		{
			name: "valid cascade",
			cfg: DownsamplingConfig{Enabled: true, Interval: Duration(time.Minute), Rules: []DownsamplingRule{
				dsRule(5*time.Second, time.Minute, 30*day),
				dsRule(time.Minute, time.Hour, 365*day),
			}},
		},
		{
			name: "disabled skips all checks",
			cfg:  DownsamplingConfig{Enabled: false, Rules: []DownsamplingRule{dsRule(time.Minute, time.Second, 0)}},
		},
		{
			name:    "enabled with no rules",
			cfg:     DownsamplingConfig{Enabled: true},
			wantErr: true,
		},
		{
			name: "target not a multiple of source",
			cfg: DownsamplingConfig{Enabled: true, Rules: []DownsamplingRule{
				dsRule(7*time.Second, time.Minute, 30*day), // 60s % 7s != 0
			}},
			wantErr: true,
		},
		{
			name: "target not greater than source",
			cfg: DownsamplingConfig{Enabled: true, Rules: []DownsamplingRule{
				dsRule(time.Minute, time.Minute, 30*day),
			}},
			wantErr: true,
		},
		{
			name: "coarser tier retained shorter than finer",
			cfg: DownsamplingConfig{Enabled: true, Rules: []DownsamplingRule{
				dsRule(5*time.Second, time.Minute, 30*day),
				dsRule(time.Minute, time.Hour, 7*day), // coarser kept shorter
			}},
			wantErr: true,
		},
	}
	for _, c := range cases {
		err := c.cfg.Validate()
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", c.name, err, c.wantErr)
		}
	}
}

func TestDownsamplingTranslation(t *testing.T) {
	cfg := DefaultConfig().Downsampling
	rules := cfg.DownsampleRules()
	if len(rules) != 2 {
		t.Fatalf("rules: %d", len(rules))
	}
	if rules[0].TargetInterval != time.Minute || rules[1].TargetInterval != time.Hour {
		t.Fatalf("targets: %v", rules)
	}
	rr := cfg.RollupRetentions()
	if rr[time.Minute.Milliseconds()] != 30*24*time.Hour {
		t.Errorf("1m retention: %v", rr[time.Minute.Milliseconds()])
	}
	if rr[time.Hour.Milliseconds()] != 365*24*time.Hour {
		t.Errorf("1h retention: %v", rr[time.Hour.Milliseconds()])
	}
}
