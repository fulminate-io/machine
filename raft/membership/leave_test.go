package membership

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/ledger"
	"github.com/whitaker-io/machine/raft/transport"
)

// bindable reports whether a flow's group id can be bound on a mux, which is the
// observable that distinguishes a closed ledger from one still holding its
// binding. It releases whatever it took, so the check is repeatable.
func bindable(t *testing.T, mux *transport.Mux, flow string) bool {
	t.Helper()
	g, err := mux.Bind(transport.GroupID(flow))
	if err != nil {
		if errors.Is(err, transport.ErrGroupBound) {
			return false
		}
		t.Fatalf("binding %q: %v", flow, err)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("releasing the probe binding: %v", err)
	}
	return true
}

// awaitKnowsLeader waits until a node's raft handle for a flow has learned who
// leads it.
//
// IT IS A PRECONDITION, NOT A WORKAROUND. A leave is leader-directed, so a node
// that has just been staged and has not yet received a single AppendEntries
// genuinely cannot leave — and the implementation says so loudly with
// ErrLeaveUnreachable rather than closing locally, which is the property the
// third test in this file asserts on purpose.
func awaitKnowsLeader(t *testing.T, node *clusterNode, flow string) {
	t.Helper()
	r := node.raftFor(t, flow)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if addr, _ := r.LeaderWithID(); addr != "" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("node %s never learned the leader of flow %q", node.id, flow)
}

// awaitAbsent waits for id to leave a flow's committed configuration.
func awaitAbsent(t *testing.T, r *raft.Raft, id string, window time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if _, ok := suffrageIn(t, r, id); !ok {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func TestLeavingFlowIsRemovedAndItsLedgerClosedOnTheDepartingNode(t *testing.T) {
	leader := newClusterNode(t, "a-leader", []string{"alpha"}, 0)
	leader.start(t)
	leader.awaitLeader(t, "alpha")

	leaver := newClusterNode(t, "b-leaver", []string{"alpha"}, 2)
	leaver.peering(leader.addr)
	leaver.start(t)
	awaitKnowsLeader(t, leaver, "alpha")

	// THE CONTROL that makes the rebind assertion mean something: while the
	// ledger is open the flow's group id is NOT bindable.
	if bindable(t, leaver.mux, "alpha") {
		t.Fatal("CONTROL FAILED: the flow id was bindable while its ledger was open, so a later successful " +
			"bind would prove nothing about the close")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := leaver.mgr.SetFlows(ctx, nil); err != nil {
		t.Fatalf("SetFlows to the empty set: %v", err)
	}

	if !awaitAbsent(t, leader.raftFor(t, "alpha"), "b-leaver", 15*time.Second) {
		t.Fatal("the departed node is still in the flow's committed configuration on the leader")
	}
	if !bindable(t, leaver.mux, "alpha") {
		t.Fatal("the flow id is still bound after the leave: the departing node did not close its ledger, " +
			"and nothing else will")
	}
	if _, ok := leaver.mgr.Ledger("alpha"); ok {
		t.Fatal("the departed flow is still in the manager's map")
	}
}

func TestRemovedFollowerIsClosedByOurPathNotByRaft(t *testing.T) {
	leader := newClusterNode(t, "a-leader", []string{"alpha"}, 0)
	leader.start(t)
	leader.awaitLeader(t, "alpha")

	follower := newClusterNode(t, "b-follower", []string{"alpha"}, 2)
	follower.peering(leader.addr)
	follower.start(t)
	awaitKnowsLeader(t, follower, "alpha")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Removal alone, WITHOUT our close, so the state between the two steps is
	// observable. That gap is the whole subject of this test.
	if err := follower.mgr.requestRemoval(ctx, "alpha"); err != nil {
		t.Fatalf("requestRemoval: %v", err)
	}
	if !awaitAbsent(t, leader.raftFor(t, "alpha"), "b-follower", 15*time.Second) {
		t.Fatal("the removal did not reach the leader's committed configuration")
	}

	// THE CONTROL LEG, and the reason the unconditional close is load-bearing:
	// raft tells a removed FOLLOWER nothing. ShutdownOnRemove is consulted at one
	// site inside leaderLoop, under a step-down guard that fires only when the
	// LEADER commits a configuration giving its own id no vote.
	state := follower.raftFor(t, "alpha").State()
	if state == raft.Shutdown {
		t.Fatalf("the removed follower shut itself down (state %v): if raft did the tear-down, the "+
			"unconditional close would be dead code and this test would be asserting the wrong thing", state)
	}
	t.Logf("removed follower observed in state %v", state)
	if bindable(t, follower.mux, "alpha") {
		t.Fatal("CONTROL FAILED: the group id was already free before our close ran")
	}

	if err := follower.mgr.closeFlow("alpha"); err != nil {
		t.Fatalf("closeFlow: %v", err)
	}
	if !bindable(t, follower.mux, "alpha") {
		t.Fatal("the group id is still bound after our close")
	}
}

func TestALeaveThatCannotReachTheLeaderIsAnErrorAndClosesNothing(t *testing.T) {
	leader := newClusterNode(t, "a-leader", []string{"alpha"}, 0)
	leader.start(t)
	leader.awaitLeader(t, "alpha")

	follower := newClusterNode(t, "b-follower", []string{"alpha"}, 2)
	follower.peering(leader.addr)
	follower.start(t)
	// The follower must KNOW its leader first, so the refusal below is a dial
	// that failed rather than a leader that was never learned — the weaker of
	// the two paths into ErrLeaveUnreachable.
	awaitKnowsLeader(t, follower, "alpha")

	// The leader goes away. The follower still hosts the flow and can no longer
	// reach anyone able to remove it.
	if err := leader.mgr.Close(); err != nil {
		t.Fatalf("closing the leader: %v", err)
	}
	if err := leader.mux.Close(); err != nil {
		t.Fatalf("closing the leader's listener: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := follower.mgr.SetFlows(ctx, nil)
	if err == nil {
		t.Fatal("a leave that could not reach the leader reported success: the group still carries this " +
			"member, and closing anyway would manufacture exactly the orphan recovery exists to clean up")
	}
	if !errors.Is(err, ErrLeaveUnreachable) {
		t.Fatalf("err = %v, want ErrLeaveUnreachable", err)
	}

	// AND IT CLOSED NOTHING, checked by using the ledger afterwards: it is still
	// in the manager's map and still holds its group id.
	if _, ok := follower.mgr.Ledger("alpha"); !ok {
		t.Fatal("the ledger was dropped from the map on a leave that never happened")
	}
	if bindable(t, follower.mux, "alpha") {
		t.Fatal("the ledger was closed on a leave that never happened: the group still carries this member")
	}
}

func TestOneFlowIDRebindsTwoHundredTimes(t *testing.T) {
	const cycles = 200
	const flow = "rebound"
	mux, err := transport.New(transport.Config{
		BindAddr: "127.0.0.1:0", HandshakeTimeout: 2 * time.Second, RPCTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mux.Close() })
	mgr, err := New(Config{Node: "a-node", Advertise: mux.Addr().String(), Mux: mux})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	// THE CEILING makes a cycle that blocks fail rather than pass slowly.
	deadline := time.Now().Add(4 * time.Minute)
	done := 0
	for i := 0; i < cycles; i++ {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d rebind cycles completed inside the ceiling: a cycle is blocking", done, cycles)
		}
		l, err := ledger.Open(ledger.Config{Flow: flow, LocalID: "a-node", Mux: mux})
		if err != nil {
			t.Fatalf("cycle %d: opening the flow failed: %v — the previous cycle's close did not free the "+
				"binding synchronously", i, err)
		}
		mgr.addFlow(flow, l)
		// join-or-bootstrap: with no peer hosting it, the creation rule makes this
		// node the creator, which is what BootstrapCluster is here.
		if err := mgr.createFlow(flow); err != nil {
			t.Fatalf("cycle %d: creating the group failed: %v", i, err)
		}
		// THE KNOWN-POSITIVE, and this loop needs one badly: two hundred cycles
		// complete in single-digit milliseconds, which is fast enough that "every
		// cycle bound successfully" and "nothing was ever bound at all" would look
		// identical from the outside. Asserting the id is HELD while the ledger is
		// open makes the successful bind at the top of the next cycle mean
		// something.
		if bindable(t, mux, flow) {
			t.Fatalf("cycle %d: the flow id was bindable while its ledger was open, so this loop is not "+
				"exercising the binding at all", i)
		}
		if err := mgr.closeFlow(flow); err != nil {
			t.Fatalf("cycle %d: closing the flow failed: %v", i, err)
		}
		if !bindable(t, mux, flow) {
			t.Fatalf("cycle %d: the flow id was still bound after the close: the binding is freed "+
				"asynchronously, which a single-cycle test would never show", i)
		}
		done++
	}
	t.Logf("completed %d rebind cycles in %v", done, time.Since(deadline.Add(-4*time.Minute)))
}
