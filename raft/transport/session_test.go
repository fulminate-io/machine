package transport

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// tapConn records the bytes that actually cross the socket in each direction.
// It sits UNDER the session layer, between it and the real connection, so what
// it holds is what a passive observer on the wire would see — not what any of
// our own buffers happen to contain.
type tapConn struct {
	net.Conn
	mu   sync.Mutex
	read []byte
	sent []byte
}

func (c *tapConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.mu.Lock()
		c.read = append(c.read, p[:n]...)
		c.mu.Unlock()
	}
	return n, err
}

func (c *tapConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.mu.Lock()
		c.sent = append(c.sent, p[:n]...)
		c.mu.Unlock()
	}
	return n, err
}

func (c *tapConn) observed() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append(append([]byte{}, c.read...), c.sent...)
}

// tapListener hands the mux tapped connections, so every byte the mux reads or
// writes on an accepted connection is recorded at the socket.
type tapListener struct {
	net.Listener
	mu   sync.Mutex
	taps []*tapConn
}

func (l *tapListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tapped := &tapConn{Conn: c}
	l.mu.Lock()
	l.taps = append(l.taps, tapped)
	l.mu.Unlock()
	return tapped, nil
}

func (l *tapListener) tapAt(i int) *tapConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	if i >= len(l.taps) {
		return nil
	}
	return l.taps[i]
}

// tappedMux builds a mux over a tapping listener.
func tappedMux(t *testing.T, tokens ...Token) (*Mux, *tapListener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tl := &tapListener{Listener: ln}
	m, err := newMux(tl, Config{HandshakeTimeout: 2 * time.Second, Tokens: tokens})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	go m.accept()
	return m, tl
}

// framedPlaintext frames payload as a record whose DECLARED length is honest but
// whose body is plaintext. The declared length is what makes it the control:
// without it the acceptor rejects the framing after two bytes and the tap never
// sees the payload at all, so a clean session tap could not be distinguished
// from a tap that recorded nothing.
func framedPlaintext(payload string) []byte {
	body := make([]byte, len(payload)+recordOverhead)
	copy(body, payload)
	out := make([]byte, 2, 2+len(body))
	binary.BigEndian.PutUint16(out, uint16(len(body)))
	return append(out, body...)
}

func TestASessionProtectedStreamCarriesNoPlaintextRaftBytes(t *testing.T) {
	const tok Token = "join-token"
	const payload = "PLAINTEXT-CANARY-0123456789"
	m, taps := tappedMux(t, tok)
	s, err := m.bindStream("known")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	// ARM ONE: the payload crosses a session-protected connection.
	protected := dialHeadSession(t, m, headUnder(t, KindRaft, "known", tok), tok)
	if _, err := protected.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	accepted, err := s.Accept()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(accepted, got); err != nil {
		t.Fatalf("the protected stream did not deliver its payload: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("delivered %q, want %q", got, payload)
	}

	// ARM TWO, THE CONTROL: the same payload crosses a connection that speaks the
	// preamble and then plaintext. It is refused, but the tap records it — which
	// is what proves the instrument can see plaintext when plaintext is there.
	clear := dialRaw(t, m, headUnder(t, KindRaft, "known", tok))
	if _, err := io.ReadFull(clear, make([]byte, sessionNonceLen)); err != nil {
		t.Fatal(err)
	}
	if _, err := clear.Write(framedPlaintext(payload)); err != nil {
		t.Fatal(err)
	}
	acceptedClear, err := s.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acceptedClear.Read(make([]byte, 64)); !errors.Is(err, ErrSessionRecord) {
		t.Fatalf("a plaintext body was read as a record: err = %v, want ErrSessionRecord", err)
	}

	protectedTap, clearTap := taps.tapAt(0), taps.tapAt(1)
	if protectedTap == nil || clearTap == nil {
		t.Fatal("CONTROL FAILED: the tapping listener recorded fewer than two connections")
	}
	protectedBytes, clearBytes := protectedTap.observed(), clearTap.observed()
	t.Logf("observed on the socket: %d bytes on the protected connection, %d on the cleartext control",
		len(protectedBytes), len(clearBytes))
	if !bytes.Contains(clearBytes, []byte(payload)) {
		t.Fatal("CONTROL FAILED: the tap did not record the payload even on the cleartext connection, " +
			"so a clean protected tap proves nothing")
	}
	if bytes.Contains(protectedBytes, []byte(payload)) {
		t.Fatalf("the payload crossed the socket in the clear on the protected connection")
	}
}

// capturedSession dials m by hand and returns the wrapped connection together
// with the preamble it wrote and the exact record bytes its first write put on
// the wire — everything a passive observer could capture.
func capturedSession(t *testing.T, m *Mux, tok Token, id GroupID, payload string) (head, record []byte) {
	t.Helper()
	raw, err := net.DialTimeout("tcp", m.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	tapped := &tapConn{Conn: raw}
	head = headUnder(t, KindRaft, id, tok)
	if _, err := tapped.Write(head); err != nil {
		t.Fatal(err)
	}
	wrapped, err := wrapDialed(tapped, tok, head[nonceOff:stampOff], 3*time.Second)
	if err != nil {
		t.Fatalf("session exchange: %v", err)
	}
	tapped.mu.Lock()
	before := len(tapped.sent)
	tapped.mu.Unlock()
	if _, err := wrapped.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	tapped.mu.Lock()
	record = append([]byte{}, tapped.sent[before:]...)
	tapped.mu.Unlock()
	return head, record
}

func TestAReplayedHandshakeCannotOpenASecondSession(t *testing.T) {
	const tok Token = "join-token"
	const payload = "REPLAY-SUBJECT-PAYLOAD"
	m := tokenMux(t, tok)
	s, err := m.bindStream("known")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	// The observer captures a complete, valid exchange.
	head, record := capturedSession(t, m, tok, "known", payload)
	original, err := s.Accept()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(original, got); err != nil || string(got) != payload {
		t.Fatalf("CONTROL FAILED: the captured exchange was not itself valid: %q err=%v", got, err)
	}

	// THE REPLAY. The preamble is replayed VERBATIM and is still inside the clock
	// window, so admission is not what stops it. The acceptor answers with a
	// FRESH nonce, which derives a different key, so the captured record — a
	// record that was valid moments ago on this very mux — cannot be opened.
	replay := dialRaw(t, m, head)
	if _, err := io.ReadFull(replay, make([]byte, sessionNonceLen)); err != nil {
		t.Fatalf("the replayed preamble was not admitted, so this test would pass for the wrong reason: %v", err)
	}
	if _, err := replay.Write(record); err != nil {
		t.Fatal(err)
	}
	replayed, err := s.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replayed.Read(make([]byte, 64)); !errors.Is(err, ErrSessionRecord) {
		t.Fatalf("a replayed record was accepted: err = %v, want ErrSessionRecord", err)
	}
}

func TestATamperedRecordIsRefused(t *testing.T) {
	const tok Token = "join-token"
	const payload = "TAMPER-SUBJECT-PAYLOAD"
	clientNonce := bytes.Repeat([]byte{0xA1}, sessionNonceLen)
	serverNonce := bytes.Repeat([]byte{0xB2}, sessionNonceLen)
	clientSend, _, err := deriveSession(tok, clientNonce, serverNonce, true)
	if err != nil {
		t.Fatal(err)
	}

	// One record, sealed exactly as writeRecord seals it.
	sealed := clientSend.Seal(make([]byte, 2), make([]byte, 12), []byte(payload), nil)
	binary.BigEndian.PutUint16(sealed[:2], uint16(len(sealed)-2))

	// THE CONTROL runs first and through the same instrument: the untampered
	// record opens and delivers, so the refusal below is the tamper rather than
	// the framing, the nonce or the key pairing.
	if got := readOneRecord(t, tok, clientNonce, serverNonce, sealed); got != payload {
		t.Fatalf("CONTROL FAILED: the untampered record delivered %q, want %q", got, payload)
	}

	tampered := append([]byte{}, sealed...)
	tampered[len(tampered)-1] ^= 0x01
	if got := readOneRecord(t, tok, clientNonce, serverNonce, tampered); got != "" {
		t.Fatalf("a record with one flipped ciphertext byte was delivered as %q: the AEAD tag is not being checked", got)
	}
}

// readOneRecord feeds raw bytes to a receiving session over a pipe and reports
// the plaintext it yielded, or "" when the session refused them.
func readOneRecord(t *testing.T, tok Token, clientNonce, serverNonce, raw []byte) string {
	t.Helper()
	_, serverRecv, err := deriveSession(tok, clientNonce, serverNonce, false)
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	go func() { _, _ = client.Write(raw) }()
	if err := server.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn := newSessionConn(server, serverRecv, serverRecv)
	buf := make([]byte, 128)
	n, err := conn.Read(buf)
	if err != nil {
		if !errors.Is(err, ErrSessionRecord) {
			t.Fatalf("the session failed for an unexpected reason: %v", err)
		}
		return ""
	}
	return string(buf[:n])
}

func TestARefusedConnectionIsClosedWithoutTheServerWritingAByte(t *testing.T) {
	const tok Token = "join-token"
	m := tokenMux(t, tok)
	s, err := m.bindStream("known")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	// THE CONTROL, same instrument, same run: a DELIVERED connection does receive
	// the acceptor's nonce. Without it, "no bytes arrived" could mean the read
	// never worked rather than that the server stayed silent.
	admitted := dialRaw(t, m, headUnder(t, KindRaft, "known", tok))
	if err := admitted.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if n, err := io.ReadFull(admitted, make([]byte, sessionNonceLen)); err != nil || n != sessionNonceLen {
		t.Fatalf("CONTROL FAILED: a delivered connection received %d bytes, err=%v", n, err)
	}

	for _, tc := range []struct {
		name string
		head []byte
	}{
		{"an unbound group", headUnder(t, KindRaft, "not-bound", tok)},
		{"an unacceptable proof", headUnder(t, KindRaft, "known", "not-the-token")},
	} {
		refused := dialRaw(t, m, tc.head)
		if err := refused.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		n, err := refused.Read(buf)
		if n != 0 {
			t.Fatalf("%s: the server wrote %d bytes before refusing (% x): a peer can learn what this node hosts "+
				"by watching for a nonce", tc.name, n, buf[:n])
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("%s: the connection was left open rather than closed: %v", tc.name, err)
		}
	}
}
