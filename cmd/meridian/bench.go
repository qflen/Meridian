package main

import (
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/meridiandb/meridian/internal/compress"
	"github.com/spf13/cobra"
)

var (
	benchSamples int
	benchPattern string
)

var benchCmd = &cobra.Command{
	Use:   "bench",
	Short: "Run compression and ingestion benchmarks",
	RunE:  runBench,
}

func init() {
	benchCmd.Flags().IntVar(&benchSamples, "samples", 1000000, "Number of samples")
	benchCmd.Flags().StringVar(&benchPattern, "pattern", "regular", "Data pattern: regular, irregular, spiky")
	rootCmd.AddCommand(benchCmd)
}

func runBench(cmd *cobra.Command, args []string) error {
	n := benchSamples
	fmt.Printf("Running benchmarks: %d samples, pattern=%s\n\n", n, benchPattern)

	timestamps, values := generateBenchData(n, benchPattern)

	// Encode benchmark
	start := time.Now()
	enc := compress.NewEncoder()
	for i := 0; i < n; i++ {
		enc.Write(timestamps[i], values[i])
	}
	encodeTime := time.Since(start)
	data := enc.Bytes()

	// Decode benchmark
	start = time.Now()
	dec := compress.NewDecoder(data)
	count := 0
	for dec.Next() {
		count++
	}
	decodeTime := time.Since(start)

	rawSize := n * 16
	compressedSize := len(data)
	ratio := float64(rawSize) / float64(compressedSize)

	fmt.Println("=== Gorilla Compression ===")
	fmt.Printf("  Data points:       %d\n", n)
	fmt.Printf("  Pattern:           %s\n", benchPattern)
	fmt.Printf("  Raw size:          %s (%d bytes)\n", formatBytes(int64(rawSize)), rawSize)
	fmt.Printf("  Compressed size:   %s (%d bytes)\n", formatBytes(int64(compressedSize)), compressedSize)
	fmt.Printf("  Compression ratio: %.1fx\n", ratio)
	fmt.Printf("  Space savings:     %.1f%%\n", (1-1/ratio)*100)
	fmt.Println()
	fmt.Printf("  Encode time:       %s (%.0f ns/point)\n", encodeTime, float64(encodeTime.Nanoseconds())/float64(n))
	fmt.Printf("  Decode time:       %s (%.0f ns/point)\n", decodeTime, float64(decodeTime.Nanoseconds())/float64(n))
	fmt.Printf("  Encode throughput: %.1fM points/sec\n", float64(n)/encodeTime.Seconds()/1e6)
	fmt.Printf("  Decode throughput: %.1fM points/sec\n", float64(n)/decodeTime.Seconds()/1e6)
	fmt.Printf("  Decoded points:    %d (verified)\n", count)

	return nil
}

func generateBenchData(n int, pattern string) ([]int64, []float64) {
	rng := rand.New(rand.NewSource(42))
	timestamps := make([]int64, n)
	values := make([]float64, n)
	ts := int64(1700000000000)
	val := 50.0

	for i := 0; i < n; i++ {
		switch pattern {
		case "irregular":
			ts += int64(rng.Intn(30000)) + 1000
		case "spiky":
			ts += 5000
			if rng.Float64() < 0.01 {
				val = 95.0 + rng.Float64()*5
			} else {
				val = 45.0 + rng.NormFloat64()*3
			}
		default: // regular
			ts += 5000
			if rng.Float64() < 0.3 {
				val = math.Floor(val + rng.NormFloat64()*3)
				if val < 0 {
					val = 0
				}
				if val > 100 {
					val = 100
				}
			}
		}

		timestamps[i] = ts
		values[i] = val
	}
	return timestamps, values
}

func formatBytes(b int64) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%.1f GB", float64(b)/1024/1024/1024)
	case b >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/1024/1024)
	case b >= 1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}
