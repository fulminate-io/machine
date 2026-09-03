// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// awaitEviction waits for the next SignalPeerEvicted after a cursor and returns
// it, so each arm reads only the signal IT caused.
func awaitEviction(t *testing.T, m *Manager, cursor uint64, within time.Duration) (Signal, bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		batch := m.signals.since(cursor)
		if batch.err != nil {
			t.Fatalf("reading signals since %d: %v", cursor, batch.err)
		}
		for _, sig := range batch.signals {
			if sig.Kind == SignalPeerEvicted {
				return sig, true
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return Signal{}, false
}

// TestAnEvictionSignalSaysWhetherTheMemberWasStillReachable gates the fact a
// consumer needs to tell a member the orchestrator retired from a member a
// momentarily wrong resolution merely failed to mention.
//
// THE REGISTRY CANNOT SUPPLY THAT FACT, and this test demonstrates it rather
// than asserting it in prose: absentMember returns only members whose address
// the resolution does NOT list, so in BOTH arms below the victim is absent from
// the live set, and the live set therefore says the same thing about a member
// that answers and a member that does not. The separating observable is a dial.
func TestAnEvictionSignalSaysWhetherTheMemberWasStillReachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// ARM 1, THE RESIDUAL SHAPE: a LIVE nonvoter that a right-size
	// wrong-membership resolution does not mention.
	leader := newClusterNode(t, "a-leader", []string{"alpha"}, 0)
	leader.start(t)
	leader.awaitLeader(t, "alpha")
	leaderRaft := leader.raftFor(t, "alpha")

	other := newClusterNode(t, "b-other", []string{"alpha"}, 2)
	other.peering(leader.addr)
	other.start(t)
	addStaleVoter(t, leaderRaft, "b-other", other.addr)

	livejoiner := newClusterNode(t, "c-livejoiner", []string{"alpha"}, 3)
	livejoiner.peering(leader.addr)
	livejoiner.start(t)
	if got, ok := suffrageIn(t, leaderRaft, "c-livejoiner"); !ok || got != raft.Nonvoter {
		t.Fatalf("CONTROL FAILED: the live joiner is %v present=%v, want a staged Nonvoter", got, ok)
	}

	// The resolution is the RIGHT SIZE and the WRONG MEMBERSHIP: it names the
	// leader and the other voter, and omits the live joiner.
	leader.mgr.cfg.Peers = "peers.invalid:0"
	leader.mgr.cfg.Expect = 2
	resolution := []string{leader.addr, other.addr}
	leader.mgr.resolve = func(context.Context, string) ([]string, error) {
		return append([]string(nil), resolution...), nil
	}
	// THE DEMONSTRATION THAT THE REGISTRY CANNOT ANSWER THIS: the victim is
	// absent from the very set a reader would consult.
	if containsAddr(resolution, livejoiner.addr) {
		t.Fatalf("the resolution names the victim at %s, so this arm is not the residual shape",
			livejoiner.addr)
	}
	t.Logf("the live victim %s is ABSENT from the resolution %v, which is what absentMember requires",
		livejoiner.addr, resolution)

	before := leader.mgr.signals.since(0).cursor
	leader.mgr.evictRound(ctx, "alpha")
	sig, ok := awaitEviction(t, leader.mgr, before, 20*time.Second)
	if !ok {
		t.Fatalf("no eviction signal was published; the configuration is %v",
			memberIDs(t, leaderRaft))
	}
	if sig.Node != "c-livejoiner" {
		t.Fatalf("the eviction named %q, want c-livejoiner", sig.Node)
	}
	if !sig.EvictedWhileReachable {
		t.Fatal("a LIVE member that answers a control dial was evicted with the signal marked " +
			"unreachable: a consumer reading that as death would offer its work beside its " +
			"still-running owner, which is a single-writer violation")
	}
	t.Logf("the eviction of a live member carries EvictedWhileReachable=%v naming %q",
		sig.EvictedWhileReachable, sig.Node)

	// THE DEADLINE IS A MEASURED CLAIM, NOT AN ASSERTED CONSTANT. The signal
	// says a readmission of this member is no longer in flight after
	// ReadmissionExpectedBy, and a consumer suspends its work until then, so the
	// bound has to be watched rather than reasoned about. NOTHING DRIVES THE
	// RE-PLACE ROUND HERE: the evicted node's own supervisor ticker is what
	// brings it back, which is the mechanism the deadline describes.
	assertDeadlineBounded(t, leader.mgr, sig, "live")
	deadline := sig.ReadmissionExpectedBy
	readmitted := false
	for time.Now().Before(deadline) {
		if _, back := suffrageIn(t, leaderRaft, "c-livejoiner"); back {
			readmitted = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !readmitted {
		t.Fatalf("the live member was not readmitted by %s, the deadline its own eviction signal "+
			"published; the configuration is %v — a consumer that waited for that instant and then "+
			"treated the member as gone would be offering a live owner's work",
			deadline.Format(time.RFC3339Nano), memberIDs(t, leaderRaft))
	}
	t.Logf("the live member was readmitted with %s of its published readmission deadline to spare",
		time.Until(deadline).Round(time.Millisecond))

	// ARM 2, THE DISCRIMINATING CONTROL, on a fresh group so the two arms cannot
	// read each other's signals. The ONE input that moves is whether the victim
	// answers: same eviction path, same bounds, same absent-from-the-resolution
	// victim. If arm 1's true were an artifact rather than the dial, this would
	// not read false.
	leader2 := newClusterNode(t, "d-leader", []string{"beta"}, 0)
	leader2.start(t)
	leader2.awaitLeader(t, "beta")
	leader2Raft := leader2.raftFor(t, "beta")

	other2 := newClusterNode(t, "e-other", []string{"beta"}, 2)
	other2.peering(leader2.addr)
	other2.start(t)
	addStaleVoter(t, leader2Raft, "e-other", other2.addr)
	addStaleNonvoter(t, leader2Raft, "f-dead", deadPeerAddress(t))

	leader2.mgr.cfg.Peers = "peers.invalid:0"
	leader2.mgr.cfg.Expect = 2
	leader2.mgr.resolve = func(context.Context, string) ([]string, error) {
		return []string{leader2.addr, other2.addr}, nil
	}

	before2 := leader2.mgr.signals.since(0).cursor
	leader2.mgr.evictRound(ctx, "beta")
	sig2, ok := awaitEviction(t, leader2.mgr, before2, 20*time.Second)
	if !ok {
		t.Fatalf("no eviction signal was published for the dead member; the configuration is %v",
			memberIDs(t, leader2Raft))
	}
	if sig2.Node != "f-dead" {
		t.Fatalf("the eviction named %q, want f-dead", sig2.Node)
	}
	if sig2.EvictedWhileReachable {
		t.Fatal("a member at an address nothing answers on was evicted with the signal marked " +
			"reachable: the fact is not being taken from a dial")
	}
	t.Logf("the eviction of an unreachable member carries EvictedWhileReachable=%v naming %q",
		sig2.EvictedWhileReachable, sig2.Node)
	// THE DEADLINE IS STAMPED ON EVERY EVICTION, not only on the reachable ones,
	// and the test asserts that rather than leaving it to be discovered. A member
	// that did not answer one dial may still return and re-place itself, so the
	// instant after which a readmission is no longer in flight is meaningful for
	// it too; a consumer that reads the fact as gone simply has no use for it.
	assertDeadlineBounded(t, leader2.mgr, sig2, "unreachable")
}

// assertDeadlineBounded asserts that an eviction signal carries a readmission
// deadline and that the deadline is bounded by TWO re-place cadences.
//
// THE BOUND IS READ FROM THE MANAGER'S OWN INTERVAL, never written here as a
// number: the deadline and the re-place round ride the same orDuration
// expression, and a bound restated as a literal is exactly how the two come
// apart. A consumer holds no constant, so the constant is this side's to keep
// honest.
func assertDeadlineBounded(t *testing.T, m *Manager, sig Signal, arm string) {
	t.Helper()
	if sig.ReadmissionExpectedBy.IsZero() {
		t.Fatalf("the %s eviction signal carries no readmission deadline, so a consumer suspending the "+
			"member's work has nothing to wait on and must either offer it at once or hold it forever", arm)
	}
	window := sig.ReadmissionExpectedBy.Sub(sig.Since)
	bound := 2 * m.refreshInterval()
	if window <= 0 || window > bound+time.Second {
		t.Fatalf("the %s eviction's readmission deadline is %s after the signal, outside the two "+
			"re-place cadences of %s it must be bounded by: a deadline shorter than a round releases a "+
			"suspension while the readmission is still in flight, and one longer holds it after the "+
			"readmission can no longer arrive", arm, window, bound)
	}
	t.Logf("the %s eviction carries a readmission deadline %s after the signal, within the two re-place "+
		"cadences of %s", arm, window.Round(time.Millisecond), bound)
}
