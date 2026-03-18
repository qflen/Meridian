// Package main is the entry point for the Meridian time-series database.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "meridian",
	Short: "Meridian — a distributed time-series database",
	Long: `Meridian is a distributed time-series database with Gorilla compression,
a PromQL-subset query engine, hash-ring sharding, and a React dashboard.`,
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
