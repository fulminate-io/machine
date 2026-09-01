package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/transport"
)

// fastElections shortens raft's timers so a test elects in milliseconds. It is the
// only thing tests here change about the raft config: the compaction triggers stay
// at raft's defaults, which are the ruled production values.
func fastElections(c *raft.Config) {
	c.HeartbeatTimeout = 200 * time.Millisecond
	c.ElectionTimeout = 200 * time.Millisecond
	c.LeaderLeaseTimeout = 100 * time.Millisecond
	c.CommitTimeout = 20 * time.Millisecond
}

func testMux(t *testing.T) *transport.Mux {
	t.Helper()
	m, err := transport.New(transport.Config{BindAddr: "127.0.0.1:0", RPCTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("binding a test mux: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	return m
}

// openTestLedger opens a ledger with fast elections and closes it at test end.
func openTestLedger(t *testing.T, cfg Config) *Ledger {
	t.Helper()
	cfg.tuning = fastElections
	l, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open(%+v): %v", cfg.Flow, err)
	}
	t.Cleanup(func() { closeWithin(t, l, cleanupCloseCeiling) })

	return l
}

// cleanupCloseCeiling bounds a cleanup's Close. A correct Close returns in
// microseconds, so this costs correct work nothing.
const cleanupCloseCeiling = 30 * time.Second

// closeWithin runs Close in a goroutine and waits a ceiling, REPORTING rather than
// blocking when it does not return.
//
// AN UNBOUNDED CLEANUP TURNS EVERY Close DEADLOCK INTO A TEST-BINARY TIMEOUT. The
// subtest reports its own named failure at its own ceiling, then the cleanup blocks on
// the in-progress sync.Once forever, and the only reporter left is `go test -timeout`
// — which costs minutes and buries the assertion that already fired under a goroutine
// dump. Measured on the wait-group-join-too-early defect: 600 seconds unbounded against
// 31 seconds bounded, reporting the same named failure.
func closeWithin(t *testing.T, l *Ledger, ceiling time.Duration) {
	t.Helper()
	done := make(chan error, 1)

	go func() { done <- l.Close() }()

	select {
	case <-done:
	case <-time.After(ceiling):
		t.Errorf("Close for flow %q did not return within %s during cleanup", l.cfg.Flow, ceiling)
	}
}

func waitLeadership(t *testing.T, l *Ledger) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if l.raft.State() == raft.Leader {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("flow %q never elected a leader; state is %s", l.cfg.Flow, l.raft.State())
}

func TestOneVoterLocalLedgerServesReadsAndWritesWithNoPeers(t *testing.T) {
	l := openTestLedger(t, Config{Flow: "flow-solo", LocalID: "n0", Mux: testMux(t), Bootstrap: true})
	waitLeadership(t, l)

	// The dev selection really is in-memory: no bolt handle was opened.
	if l.bolt != nil {
		t.Fatal("an empty Dir opened a bolt store; the dev mode is supposed to be in-memory")
	}
	// Zero peers is the normal case here, not a degenerate one.
	servers := l.raft.GetConfiguration().Configuration().Servers
	if len(servers) != 1 || servers[0].ID != raft.ServerID("n0") {
		t.Fatalf("the one-voter ledger has configuration %+v, want exactly n0", servers)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	index, err := l.Append(ctx, Entry{Kind: KindSet, Path: "heap/alpha", Value: []byte("written")})
	if err != nil {
		t.Fatalf("appending to a one-voter ledger: %v", err)
	}
	// The append reports the journal position the entry landed at, which is what a
	// checkpoint references.
	if index == 0 {
		t.Fatal("the append reported journal index 0, which no committed entry occupies")
	}

	entry, ok, err := l.Get(ctx, "heap/alpha")
	if err != nil {
		t.Fatalf("reading from a one-voter ledger: %v", err)
	}
	if !ok || string(entry.Value) != "written" {
		t.Fatalf("read back %+v present=%v, want the written value", entry, ok)
	}

	// CONTROL: a path never written reads as absent rather than as an error, so the
	// read above proves a stored value and not merely a successful barrier.
	if _, ok, err := l.Get(ctx, "heap/never"); err != nil || ok {
		t.Fatalf("CONTROL FAILED: an unwritten path read present=%v err=%v, want absent and no error", ok, err)
	}
}

func TestDirBackedLedgerSurvivesCloseAndReopen(t *testing.T) {
	dir := t.TempDir()
	mux := testMux(t)
	cfg := Config{Flow: "flow-durable", LocalID: "n0", Mux: mux, Dir: dir, Bootstrap: true, tuning: fastElections}

	first, err := Open(cfg)
	if err != nil {
		t.Fatalf("opening a Dir-backed ledger: %v", err)
	}
	waitLeadership(t, first)

	// A non-empty Dir selects raft-boltdb/v2 for the log and stable stores.
	if first.bolt == nil {
		t.Fatal("a non-empty Dir did not open a bolt store")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := l0Append(ctx, first); err != nil {
		t.Fatalf("appending before the close: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("closing the first ledger: %v", err)
	}

	// The bolt database and the file snapshot store are both on disk under Dir.
	if _, err := os.Stat(filepath.Join(dir, boltFileName)); err != nil {
		t.Fatalf("no bolt database under the ledger's Dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshots")); err != nil {
		t.Fatalf("no file snapshot store under the ledger's Dir: %v", err)
	}

	// Reopening against the SAME directory recovers the journal from it.
	second, err := Open(cfg)
	if err != nil {
		t.Fatalf("reopening against the same Dir: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	waitLeadership(t, second)

	entry, ok, err := second.Get(ctx, "heap/durable")
	if err != nil {
		t.Fatalf("reading after the reopen: %v", err)
	}
	if !ok || string(entry.Value) != "survives" {
		t.Fatalf("after the reopen heap/durable reads %+v present=%v, want the value written before the close", entry, ok)
	}

	// CONTROL: a fresh directory does NOT carry that value, so the read above is
	// evidence of recovery from disk and not of a value this test always sees.
	fresh := openTestLedger(t, Config{Flow: "flow-fresh", LocalID: "n0", Mux: mux, Dir: t.TempDir(), Bootstrap: true})
	waitLeadership(t, fresh)
	if _, ok, err := fresh.Get(ctx, "heap/durable"); err != nil || ok {
		t.Fatalf("CONTROL FAILED: a ledger over a fresh Dir already holds heap/durable (present=%v err=%v)", ok, err)
	}
}

func l0Append(ctx context.Context, l *Ledger) error {
	_, err := l.Append(ctx, Entry{Kind: KindSet, Path: "heap/durable", Value: []byte("survives")})

	return err
}

func TestUnopenableDirIsAnErrorNotAnInMemoryFallback(t *testing.T) {
	mux := testMux(t)

	// A regular FILE where the ledger expects a directory: joining the bolt name
	// onto it cannot resolve, so the store cannot be opened.
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("occupied"), 0o600); err != nil {
		t.Fatalf("seeding the unopenable path: %v", err)
	}

	l, err := Open(Config{Flow: "flow-blocked", LocalID: "n0", Mux: mux, Dir: blocked, Bootstrap: true, tuning: fastElections})
	if err == nil {
		_ = l.Close()
		t.Fatal("Open accepted a Dir it cannot use; an unopenable Dir must be an error, never a silent fall back to the in-memory dev stores")
	}
	if l != nil {
		t.Fatalf("Open returned a ledger (%p) alongside its error; a failed Open hands back nothing to use", l)
	}

	// The failed Open left nothing behind: the flow's group id is free, so a later
	// Open of the same flow is not refused by a binding the failure leaked.
	group, bindErr := mux.Bind(transport.GroupID("flow-blocked"))
	if bindErr != nil {
		t.Fatalf("the failed Open leaked its transport binding: %v", bindErr)
	}
	if err := group.Close(); err != nil {
		t.Fatalf("closing the probe binding: %v", err)
	}

	// CONTROL: the identical config with a usable Dir opens, so the refusal above is
	// about the Dir and not about this configuration being rejected for some other
	// reason.
	ok := openTestLedger(t, Config{Flow: "flow-blocked", LocalID: "n0", Mux: mux, Dir: t.TempDir(), Bootstrap: true})
	waitLeadership(t, ok)
	if ok.bolt == nil {
		t.Fatal("CONTROL FAILED: the usable-Dir ledger did not open a bolt store")
	}
	if errors.Is(err, ErrConfigIncomplete) {
		t.Fatalf("the unopenable Dir was reported as an incomplete config (%v); it is a store-open failure", err)
	}
}
