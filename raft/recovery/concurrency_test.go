// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package recovery

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/membership"
)

// pollingView answers Membership from a fixed set and returns from Watch promptly,
// so every caller completes a round and writes the cursor.
type pollingView struct{ servers []raft.ServerID }

func (v *pollingView) Membership(string) (raft.Configuration, uint64, bool) {
	servers := make([]raft.Server, 0, len(v.servers))
	for _, id := range v.servers {
		servers = append(servers, raft.Server{ID: id})
	}

	return raft.Configuration{Servers: servers}, uint64(len(servers)), true
}

// Watch returns promptly so every caller completes a round and writes the cursor,
// and HONORS THE CONTEXT so the loop it drives ends rather than spinning until the
// runner's own timeout — a driver that ignored ctx would report a hang as a failure
// of the subject.
func (v *pollingView) Watch(ctx context.Context, since uint64) ([]membership.Signal, uint64, error) {
	select {
	case <-time.After(time.Millisecond):
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}

	return []membership.Signal{{
		Kind: membership.SignalMembershipChanged, Flow: "alpha", Node: "a", Since: time.Now(),
	}}, since + 1, nil
}

// TestConcurrentDetectionRoundsShareOneCursorSafely drives the shape a flow with two
// checkpointed nodes produces in production: ONE journal per machine, and the root
// starting a resume loop per worker, so two Orphans calls walk one detector's cursor
// at once. The race detector is the instrument; this test is its driver.
func TestConcurrentDetectionRoundsShareOneCursorSafely(t *testing.T) {
	nodes := newGroup(t, "alpha", 1)
	leader := awaitLeader(t, nodes)

	detector := New(leader.ledger, &pollingView{servers: []raft.ServerID{
		raft.ServerID(leader.id), "b-live",
	}}, "alpha")

	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = detector.Orphans(ctx, "alpha")
		}()
	}
	wg.Wait()

	// THE VACUITY CONTROL. A cursor that never advanced would mean no round ever
	// completed, and the concurrency this test exists to exercise never happened.
	detector.mu.Lock()
	cursor := detector.cursor
	detector.mu.Unlock()
	t.Logf("three concurrent detection rounds advanced the shared cursor to %d", cursor)
	if cursor == 0 {
		t.Fatal("CONTROL FAILED: the cursor never advanced, so no concurrent round completed " +
			"and the race detector had nothing to observe")
	}
}

// foldingView returns a batch on EVERY round, cycling the kinds that WRITE detector
// state, and honors its context so the loops it drives end.
//
// EMITTING ONLY SignalMembershipChanged IS THE TRAP THIS DRIVER EXISTS TO AVOID:
// noteHealth skips that kind by design, so a driver that sends nothing else leaves
// the health fields NEVER WRITTEN and the race detector with nothing to see.
type foldingView struct {
	mutex   sync.Mutex
	servers []raft.ServerID
	counts  map[membership.SignalKind]int
	round   int
}

func newFoldingView(ids ...string) *foldingView {
	v := &foldingView{counts: map[membership.SignalKind]int{}}
	for _, id := range ids {
		v.servers = append(v.servers, raft.ServerID(id))
	}

	return v
}

func (v *foldingView) Membership(string) (raft.Configuration, uint64, bool) {
	v.mutex.Lock()
	defer v.mutex.Unlock()
	servers := make([]raft.Server, 0, len(v.servers))
	for _, id := range v.servers {
		servers = append(servers, raft.Server{ID: id})
	}

	return raft.Configuration{Servers: servers}, uint64(len(servers)), true
}

func (v *foldingView) Watch(ctx context.Context, since uint64) ([]membership.Signal, uint64, error) {
	select {
	case <-time.After(time.Millisecond):
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}

	v.mutex.Lock()
	turn := v.round % 3
	v.round++
	var sig membership.Signal
	switch turn {
	case 0:
		sig = membership.Signal{Kind: membership.SignalPeerUnreachable, Flow: "alpha", Node: "p-down"}
	case 1:
		sig = membership.Signal{Kind: membership.SignalPeerReturned, Flow: "alpha", Node: "p-back"}
	default:
		sig = membership.Signal{
			Kind: membership.SignalPeerEvicted, Flow: "alpha", Node: "e-live",
			EvictedWhileReachable: true,
			ReadmissionExpectedBy: time.Now().Add(time.Hour),
		}
	}
	sig.Since = time.Now()
	v.counts[sig.Kind]++
	v.mutex.Unlock()

	return []membership.Signal{sig}, since + 1, nil
}

func (v *foldingView) folded(kind membership.SignalKind) int {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	return v.counts[kind]
}

// TestConcurrentRoundsWriteEveryGuardedFieldWithoutRacing drives concurrent detection
// rounds that WRITE all three fields the detector's mutex guards.
func TestConcurrentRoundsWriteEveryGuardedFieldWithoutRacing(t *testing.T) {
	nodes := newGroup(t, "alpha", 1)
	leader := awaitLeader(t, nodes)

	// p-down is CONFIGURED so the prune keeps it: a member pruned out would leave
	// the unreachable map empty at the end and the control below could not tell a
	// written-then-pruned field from one never written.
	view := newFoldingView(leader.id, "p-down")
	detector := New(leader.ledger, view, "alpha")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = detector.Orphans(ctx, "alpha")
		}()
	}
	wg.Wait()

	detector.mu.Lock()
	cursor := detector.cursor
	_, downHeld := detector.unreachable["p-down"]
	_, liveSuspended := detector.suspended["e-live"]
	unreachableSize, suspendedSize := len(detector.unreachable), len(detector.suspended)
	detector.mu.Unlock()

	t.Logf("concurrent detection rounds advanced the shared cursor to %d", cursor)
	t.Logf("guarded field writes observed: unreachable=%d (p-down held=%v) suspended=%d (e-live held=%v)",
		unreachableSize, downHeld, suspendedSize, liveSuspended)
	t.Logf("signals folded by kind: unreachable=%d returned=%d evicted=%d",
		view.folded(membership.SignalPeerUnreachable),
		view.folded(membership.SignalPeerReturned),
		view.folded(membership.SignalPeerEvicted))

	if cursor == 0 {
		t.Fatal("CONTROL FAILED: the cursor never advanced, so no concurrent round completed")
	}
	// PER-FIELD CONTROLS. Each fails when its field was never written by a
	// concurrent round, which is exactly the blind-gate state a driver emitting
	// only SignalMembershipChanged produces.
	if !downHeld {
		t.Fatal("CONTROL FAILED: the unreachable map was never written, so the race detector " +
			"observed no concurrent access to it and this gate says nothing about that field")
	}
	if !liveSuspended {
		t.Fatal("CONTROL FAILED: the suspended map was never written, so the race detector " +
			"observed no concurrent access to it and this gate says nothing about that field")
	}
	if view.folded(membership.SignalPeerReturned) == 0 {
		t.Fatal("CONTROL FAILED: no returned signal was folded, so noteHealth's delete branch " +
			"never ran under concurrency")
	}
}
