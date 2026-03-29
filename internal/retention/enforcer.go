// Package retention implements TTL-based block deletion and automatic downsampling.
package retention

import (
	"log"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// Enforcer periodically deletes blocks whose data has exceeded the retention period.
type Enforcer struct {
	db        *storage.TSDB
	retention time.Duration
	ticker    *time.Ticker
	done      chan struct{}
}

// NewEnforcer creates a retention enforcer that runs every 5 minutes.
func NewEnforcer(db *storage.TSDB, retention time.Duration) *Enforcer {
	return &Enforcer{
		db:        db,
		retention: retention,
		done:      make(chan struct{}),
	}
}

// Start begins the retention enforcement loop.
func (e *Enforcer) Start() {
	e.ticker = time.NewTicker(5 * time.Minute)
	go e.loop()
}

// Stop halts the retention enforcement loop.
func (e *Enforcer) Stop() {
	close(e.done)
	if e.ticker != nil {
		e.ticker.Stop()
	}
}

// Enforce runs a single retention check, deleting expired blocks.
func (e *Enforcer) Enforce() int {
	cutoff := time.Now().UnixMilli() - e.retention.Milliseconds()
	deleted := 0

	for _, block := range e.db.Blocks() {
		meta := block.Meta()
		if meta.MaxTime < cutoff {
			log.Printf("Retention: deleting block %s (max_time=%d < cutoff=%d)", meta.ULID, meta.MaxTime, cutoff)
			if err := e.db.DeleteBlock(meta.ULID); err != nil {
				log.Printf("Retention: error deleting block %s: %v", meta.ULID, err)
				continue
			}
			deleted++
		}
	}
	return deleted
}

func (e *Enforcer) loop() {
	for {
		select {
		case <-e.done:
			return
		case <-e.ticker.C:
			e.Enforce()
		}
	}
}
