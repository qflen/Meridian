package storage

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// benchmarkWALConcurrent drives exactly b.N single-frame writes spread across a fixed
// number of concurrent writers, so the concurrency level is independent of GOMAXPROCS.
// It reports frames/fsync alongside ns/op, making the coalescing visible: the
// synchronous path fsyncs once per frame (frames/fsync == 1), while group commit
// shares one fsync across every frame submitted during the prior fsync.
func benchmarkWALConcurrent(b *testing.B, opts WALOptions, writers int) {
	dir := b.TempDir()
	w, err := OpenWALWithOptions(dir, opts)
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()

	remaining := int64(b.N)
	before := w.fsyncs.Load()
	b.ResetTimer()

	var wg sync.WaitGroup
	for k := 0; k < writers; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			s := []Sample{{SeriesID: uint64(k), Timestamp: 1, Value: 1}}
			// Pull work from a shared counter so the writers perform exactly b.N ops in
			// total regardless of how unevenly the scheduler distributes them.
			for atomic.AddInt64(&remaining, -1) >= 0 {
				if err := w.LogSamples(s); err != nil {
					b.Error(err)
					return
				}
			}
		}(k)
	}
	wg.Wait()

	b.StopTimer()
	if fsyncs := w.fsyncs.Load() - before; fsyncs > 0 {
		b.ReportMetric(float64(b.N)/float64(fsyncs), "frames/fsync")
	}
}

// BenchmarkWALConcurrentWrite compares synchronous and group-commit throughput under
// concurrent writers. Group commit's win grows with concurrency, since more frames
// land in each coalesced fsync.
func BenchmarkWALConcurrentWrite(b *testing.B) {
	variants := []struct {
		name string
		opts WALOptions
	}{
		{"sync", WALOptions{}},
		{"group", WALOptions{GroupCommit: true}},
		{"group-linger200us", WALOptions{GroupCommit: true, Linger: 200 * time.Microsecond}},
	}
	for _, writers := range []int{8, 64} {
		for _, v := range variants {
			b.Run(fmt.Sprintf("writers=%d/%s", writers, v.name), func(b *testing.B) {
				benchmarkWALConcurrent(b, v.opts, writers)
			})
		}
	}
}
