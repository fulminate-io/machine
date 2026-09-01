package transport

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// dialForwardTagged is dialTagged's forwarding counterpart: the same dial to the
// same shared listener, announcing the same group id, differing only in the kind
// it writes into the handshake.
func dialForwardTagged(t *testing.T, m *Mux, id GroupID) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", m.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := writePreamble(c, id, KindForward, 2*time.Second); err != nil {
		t.Fatalf("forwarding handshake: %v", err)
	}
	return c
}

// assertDelivers reads what one arm was handed and fails if it is the other
// arm's payload, which is the shape a swapped dispatch produces while both
// queues still move.
func assertDelivers(t *testing.T, accept func() (net.Conn, error), want, arm string) {
	t.Helper()
	c, err := accept()
	if err != nil {
		t.Fatalf("%s: Accept: %v", arm, err)
	}
	defer func() { _ = c.Close() }()
	if err := c.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("%s: read: %v", arm, err)
	}
	if string(got) != want {
		t.Fatalf("%s received %q, want %q: the delivery arms are crossed", arm, got, want)
	}
}

func TestForwardingAndRaftStreamsShareOneListener(t *testing.T) {
	m := testMux(t)
	s, err := m.bindStream("shared")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	fwd := &forwardListener{stream: s}

	// ONE listener and ONE group id, two kinds. Each connection writes a payload
	// naming the arm it belongs to.
	raftConn := dialTagged(t, m, "shared")
	defer func() { _ = raftConn.Close() }()
	if _, err := raftConn.Write([]byte("RAFT!")); err != nil {
		t.Fatal(err)
	}
	forwardConn := dialForwardTagged(t, m, "shared")
	defer func() { _ = forwardConn.Close() }()
	if _, err := forwardConn.Write([]byte("FWD!!")); err != nil {
		t.Fatal(err)
	}

	assertDelivers(t, s.Accept, "RAFT!", "the raft arm")
	assertDelivers(t, fwd.Accept, "FWD!!", "the forwarding arm")

	st := m.Stats()
	if st.Handshakes != 1 {
		t.Fatalf("Handshakes = %d, want 1: the arms must count separately", st.Handshakes)
	}
	if st.ForwardHandshakes != 1 {
		t.Fatalf("ForwardHandshakes = %d, want 1: the arms must count separately", st.ForwardHandshakes)
	}
}

func TestUndeclaredStreamKindIsRefusedAndCounted(t *testing.T) {
	m := testMux(t)
	s, err := m.bindStream("declared-only")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	// encodePreamble writes the kind it is given rather than policing it, which
	// is what lets this test announce a kind this build does not declare.
	head, err := encodePreamble("declared-only", StreamKind(99))
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", m.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write(head); err != nil {
		t.Fatal(err)
	}
	if err := c.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("a connection announcing an undeclared stream kind was not closed")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && m.Stats().RejectedUnknownKind == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	st := m.Stats()
	if st.RejectedUnknownKind != 1 {
		t.Fatalf("RejectedUnknownKind = %d, want 1", st.RejectedUnknownKind)
	}
	if st.Handshakes != 0 {
		t.Fatalf("Handshakes = %d, want 0: an undeclared kind was admitted as a raft handshake", st.Handshakes)
	}
	if st.RejectedMalformed != 0 {
		t.Fatalf("RejectedMalformed = %d, want 0: a version skew must stay distinguishable from a stray client", st.RejectedMalformed)
	}

	// CONTROL: the same dial to the same group with a DECLARED kind is delivered,
	// so the refusal above is about the kind rather than about a broken listener.
	ctl := dialTagged(t, m, "declared-only")
	defer func() { _ = ctl.Close() }()
	accepted, err := s.Accept()
	if err != nil {
		t.Fatalf("CONTROL FAILED: a declared kind was not delivered: %v", err)
	}
	_ = accepted.Close()
}

func TestForwardingRefusalsAreCountedLikeTheRaftArms(t *testing.T) {
	t.Run("backlogged forwarding queue", func(t *testing.T) {
		m := testMux(t)
		slow, err := m.bindStream("slow-forward")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = slow.Close() }()
		// Nothing ever Accepts the forwarding arm: fill its queue and over-fill it.
		for i := 0; i < m.cfg.AcceptQueueDepth+3; i++ {
			_ = dialForwardTagged(t, m, "slow-forward")
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && m.Stats().RejectedForwardQueueFull == 0 {
			time.Sleep(20 * time.Millisecond)
		}
		st := m.Stats()
		if st.RejectedForwardQueueFull == 0 {
			t.Fatal("an over-filled forwarding queue refused nothing: connections are held rather than refused and counted")
		}
		if st.ForwardHandshakes < uint64(m.cfg.AcceptQueueDepth+3) {
			t.Fatalf("CONTROL FAILED: only %d forwarding handshakes completed, so the queue was never filled", st.ForwardHandshakes)
		}
		if st.RejectedQueueFull != 0 {
			t.Fatalf("RejectedQueueFull = %d, want 0: a forwarding backlog must not count against the raft arm", st.RejectedQueueFull)
		}
	})

	t.Run("group released mid-handshake", func(t *testing.T) {
		m := testMux(t)
		s, err := m.bindStream("vanishing-forward")
		if err != nil {
			t.Fatal(err)
		}
		// Fill the forwarding queue so the next connection parks on the select,
		// then unbind underneath it: the done arm is the one that must count.
		for i := 0; i < m.cfg.AcceptQueueDepth; i++ {
			_ = dialForwardTagged(t, m, "vanishing-forward")
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && m.Stats().ForwardHandshakes < uint64(m.cfg.AcceptQueueDepth) {
			time.Sleep(10 * time.Millisecond)
		}
		_ = dialForwardTagged(t, m, "vanishing-forward")
		time.Sleep(100 * time.Millisecond)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		deadline = time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && m.Stats().RejectedGroupClosed == 0 {
			time.Sleep(20 * time.Millisecond)
		}
		if got := m.Stats().RejectedGroupClosed; got == 0 {
			t.Fatal("a forwarding connection was dropped on the group-closed arm without any counter moving")
		}
	})
}

func TestForwardingListenerReportsTheSharedAddressAndClosesWithItsGroup(t *testing.T) {
	m := testMux(t)
	g, err := m.Bind("flow-lifetime")
	if err != nil {
		t.Fatal(err)
	}
	l := g.Forward()
	if l.Addr().String() != m.Addr().String() {
		t.Fatalf("forwarding listener Addr = %s, want the shared mux address %s", l.Addr(), m.Addr())
	}

	// The listener's own Close is a no-op because the stream's lifetime is the
	// GROUP's. The control that separates a no-op from an unbind is that
	// forwarding still works after it.
	if err := l.Close(); err != nil {
		t.Fatalf("forwardListener.Close: %v", err)
	}
	sent := dialForwardTagged(t, m, "flow-lifetime")
	defer func() { _ = sent.Close() }()
	accepted, err := l.Accept()
	if err != nil {
		t.Fatalf("Accept after the listener's own Close: %v — that Close must not end the stream", err)
	}
	_ = accepted.Close()

	// The group's door is the one that ends it.
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Accept(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Accept after Group.Close: err = %v, want ErrClosed", err)
	}
}

func TestDialForwardReturnsTheRawTCPConnection(t *testing.T) {
	m := testMux(t)
	s, err := m.bindStream("dial-forward-shape")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	g := &Group{stream: s}
	conn, err := g.DialForward(m.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, ok := conn.(*net.TCPConn); !ok {
		t.Fatalf("DialForward returned %T, want *net.TCPConn: the caller must receive the connection unwrapped", conn)
	}
}
