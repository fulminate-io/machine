package ledger

import (
	"errors"
	"testing"
	"time"

	"github.com/whitaker-io/machine/raft/transport"
)

// The three close orderings Ledger.Close depends on. Two of them are PREMISES of
// the sequence Close runs rather than neighboring trivia: ordering A is exactly the
// order Close uses, and ordering B is what licenses calling Group.Close
// unconditionally after a raft shutdown that may already have closed the transport.
// Ordering C is the one a process teardown can invert on us.

func TestCloseOrderingRaftFirstThenGroup(t *testing.T) {
	mux := testMux(t)
	l := openTestLedger(t, Config{Flow: "flow-order-a", LocalID: "n0", Mux: mux, Bootstrap: true})
	waitLeadership(t, l)

	if err := l.raft.Shutdown().Error(); err != nil {
		t.Fatalf("draining raft's shutdown future: %v", err)
	}
	if err := l.group.Close(); err != nil {
		t.Fatalf("closing the group after a drained raft shutdown: %v", err)
	}

	assertRebindable(t, mux, "flow-order-a")
}

func TestCloseOrderingGroupFirstThenRaft(t *testing.T) {
	mux := testMux(t)
	l := openTestLedger(t, Config{Flow: "flow-order-b", LocalID: "n0", Mux: mux, Bootstrap: true})
	waitLeadership(t, l)

	// Closing the transport out from under a running raft is safe, which is what
	// lets Close call Group.Close unconditionally.
	if err := l.group.Close(); err != nil {
		t.Fatalf("closing the group before raft: %v", err)
	}
	if err := l.raft.Shutdown().Error(); err != nil {
		t.Fatalf("draining raft's shutdown future after the transport closed: %v", err)
	}

	assertRebindable(t, mux, "flow-order-b")
}

func TestCloseOrderingGroupCloseAfterMuxClose(t *testing.T) {
	// This mux is closed by the test rather than by a cleanup, because closing it
	// early IS the ordering under test.
	mux, err := transport.New(transport.Config{BindAddr: "127.0.0.1:0", RPCTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("binding a mux: %v", err)
	}
	l := openTestLedger(t, Config{Flow: "flow-order-c", LocalID: "n0", Mux: mux, Bootstrap: true})
	waitLeadership(t, l)

	if err := l.raft.Shutdown().Error(); err != nil {
		t.Fatalf("draining raft's shutdown future: %v", err)
	}
	if err := mux.Close(); err != nil {
		t.Fatalf("closing the mux: %v", err)
	}

	// A process tearing down closes the mux last, and a caller can invert that. If
	// Group.Close blocked here, shutdown would hang — so this is asserted against a
	// ceiling rather than simply awaited, and a block fails instead of passing
	// slowly.
	done := make(chan error, 1)
	go func() { done <- l.group.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("closing a group after its mux gave %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Group.Close blocked for 5s after the mux was closed; a process teardown in this order would hang")
	}
}

func assertRebindable(t *testing.T, mux *transport.Mux, flow string) {
	t.Helper()
	group, err := mux.Bind(transport.GroupID(flow))
	if err != nil {
		t.Fatalf("the group id %q was not freed by this close ordering: %v", flow, err)
	}
	if err := group.Close(); err != nil {
		t.Fatalf("closing the rebind probe for %q: %v", flow, err)
	}
}

func TestCloseReleasesTheGroupBindingAfterAnUndrainedShutdown(t *testing.T) {
	mux := testMux(t)
	l := openTestLedger(t, Config{Flow: "flow-removed", LocalID: "n0", Mux: mux, Bootstrap: true})
	waitLeadership(t, l)

	// EXACTLY THE SHAPE ShutdownOnRemove PRODUCES: raft shuts itself down and
	// DISCARDS the future. Only a drained future closes the transport, so the
	// binding survives this.
	_ = l.raft.Shutdown()

	// CONTROL, and it is the whole point of the test: the id is still held. If this
	// bound successfully, the assertion below would prove nothing about Close.
	if group, err := mux.Bind(transport.GroupID("flow-removed")); err == nil {
		_ = group.Close()
		t.Fatal("CONTROL FAILED: the group id was already free after an undrained raft shutdown, so this test cannot show that Close is what frees it")
	} else if !errors.Is(err, transport.ErrGroupBound) {
		t.Fatalf("rebinding after an undrained shutdown gave %v, want ErrGroupBound", err)
	}

	if err := l.Close(); err != nil {
		t.Fatalf("Close after an undrained raft shutdown: %v", err)
	}

	assertRebindable(t, mux, "flow-removed")
}
