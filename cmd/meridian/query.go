package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

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
	promql := args[0]

	now := time.Now().UnixMilli()
	start := now - 3600000
	end := now

	if queryStart != "" {
		if v, err := strconv.ParseInt(queryStart, 10, 64); err == nil {
			start = v
		}
	}
	if queryEnd != "" {
		if v, err := strconv.ParseInt(queryEnd, 10, 64); err == nil {
			end = v
		}
	}

	u := fmt.Sprintf("http://%s/api/v1/query?q=%s&start=%d&end=%d&step=%s",
		queryAddr, url.QueryEscape(promql), start, end, queryStep)

	resp, err := http.Get(u)
	if err != nil {
		return fmt.Errorf("query failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	switch queryFormat {
	case "json":
		fmt.Println(string(body))
	case "csv":
		return printCSV(body)
	default:
		return printTable(body)
	}
	return nil
}

func printTable(body []byte) error {
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}

	if status, ok := result["status"].(string); ok && status == "error" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", result["error"])
		return nil
	}

	data, ok := result["data"].(map[string]interface{})
	if !ok {
		fmt.Println("No data")
		return nil
	}

	series, ok := data["result"].([]interface{})
	if !ok || len(series) == 0 {
		fmt.Println("No results")
		return nil
	}

	if execTime, ok := result["exec_time"].(string); ok {
		fmt.Printf("Execution time: %s\n\n", execTime)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "SERIES\tTIMESTAMP\tVALUE\n")
	fmt.Fprintf(w, "------\t---------\t-----\n")

	for _, s := range series {
		sm := s.(map[string]interface{})
		name := fmt.Sprintf("%v", sm["name"])
		labels := ""
		if l, ok := sm["labels"].(map[string]interface{}); ok {
			for k, v := range l {
				if k == "__name__" {
					continue
				}
				if labels != "" {
					labels += ","
				}
				labels += fmt.Sprintf("%s=%v", k, v)
			}
		}
		seriesName := name
		if labels != "" {
			seriesName += "{" + labels + "}"
		}

		if values, ok := sm["values"].([]interface{}); ok {
			for _, v := range values {
				pair := v.([]interface{})
				ts := int64(pair[0].(float64))
				val := pair[1].(float64)
				t := time.UnixMilli(ts).Format("15:04:05")
				fmt.Fprintf(w, "%s\t%s\t%.4f\n", seriesName, t, val)
			}
		}
	}
	w.Flush()
	return nil
}

func printCSV(body []byte) error {
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}

	data, ok := result["data"].(map[string]interface{})
	if !ok {
		return nil
	}
	series, ok := data["result"].([]interface{})
	if !ok {
		return nil
	}

	fmt.Println("series,timestamp,value")
	for _, s := range series {
		sm := s.(map[string]interface{})
		name := fmt.Sprintf("%v", sm["name"])
		if values, ok := sm["values"].([]interface{}); ok {
			for _, v := range values {
				pair := v.([]interface{})
				fmt.Printf("%s,%d,%.6f\n", name, int64(pair[0].(float64)), pair[1].(float64))
			}
		}
	}
	return nil
}
