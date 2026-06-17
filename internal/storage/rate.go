package storage

import (
	"sync"
	"time"
)

// rateMeter computes a moving-average ingestion rate (samples/sec) over a fixed
// time window. It is fed the cumulative sample count at roughly fixed intervals; an
// idle interval contributes the same total as the previous one, so the reported rate
// decays smoothly toward zero when ingestion stops. This is what lets IngestionRate
// report an actual rate while the Prometheus counter stays cumulative. See the ADR
// on ingestion-rate semantics.
type rateMeter struct {
	mu     sync.Mutex
	window time.Duration
	obs    []rateObs // observations within the window, oldest first
}

type rateObs struct {
	t     time.Time
	total int64
}

func newRateMeter(window time.Duration) *rateMeter {
	if window <= 0 {
		window = 5 * time.Second
	}
	return &rateMeter{window: window}
}

// observe records the cumulative total at time t (which must be non-decreasing
// across calls) and drops observations that have aged out of the window.
func (m *rateMeter) observe(total int64, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.obs = append(m.obs, rateObs{t: t, total: total})
	m.prune(t)
}

// rate returns the samples/sec across the retained window as of now.
func (m *rateMeter) rate(now time.Time) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune(now)
	if len(m.obs) < 2 {
		return 0
	}
	oldest := m.obs[0]
	newest := m.obs[len(m.obs)-1]
	dt := newest.t.Sub(oldest.t).Seconds()
	if dt <= 0 {
		return 0
	}
	delta := newest.total - oldest.total
	if delta < 0 {
		delta = 0 // counter reset (e.g. process restart); never report a negative rate
	}
	return float64(delta) / dt
}

// prune drops observations strictly older than now-window. Called holding m.mu.
func (m *rateMeter) prune(now time.Time) {
	cutoff := now.Add(-m.window)
	keep := 0
	for _, o := range m.obs {
		if !o.t.Before(cutoff) {
			m.obs[keep] = o
			keep++
		}
	}
	m.obs = m.obs[:keep]
}
