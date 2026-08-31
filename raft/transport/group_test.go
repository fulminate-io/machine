package transport

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func listenGoroutines() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "raft.(*NetworkTransport).listen")
}

func TestGroupCloseStopsOnlyThatGroupsListener(t *testing.T) {
	m := testMux(t)
	alpha, err := m.Bind("alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := m.Bind("beta")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = beta.Close() }()
	time.Sleep(50 * time.Millisecond)

	before := listenGoroutines()
	if before < 2 {
		t.Fatalf("CONTROL FAILED: expected at least 2 raft listen goroutines, saw %d", before)
	}
	// Every group on a node answers at the one shared address.
	if string(alpha.Transport().LocalAddr()) != m.Addr().String() {
		t.Fatalf("alpha LocalAddr = %s, want the mux address %s", alpha.Transport().LocalAddr(), m.Addr())
	}
	if string(beta.Transport().LocalAddr()) != m.Addr().String() {
		t.Fatalf("beta LocalAddr = %s, want the mux address %s", beta.Transport().LocalAddr(), m.Addr())
	}
	if alpha.ID() != "alpha" {
		t.Fatalf("ID = %q, want alpha", alpha.ID())
	}

	if err := alpha.Close(); err != nil {
		t.Fatal(err)
	}
	// alpha's listen goroutine must go. The count is process-global, so other
	// tests tearing down concurrently can only push it further down: a leak is
	// the case where it never drops at all.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && listenGoroutines() > before-1 {
		time.Sleep(20 * time.Millisecond)
	}
	if after := listenGoroutines(); after > before-1 {
		t.Fatalf("raft listen goroutines went %d -> %d: the closed group's listener never exited", before, after)
	}
	if err := alpha.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// alpha's own transport reports itself shut down; beta's does not.
	if !alpha.Transport().IsShutdown() {
		t.Fatal("the closed group's transport does not report itself shut down")
	}
	if beta.Transport().IsShutdown() {
		t.Fatal("closing one group shut down a sibling group's transport")
	}

	// alpha's tag is unknown now; beta still routes a connection end to end.
	_ = dialTagged(t, m, "alpha")
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && m.Stats().RejectedUnknownGroup == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if s := m.Stats(); s.RejectedUnknownGroup != 1 {
		t.Fatalf("RejectedUnknownGroup = %d after dialing the closed group, want 1", s.RejectedUnknownGroup)
	}
	before2 := m.Stats().Handshakes
	_ = dialTagged(t, m, "beta")
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && m.Stats().Handshakes == before2 {
		time.Sleep(20 * time.Millisecond)
	}
	if m.Stats().Handshakes == before2 {
		t.Fatal("the surviving group stopped routing after its sibling was closed")
	}
	if _, err := m.Bind("alpha"); err != nil {
		t.Fatalf("rebinding a closed group id: %v", err)
	}
}

func TestBindAfterMuxCloseIsRefused(t *testing.T) {
	m, err := New(Config{BindAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Bind("x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}
