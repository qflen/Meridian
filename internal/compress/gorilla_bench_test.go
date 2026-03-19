package compress

import (
	"math"
	"math/rand"
	"testing"
)

func BenchmarkEncode(b *testing.B) {
	n := 1000000
	rng := rand.New(rand.NewSource(42))
	timestamps := make([]int64, n)
	values := make([]float64, n)
	ts := int64(1700000000000)
	val := 0.5
	for i := 0; i < n; i++ {
		ts += 5000
		val += rng.NormFloat64() * 0.01
		timestamps[i] = ts
		values[i] = val
	}

	b.ResetTimer()
	b.ReportAllocs()

	for iter := 0; iter < b.N; iter++ {
		enc := NewEncoder()
		for i := 0; i < n; i++ {
			enc.Write(timestamps[i], values[i])
		}
	}

	b.ReportMetric(float64(n), "points/op")
}

func BenchmarkDecode(b *testing.B) {
	n := 1000000
	rng := rand.New(rand.NewSource(42))
	enc := NewEncoder()
	ts := int64(1700000000000)
	val := 0.5
	for i := 0; i < n; i++ {
		ts += 5000
		val += rng.NormFloat64() * 0.01
		enc.Write(ts, val)
	}
	data := enc.Bytes()
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	b.ResetTimer()
	b.ReportAllocs()

	for iter := 0; iter < b.N; iter++ {
		dec := NewDecoder(dataCopy)
		count := 0
		for dec.Next() {
			count++
		}
		if count != n {
			b.Fatalf("decoded %d points, expected %d", count, n)
		}
	}

	b.ReportMetric(float64(n), "points/op")
}

func BenchmarkCompressionRatio(b *testing.B) {
	patterns := []struct {
		name string
		gen  func(rng *rand.Rand, n int) ([]int64, []float64)
	}{
		{
			name: "regular_cpu",
			gen: func(rng *rand.Rand, n int) ([]int64, []float64) {
				ts := make([]int64, n)
				vs := make([]float64, n)
				t := int64(1700000000000)
				v := 0.5
				for i := 0; i < n; i++ {
					t += 5000
					v += rng.NormFloat64() * 0.01
					ts[i] = t
					vs[i] = v
				}
				return ts, vs
			},
		},
		{
			name: "counter",
			gen: func(rng *rand.Rand, n int) ([]int64, []float64) {
				ts := make([]int64, n)
				vs := make([]float64, n)
				t := int64(1700000000000)
				v := 0.0
				for i := 0; i < n; i++ {
					t += 5000
					v += rng.Float64() * 10
					ts[i] = t
					vs[i] = v
				}
				return ts, vs
			},
		},
		{
			name: "sinusoidal",
			gen: func(rng *rand.Rand, n int) ([]int64, []float64) {
				ts := make([]int64, n)
				vs := make([]float64, n)
				t := int64(1700000000000)
				for i := 0; i < n; i++ {
					t += 5000
					ts[i] = t
					vs[i] = 50.0 + 30.0*math.Sin(float64(i)/200.0)
				}
				return ts, vs
			},
		},
	}

	for _, p := range patterns {
		b.Run(p.name, func(b *testing.B) {
			rng := rand.New(rand.NewSource(42))
			n := 100000
			timestamps, values := p.gen(rng, n)

			b.ResetTimer()
			for iter := 0; iter < b.N; iter++ {
				enc := NewEncoder()
				for i := 0; i < n; i++ {
					enc.Write(timestamps[i], values[i])
				}
				compressed := len(enc.Bytes())
				raw := n * 16
				b.ReportMetric(float64(raw)/float64(compressed), "ratio")
			}
		})
	}
}
