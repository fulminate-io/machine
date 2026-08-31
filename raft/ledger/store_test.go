package ledger

import (
	"context"
	"encoding/gob"
	"errors"
	"testing"
	"time"
)

type heapValue struct {
	Count int
}

func init() { gob.Register(heapValue{}) }

func TestSaveIsVisibleToTheNextLoadWithoutABarrier(t *testing.T) {
	l := openTestLedger(t, Config{Flow: "flow-store-rw", LocalID: "n0", Mux: testMux(t), Bootstrap: true})
	waitLeadership(t, l)
	store := l.Store()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// CONTROL: a Save DOES move the log's last index, so the unchanged index across
	// the Load below is evidence about reads rather than an index that never moves.
	beforeSave := l.raft.LastIndex()
	if err := store.Save(ctx, "heap/alpha", "first"); err != nil {
		t.Fatalf("saving: %v", err)
	}
	afterSave := l.raft.LastIndex()
	if afterSave <= beforeSave {
		t.Fatalf("CONTROL FAILED: a Save left the log's last index at %d; this instrument cannot see an append", afterSave)
	}

	// The write is visible to the very next read, with no barrier acquired. raft
	// responds to an ApplyFuture from the state machine goroutine AFTER Apply
	// returns, so a caller that awaited its own write already observes it.
	value, ok, err := store.Load(ctx, "heap/alpha")
	afterLoad := l.raft.LastIndex()
	if err != nil {
		t.Fatalf("loading straight after a save: %v", err)
	}
	if !ok || value != "first" {
		t.Fatalf("the load returned %v present=%v, want the value just saved", value, ok)
	}
	if afterLoad != afterSave {
		t.Fatalf("the read appended %d log entries (last index %d -> %d): a read must not write to answer",
			afterLoad-afterSave, afterSave, afterLoad)
	}

	// A registered composite type round-trips too, so the seam is not string-only.
	if err := store.Save(ctx, "heap/beta", heapValue{Count: 3}); err != nil {
		t.Fatalf("saving a registered struct: %v", err)
	}
	loaded, ok, err := store.Load(ctx, "heap/beta")
	if err != nil || !ok {
		t.Fatalf("loading a registered struct gave present=%v err=%v", ok, err)
	}
	if loaded != (heapValue{Count: 3}) {
		t.Fatalf("the registered struct round-tripped as %#v", loaded)
	}

	// An absent path reports absent WITHOUT an error, which is a different outcome
	// from the store failing to answer.
	if value, ok, err := store.Load(ctx, "heap/never"); err != nil || ok || value != nil {
		t.Fatalf("an absent path loaded as %v present=%v err=%v, want nil/false/nil", value, ok, err)
	}

	// Update computes at the caller and replicates the result.
	updated, err := store.Update(ctx, "heap/alpha", func(current any) any {
		if current != "first" {
			t.Errorf("Update saw %v, want the stored value", current)
		}

		return "second"
	})
	if err != nil {
		t.Fatalf("updating: %v", err)
	}
	if updated != "second" {
		t.Fatalf("Update returned %v, want the computed value", updated)
	}
	if value, _, err := store.Load(ctx, "heap/alpha"); err != nil || value != "second" {
		t.Fatalf("after Update the path loads %v (err %v), want the replicated result", value, err)
	}
}

func TestFollowerStoreRefusesWithErrNotLeader(t *testing.T) {
	nodes := newCluster(t, "flow-store-follower", 3)
	leader := waitClusterLeader(t, nodes)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// CONTROL: the leader's store answers, so the refusals below are about being a
	// follower rather than about the store being broken everywhere.
	if err := leader.ledger.Store().Save(ctx, "heap/alpha", "leader-write"); err != nil {
		t.Fatalf("CONTROL FAILED: saving on the leader: %v", err)
	}

	checked := 0
	for _, node := range nodes {
		if node == leader {
			continue
		}
		store := node.ledger.Store()

		if value, ok, err := store.Load(ctx, "heap/alpha"); !errors.Is(err, ErrNotLeader) {
			t.Fatalf("Load on the follower %s gave %v present=%v err=%v, want ErrNotLeader rather than a value it cannot prove current",
				node.id, value, ok, err)
		} else if value != nil || ok {
			t.Fatalf("a refused Load on %s still returned %v present=%v; a failed read reports nothing", node.id, value, ok)
		}
		if err := store.Save(ctx, "heap/alpha", "follower-write"); !errors.Is(err, ErrNotLeader) {
			t.Fatalf("Save on the follower %s gave %v, want ErrNotLeader", node.id, err)
		}
		if _, err := store.Update(ctx, "heap/alpha", func(v any) any { return v }); !errors.Is(err, ErrNotLeader) {
			t.Fatalf("Update on the follower %s gave %v, want ErrNotLeader", node.id, err)
		}
		checked++
	}
	if checked != 2 {
		t.Fatalf("CONTROL FAILED: only %d followers were exercised, want 2", checked)
	}
}

func TestStoreRefusesANilContext(t *testing.T) {
	l := openTestLedger(t, Config{Flow: "flow-store-nilctx", LocalID: "n0", Mux: testMux(t), Bootstrap: true})
	waitLeadership(t, l)
	store := l.Store()

	// CONTROL: this ledger serves a real context, so the refusals below are caused
	// by the nil context and not by an unusable ledger.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := store.Save(ctx, "heap/alpha", "present"); err != nil {
		t.Fatalf("CONTROL FAILED: saving with a real context: %v", err)
	}

	// A nil context is refused BEFORE raft is reached — nothing is appended and
	// nothing panics in a select.
	before := l.raft.LastIndex()
	//nolint:staticcheck // passing a nil context is exactly what this test exercises.
	if _, _, err := store.Load(nil, "heap/alpha"); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Load with a nil context gave %v, want ErrNilContext", err)
	}
	//nolint:staticcheck // passing a nil context is exactly what this test exercises.
	if err := store.Save(nil, "heap/alpha", "unreachable"); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Save with a nil context gave %v, want ErrNilContext", err)
	}
	//nolint:staticcheck // passing a nil context is exactly what this test exercises.
	if _, err := store.Update(nil, "heap/alpha", func(v any) any { return "unreachable" }); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Update with a nil context gave %v, want ErrNilContext", err)
	}
	if after := l.raft.LastIndex(); after != before {
		t.Fatalf("a nil-context call appended %d log entries (last index %d -> %d): it reached raft before being refused",
			after-before, before, after)
	}
	if value, _, err := store.Load(ctx, "heap/alpha"); err != nil || value != "present" {
		t.Fatalf("after the nil-context refusals the path loads %v (err %v), want the value from before them", value, err)
	}
}
