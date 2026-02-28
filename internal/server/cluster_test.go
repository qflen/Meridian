package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

func openTempDB(t *testing.T) *storage.TSDB {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(dir, storage.TSDBOptions{
		WALDir:             dir + "/wal",
		BlockDir:           dir + "/blocks",
		FlushInterval:      time.Hour,
		RateSampleInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func clusterNodes(t *testing.T, s *HTTPServer) []map[string]interface{} {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleCluster(rec, httptest.NewRequest("GET", "/api/v1/cluster", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/v1/cluster status = %d", rec.Code)
	}
	var resp struct {
		Nodes []map[string]interface{} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode cluster response: %v", err)
	}
	return resp.Nodes
}

func TestClusterSingleNodeIsHonest(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()
	for i := 0; i < 5; i++ {
		if err := db.Ingest("cpu", map[string]string{"host": "a"}, int64(i)*1000, 1); err != nil {
			t.Fatal(err)
		}
	}

	s := NewHTTPServer(db, "node-1", nil) // no peers configured
	nodes := clusterNodes(t, s)

	if len(nodes) != 1 {
		t.Fatalf("single-node cluster returned %d nodes, want exactly 1 (no fabricated peers)", len(nodes))
	}
	n := nodes[0]
	if n["id"] != "node-1" || n["state"] != "active" {
		t.Fatalf("self node = %v", n)
	}
	if got := int(n["series"].(float64)); got != db.Stats().TotalSeries {
		t.Fatalf("self series = %d, want real %d", got, db.Stats().TotalSeries)
	}
}

func fakePeer(nodeID string, series int, samples int64, healthy bool) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if !healthy {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "node_id": nodeID})
	})
	mux.HandleFunc("/api/v1/stats", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"total_series": series, "total_samples": samples})
	})
	return httptest.NewServer(mux)
}

func TestClusterReportsRealPeerStats(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()

	live := fakePeer("peer-live", 7, 700, true)
	defer live.Close()
	dead := fakePeer("peer-dead", 0, 0, false)
	defer dead.Close()

	liveAddr := strings.TrimPrefix(live.URL, "http://")
	deadAddr := strings.TrimPrefix(dead.URL, "http://")
	s := NewHTTPServer(db, "self-1", []string{liveAddr, deadAddr})

	nodes := clusterNodes(t, s)
	if len(nodes) != 3 {
		t.Fatalf("expected self + 2 peers = 3 nodes, got %d", len(nodes))
	}

	byID := map[string]map[string]interface{}{}
	for _, n := range nodes {
		byID[n["id"].(string)] = n
	}

	livePeer := byID["peer-live"]
	if livePeer == nil || livePeer["state"] != "active" {
		t.Fatalf("live peer not active: %v", livePeer)
	}
	if got := int(livePeer["series"].(float64)); got != 7 {
		t.Errorf("live peer series = %d, want real 7 (not fabricated 0)", got)
	}
	if got := int64(livePeer["samples"].(float64)); got != 700 {
		t.Errorf("live peer samples = %d, want real 700", got)
	}

	deadPeer := byID[deadAddr]
	if deadPeer == nil || deadPeer["state"] != "dead" {
		t.Fatalf("dead peer not reported dead: %v", deadPeer)
	}
}
