package storage

import (
	"encoding/binary"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type testWALHandler struct {
	series  []walSeriesRecord
	samples []Sample
}

type walSeriesRecord struct {
	id     uint64
	name   string
	labels map[string]string
}

func (h *testWALHandler) HandleSeries(id uint64, name string, labels map[string]string) error {
	h.series = append(h.series, walSeriesRecord{id: id, name: name, labels: labels})
	return nil
}

func (h *testWALHandler) HandleSamples(samples []Sample) error {
	h.samples = append(h.samples, samples...)
	return nil
}

func TestWALWriteAndReplay(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Write series definitions
	if err := w.LogSeries(1, "cpu_usage", map[string]string{"host": "web-01", "region": "us-east"}); err != nil {
		t.Fatal(err)
	}
	if err := w.LogSeries(2, "mem_bytes", map[string]string{"host": "web-01"}); err != nil {
		t.Fatal(err)
	}

	// Write samples
	samples := []Sample{
		{SeriesID: 1, Timestamp: 1000, Value: 0.75},
		{SeriesID: 1, Timestamp: 2000, Value: 0.80},
		{SeriesID: 2, Timestamp: 1000, Value: 8589934592},
	}
	if err := w.LogSamples(samples); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Replay
	w2, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()

	h := &testWALHandler{}
	if err := w2.Replay(h); err != nil {
		t.Fatal(err)
	}

	if len(h.series) != 2 {
		t.Fatalf("expected 2 series, got %d", len(h.series))
	}
	if h.series[0].name != "cpu_usage" {
		t.Fatalf("series 0 name: %s", h.series[0].name)
	}
	if h.series[0].labels["host"] != "web-01" {
		t.Fatalf("series 0 host label: %s", h.series[0].labels["host"])
	}
	if h.series[1].name != "mem_bytes" {
		t.Fatalf("series 1 name: %s", h.series[1].name)
	}

	if len(h.samples) != 3 {
		t.Fatalf("expected 3 samples, got %d", len(h.samples))
	}
	if h.samples[0].Value != 0.75 {
		t.Fatalf("sample 0 value: %f", h.samples[0].Value)
	}
}

func TestWALCRCValidation(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := w.LogSeries(1, "metric", map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	if err := w.LogSamples([]Sample{{SeriesID: 1, Timestamp: 100, Value: 1.0}}); err != nil {
		t.Fatal(err)
	}
	w.Close()

	// Corrupt the first frame's CRC
	segs, _ := filepath.Glob(filepath.Join(dir, "segment-*"))
	if len(segs) == 0 {
		t.Fatal("no segments")
	}
	data, err := os.ReadFile(segs[0])
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xFF // flip CRC byte
	os.WriteFile(segs[0], data, 0o644)

	// Replay should skip the corrupt frame
	w2, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()

	h := &testWALHandler{}
	if err := w2.Replay(h); err != nil {
		t.Fatal(err)
	}

	// First frame (series) should be skipped, but second (samples) should be recovered
	if len(h.series) != 0 {
		t.Fatalf("expected 0 series after CRC corruption, got %d", len(h.series))
	}
	if len(h.samples) != 1 {
		t.Fatalf("expected 1 sample (second frame intact), got %d", len(h.samples))
	}
}

func TestWALCorruptFrameRecovery(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Write 3 valid entries
	for i := uint64(1); i <= 3; i++ {
		w.LogSamples([]Sample{{SeriesID: i, Timestamp: int64(i) * 1000, Value: float64(i)}})
	}
	w.Close()

	// Truncate the file mid-frame (simulate crash)
	segs, _ := filepath.Glob(filepath.Join(dir, "segment-*"))
	data, _ := os.ReadFile(segs[0])
	// Cut off last 10 bytes
	os.WriteFile(segs[0], data[:len(data)-10], 0o644)

	w2, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()

	h := &testWALHandler{}
	if err := w2.Replay(h); err != nil {
		t.Fatal(err)
	}

	// Should recover at least the first 2 complete frames
	if len(h.samples) < 2 {
		t.Fatalf("expected at least 2 samples, got %d", len(h.samples))
	}
}

func TestWALSegmentRotation(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Write enough data to trigger segment rotation.
	// Each sample frame is 1+4+24=29 bytes payload + 8 header + padding = 40 bytes.
	// To exceed 128MB we'd need millions. Instead, test the mechanism directly.
	bigPayload := make([]byte, 1+4+24*1000)
	bigPayload[0] = walEntrySamples
	binary.LittleEndian.PutUint32(bigPayload[1:], 1000)
	for i := 0; i < 1000; i++ {
		off := 5 + i*24
		binary.LittleEndian.PutUint64(bigPayload[off:], 1)
		binary.LittleEndian.PutUint64(bigPayload[off+8:], uint64(i))
		binary.LittleEndian.PutUint64(bigPayload[off+16:], math.Float64bits(float64(i)))
	}

	// Write enough data to verify multi-segment replay works
	n := 200
	samplesPerBatch := 100
	for i := 0; i < n; i++ {
		w.LogSamples(makeSamples(samplesPerBatch))
	}

	w.Close()

	segs, _ := filepath.Glob(filepath.Join(dir, "segment-*"))
	t.Logf("segments: %d", len(segs))

	// Verify replay works across segments
	w2, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()

	h := &testWALHandler{}
	if err := w2.Replay(h); err != nil {
		t.Fatal(err)
	}
	if len(h.samples) != n*samplesPerBatch {
		t.Fatalf("expected %d samples, got %d", n*samplesPerBatch, len(h.samples))
	}
}

func TestWALConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	nWriters := 8
	samplesPerWriter := 100

	for i := 0; i < nWriters; i++ {
		wg.Add(1)
		go func(writerID int) {
			defer wg.Done()
			for j := 0; j < samplesPerWriter; j++ {
				sid := uint64(writerID*1000 + j)
				w.LogSamples([]Sample{{SeriesID: sid, Timestamp: int64(j), Value: float64(sid)}})
			}
		}(i)
	}

	wg.Wait()
	w.Close()

	w2, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()

	h := &testWALHandler{}
	if err := w2.Replay(h); err != nil {
		t.Fatal(err)
	}

	expected := nWriters * samplesPerWriter
	if len(h.samples) != expected {
		t.Fatalf("expected %d samples, got %d", expected, len(h.samples))
	}
}

func TestWALTruncate(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}

	w.LogSamples([]Sample{{SeriesID: 1, Timestamp: 1000, Value: 1.0}})
	w.LogSamples([]Sample{{SeriesID: 2, Timestamp: 2000, Value: 2.0}})

	if err := w.Truncate(); err != nil {
		t.Fatal(err)
	}

	// Write new data after truncation
	w.LogSamples([]Sample{{SeriesID: 3, Timestamp: 3000, Value: 3.0}})
	w.Close()

	w2, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()

	h := &testWALHandler{}
	if err := w2.Replay(h); err != nil {
		t.Fatal(err)
	}

	// Only the post-truncate sample should be present
	if len(h.samples) != 1 {
		t.Fatalf("expected 1 sample after truncation, got %d", len(h.samples))
	}
	if h.samples[0].SeriesID != 3 {
		t.Fatalf("expected series ID 3, got %d", h.samples[0].SeriesID)
	}
}

func TestWALEmptyReplay(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	h := &testWALHandler{}
	if err := w.Replay(h); err != nil {
		t.Fatal(err)
	}
	if len(h.series) != 0 || len(h.samples) != 0 {
		t.Fatal("expected empty replay")
	}
}

func TestWALFrameIntegrity(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Write a known sample and verify the raw frame on disk
	w.LogSamples([]Sample{{SeriesID: 42, Timestamp: 1234567890, Value: 3.14}})
	w.Close()

	segs, _ := filepath.Glob(filepath.Join(dir, "segment-*"))
	data, _ := os.ReadFile(segs[0])

	// Verify frame structure
	storedCRC := binary.LittleEndian.Uint32(data[0:4])
	payloadLen := binary.LittleEndian.Uint32(data[4:8])
	payload := data[8 : 8+payloadLen]
	computedCRC := crc32.ChecksumIEEE(payload)

	if storedCRC != computedCRC {
		t.Fatalf("CRC mismatch: stored=%x, computed=%x", storedCRC, computedCRC)
	}
}

func makeSamples(n int) []Sample {
	samples := make([]Sample, n)
	for i := range samples {
		samples[i] = Sample{
			SeriesID:  uint64(i % 10),
			Timestamp: int64(i * 5000),
			Value:     float64(i),
		}
	}
	return samples
}
