// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package transport

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"time"
)

// GroupID names one raft group among the several sharing a listener. It is the
// only identity on the wire that raft itself does not define, which is why it
// is declared beside the handshake that carries it.
type GroupID string

const (
	// preambleLen is the fixed head of the handshake: magic, version, reserved
	// and the group id length.
	preambleLen = 8
	// MaxGroupIDLen bounds the group id a peer may announce. A connection
	// announcing more is refused before any allocation is sized by it, so a
	// peer cannot make this process allocate 64KB per connection by lying in
	// two bytes.
	MaxGroupIDLen = 256
	// protoVersion is the handshake version this package writes and accepts.
	protoVersion = 1
)

// preambleMagic opens every handshake so a connection from an unrelated client
// is refused on its first eight bytes rather than confusing raft's decoder.
var preambleMagic = [4]byte{'m', 'r', 'm', 'x'}

// Handshake rejection reasons. They are sentinels so a caller can tell a
// malformed peer from a closed mux.
var (
	ErrBadMagic     = errors.New("transport: handshake magic mismatch")
	ErrBadVersion   = errors.New("transport: unsupported handshake version")
	ErrGroupIDRange = errors.New("transport: group id length out of range")
)

// readPreamble reads exactly the handshake bytes from conn and returns the
// group id the peer announced. It reads with io.ReadFull on the raw connection
// and never wraps it in a buffered reader: raft's handleConn builds its own
// bufio.Reader over the same connection, so a byte read ahead here would be
// lost from the RPC stream and the first AppendEntries would decode as garbage.
//
// The read deadline is cleared before returning. NetworkTransport sets a
// per-RPC deadline only when its Timeout config is positive, so a deadline left
// on the connection would expire mid-RPC on a connection raft believes has none.
func readPreamble(conn net.Conn, timeout time.Duration) (GroupID, error) {
	if err := setReadDeadline(conn, timeout); err != nil {
		return "", err
	}
	var head [preambleLen]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return "", err
	}
	n, err := decodeHead(head)
	if err != nil {
		return "", err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return "", err
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return "", err
	}
	return GroupID(buf), nil
}

// decodeHead validates the fixed head and returns the announced id length.
// Byte 5 is reserved: it is written as zero and deliberately not validated, so
// a later version can use it without this version refusing the connection.
func decodeHead(head [preambleLen]byte) (int, error) {
	if [4]byte(head[0:4]) != preambleMagic {
		return 0, ErrBadMagic
	}
	if head[4] != protoVersion {
		return 0, ErrBadVersion
	}
	n := int(binary.BigEndian.Uint16(head[6:8]))
	if n == 0 || n > MaxGroupIDLen {
		return 0, ErrGroupIDRange
	}
	return n, nil
}

// writePreamble announces id on conn and clears the deadline it set, so the
// connection is handed to raft with no deadline of ours left on it.
func writePreamble(conn net.Conn, id GroupID, timeout time.Duration) error {
	buf, err := encodePreamble(id)
	if err != nil {
		return err
	}
	if err := setWriteDeadline(conn, timeout); err != nil {
		return err
	}
	if _, err := conn.Write(buf); err != nil {
		return err
	}
	return conn.SetWriteDeadline(time.Time{})
}

// encodePreamble renders the handshake for id. The buffer is allocated at its
// exact size rather than sliced out of a stack scratch array: passing a scratch
// array to conn.Write escapes it, which measured worse.
func encodePreamble(id GroupID) ([]byte, error) {
	n := len(id)
	// The math.MaxUint16 comparison is what gosec's G115 recognizes as a bound
	// on the conversion below; the MaxGroupIDLen check is the policy cap.
	if n <= 0 || n > MaxGroupIDLen || n > math.MaxUint16 {
		return nil, ErrGroupIDRange
	}
	buf := make([]byte, preambleLen+n)
	copy(buf[0:4], preambleMagic[:])
	buf[4] = protoVersion
	binary.BigEndian.PutUint16(buf[6:8], uint16(n))
	copy(buf[preambleLen:], id)
	return buf, nil
}

func setReadDeadline(conn net.Conn, timeout time.Duration) error {
	if timeout <= 0 {
		return nil
	}
	return conn.SetReadDeadline(time.Now().Add(timeout))
}

func setWriteDeadline(conn net.Conn, timeout time.Duration) error {
	if timeout <= 0 {
		return nil
	}
	return conn.SetWriteDeadline(time.Now().Add(timeout))
}
