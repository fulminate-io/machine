package transport

import (
	"errors"
	"net"
	"testing"
	"time"
)

// assertNoFurtherAccept fails if anything else arrives at a binding within a
// window. It is what turns "each side got its own connection" into "each side
// got EXACTLY ONE", so a table that delivered both connections to one binding
// cannot read as isolation.
func assertNoFurtherAccept(t *testing.T, accept func() (net.Conn, error), why string) {
	t.Helper()
	extra := make(chan net.Conn, 1)
	go func() {
		c, err := accept()
		if err == nil {
			extra <- c
		}
	}()
	select {
	case c := <-extra:
		_ = c.Close()
		t.Fatalf("%s: a second connection arrived: the two kinds are sharing one binding", why)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestMembershipAndRaftKindsWithTheSameIDReachDifferentBindings(t *testing.T) {
	m := testMux(t)
	// THE DISCRIMINATING INPUT: the raft group is bound under the very id string
	// the control channel uses. An implementation keyed by the id alone would
	// deliver both connections to whichever bound first, and would pass any test
	// that used two different ids.
	group, err := m.bindStream(membershipID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = group.Close() }()
	link, err := m.BindMembership()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = link.Close() }()

	control, err := link.Dial(m.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("membership dial: %v", err)
	}
	defer func() { _ = control.Close() }()
	assertDeliveredVia(t, link.Accept, control, "MEMBER", "a membership connection announcing the shared id")

	raftConn := dialSessionTagged(t, m, KindRaft, membershipID)
	assertDeliveredVia(t, group.Accept, raftConn, "RAFT!!", "a raft connection announcing the shared id")

	assertNoFurtherAccept(t, link.Accept, "the membership link")
	assertNoFurtherAccept(t, group.Accept, "the raft group")
}

func TestMembershipLinkCloseIsIdempotentAndLeavesTheListenerRunning(t *testing.T) {
	m := testMux(t)
	group, err := m.bindStream("still-running")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = group.Close() }()
	link, err := m.BindMembership()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.BindMembership(); !errors.Is(err, ErrGroupBound) {
		t.Fatalf("a second BindMembership: err = %v, want ErrGroupBound", err)
	}

	if err := link.Close(); err != nil {
		t.Fatalf("MembershipLink.Close: %v", err)
	}
	if err := link.Close(); err != nil {
		t.Fatalf("a second MembershipLink.Close must be idempotent, got %v", err)
	}
	if _, err := link.Accept(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Accept after Close: err = %v, want ErrClosed: the serve loop would park forever", err)
	}

	// THE CONTROL that separates "the link closed" from "the node closed": the
	// shared listener and every raft group on it must still be serving.
	assertDeliveredVia(t, group.Accept, dialSessionTagged(t, m, KindRaft, "still-running"), "ALIVE",
		"a raft group after the membership link closed")

	// And the control channel is rebindable, because Close frees its key exactly
	// as a group's Close frees its own.
	rebound, err := m.BindMembership()
	if err != nil {
		t.Fatalf("BindMembership after Close: %v", err)
	}
	_ = rebound.Close()
}
