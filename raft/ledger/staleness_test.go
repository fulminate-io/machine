package ledger

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
)

func TestFreshLeaderReadIsNotStale(t *testing.T) {
	dir := t.TempDir()
	mux := testMux(t)
	cfg := Config{Flow: "flow-fresh-leader", LocalID: "n0", Mux: mux, Dir: dir, Bootstrap: true, tuning: fastElections}

	seed, err := Open(cfg)
	if err != nil {
		t.Fatalf("opening the seed ledger: %v", err)
	}
	waitLeadership(t, seed)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := seed.Append(ctx, Entry{Kind: KindSet, Path: fencedPath, Value: []byte(fencedValue)}); err != nil {
		t.Fatalf("seeding the committed value: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("closing the seed ledger: %v", err)
	}

	// THE WINDOW IS FORCED, NOT SAMPLED. A leader replaying a log from disk reports
	// a commit index behind its last index until something commits in its current
	// term. Each round reopens, spins to the instant raft reports Leader, and takes
	// the read only while that precondition holds — so every round that observes the
	// window is a real observation rather than a lucky one.
	const rounds = 12
	windows := 0
	for round := range rounds {
		l, err := Open(cfg)
		if err != nil {
			t.Fatalf("round %d: reopening: %v", round, err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) && l.raft.State() != raft.Leader {
		}

		commit, last := l.raft.CommitIndex(), l.raft.LastIndex()
		if l.raft.State() == raft.Leader && commit < last {
			windows++
			entry, ok, readErr := l.Get(ctx, fencedPath)
			applied := l.fsm.appliedIndex()
			switch {
			case readErr != nil:
				t.Fatalf("round %d: the in-window read failed (commitIndex=%d lastIndex=%d applied=%d): %v",
					round, commit, last, applied, readErr)
			case !ok:
				t.Fatalf("round %d: the in-window read answered ABSENT for a value that is committed and on disk (commitIndex=%d lastIndex=%d applied=%d): the read completed and was WRONG",
					round, commit, last, applied)
			case string(entry.Value) != fencedValue:
				t.Fatalf("round %d: the in-window read returned %q, want %q", round, entry.Value, fencedValue)
			}
		}
		if err := l.Close(); err != nil {
			t.Fatalf("round %d: closing: %v", round, err)
		}
	}

	t.Logf("windows observed: %d of %d rounds entered the fresh-leader window", windows, rounds)
	// A run that never entered the window has proven NOTHING, so it fails as a
	// control rather than passing quietly.
	if windows == 0 {
		t.Fatal("CONTROL FAILED: no round observed a leader whose commit index was behind its last index, so this probe never entered the state it exists to test")
	}
}

func TestCloseDoesNotWaitOnAnUnresolvedEpochAppend(t *testing.T) {
	// The natural trigger is a raft race — an Apply future that wins the send into
	// applyCh in the instant before Shutdown, which nothing then dequeues — and it
	// reproduces about once in 45 runs, which is not a gate. This drives the
	// package's own establish seam with a function that never returns, which is
	// byte-for-byte the state that orphaned future leaves behind and is reachable on
	// every run.
	l := &Ledger{
		cfg:    Config{Flow: "flow-close-race"},
		logger: hclog.NewNullLogger(),
		fsm:    newFSM(),
		notify: make(chan bool, leadershipNotifyBuffer),
		done:   make(chan struct{}),
	}
	started := make(chan struct{})
	orphaned := make(chan struct{}) // never closed: the append never resolves
	l.establish = func() {
		close(started)
		<-orphaned
	}
	l.startLeadershipDrain()

	l.notify <- true

	// CONTROL: the append must have STARTED before the close, or a Close that
	// returned quickly because there was nothing outstanding would read as a pass.
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("CONTROL FAILED: the epoch append never started, so this close raced nothing")
	}

	closed := make(chan error, 1)
	begin := time.Now()
	go func() { closed <- l.Close() }()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close reported %v", err)
		}
		t.Logf("Close returned in time (%s) with an epoch append still unresolved", time.Since(begin).Round(time.Millisecond))
	case <-time.After(15 * time.Second):
		t.Fatal("Close parked for 15s on an epoch append raft will never resolve: a caller closing a flow would hang forever and the group id would stay bound")
	}
}

// snapshotOf takes a real raft snapshot of a ledger and opens it for delivery.
func snapshotOf(t *testing.T, l *Ledger) (*raft.SnapshotMeta, io.Reader) {
	t.Helper()

	future := l.raft.Snapshot()
	if err := future.Error(); err != nil {
		t.Fatalf("taking a snapshot of %s: %v", l.Flow(), err)
	}
	meta, reader, err := future.Open()
	if err != nil {
		t.Fatalf("opening the snapshot of %s: %v", l.Flow(), err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	return meta, reader
}

func TestReadHealsAfterADeliveredRestoreWithNoInterveningWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	source := openTestLedger(t, Config{Flow: "flow-restore-source", LocalID: "n0", Mux: testMux(t), Bootstrap: true})
	waitLeadership(t, source)
	if _, err := source.Append(ctx, Entry{Kind: KindSet, Path: fencedPath, Value: []byte(fencedValue)}); err != nil {
		t.Fatalf("seeding the source ledger: %v", err)
	}
	meta, reader := snapshotOf(t, source)

	target := openTestLedger(t, Config{Flow: "flow-restore-target", LocalID: "n0", Mux: testMux(t), Bootstrap: true})
	waitLeadership(t, target)

	// CONTROL: the target does NOT hold the value before the restore, so the read
	// below is evidence of the restore rather than of a value it always had.
	if _, ok, err := target.Get(ctx, fencedPath); err != nil || ok {
		t.Fatalf("CONTROL FAILED: the target already holds %s before any restore (present=%v err=%v)", fencedPath, ok, err)
	}

	// Delivered through Ledger.Restore — NOT Raft().Restore. That is the whole fix:
	// the epoch epilogue runs after raft.Restore returns, which is the first moment
	// the trailing no-op is known committed.
	if err := target.Restore(meta, reader, 60*time.Second); err != nil {
		t.Fatalf("delivering the snapshot: %v", err)
	}

	// THE FIRST READ, with NO intervening write and NO leadership transfer. Either
	// would unstick this door by itself and mask the mechanism.
	entry, ok, err := target.Get(ctx, fencedPath)
	if err != nil {
		t.Fatalf("the first read after a delivered restore failed (commit %d, tracked %d): %v",
			target.raft.CommitIndex(), target.fsm.appliedIndex(), err)
	}
	if !ok || string(entry.Value) != fencedValue {
		t.Fatalf("the first read after a delivered restore gave %+v present=%v, want the restored value", entry, ok)
	}
}

func TestRestoredFollowerBecomingLeaderReadsWithoutAnInterveningWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	source := openTestLedger(t, Config{Flow: "flow-follower-source", LocalID: "n0", Mux: testMux(t), Bootstrap: true})
	waitLeadership(t, source)
	if _, err := source.Append(ctx, Entry{Kind: KindSet, Path: fencedPath, Value: []byte(fencedValue)}); err != nil {
		t.Fatalf("seeding the source ledger: %v", err)
	}
	meta, reader := snapshotOf(t, source)

	// This is the arm the restore epilogue structurally cannot reach: raft.Restore
	// is leader-only, so the FOLLOWERS take this snapshot through raft's
	// install-snapshot replication path and never enter Ledger.Restore. The claim
	// is that they need nothing extra, because winning a term appends a per-term
	// epoch that carries the state machine past whatever the install left behind.
	nodes := newCluster(t, "flow-follower-restore", 3)
	leader := waitClusterLeader(t, nodes)
	if err := leader.ledger.Restore(meta, reader, 60*time.Second); err != nil {
		t.Fatalf("delivering the snapshot to the leader: %v", err)
	}

	promoted := otherThan(nodes, leader)
	transferLeadership(t, leader, promoted)

	// CONTROL: leadership really moved to a node that took the snapshot as a
	// follower. An arm that never changed leader tested nothing.
	if promoted.ledger.Raft().State() != raft.Leader {
		t.Fatalf("CONTROL FAILED: %s was not promoted; it is %s", promoted.id, promoted.ledger.Raft().State())
	}

	entry, ok, err := promoted.ledger.Get(ctx, fencedPath)
	if err != nil {
		t.Fatalf("the first read on the promoted follower failed (commit %d, tracked %d): %v",
			promoted.ledger.raft.CommitIndex(), promoted.ledger.fsm.appliedIndex(), err)
	}
	if !ok || string(entry.Value) != fencedValue {
		t.Fatalf("the first read on the promoted follower gave %+v present=%v, want the restored value", entry, ok)
	}
}
