package membership

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
)

// TestPeerFailuresReportsAnUnreachablePeerRatherThanRetrying is where the
// control client's stated negative is read: a failed call is REPORTED and not
// retried in a loop. A peer that cannot be reached is exactly the condition the
// failure signal exists to report, so a retry loop here would hide it — and a
// silent omission from the view would be indistinguishable from a peer that
// answered with nothing.
func TestPeerFailuresReportsAnUnreachablePeerRatherThanRetrying(t *testing.T) {
	asker, _ := testNode(t, "a-asker")
	dead := unreachableAddress(t)
	dials := countingDialer(asker)
	asker.peers.setAddresses([]string{dead})
	asker.peers.setFlows([]string{"alpha"})

	view := asker.peers.statsView()
	if _, present := view[dead]; present {
		t.Fatal("an unreachable peer appears in the stats view: a peer that answered nothing and a peer " +
			"that could not be reached must not read the same")
	}

	failures := asker.PeerFailures()
	err, reported := failures[dead]
	if !reported {
		t.Fatalf("the unreachable peer is absent from PeerFailures: a silent omission is exactly the "+
			"report the failure signal depends on; failures were %v", failures)
	}
	if err == nil {
		t.Fatal("the peer is named in PeerFailures with a nil error, so nothing says why it failed")
	}

	// NOT RETRIED. One round dials each peer once; a retry loop inside the round
	// would show up here as more than one dial for a single unreachable peer.
	if *dials != 1 {
		t.Fatalf("one round against one unreachable peer made %d dials, want exactly 1: a failed call is "+
			"reported, not retried", *dials)
	}

	// THE CONTROL, same instrument: a REACHABLE peer lands in the view and not in
	// the failures, so the assertions above are about reachability rather than
	// about a client that reports everything as failed.
	answerer, answererMux := testNode(t, "b-answerer")
	answersFlows(answerer, "alpha")
	asker.peers.setAddresses([]string{answererMux.Addr().String()})
	asker.peers.setFlows([]string{"alpha"})
	ok := asker.peers.statsView()
	if _, present := ok[answererMux.Addr().String()]; !present {
		t.Fatalf("CONTROL FAILED: a reachable peer is absent from the view; failures were %v",
			asker.PeerFailures())
	}
	if _, failed := asker.PeerFailures()[answererMux.Addr().String()]; failed {
		t.Fatal("CONTROL FAILED: a reachable peer was reported as failed")
	}
}

// TestALeaveTheLeaderRefusesIsReportedRatherThanTreatedAsDone closes the other
// half of the leave contract. A leave that cannot REACH the leader is an error;
// so is one the leader reached and declined. Treating a refusal as done would
// close the departing node's ledger while the group still carried it, which is
// the orphan the unconditional close exists to avoid.
func TestALeaveTheLeaderRefusesIsReportedRatherThanTreatedAsDone(t *testing.T) {
	host := newClusterNode(t, "a-host", []string{"alpha"}, 0)
	host.start(t)
	host.awaitLeader(t, "alpha")
	leaver, _ := testNode(t, "b-leaver")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// The host does not carry "bravo", so it refuses by name rather than removing.
	err := leaver.leaveVia(ctx, host.addr, "bravo")
	if err == nil {
		t.Fatal("a leave the leader refused was reported as done: the departing node would close its " +
			"ledger while the group still carried it")
	}
	if !errors.Is(err, ErrLeaveRefused) {
		t.Fatalf("err = %v, want ErrLeaveRefused", err)
	}
	if !strings.Contains(err.Error(), "bravo") {
		t.Fatalf("the refusal %q does not name the flow", err)
	}

	// THE CONTROL: the same path against a flow the host DOES lead succeeds, so
	// the error above is the refusal rather than a leave path that never works.
	if err := leaver.leaveVia(ctx, host.addr, "alpha"); err != nil {
		t.Fatalf("CONTROL FAILED: a leave the leader accepts reported %v", err)
	}
}

// TestRequestRemovalRefusesAFlowThisNodeDoesNotHost pins the precondition: a node
// cannot leave a flow it never joined, and saying so beats dialing anyone.
func TestRequestRemovalRefusesAFlowThisNodeDoesNotHost(t *testing.T) {
	mgr, _ := testNode(t, "a-node")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := mgr.requestRemoval(ctx, "never-hosted")
	if err == nil {
		t.Fatal("leaving a flow this node does not host reported success")
	}
	if !strings.Contains(err.Error(), "never-hosted") {
		t.Fatalf("the refusal %q does not name the flow", err)
	}
}

// TestTheWireRefusesAKindOrARequestTypeItDoesNotDeclare pins the two write-side
// refusals. The declared set is enumerated in one place so a kind cannot be sent
// under a name the read path would reject, and a request type with no reply
// pairing is a programming error that must not reach the socket.
func TestTheWireRefusesAKindOrARequestTypeItDoesNotDeclare(t *testing.T) {
	if err := writeMessage(io.Discard, msgKind(200), statsRequest{}); !errors.Is(err, ErrUnknownMessage) {
		t.Fatalf("writeMessage with an undeclared kind: err = %v, want ErrUnknownMessage", err)
	}
	if _, _, err := replyFor("not a control request"); !errors.Is(err, ErrUnknownMessage) {
		t.Fatalf("replyFor with a non-request type: err = %v, want ErrUnknownMessage", err)
	}
	// THE CONTROL: a declared kind and a real request are both accepted, so the
	// refusals above are about the inputs rather than about a writer that refuses
	// everything.
	if err := writeMessage(io.Discard, msgStats, statsRequest{Flows: []string{"alpha"}}); err != nil {
		t.Fatalf("CONTROL FAILED: a declared kind was refused: %v", err)
	}
	if kind, reply, err := replyFor(statsRequest{}); err != nil || kind != msgStats || reply == nil {
		t.Fatalf("CONTROL FAILED: a real request was refused: kind=%d reply=%v err=%v", uint8(kind), reply, err)
	}
}

// TestAReplyOfTheWrongKindIsRefused pins the pairing the client enforces on the
// way back. Kinds are paired in one place so a request cannot be sent under one
// name and read back under another, and a peer answering with the wrong kind
// must be refused rather than have its bytes decoded into the caller's reply
// type, where a partially-populated struct would read as a real answer.
func TestAReplyOfTheWrongKindIsRefused(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	go func() {
		if _, _, err := readMessage(server); err != nil {
			return
		}
		// A stats request answered with a LEAVE reply.
		_ = writeMessage(server, msgLeaveReply, leaveReply{Removed: []string{"alpha"}})
	}()

	p := &peers{timeout: 3 * time.Second, logger: hclog.NewNullLogger()}
	var reply statsReply
	err := p.exchange(client, msgStats, statsRequest{Flows: []string{"alpha"}}, &reply)
	if !errors.Is(err, ErrUnknownMessage) {
		t.Fatalf("err = %v, want ErrUnknownMessage: a reply of the wrong kind must not be decoded into "+
			"the caller's reply type", err)
	}
	if len(reply.PerFlow) != 0 {
		t.Fatalf("the mismatched reply was decoded anyway: %v", reply.PerFlow)
	}
}
