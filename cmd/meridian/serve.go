package main

import (
	"fmt"
	"log"

	"github.com/meridiandb/meridian/internal/config"
	"github.com/spf13/cobra"
)

var (
	configPath string
	dataDir    string
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start a Meridian node",
	RunE:  runServe,
}

func init() {
	serveCmd.Flags().StringVar(&configPath, "config", "meridian.yaml", "Path to config file")
	serveCmd.Flags().StringVar(&dataDir, "data-dir", "", "Data directory (overrides config)")
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Printf("Warning: could not load config file %s: %v, using defaults", configPath, err)
		cfg = config.DefaultConfig()
	}

	if dataDir != "" {
		cfg.Storage.DataDir = dataDir
		cfg.Storage.WALDir = dataDir + "/wal"
	}

	fmt.Printf("Meridian node starting (HTTP %s, gRPC %s)\n", cfg.Server.HTTPAddr, cfg.Server.GRPCAddr)

	// Will be wired up in later sections
	select {}
}
