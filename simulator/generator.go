package simulator

import (
	"math"
	"math/rand"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// Generator produces realistic infrastructure metrics for the simulator.
type Generator struct {
	profiles []HostProfile
	rng      *rand.Rand

	// Per-metric state
	counters map[string]float64 // metric key → current counter value
	memDrift map[string]float64 // metric key → memory drift accumulator
	spikeDecay map[string]float64 // metric key → remaining spike multiplier
}

// MetricSample is a single generated metric data point.
type MetricSample struct {
	Name      string
	Labels    map[string]string
	Timestamp int64
	Value     float64
}

// NewGenerator creates a generator with the given host profiles.
func NewGenerator(profiles []HostProfile) *Generator {
	return &Generator{
		profiles:   profiles,
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
		counters:   make(map[string]float64),
		memDrift:   make(map[string]float64),
		spikeDecay: make(map[string]float64),
	}
}

// Generate produces one tick of metrics for all hosts at the given timestamp.
func (g *Generator) Generate(ts time.Time) []MetricSample {
	var samples []MetricSample

	hourOfDay := float64(ts.Hour()) + float64(ts.Minute())/60.0
	diurnalFactor := diurnalPattern(hourOfDay)

	for _, profile := range g.profiles {
		for _, mp := range profile.Metrics {
			key := mp.Name + "{" + profile.Hostname + "}"
			val := g.generateValue(key, mp, diurnalFactor, ts.UnixMilli())

			samples = append(samples, MetricSample{
				Name:      mp.Name,
				Labels:    mp.Labels,
				Timestamp: ts.UnixMilli(),
				Value:     val,
			})
		}
	}

	return samples
}

// GenerateToTSDB generates metrics and ingests them directly into a TSDB.
func (g *Generator) GenerateToTSDB(db *storage.TSDB, ts time.Time) int {
	samples := g.Generate(ts)
	for _, s := range samples {
		db.Ingest(s.Name, s.Labels, s.Timestamp, s.Value)
	}
	return len(samples)
}

func (g *Generator) generateValue(key string, mp MetricProfile, diurnalFactor float64, tsMs int64) float64 {
	switch mp.Type {
	case MetricCounter:
		return g.generateCounter(key, mp, diurnalFactor)
	case MetricGaugeCPU:
		return g.generateCPU(key, mp, diurnalFactor)
	case MetricGaugeMemory:
		return g.generateMemory(key, mp, tsMs)
	case MetricGaugeRatio:
		return g.generateRatio(mp)
	default:
		return g.generateGauge(mp, diurnalFactor)
	}
}

func (g *Generator) generateCounter(key string, mp MetricProfile, diurnalFactor float64) float64 {
	increment := mp.BaseVal * diurnalFactor
	increment += g.rng.NormFloat64() * mp.NoiseAmp
	if increment < 0 {
		increment = 0
	}
	g.counters[key] += increment
	return math.Round(g.counters[key])
}

func (g *Generator) generateCPU(key string, mp MetricProfile, diurnalFactor float64) float64 {
	// Base sinusoidal pattern following time of day
	base := mp.BaseVal * diurnalFactor

	// Gaussian noise
	noise := g.rng.NormFloat64() * mp.NoiseAmp

	// Occasional spikes (1% chance per tick)
	spike := g.spikeDecay[key]
	if g.rng.Float64() < 0.01 {
		spike = (2.0 + g.rng.Float64()) * mp.BaseVal * 0.3 // 2-3x spike
	}
	if spike > 0 {
		spike *= 0.85 // decay over ~30s at 5s intervals
	}
	g.spikeDecay[key] = spike

	val := base + noise + spike
	if val < 0 {
		val = 0
	}
	if val > 100 {
		val = 100
	}
	return math.Round(val*10) / 10
}

func (g *Generator) generateMemory(key string, mp MetricProfile, tsMs int64) float64 {
	// Slow upward drift (simulated memory leak)
	g.memDrift[key] += g.rng.Float64() * mp.NoiseAmp * 0.01

	// Periodic drops (GC or restart) every ~5 minutes
	if g.rng.Float64() < 0.003 {
		g.memDrift[key] = 0
	}

	val := mp.BaseVal + g.memDrift[key] + g.rng.NormFloat64()*mp.NoiseAmp*0.1
	if val < 0 {
		val = 0
	}
	return math.Round(val)
}

func (g *Generator) generateGauge(mp MetricProfile, diurnalFactor float64) float64 {
	base := mp.BaseVal * diurnalFactor
	noise := g.rng.NormFloat64() * mp.NoiseAmp
	val := base + noise
	if val < 0 {
		val = 0
	}
	return math.Round(val*100) / 100
}

func (g *Generator) generateRatio(mp MetricProfile) float64 {
	val := mp.BaseVal + g.rng.NormFloat64()*mp.NoiseAmp
	if val < 0 {
		val = 0
	}
	if val > 1 {
		val = 1
	}
	return math.Round(val*1000) / 1000
}

// diurnalPattern models a 24-hour load cycle with peak at 14:00 and trough at 02:00.
func diurnalPattern(hourOfDay float64) float64 {
	// Sinusoidal with 24-hour period: peak at 14:00
	// sin(pi/2) = 1, so we need phase = pi/2 when hourOfDay = 14
	phase := (hourOfDay-14.0)*math.Pi/12.0 + math.Pi/2.0
	return 0.5 + 0.3*math.Sin(phase) // range: [0.2, 0.8]
}
