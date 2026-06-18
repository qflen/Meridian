package server

import (
	"encoding/json"
	"testing"

	"github.com/meridiandb/meridian/internal/anomaly"
)

func TestSeriesKeyIsDeterministicAndSorted(t *testing.T) {
	labels := map[string]string{"__name__": "cpu_usage_percent", "role": "web", "host": "web-01"}
	want := `cpu_usage_percent{host="web-01",role="web"}`
	// Build many times: map iteration order varies, the key must not.
	for i := 0; i < 50; i++ {
		if got := SeriesKey("cpu_usage_percent", labels); got != want {
			t.Fatalf("SeriesKey = %q, want %q", got, want)
		}
	}
	if got := SeriesKey("up", nil); got != "up" {
		t.Errorf("no-label key = %q, want %q", got, "up")
	}
	if got := SeriesKey("up", map[string]string{"__name__": "up"}); got != "up" {
		t.Errorf("only-__name__ key = %q, want %q", got, "up")
	}
}

func TestAnomalyFrameWireShape(t *testing.T) {
	ev := anomaly.Event{
		Seq: 7, Series: `cpu{host="a"}`, Metric: "cpu",
		Labels: map[string]string{"host": "a"}, Value: 95, Baseline: 40, Score: 8.2,
		Severity: anomaly.SeverityCrit, State: anomaly.StateFiring, TimestampMs: 1234,
	}
	data, err := json.Marshal(anomalyFrame{Type: "anomaly", Event: ev})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	// The dashboard validator branches on these promoted fields.
	checks := map[string]any{
		"type": "anomaly", "seq": float64(7), "series": `cpu{host="a"}`,
		"metric": "cpu", "value": float64(95), "baseline": float64(40),
		"score": 8.2, "severity": "crit", "state": "firing", "timestamp": float64(1234),
	}
	for k, want := range checks {
		if got[k] != want {
			t.Errorf("frame[%q] = %v, want %v", k, got[k], want)
		}
	}
}

func TestRecentAnomaliesPayload(t *testing.T) {
	// Nil detector → well-formed empty payload.
	p := RecentAnomaliesPayload(nil)
	if list, ok := p["anomalies"].([]anomaly.Event); !ok || len(list) != 0 {
		t.Fatalf("nil payload anomalies = %v, want empty slice", p["anomalies"])
	}

	cfg := anomaly.DefaultConfig()
	cfg.Enabled = true
	cfg.Warmup = 2
	cfg.DebounceK = 1
	d := anomaly.New(cfg)
	d.Observe(anomaly.Sample{Series: "s", Value: 0, TimestampMs: 1000})
	d.Observe(anomaly.Sample{Series: "s", Value: 0, TimestampMs: 2000})
	d.Observe(anomaly.Sample{Series: "s", Value: 1e6, TimestampMs: 3000}) // fire

	p = RecentAnomaliesPayload(d)
	list, ok := p["anomalies"].([]anomaly.Event)
	if !ok || len(list) == 0 {
		t.Fatalf("expected at least one recent anomaly, got %v", p["anomalies"])
	}
	if p["total"].(uint64) == 0 {
		t.Errorf("expected total > 0, got %v", p["total"])
	}
	if p["active"].(int) != 1 {
		t.Errorf("expected active = 1, got %v", p["active"])
	}
	// Most-recent-first ordering.
	for i := 1; i < len(list); i++ {
		if list[i].Seq > list[i-1].Seq {
			t.Fatalf("anomalies not most-recent-first: %d before %d", list[i-1].Seq, list[i].Seq)
		}
	}
}
