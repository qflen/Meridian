package server

import (
	"sort"
	"strings"

	"github.com/meridiandb/meridian/internal/anomaly"
)

// SeriesKey builds a stable, human-readable key for a series: the metric name
// followed by its non-__name__ labels in sorted order, e.g.
// `cpu_usage_percent{host="web-01",role="web"}`. Sorting makes the key
// deterministic across ticks and restarts, which the anomaly detector relies on
// to keep one state entry per series. A series with no extra labels is just its
// name.
func SeriesKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if k == "__name__" {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return name
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(labels[k])
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// anomalyFrame is the WebSocket envelope for an anomaly event: the detector's
// Event fields (promoted via embedding, with their own json tags) under a distinct
// "anomaly" type so the dashboard's frame validator can branch on it.
type anomalyFrame struct {
	Type string `json:"type"`
	anomaly.Event
}

// BroadcastAnomalies sends each event to every connected dashboard as its own
// "anomaly" frame over the shared hub. Anomaly transitions are rare, so one
// broadcast per event keeps the hot path (the per-tick stats/metric frames)
// untouched.
func BroadcastAnomalies(hub *WebSocketHub, events []anomaly.Event) {
	for _, ev := range events {
		hub.BroadcastMetrics(anomalyFrame{Type: "anomaly", Event: ev})
	}
}
