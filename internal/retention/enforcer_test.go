package retention

import (
	"testing"
	"time"

	"github.com/meridiandb/meridian/internal/storage"
)

// ingestBlock writes one raw block of two series spanning [from, from+dur) at 15s
// spacing, then seals it.
func ingestBlock(t *testing.T, db *storage.TSDB, from int64, dur time.Duration) {
	t.Helper()
	end := from + dur.Milliseconds()
	for _, host := range []string{"a", "b"} {
		for ts := from; ts < end; ts += 15000 {
			if err := db.Ingest("cpu", map[string]string{"host": host}, ts, float64(ts/1000%50)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
}

func realCascadeRules() []DownsampleRule {
	return []DownsampleRule{
		{SourceInterval: 5 * time.Second, TargetInterval: time.Minute, Retention: 7 * 24 * time.Hour},
		{SourceInterval: time.Minute, TargetInterval: time.Hour, Retention: 30 * 24 * time.Hour},
	}
}

func rollupTTLs(oneM, oneH time.Duration) map[int64]time.Duration {
	return map[int64]time.Duration{
		time.Minute.Milliseconds(): oneM,
		time.Hour.Milliseconds():   oneH,
	}
}

// TestRawExpiresWhileRollupsSurvive is the Tier-3 requirement: with a short raw TTL and
// longer rollup TTLs, an old raw block (already captured by the rollup tiers) is deleted
// while the 1m and 1h rollups it fed are retained.
func TestRawExpiresWhileRollupsSurvive(t *testing.T) {
	dir := t.TempDir()
	db := openManualDB(t, dir)
	defer db.Close()

	twoDaysAgo := time.Now().UnixMilli() - 2*24*time.Hour.Milliseconds()
	ingestBlock(t, db, twoDaysAgo, 5*time.Minute)           // old block, fully coverable
	ingestBlock(t, db, twoDaysAgo+time.Hour.Milliseconds(), time.Minute) // pushes the frontier past it

	NewDownsampler(db, realCascadeRules(), time.Hour).Downsample()
	if len(db.RollupBlocks(time.Minute.Milliseconds())) == 0 {
		t.Fatal("expected 1m rollups to exist before enforcement")
	}
	rawBefore := len(db.Blocks())

	enf := NewEnforcerWithTiers(db, time.Hour, rollupTTLs(7*24*time.Hour, 30*24*time.Hour), time.Hour)
	if deleted := enf.Enforce(); deleted == 0 {
		t.Fatal("expected at least the old raw block to be deleted")
	}

	if len(db.Blocks()) >= rawBefore {
		t.Fatalf("raw blocks not expired: %d (was %d)", len(db.Blocks()), rawBefore)
	}
	if len(db.RollupBlocks(time.Minute.Milliseconds())) == 0 {
		t.Fatal("1m rollups must survive their (longer) TTL")
	}
	if len(db.RollupBlocks(time.Hour.Milliseconds())) == 0 {
		t.Fatal("1h rollups must survive their (longer) TTL")
	}
}

// TestRollupTierExpiry shows a coarser tier outliving a finer one: with raw and 1m past
// their TTLs but 1h within its, only the 1h tier remains.
func TestRollupTierExpiry(t *testing.T) {
	dir := t.TempDir()
	db := openManualDB(t, dir)
	defer db.Close()

	twoDaysAgo := time.Now().UnixMilli() - 2*24*time.Hour.Milliseconds()
	ingestBlock(t, db, twoDaysAgo, 5*time.Minute)
	ingestBlock(t, db, twoDaysAgo+time.Hour.Milliseconds(), time.Minute)
	NewDownsampler(db, realCascadeRules(), time.Hour).Downsample()

	// raw TTL 1h, 1m TTL 1h (expired at 2 days), 1h TTL 30d (retained).
	enf := NewEnforcerWithTiers(db, time.Hour, rollupTTLs(time.Hour, 30*24*time.Hour), time.Hour)
	enf.Enforce()

	if got := len(db.RollupBlocks(time.Minute.Milliseconds())); got != 0 {
		t.Fatalf("1m tier should have expired, found %d blocks", got)
	}
	if got := len(db.RollupBlocks(time.Hour.Milliseconds())); got == 0 {
		t.Fatal("1h tier should be retained within its TTL")
	}
}

// TestRawNotDeletedBeforeDownsampled verifies the safety guard: when downsampling is on
// but a raw block has not been captured by the finest tier yet, it is kept past the raw
// TTL; with downsampling off, the same block is a pure-TTL deletion.
func TestRawNotDeletedBeforeDownsampled(t *testing.T) {
	dir := t.TempDir()
	db := openManualDB(t, dir)
	defer db.Close()

	old := time.Now().UnixMilli() - 2*24*time.Hour.Milliseconds()
	ingestBlock(t, db, old, 5*time.Minute) // never downsampled

	// Downsampling configured, but no rollups generated → finest covered-through is 0,
	// so the block is not eligible despite being long past the raw TTL.
	guarded := NewEnforcerWithTiers(db, time.Millisecond, rollupTTLs(7*24*time.Hour, 30*24*time.Hour), time.Hour)
	if deleted := guarded.Enforce(); deleted != 0 {
		t.Fatalf("guard failed: deleted %d un-downsampled raw blocks", deleted)
	}
	if len(db.Blocks()) == 0 {
		t.Fatal("un-downsampled raw block must be retained")
	}

	// Raw-only enforcer has no guard: the same block is deleted on TTL alone.
	rawOnly := NewEnforcer(db, time.Millisecond)
	if deleted := rawOnly.Enforce(); deleted == 0 {
		t.Fatal("raw-only enforcer should delete the expired block")
	}
	if len(db.Blocks()) != 0 {
		t.Fatalf("raw-only enforcer left %d blocks", len(db.Blocks()))
	}
}
