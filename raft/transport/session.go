// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package transport

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"sync"
	"time"
)

const (
	// sessionNonceLen is the size of each side's contribution to the key
	// derivation. The dialer's rides the authentication block the admission gate
	// already carries; the acceptor answers with its own.
	sessionNonceLen = 16
	// keyLen is an AES-256 key. Two are derived, so the two directions never
	// share a keystream.
	keyLen = 32
	// maxRecord caps the PLAINTEXT carried by one record. A larger write is
	// split across records rather than refused.
	maxRecord = 16 * 1024
	// recordOverhead is the AES-GCM authentication tag appended to every record.
	recordOverhead = 16
	// sessionInfo domain-separates this derivation from any other use of the
	// same token.
	sessionInfo = "machine raft transport session v1"
)

// ErrSessionRecord refuses a record that is framed impossibly or fails its
// authentication tag. The two are one error on purpose: a peer that cannot
// produce a valid record learns nothing from us about which of the two it got
// wrong.
var ErrSessionRecord = errors.New("transport: session record is malformed or forged")

// deriveSession turns the shared token and the two nonces into the AEAD pair for
// one connection.
//
// BOTH SIDES CONTRIBUTE, AND THAT IS WHAT CLOSES THE REPLAY RESIDUAL. A replayed
// preamble meets a FRESH acceptor nonce, so it derives different keys and the
// replayer cannot produce a single record the acceptor will open — the captured
// handshake buys nothing even inside the clock window.
//
// AN EMPTY TOKEN STILL DERIVES A SESSION, and the consequence is stated rather
// than hidden: with no shared secret the exchange gives confidentiality against
// a passive observer and NO authentication. That is exactly the admission model
// a tokenless mux already runs, since it admits every peer — and a tokenless mux
// is refused on any address but loopback, so the shape cannot reach a network.
func deriveSession(tok Token, clientNonce, serverNonce []byte, dialing bool) (send, recv cipher.AEAD, err error) {
	salt := make([]byte, 0, len(clientNonce)+len(serverNonce))
	salt = append(salt, clientNonce...)
	salt = append(salt, serverNonce...)
	material, err := hkdf.Key(sha256.New, []byte(tok), salt, sessionInfo, 2*keyLen)
	if err != nil {
		return nil, nil, err
	}
	clientToServer, err := newAEAD(material[:keyLen])
	if err != nil {
		return nil, nil, err
	}
	serverToClient, err := newAEAD(material[keyLen:])
	if err != nil {
		return nil, nil, err
	}
	if dialing {
		return clientToServer, serverToClient, nil
	}
	return serverToClient, clientToServer, nil
}

// newAEAD builds AES-256-GCM over key. GCM is chosen over ChaCha20-Poly1305
// because it is stdlib and hardware-accelerated on both architectures the CI
// matrix runs.
func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sessionConn is the encrypted record layer over one TCP connection.
//
// EXACTLY ONE WRAPPER SITS BETWEEN RAFT AND THE SOCKET. net.Conn is embedded, so
// Close, the addresses and every deadline reach the socket directly rather than
// through a second layer, and the underlying connection stays the object the
// listener produced.
//
// A SESSION IS EXACTLY ONE CONNECTION, which is what makes the counter nonce
// safe: the sequence starts at zero per direction and never repeats under a key,
// because the key is never reused across connections.
type sessionConn struct {
	net.Conn
	send, recv cipher.AEAD

	wmu       sync.Mutex
	sendNonce [12]byte
	wbuf      []byte

	rmu       sync.Mutex
	recvNonce [12]byte
	hdr       [2]byte
	cbuf      []byte
	plain     []byte
}

// newSessionConn wraps c with the AEAD pair derived for it.
func newSessionConn(c net.Conn, send, recv cipher.AEAD) *sessionConn {
	return &sessionConn{Conn: c, send: send, recv: recv}
}

// Write seals p into as many records as it takes. A short write reports the
// plaintext bytes that actually reached the socket, so a caller retrying from
// that offset does not duplicate a record.
func (c *sessionConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	written := 0
	rest := p
	for len(rest) > 0 {
		chunk := rest
		if len(chunk) > maxRecord {
			chunk = chunk[:maxRecord]
		}
		if err := c.writeRecord(chunk); err != nil {
			return written, err
		}
		written += len(chunk)
		rest = rest[len(chunk):]
	}
	return written, nil
}

// writeRecord seals one chunk and frames it. Seal writes into the connection's
// own scratch buffer, past the two header bytes, so a record costs no allocation
// once the buffer has grown to the largest record this connection has carried.
func (c *sessionConn) writeRecord(chunk []byte) error {
	need := 2 + len(chunk) + recordOverhead
	if cap(c.wbuf) < need {
		c.wbuf = make([]byte, need)
	}
	buf := c.wbuf[:2]
	buf = c.send.Seal(buf, c.sendNonce[:], chunk, nil)
	nextNonce(&c.sendNonce)
	n := len(buf) - 2
	// The ceiling is maxRecord+recordOverhead, far below MaxUint16; the check is
	// what makes the conversion below provably in range rather than merely so.
	if n > math.MaxUint16 {
		return ErrSessionRecord
	}
	binary.BigEndian.PutUint16(buf[:2], uint16(n))
	_, err := c.Conn.Write(buf)
	return err
}

// Read serves decrypted plaintext, reading one more record whenever the last
// one is drained. A record is opened whole before any of it is handed out,
// because a partially delivered record cannot be un-delivered once its tag turns
// out to be wrong.
func (c *sessionConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if len(c.plain) == 0 {
		if err := c.readRecord(); err != nil {
			return 0, err
		}
	}
	n := copy(p, c.plain)
	c.plain = c.plain[n:]
	return n, nil
}

// readRecord reads one framed record and opens it in place.
//
// THE DECLARED LENGTH IS BOUNDED BEFORE IT SIZES ANYTHING. A peer that lies in
// two bytes cannot make this process allocate more than one record's worth, the
// same discipline the handshake applies to the announced group id length.
func (c *sessionConn) readRecord() error {
	if _, err := io.ReadFull(c.Conn, c.hdr[:]); err != nil {
		return err
	}
	n := int(binary.BigEndian.Uint16(c.hdr[:]))
	if n < recordOverhead || n > maxRecord+recordOverhead {
		return ErrSessionRecord
	}
	if cap(c.cbuf) < n {
		c.cbuf = make([]byte, n)
	}
	sealed := c.cbuf[:n]
	if _, err := io.ReadFull(c.Conn, sealed); err != nil {
		return err
	}
	plain, err := c.recv.Open(sealed[:0], c.recvNonce[:], sealed, nil)
	if err != nil {
		return ErrSessionRecord
	}
	nextNonce(&c.recvNonce)
	c.plain = plain
	return nil
}

// nextNonce advances a direction's record counter. The first four bytes stay
// zero and the counter occupies the last eight, so a connection would have to
// carry 2^64 records before one repeated.
func nextNonce(n *[12]byte) {
	binary.BigEndian.PutUint64(n[4:], binary.BigEndian.Uint64(n[4:])+1)
}

// wrapAccepted answers the dialer's nonce with a fresh one and returns the
// encrypted connection.
//
// IT RUNS ONLY ONCE THE CONNECTION IS GOING TO BE DELIVERED, and that ordering
// was measured rather than preferred: a server nonce written before the group
// lookup leaves bytes in flight where a refusal must be a bare close, which is
// how a peer with no business here would learn which groups this node hosts.
func wrapAccepted(conn net.Conn, tok Token, clientNonce []byte, timeout time.Duration) (net.Conn, error) {
	serverNonce := make([]byte, sessionNonceLen)
	// crypto/rand.Read never returns an error and always fills its argument.
	_, _ = rand.Read(serverNonce)
	send, recv, err := deriveSession(tok, clientNonce, serverNonce, false)
	if err != nil {
		return nil, err
	}
	if err := setWriteDeadline(conn, timeout); err != nil {
		return nil, err
	}
	if _, err := conn.Write(serverNonce); err != nil {
		return nil, err
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return newSessionConn(conn, send, recv), nil
}

// wrapDialed reads the acceptor's nonce and returns the encrypted connection.
// The deadline is cleared before returning, for the reason readPreamble gives:
// raft sets a per-RPC deadline only when its own Timeout is positive.
func wrapDialed(conn net.Conn, tok Token, clientNonce []byte, timeout time.Duration) (net.Conn, error) {
	if err := setReadDeadline(conn, timeout); err != nil {
		return nil, err
	}
	serverNonce := make([]byte, sessionNonceLen)
	if _, err := io.ReadFull(conn, serverNonce); err != nil {
		return nil, err
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, err
	}
	send, recv, err := deriveSession(tok, clientNonce, serverNonce, true)
	if err != nil {
		return nil, err
	}
	return newSessionConn(conn, send, recv), nil
}

// dialSession opens a connection to address, announces kind and id under this
// mux's dialing token, completes the session exchange and returns the encrypted
// connection. It is the one dial path: raft's stream layer, the forwarding arm
// and the membership control channel all reach the wire through it, so none of
// them can accidentally speak in the clear.
func dialSession(m *Mux, kind StreamKind, id GroupID, address string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return nil, err
	}
	head, err := writePreamble(conn, kind, id, timeout, m.signer)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	wrapped, err := wrapDialed(conn, m.signer.dialing(), head[nonceOff:stampOff], timeout)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return wrapped, nil
}
