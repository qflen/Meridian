package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	simAddr  string
	simHosts int
	simRate  string
)

var simulateCmd = &cobra.Command{
	Use:   "simulate",
	Short: "Start the metric simulator",
	RunE:  runSimulate,
}

func init() {
	simulateCmd.Flags().StringVar(&simAddr, "addr", "localhost:9090", "Target Meridian gRPC address")
	simulateCmd.Flags().IntVar(&simHosts, "hosts", 8, "Number of simulated hosts")
	simulateCmd.Flags().StringVar(&simRate, "rate", "5s", "Emission interval")
	rootCmd.AddCommand(simulateCmd)
}

func runSimulate(cmd *cobra.Command, args []string) error {
	fmt.Printf("Starting simulator: %d hosts, interval %s, target %s\n", simHosts, simRate, simAddr)
	// Will be wired up in later sections
	return nil
}
