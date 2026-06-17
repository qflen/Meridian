package storage

import (
	"testing"
	"time"
)

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestLabelNamesAndValuesUnionBlocksAfterFlush(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultTSDBOptions()
	opts.WALDir = dir + "/wal"
	opts.BlockDir = dir + "/blocks"
	opts.FlushInterval = time.Hour
	db, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i := 0; i < 10; i++ {
		if err := db.Ingest("cpu", map[string]string{"host": "web-01", "region": "us-east"}, int64(i)*1000, float64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}

	// After flush the head is empty; the labels now live only in the block.
	if db.Head().SampleCount() != 0 {
		t.Fatalf("head not empty after flush: %d", db.Head().SampleCount())
	}

	names := db.LabelNames()
	for _, want := range []string{"__name__", "host", "region"} {
		if !containsStr(names, want) {
			t.Errorf("LabelNames missing %q after flush; got %v", want, names)
		}
	}
	if vals := db.LabelValues("host"); !containsStr(vals, "web-01") {
		t.Errorf("LabelValues(host) missing web-01 after flush; got %v", vals)
	}
	if vals := db.LabelValues("region"); !containsStr(vals, "us-east") {
		t.Errorf("LabelValues(region) missing us-east after flush; got %v", vals)
	}
}
