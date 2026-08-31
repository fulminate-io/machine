package ledger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

func TestLedgerCompactsAndRestoresUnderSnapshotTriggers(t *testing.T) {
	dir := t.TempDir()
	mux := testMux(t)

	// THE LOWERED TRIGGERS LIVE IN THE TEST. Production keeps raft's defaults, which
	// a sibling gate pins as literals; a snapshot under those would take minutes and
	// thousands of entries to observe. raft refuses a SnapshotInterval below 5ms.
	cfg := Config{
		Flow: "flow-compact", LocalID: "n0", Mux: mux, Dir: dir, Bootstrap: true,
		tuning: func(c *raft.Config) {
			fastElections(c)
			c.SnapshotInterval = 20 * time.Millisecond
			c.SnapshotThreshold = 8
			c.TrailingLogs = 4
		},
	}

	first, err := Open(cfg)
	if err != nil {
		t.Fatalf("opening the ledger: %v", err)
	}
	waitLeadership(t, first)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	const writes = 40
	for i := range writes {
		if err := first.Store().Save(ctx, fmt.Sprintf("heap/entry-%d", i), fmt.Sprintf("value-%d", i)); err != nil {
			t.Fatalf("saving entry-%d: %v", i, err)
		}
	}

	// Wait for raft's own trigger to fire. This is a REAL snapshot taken by raft on
	// its schedule, not a snapshot this test asked for.
	snapshotIndex := awaitSnapshot(t, first)
	if snapshotIndex == 0 {
		t.Fatal("no snapshot was taken within the deadline")
	}

	// Compaction really truncated the log: the first surviving index has moved past
	// the beginning, so a reopen cannot replay the whole journal from the log alone.
	firstIndex, err := first.bolt.FirstIndex()
	if err != nil {
		t.Fatalf("reading the log's first index: %v", err)
	}
	if firstIndex <= 1 {
		t.Fatalf("the log still begins at index %d after a snapshot at %d: nothing was compacted, so a reopen would replay rather than restore", firstIndex, snapshotIndex)
	}
	t.Logf("snapshot at index %d; the log now begins at index %d", snapshotIndex, firstIndex)

	// A completed snapshot is on disk under the ledger's own Dir.
	entries, err := os.ReadDir(filepath.Join(dir, "snapshots"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no snapshot directory under the ledger's Dir (%d entries, err %v)", len(entries), err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("closing the ledger: %v", err)
	}

	// Reopened against the same Dir, the ledger comes back from the SNAPSHOT for
	// everything below firstIndex, since those log entries no longer exist.
	second, err := Open(cfg)
	if err != nil {
		t.Fatalf("reopening against the same Dir: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	waitLeadership(t, second)

	if applied := second.fsm.appliedIndex(); applied < snapshotIndex {
		t.Fatalf("the reopened ledger tracks applied index %d, below the snapshot's %d: it did not restore the snapshot's index", applied, snapshotIndex)
	}
	for i := range writes {
		value, ok, err := second.Store().Load(ctx, fmt.Sprintf("heap/entry-%d", i))
		if err != nil || !ok {
			t.Fatalf("after the reopen heap/entry-%d loaded present=%v err=%v", i, ok, err)
		}
		if want := fmt.Sprintf("value-%d", i); value != want {
			t.Fatalf("after the reopen heap/entry-%d holds %v, want %q", i, value, want)
		}
	}

	// CONTROL: a ledger over a FRESH Dir holds none of this, so the values above
	// came off disk rather than being something every ledger reports.
	fresh := openTestLedger(t, Config{Flow: "flow-compact-fresh", LocalID: "n0", Mux: mux, Dir: t.TempDir(), Bootstrap: true})
	waitLeadership(t, fresh)
	if _, ok, err := fresh.Store().Load(ctx, "heap/entry-0"); err != nil || ok {
		t.Fatalf("CONTROL FAILED: a ledger over a fresh Dir already holds heap/entry-0 (present=%v err=%v)", ok, err)
	}
}

// awaitSnapshot polls raft's own stats until it reports a snapshot, and returns the
// index it was taken at.
func awaitSnapshot(t *testing.T, l *Ledger) uint64 {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		raw := l.raft.Stats()["last_snapshot_index"]
		index, err := strconv.ParseUint(raw, 10, 64)
		if err == nil && index > 0 {
			return index
		}
		time.Sleep(20 * time.Millisecond)
	}

	return 0
}
