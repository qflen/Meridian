package compress

import (
	"math"
	"math/rand"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	enc := NewEncoder()
	baseTS := int64(1700000000000)
	n := 1000

	type pair struct {
		ts  int64
		val float64
	}
	points := make([]pair, n)
	for i := 0; i < n; i++ {
		points[i] = pair{
			ts:  baseTS + int64(i)*5000,
			val: 50.0 + 10.0*math.Sin(float64(i)/100.0),
		}
		enc.Write(points[i].ts, points[i].val)
	}

	if enc.Count() != n {
		t.Fatalf("encoder count: got %d, want %d", enc.Count(), n)
	}

	dec := NewDecoder(enc.Bytes())
	for i := 0; i < n; i++ {
		if !dec.Next() {
			t.Fatalf("decoder stopped at %d, err=%v", i, dec.Err())
		}
		ts, val := dec.Values()
		if ts != points[i].ts {
			t.Fatalf("point %d: ts got %d, want %d", i, ts, points[i].ts)
		}
		if val != points[i].val {
			t.Fatalf("point %d: val got %f, want %f", i, val, points[i].val)
		}
	}
	if dec.Next() {
		t.Fatal("decoder returned extra data")
	}
	if dec.Err() != nil {
		t.Fatalf("decoder error: %v", dec.Err())
	}
}

func TestConstantValues(t *testing.T) {
	enc := NewEncoder()
	baseTS := int64(1700000000000)
	n := 500
	for i := 0; i < n; i++ {
		enc.Write(baseTS+int64(i)*5000, 42.0)
	}

	data := enc.Bytes()
	// Constant values should compress extremely well
	rawSize := n * 16
	ratio := float64(rawSize) / float64(len(data))
	t.Logf("constant values: %d points, %d bytes compressed (ratio %.1fx)", n, len(data), ratio)

	dec := NewDecoder(data)
	for i := 0; i < n; i++ {
		if !dec.Next() {
			t.Fatalf("decoder stopped at %d", i)
		}
		ts, val := dec.Values()
		if ts != baseTS+int64(i)*5000 {
			t.Fatalf("point %d: ts mismatch", i)
		}
		if val != 42.0 {
			t.Fatalf("point %d: val got %f, want 42.0", i, val)
		}
	}
}

func TestIrregularTimestamps(t *testing.T) {
	enc := NewEncoder()
	rng := rand.New(rand.NewSource(42))
	ts := int64(1700000000000)
	n := 200

	type pair struct {
		ts  int64
		val float64
	}
	points := make([]pair, n)
	for i := 0; i < n; i++ {
		ts += int64(rng.Intn(30000)) + 1000 // 1–31 second gaps
		points[i] = pair{ts: ts, val: rng.Float64() * 100}
		enc.Write(points[i].ts, points[i].val)
	}

	dec := NewDecoder(enc.Bytes())
	for i := 0; i < n; i++ {
		if !dec.Next() {
			t.Fatalf("decoder stopped at %d, err=%v", i, dec.Err())
		}
		gTS, gVal := dec.Values()
		if gTS != points[i].ts {
			t.Fatalf("point %d: ts got %d, want %d", i, gTS, points[i].ts)
		}
		if gVal != points[i].val {
			t.Fatalf("point %d: val got %f, want %f", i, gVal, points[i].val)
		}
	}
}

func TestSpecialFloatValues(t *testing.T) {
	specials := []float64{
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
		0,
		-0,
		math.MaxFloat64,
		math.SmallestNonzeroFloat64,
	}

	enc := NewEncoder()
	baseTS := int64(1700000000000)
	for i, v := range specials {
		enc.Write(baseTS+int64(i)*5000, v)
	}

	dec := NewDecoder(enc.Bytes())
	for i, want := range specials {
		if !dec.Next() {
			t.Fatalf("decoder stopped at %d, err=%v", i, dec.Err())
		}
		_, val := dec.Values()
		if math.IsNaN(want) {
			if !math.IsNaN(val) {
				t.Fatalf("point %d: expected NaN, got %f", i, val)
			}
		} else if val != want {
			t.Fatalf("point %d: got %f, want %f", i, val, want)
		}
	}
}

func TestSinglePoint(t *testing.T) {
	enc := NewEncoder()
	enc.Write(1700000000000, 3.14)

	if enc.Count() != 1 {
		t.Fatalf("count: got %d, want 1", enc.Count())
	}

	dec := NewDecoder(enc.Bytes())
	if !dec.Next() {
		t.Fatal("no data")
	}
	ts, val := dec.Values()
	if ts != 1700000000000 || val != 3.14 {
		t.Fatalf("got (%d, %f), want (1700000000000, 3.14)", ts, val)
	}
	if dec.Next() {
		t.Fatal("unexpected extra data")
	}
}

func TestEmptySeries(t *testing.T) {
	enc := NewEncoder()
	if enc.Count() != 0 {
		t.Fatalf("count: got %d, want 0", enc.Count())
	}
	data := enc.Bytes()
	if data != nil {
		t.Fatalf("expected nil bytes for empty encoder, got %d bytes", len(data))
	}

	dec := NewDecoder(nil)
	if dec.Next() {
		t.Fatal("expected no data from empty decoder")
	}
}

func TestLargeDataset(t *testing.T) {
	enc := NewEncoder()
	baseTS := int64(1700000000000)
	n := 100000
	rng := rand.New(rand.NewSource(99))

	type pair struct {
		ts  int64
		val float64
	}
	points := make([]pair, n)
	// Realistic metrics: quantized to 2 decimal places, with some value repetition.
	// Real infrastructure metrics (CPU%, memory) don't have full float64 noise.
	val := 50.0
	for i := 0; i < n; i++ {
		if rng.Float64() < 0.3 {
			// 30% chance value changes
			val += rng.NormFloat64() * 2.0
			val = math.Round(val*100) / 100
		}
		points[i] = pair{
			ts:  baseTS + int64(i)*5000,
			val: val,
		}
		enc.Write(points[i].ts, points[i].val)
	}

	data := enc.Bytes()
	rawSize := n * 16
	ratio := float64(rawSize) / float64(len(data))
	t.Logf("large dataset: %d points, %d bytes compressed, ratio %.1fx", n, len(data), ratio)

	if ratio < 5.0 {
		t.Errorf("compression ratio too low: %.1fx (expected >5x)", ratio)
	}

	dec := NewDecoder(data)
	for i := 0; i < n; i++ {
		if !dec.Next() {
			t.Fatalf("decoder stopped at %d, err=%v", i, dec.Err())
		}
		ts, val := dec.Values()
		if ts != points[i].ts {
			t.Fatalf("point %d: ts mismatch", i)
		}
		if val != points[i].val {
			t.Fatalf("point %d: val mismatch", i)
		}
	}
}

func TestLargeDeltas(t *testing.T) {
	enc := NewEncoder()
	// Timestamps with large gaps
	timestamps := []int64{
		1700000000000,
		1700000000000 + 1000000000, // +1 billion ms
		1700000000000 + 1000000000 + 5000,
		1700000000000 + 1000000000 + 10000,
	}
	for _, ts := range timestamps {
		enc.Write(ts, 1.0)
	}

	dec := NewDecoder(enc.Bytes())
	for i, want := range timestamps {
		if !dec.Next() {
			t.Fatalf("decoder stopped at %d, err=%v", i, dec.Err())
		}
		ts, _ := dec.Values()
		if ts != want {
			t.Fatalf("point %d: ts got %d, want %d", i, ts, want)
		}
	}
}

func TestFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(12345))

	for trial := 0; trial < 100; trial++ {
		n := rng.Intn(500) + 1
		enc := NewEncoder()

		type pair struct {
			ts  int64
			val float64
		}
		points := make([]pair, n)
		ts := rng.Int63n(2000000000000)
		for i := 0; i < n; i++ {
			ts += rng.Int63n(60000) + 1
			val := rng.Float64()*200 - 100
			points[i] = pair{ts: ts, val: val}
			enc.Write(ts, val)
		}

		dec := NewDecoder(enc.Bytes())
		for i := 0; i < n; i++ {
			if !dec.Next() {
				t.Fatalf("trial %d, point %d: decoder stopped, err=%v", trial, i, dec.Err())
			}
			gTS, gVal := dec.Values()
			if gTS != points[i].ts || gVal != points[i].val {
				t.Fatalf("trial %d, point %d: mismatch", trial, i)
			}
		}
		if dec.Next() {
			t.Fatalf("trial %d: extra data", trial)
		}
	}
}

func TestCompressionRatioRegularMetrics(t *testing.T) {
	enc := NewEncoder()
	baseTS := int64(1700000000000)
	n := 10000

	// Realistic CPU metrics: 5s interval, integer-like values that repeat often.
	// Real infrastructure metrics are typically whole numbers or have very limited
	// precision (e.g. CPU=45%, memory=8192MB, connections=120).
	val := 45.0
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < n; i++ {
		if rng.Float64() < 0.3 {
			val = math.Floor(val + rng.NormFloat64()*3)
			if val < 0 {
				val = 0
			}
			if val > 100 {
				val = 100
			}
		}
		enc.Write(baseTS+int64(i)*5000, val)
	}

	data := enc.Bytes()
	rawSize := n * 16
	ratio := float64(rawSize) / float64(len(data))
	t.Logf("regular metrics: %d points, raw=%d, compressed=%d, ratio=%.1fx", n, rawSize, len(data), ratio)

	if ratio < 10.0 {
		t.Errorf("expected >10x compression on regular metrics, got %.1fx", ratio)
	}
}

func TestEncoderReset(t *testing.T) {
	enc := NewEncoder()
	enc.Write(1000, 1.0)
	enc.Write(2000, 2.0)

	enc.Reset()
	if enc.Count() != 0 {
		t.Fatalf("count after reset: %d", enc.Count())
	}

	enc.Write(3000, 3.0)
	enc.Write(4000, 4.0)

	dec := NewDecoder(enc.Bytes())
	if !dec.Next() {
		t.Fatal("no first point")
	}
	ts, val := dec.Values()
	if ts != 3000 || val != 3.0 {
		t.Fatalf("first point: (%d, %f)", ts, val)
	}
	if !dec.Next() {
		t.Fatal("no second point")
	}
	ts, val = dec.Values()
	if ts != 4000 || val != 4.0 {
		t.Fatalf("second point: (%d, %f)", ts, val)
	}
}
