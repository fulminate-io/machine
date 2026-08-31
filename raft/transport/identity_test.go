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

func TestAcceptHandsRaftTheListenersOwnConnection(t *testing.T) {
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
	dialed := dialTagged(t, m, "identity")
	defer func() { _ = dialed.Close() }()

	accepted, err := s.Accept()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case produced := <-rec.last:
		if produced != accepted {
			t.Fatalf("raft was handed %p but the listener produced %p: the mux wrapped the connection, which would put it on the per-RPC path", accepted, produced)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CONTROL FAILED: the recording listener produced nothing")
	}
}

func TestDialReturnsTheRawTCPConnection(t *testing.T) {
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
	if _, ok := conn.(*net.TCPConn); !ok {
		t.Fatalf("Dial returned %T, want *net.TCPConn: raft must receive the connection unwrapped", conn)
	}
}
