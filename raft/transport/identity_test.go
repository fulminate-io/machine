package transport

import (
	"net"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// recordingListener remembers every connection it returns so a test can assert
// raft is handed that same object.
type recordingListener struct {
	net.Listener
	last chan net.Conn
}

func (r *recordingListener) Accept() (net.Conn, error) {
	c, err := r.Listener.Accept()
	if err == nil {
		r.last <- c
	}
	return c, err
}

// THESE TWO REPLACE lane B's TestAcceptHandsRaftTheListenersOwnConnection and
// TestDialReturnsTheRawTCPConnection, which asserted that raft is handed the
// listener's object BY POINTER IDENTITY and that Dial returns a *net.TCPConn.
// Encrypting the stream makes both false by construction, so the surface they
// protected genuinely no longer exists. What they were actually protecting —
// that the mux contributes nothing to the per-RPC path — survives as EXACTLY ONE
// wrapper, and that is what these assert: a second wrapper, a buffered reader or
// a copy still fails, which is the defect class the originals caught.

func TestRaftReceivesASessionConnOverTheListenersOwnConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingListener{Listener: ln, last: make(chan net.Conn, 4)}
	m, err := newMux(rec, Config{HandshakeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	go m.accept()

	s, err := m.bindStream("identity")
	if err != nil {
		t.Fatal(err)
	}
	dialed := dialSessionTagged(t, m, KindRaft, "identity")
	defer func() { _ = dialed.Close() }()

	accepted, err := s.Accept()
	if err != nil {
		t.Fatal(err)
	}
	session, ok := accepted.(*sessionConn)
	if !ok {
		t.Fatalf("raft was handed %T, want *sessionConn: the ruled encryption is absent", accepted)
	}
	select {
	case produced := <-rec.last:
		if session.Conn != produced {
			t.Fatalf("the session wraps %p but the listener produced %p: something else sits between the session "+
				"and the socket, which would put a second layer on the per-RPC path",
				session.Conn, produced)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CONTROL FAILED: the recording listener produced nothing")
	}
}

func TestDialReturnsASessionConnOverARawTCPConnection(t *testing.T) {
	m := testMux(t)
	if _, err := m.bindStream("dial-shape"); err != nil {
		t.Fatal(err)
	}
	s := &groupStream{mux: m, id: "dial-shape"}
	conn, err := s.Dial(raft.ServerAddress(m.Addr().String()), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	session, ok := conn.(*sessionConn)
	if !ok {
		t.Fatalf("Dial returned %T, want *sessionConn: the ruled encryption is absent", conn)
	}
	if _, ok := session.Conn.(*net.TCPConn); !ok {
		t.Fatalf("the session wraps %T, want *net.TCPConn: exactly one wrapper may sit between raft and the socket",
			session.Conn)
	}
}
