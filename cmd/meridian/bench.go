package main

import (
	"fmt"

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
	fmt.Printf("Running benchmarks with %d samples (pattern: %s)\n", benchSamples, benchPattern)
	// Will be wired up in later sections
	return nil
}
