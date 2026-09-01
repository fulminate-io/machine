package membership

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/ledger"
)

// unreachableAddress reserves a port and releases it, so the address is
// well-formed, routable and answered by nothing — which is the state every
// joiner occupies while it replays: present, announced, and not yet reachable.
func unreachableAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// commitWrite appends one entry and reports whether it committed.
func commitWrite(t *testing.T, node *clusterNode, flow, path string) error {
	t.Helper()
	l, ok := node.mgr.Ledger(flow)
	if !ok {
		t.Fatalf("node %s does not host flow %q", node.id, flow)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := l.Append(ctx, ledger.Entry{Kind: ledger.KindSet, Path: path, Value: []byte("v")})
	return err
}

// TestCommitsSurviveAJoinWhoseJoinerIsUnreachable asserts what the GROUP
// experiences while a join happens, which is the ticket's stated reason for
// staging. The join tests assert the joiner ends up in the right place; this one
// asserts the group stayed writable while it got there, and the two fail for
// different reasons.
//
// THE SEPARATING OBSERVABLE IS THE IN-WINDOW WRITE'S ERROR, NOT THE JOIN'S.
// Measured against hashicorp/raft v1.7.3: the violating voter shape returns a
// nil join-future error in zero seconds and then fails the next write with
// "leadership lost while committing log", while the staged shape commits it with
// no error. A join path that checked only its own future would report a healthy
// join on exactly the path that just broke the group.
func TestCommitsSurviveAJoinWhoseJoinerIsUnreachable(t *testing.T) {
	leader := newClusterNode(t, "a-leader", []string{"alpha"}, 0)
	leader.start(t)
	leader.awaitLeader(t, "alpha")

	// THE SAME-RUN CONTROL. Without it, a failure below could be read as a group
	// that was never writable in the first place.
	if err := commitWrite(t, leader, "alpha", "baseline"); err != nil {
		t.Fatalf("CONTROL FAILED: the baseline write did not commit before any join: %v", err)
	}
	t.Log("baseline write committed before the join")

	ghost := unreachableAddress(t)
	reply := leader.mgr.answerAnnounce(announce{Node: "b-ghost", Address: ghost, Flows: []string{"alpha"}})
	if len(reply.Staged) != 1 || reply.Staged[0] != "alpha" {
		t.Fatalf("the unreachable joiner was not staged: staged=%v refused=%v redirects=%v",
			reply.Staged, reply.Refused, reply.Redirects)
	}

	// THE ASSERTION. A write issued while the joiner is present and unreachable
	// must still commit.
	if err := commitWrite(t, leader, "alpha", "in-window"); err != nil {
		t.Fatalf("a write during the join window failed: %v — admitting a joiner that cannot yet answer "+
			"raised the quorum and cost the leader its leadership", err)
	}

	got, ok := suffrageIn(t, leader.raftFor(t, "alpha"), "b-ghost")
	if !ok {
		t.Fatal("the joiner is absent from the committed configuration")
	}
	if got != raft.Nonvoter {
		t.Fatalf("the joiner is recorded at suffrage %v, want Nonvoter", got)
	}
}
