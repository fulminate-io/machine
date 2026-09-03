// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package recovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/whitaker-io/machine/raft/membership"
)

func evicted(flow, node string, reachable bool, window time.Duration) []membership.Signal {
	return []membership.Signal{{
		Kind: membership.SignalPeerEvicted, Flow: flow, Node: node, Since: time.Now(),
		EvictedWhileReachable: reachable, ReadmissionExpectedBy: time.Now().Add(window),
	}}
}

// TestALiveMembersEvictionSuspendsItsDatumsRatherThanOrphaningThem drives the ruled
// suspension: an eviction of a REACHABLE member withholds its datums for the window,
// an eviction of an UNREACHABLE one does not, and a departure that emits no signal at
// all is gone at once.
func TestALiveMembersEvictionSuspendsItsDatumsRatherThanOrphaningThem(t *testing.T) {
	window := 3 * time.Second

	// ARM 1: EVICTED WHILE LIVE. The owner has left the configuration but the
	// eviction says it was reachable, so its datums are withheld.
	aNodes := newGroup(t, "alpha", 3)
	a := awaitLeader(t, aNodes)
	journalCheckpoint(t, a.ledger, "e-live", "d1", "progress")
	suspend := newHealthView([]string{a.id, "b-live"}, evicted("alpha", "e-live", true, window))
	det := New(a.ledger, suspend, "alpha")

	short, cancelShort := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelShort()
	orphans, err := det.Orphans(short, "alpha")
	t.Logf("ARM 1 (evicted while LIVE, inside the window): orphans=%v err=%v", datums(orphans), err)
	if len(orphans) != 0 {
		t.Fatalf("ARM 1: %v offered inside the suspension window. A live member's eviction is "+
			"not a death: lane D's re-place round readmits it, and offering now puts a second "+
			"writer beside a running one", datums(orphans))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ARM 1: ended with %v rather than a parked call hitting its deadline, so the "+
			"empty set is not evidence the datums were withheld", err)
	}

	// ARM 2: THE WINDOW EXPIRES WITH NO READMISSION, so the owner really is gone
	// and its datums must be offered. Same detector, same ledger — the ONE input
	// that moves is the passage of time.
	long, cancelLong := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelLong()
	time.Sleep(window)
	orphans, err = det.Orphans(long, "alpha")
	t.Logf("ARM 2 (window expired, no readmission): orphans=%v err=%v", datums(orphans), err)
	if err != nil {
		t.Fatalf("ARM 2: Orphans errored: %v", err)
	}
	if len(orphans) != 1 || orphans[0].Owner != "e-live" {
		t.Fatalf("ARM 2: orphans=%v, want d1 owned by e-live offered once the window passed "+
			"with no readmission; a suspension that never expires strands the datum forever",
			datums(orphans))
	}

	// ARM 3: EVICTED WHILE UNREACHABLE. The same eviction carrying the opposite
	// mark must NOT suspend — that member is dead and its datums are recovery's
	// whole purpose. This is the leg an implementation that suspends every eviction
	// fails while passing arms 1 and 2.
	bNodes := newGroup(t, "beta", 3)
	b := awaitLeader(t, bNodes)
	journalCheckpoint(t, b.ledger, "e-dead", "d2", "progress")
	dead := newHealthView([]string{b.id, "b-live"}, evicted("beta", "e-dead", false, window))
	detDead := New(b.ledger, dead, "beta")
	orphans, err = detDead.Orphans(long, "beta")
	t.Logf("ARM 3 (evicted while UNREACHABLE): orphans=%v err=%v", datums(orphans), err)
	if err != nil {
		t.Fatalf("ARM 3: Orphans errored: %v", err)
	}
	if len(orphans) != 1 || orphans[0].Owner != "e-dead" {
		t.Fatalf("ARM 3: orphans=%v, want d2 offered at once; an unreachable member's eviction "+
			"is a death and suspending it delays every recovery by the window", datums(orphans))
	}

	// ARM 4: THE GRACEFUL LEAVER, and it is the two-sided control. A member that
	// departs through the SetFlows leave path emits NO signal at all, so nothing
	// ever suspends it and nothing ever marks it unreachable. Its datums must be
	// offered immediately, which is exactly what an implementation that dropped the
	// absence arm to close arm 1's window would strand forever.
	cNodes := newGroup(t, "gamma", 3)
	c := awaitLeader(t, cNodes)
	journalCheckpoint(t, c.ledger, "e-gone", "d3", "progress")
	silent := newHealthView([]string{c.id, "b-live"})
	detGone := New(c.ledger, silent, "gamma")
	orphans, err = detGone.Orphans(long, "gamma")
	t.Logf("ARM 4 (graceful leaver, no signal at all): orphans=%v err=%v", datums(orphans), err)
	if err != nil {
		t.Fatalf("ARM 4: Orphans errored: %v", err)
	}
	if len(orphans) != 1 || orphans[0].Owner != "e-gone" {
		t.Fatalf("ARM 4: orphans=%v, want d3 offered at once. A graceful departure emits no "+
			"health signal, so an implementation that dropped the absence arm strands it "+
			"forever — which is why the absence arm is kept", datums(orphans))
	}
}
