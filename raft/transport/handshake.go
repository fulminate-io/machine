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
	ErrBadMagic      = errors.New("transport: handshake magic mismatch")
	ErrBadVersion    = errors.New("transport: unsupported handshake version")
	ErrGroupIDRange  = errors.New("transport: group id length out of range")
	ErrBadStreamKind = errors.New("transport: unsupported stream kind")
)

// StreamKind names what a connection carries once its group is known, so one
// listener and one group id can serve raft's replication alongside a second
// delivery arm. It rides the preamble's byte 5.
type StreamKind uint8

const (
	// KindRaft carries raft's own RPC stream.
	//
	// IT IS ZERO DELIBERATELY, AND THAT IS THE LOAD-BEARING CHOICE. Byte 5 was
	// already written as zero by encodePreamble and already ignored by
	// decodeHead, so giving it a meaning leaves raft's bytes on the wire
	// unchanged by one bit: a peer built before this version announces zero and
	// is still understood to mean raft.
	KindRaft StreamKind = 0
	// KindForward carries a ledger operation forwarded to the group's leader.
	KindForward StreamKind = 1
)

// declared reports whether this build knows how to deliver a kind.
//
// THIS IS THE ONE PLACE THE KIND SET IS ENUMERATED, mirroring Kind.declared in
// the ledger's entry vocabulary and for the same reason: an undeclared kind is
// REFUSED rather than silently defaulted onto the first arm, so a peer from a
// later version announcing a kind this build cannot deliver is told so instead
// of having its connection handed to raft.
func (k StreamKind) declared() bool {
	return k == KindRaft || k == KindForward
}

// readPreamble reads exactly the handshake bytes from conn and returns the
// group id and the stream kind the peer announced. It reads with io.ReadFull on
// the raw connection and never wraps it in a buffered reader: raft's handleConn
// builds its own bufio.Reader over the same connection, so a byte read ahead
// here would be lost from the RPC stream and the first AppendEntries would
// decode as garbage.
//
// The read deadline is cleared before returning. NetworkTransport sets a
// per-RPC deadline only when its Timeout config is positive, so a deadline left
// on the connection would expire mid-RPC on a connection raft believes has none.
func readPreamble(conn net.Conn, timeout time.Duration) (GroupID, StreamKind, error) {
	if err := setReadDeadline(conn, timeout); err != nil {
		return "", 0, err
	}
	var head [preambleLen]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return "", 0, err
	}
	kind, n, err := decodeHead(head)
	if err != nil {
		return "", 0, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return "", 0, err
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return "", 0, err
	}
	return GroupID(buf), kind, nil
}

// decodeHead validates the fixed head and returns the announced stream kind and
// id length. Byte 5 was reserved for exactly this — written as zero and
// deliberately not validated so a later version could use it — which is why
// giving it a meaning costs raft's wire format nothing.
//
// The kind is validated AFTER the magic and version and BEFORE the length, so a
// stray client that is not speaking this protocol at all is still refused on the
// magic rather than on a kind it never meant to announce.
func decodeHead(head [preambleLen]byte) (StreamKind, int, error) {
	if [4]byte(head[0:4]) != preambleMagic {
		return 0, 0, ErrBadMagic
	}
	if head[4] != protoVersion {
		return 0, 0, ErrBadVersion
	}
	kind := StreamKind(head[5])
	if !kind.declared() {
		return 0, 0, ErrBadStreamKind
	}
	n := int(binary.BigEndian.Uint16(head[6:8]))
	if n == 0 || n > MaxGroupIDLen {
		return 0, 0, ErrGroupIDRange
	}
	return kind, n, nil
}

// writePreamble announces id and kind on conn and clears the deadline it set, so
// the connection is handed on with no deadline of ours left on it.
func writePreamble(conn net.Conn, id GroupID, kind StreamKind, timeout time.Duration) error {
	buf, err := encodePreamble(id, kind)
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

// encodePreamble renders the handshake for id and kind. The buffer is allocated
// at its exact size rather than sliced out of a stack scratch array: passing a
// scratch array to conn.Write escapes it, which measured worse.
//
// The kind is written INTO THE BUFFER THAT ALREADY HOLDS IT, at the byte the
// fixed head always reserved, so announcing a kind costs no second allocation.
// Building the head separately and appending the id would read the same on the
// wire and allocate twice per call.
func encodePreamble(id GroupID, kind StreamKind) ([]byte, error) {
	n := len(id)
	// The math.MaxUint16 comparison is what gosec's G115 recognizes as a bound
	// on the conversion below; the MaxGroupIDLen check is the policy cap.
	if n <= 0 || n > MaxGroupIDLen || n > math.MaxUint16 {
		return nil, ErrGroupIDRange
	}
	buf := make([]byte, preambleLen+n)
	copy(buf[0:4], preambleMagic[:])
	buf[4] = protoVersion
	buf[5] = byte(kind)
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
