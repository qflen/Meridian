package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	queryAddr   string
	queryStart  string
	queryEnd    string
	queryStep   string
	queryFormat string
)

var queryCmd = &cobra.Command{
	Use:   "query [promql]",
	Short: "Execute a query from the terminal",
	Args:  cobra.ExactArgs(1),
	RunE:  runQuery,
}

func init() {
	queryCmd.Flags().StringVar(&queryAddr, "addr", "localhost:8080", "Node HTTP address")
	queryCmd.Flags().StringVar(&queryStart, "start", "", "Start time (default: 1h ago)")
	queryCmd.Flags().StringVar(&queryEnd, "end", "", "End time (default: now)")
	queryCmd.Flags().StringVar(&queryStep, "step", "15s", "Step interval")
	queryCmd.Flags().StringVar(&queryFormat, "format", "table", "Output format: table, json, csv")
	rootCmd.AddCommand(queryCmd)
}

func runQuery(cmd *cobra.Command, args []string) error {
	fmt.Printf("Querying %s: %s\n", queryAddr, args[0])
	// Will be wired up in later sections
	return nil
}
