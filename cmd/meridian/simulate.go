package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "github.com/meridiandb/meridian/internal/ingestion/proto"
	"github.com/meridiandb/meridian/simulator"
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
	interval := 5 * time.Second
	if simRate != "" {
		if d, err := time.ParseDuration(simRate); err == nil {
			interval = d
		}
	}

	profiles := simulator.DefaultProfiles()
	if simHosts < len(profiles) {
		profiles = profiles[:simHosts]
	}

	gen := simulator.NewGenerator(profiles)

	fmt.Printf("Simulator starting: %d hosts, interval %s, target %s\n", len(profiles), interval, simAddr)

	// Connect to ingestion server
	conn, err := net.DialTimeout("tcp", simAddr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", simAddr, err)
	}
	defer conn.Close()

	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	totalSamples := int64(0)
	startTime := time.Now()

	for {
		select {
		case <-sigCh:
			fmt.Printf("\nSimulator stopped. Total samples: %d\n", totalSamples)
			return nil
		case t := <-ticker.C:
			samples := gen.Generate(t)

			req := convertToWriteRequest(samples)
			if err := encoder.Encode(req); err != nil {
				log.Printf("Simulator send error: %v", err)
				continue
			}

			var resp pb.WriteResponse
			if err := decoder.Decode(&resp); err != nil {
				log.Printf("Simulator recv error: %v", err)
				continue
			}

			totalSamples += resp.SamplesIngested
			elapsed := time.Since(startTime)
			rate := float64(totalSamples) / elapsed.Seconds()
			fmt.Printf("\rSamples: %d | Rate: %.1f/s | Elapsed: %s", totalSamples, rate, elapsed.Truncate(time.Second))
		}
	}
}

func convertToWriteRequest(samples []simulator.MetricSample) pb.WriteRequest {
	// Group by metric name + labels
	groups := make(map[string]*pb.TimeSeries)
	for _, s := range samples {
		key := s.Name
		for k, v := range s.Labels {
			key += k + v
		}
		ts, ok := groups[key]
		if !ok {
			labels := make([]pb.Label, 0, len(s.Labels))
			for k, v := range s.Labels {
				labels = append(labels, pb.Label{Name: k, Value: v})
			}
			ts = &pb.TimeSeries{
				Name:   s.Name,
				Labels: labels,
			}
			groups[key] = ts
		}
		ts.Samples = append(ts.Samples, pb.Sample{
			TimestampMs: s.Timestamp,
			Value:       s.Value,
		})
	}

	req := pb.WriteRequest{}
	for _, ts := range groups {
		req.TimeSeries = append(req.TimeSeries, *ts)
	}
	return req
}
