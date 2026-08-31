package transport

import (
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"
)

func testMux(t *testing.T) *Mux {
	t.Helper()
	m, err := New(Config{BindAddr: "127.0.0.1:0", HandshakeTimeout: 500 * time.Millisecond, RPCTimeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func dialTagged(t *testing.T, m *Mux, id GroupID) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", m.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := writePreamble(c, id, 2*time.Second); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	return c
}

func TestUnknownGroupIsRefusedAndCounted(t *testing.T) {
	m := testMux(t)
	s, err := m.bindStream("known")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	c := dialTagged(t, m, "not-bound")
	if err := c.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("the connection for an unbound group was not closed")
	}

	c2 := dialTagged(t, m, "known")
	if _, err := c2.Write([]byte("HELLO")); err != nil {
		t.Fatal(err)
	}
	accepted, err := s.Accept()
	if err != nil {
		t.Fatalf("Accept after an unknown-group refusal: %v", err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(accepted, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "HELLO" {
		t.Fatalf("post-handshake stream = %q, want HELLO", got)
	}
	if s := m.Stats(); s.RejectedUnknownGroup != 1 {
		t.Fatalf("RejectedUnknownGroup = %d, want 1", s.RejectedUnknownGroup)
	}
}

func TestMalformedHandshakeIsRefusedAndCounted(t *testing.T) {
	m := testMux(t)
	c, err := net.DialTimeout("tcp", m.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("NOTMRMX\x00")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && m.Stats().RejectedMalformed == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if s := m.Stats(); s.RejectedMalformed != 1 || s.Handshakes != 0 {
		t.Fatalf("RejectedMalformed=%d Handshakes=%d, want 1 and 0", s.RejectedMalformed, s.Handshakes)
	}
}

func TestSilentPeerDoesNotDelayAnotherGroupsAccept(t *testing.T) {
	// A generous handshake window, so a serialized accept loop would hold the
	// listener for seconds rather than for a margin a fast machine hides.
	m, err := New(Config{BindAddr: "127.0.0.1:0", HandshakeTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	fast, err := m.bindStream("fast")
	if err != nil {
		t.Fatal(err)
	}

	// A peer that connects and then says nothing. If the handshake were read on
	// the accept loop, this alone would stop the node accepting anything.
	silent, err := net.DialTimeout("tcp", m.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = silent.Close() }()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	_ = dialTagged(t, m, "fast")
	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := fast.Accept()
		if aerr == nil {
			accepted <- c
		}
	}()
	select {
	case c := <-accepted:
		_ = c.Close()
	case <-time.After(time.Second):
		t.Fatalf("the fast group was not accepted within 1s while one silent peer was mid-handshake: the handshake is being read on the accept loop")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("fast group accepted only after %v", elapsed)
	}
}

func TestOverfilledGroupQueueRefusesAndCounts(t *testing.T) {
	m := testMux(t)
	slow, err := m.bindStream("slow")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slow.Close() }()
	// Nothing ever Accepts on "slow": fill its queue and then over-fill it.
	for i := 0; i < m.cfg.AcceptQueueDepth+3; i++ {
		_ = dialTagged(t, m, "slow")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && m.Stats().RejectedQueueFull == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	s := m.Stats()
	if s.RejectedQueueFull == 0 {
		t.Fatal("an over-filled group queue refused nothing: connections are being held rather than refused and counted")
	}
	if s.Handshakes < uint64(m.cfg.AcceptQueueDepth+3) {
		t.Fatalf("CONTROL FAILED: only %d handshakes completed, so the queue was never filled", s.Handshakes)
	}
}

// TestBindRejectsDuplicatesAndBadIDs drives bindStream, which is where the id
// range, duplicate and closed-mux rules actually live; Bind delegates to it.
// Phase 4 extends this test with the same three rules driven through the
// exported Bind, once *Group exists for it to return.
func TestBindRejectsDuplicatesAndBadIDs(t *testing.T) {
	m := testMux(t)
	s, err := m.bindStream("dup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.bindStream("dup"); !errors.Is(err, ErrGroupBound) {
		t.Fatalf("duplicate bind: err = %v, want ErrGroupBound", err)
	}
	if _, err := m.bindStream(""); !errors.Is(err, ErrGroupIDRange) {
		t.Fatalf("empty id bind: err = %v, want ErrGroupIDRange", err)
	}
	long := GroupID(make([]byte, MaxGroupIDLen+1))
	if _, err := m.bindStream(long); !errors.Is(err, ErrGroupIDRange) {
		t.Fatalf("over-long id bind: err = %v, want ErrGroupIDRange", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.bindStream("dup"); err != nil {
		t.Fatalf("rebind after Close: %v", err)
	}
}

func TestUnspecifiedBindAddressWithoutAdvertiseIsRefused(t *testing.T) {
	if _, err := New(Config{BindAddr: "0.0.0.0:0"}); !errors.Is(err, ErrNotAdvertisable) {
		t.Fatalf("err = %v, want ErrNotAdvertisable", err)
	}
}

func TestCloseUnblocksEveryGroupAcceptAndRefusesBind(t *testing.T) {
	m, err := New(Config{BindAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := m.bindStream("a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.bindStream("b")
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for _, s := range []*groupStream{a, b} {
		go func(s *groupStream) {
			_, err := s.Accept()
			errs <- err
		}(s)
	}
	time.Sleep(50 * time.Millisecond)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if !errors.Is(err, ErrClosed) {
				t.Fatalf("Accept after Close: err = %v, want ErrClosed", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Accept did not return after Close: raft's listen goroutine would leak")
		}
	}
	if _, err := m.bindStream("c"); !errors.Is(err, ErrClosed) {
		t.Fatalf("bind after Close: err = %v, want ErrClosed", err)
	}
}

// flakyListener returns a caller-supplied error from Accept a fixed number of
// times before delegating to the real listener, so a transient accept failure
// can be reproduced without exhausting the process's file descriptors.
type flakyListener struct {
	net.Listener
	mu        sync.Mutex
	remaining int
	err       error
	returned  int
}

func (f *flakyListener) Accept() (net.Conn, error) {
	f.mu.Lock()
	if f.remaining > 0 {
		f.remaining--
		f.returned++
		f.mu.Unlock()
		return nil, f.err
	}
	f.mu.Unlock()
	return f.Listener.Accept()
}

func (f *flakyListener) errorsReturned() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.returned
}

func TestTransientAcceptErrorDoesNotRetireTheListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// EMFILE is the real-world shape: the socket is healthy, the process is out
	// of descriptors. The Go runtime retries only EINTR and ECONNABORTED, so
	// this one reaches us.
	flaky := &flakyListener{Listener: ln, remaining: 3, err: syscall.EMFILE}
	m, err := newMux(flaky, Config{HandshakeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	go m.accept()

	s, err := m.bindStream("after-emfile")
	if err != nil {
		t.Fatal(err)
	}
	// Let the loop burn through its transient errors and back off.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && flaky.errorsReturned() < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	if flaky.errorsReturned() < 3 {
		t.Fatalf("CONTROL FAILED: the accept loop consumed only %d of 3 injected errors", flaky.errorsReturned())
	}
	if got := m.Stats().AcceptErrors; got < 3 {
		t.Fatalf("AcceptErrors = %d after 3 injected failures, want at least 3", got)
	}

	// The listener is still healthy and the mux must still serve it.
	_ = dialTagged(t, m, "after-emfile")
	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := s.Accept()
		if aerr == nil {
			accepted <- c
		}
	}()
	select {
	case c := <-accepted:
		_ = c.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the mux served nothing after a transient accept error: one EMFILE retired the node's whole inbound path")
	}
}

func TestConnectionForAGroupUnboundMidHandshakeIsCounted(t *testing.T) {
	m := testMux(t)
	s, err := m.bindStream("vanishing")
	if err != nil {
		t.Fatal(err)
	}
	// Fill the queue so the next connection parks on the select, then unbind
	// underneath it: the done arm is the one that must count.
	for i := 0; i < m.cfg.AcceptQueueDepth; i++ {
		_ = dialTagged(t, m, "vanishing")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && m.Stats().Handshakes < uint64(m.cfg.AcceptQueueDepth) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = dialTagged(t, m, "vanishing")
	time.Sleep(100 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && m.Stats().RejectedGroupClosed == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := m.Stats().RejectedGroupClosed; got == 0 {
		t.Fatal("a connection was dropped on the group-closed arm without any counter moving")
	}
}
