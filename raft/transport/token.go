// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package transport

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"math"
	"sync"
	"time"
)

// Token is the shared join secret a peer proves possession of on every
// connection. It is declared in this package because it is wire material, and
// this package already owns the only identity on the wire that raft itself does
// not define.
//
// THE TOKEN IS NEVER SENT. A bearer secret written into a plaintext handshake is
// disclosed to any passive observer, and rotating the accepted set is the only
// revocation mechanism, so a disclosed value would stay good until an operator
// rotated it. The dialer sends a MAC over the preamble bytes plus a fresh nonce
// and a timestamp; the acceptor recomputes it.
type Token string

const (
	// clockSkewWindow bounds how far a peer's handshake stamp may sit from this
	// node's clock in either direction. It bounds how long a captured preamble
	// stays replayable; it does not stop a replay inside the window, which is
	// what the derived session closes by contributing a server nonce.
	clockSkewWindow = 60 * time.Second
	// nonceLen, stampLen and macLen size the authentication block. The block is
	// ALWAYS present, zeroed when the mux carries no token, so there is one wire
	// layout and one parse rather than a conditional one.
	nonceLen = 16
	stampLen = 8
	macLen   = 32
	authLen  = nonceLen + stampLen + macLen
)

// Offsets of the authentication block inside an encoded preamble. The id begins
// at preambleLen+authLen, which is what every caller passes as idOff.
const (
	nonceOff = preambleLen
	stampOff = nonceOff + nonceLen
	macOff   = stampOff + stampLen
)

// acceptedToken carries one admissible token and a pool of HMACs keyed by it.
// The pool is per token rather than one pool for the whole signer because an
// hmac.Hash is keyed at construction and Reset returns it to that keyed state,
// so a pooled hash can only ever serve the token it was built for.
type acceptedToken struct {
	tok  Token
	pool sync.Pool
}

// newAcceptedToken keys a pool for tok.
func newAcceptedToken(tok Token) *acceptedToken {
	a := &acceptedToken{tok: tok}
	a.pool.New = func() any { return hmac.New(sha256.New, []byte(a.tok)) }
	return a
}

// mac appends the tag for buf to dst and returns the result.
//
// THE MAC COVERS THE KIND AND THE ID, not just the nonce: the head carries the
// magic, the version, the KIND and the id length, and the id bytes follow the
// authentication block. That is what stops a proof lifted from one connection
// admitting another — a membership proof cannot admit a raft connection, and a
// proof minted for flow A cannot admit a connection announcing flow B.
func (a *acceptedToken) mac(dst, buf []byte, idOff int) []byte {
	h := a.pool.Get().(hash.Hash)
	h.Reset()
	_, _ = h.Write(buf[:macOff])
	_, _ = h.Write(buf[idOff:])
	sum := h.Sum(dst)
	a.pool.Put(h)
	return sum
}

// signer holds the ordered set of tokens this node accepts, and is the same
// value read from both sides of a connection: one type with two method sets
// rather than a signer and a verifier, because the accepted set is shared and a
// rotation has to move both at once.
//
// The set is ORDERED and its first element is what this node DIALS with; every
// element is accepted on inbound. That single shape covers rotation, overlap and
// revocation with no second mechanism to rot between uses.
type signer struct {
	mu       sync.RWMutex
	accepted []*acceptedToken
}

// newSigner builds a signer over the tokens a mux was configured with.
func newSigner(tokens []Token) *signer {
	s := &signer{}
	s.set(tokens)
	return s
}

// set replaces the accepted set. The slice is rebuilt rather than mutated so a
// reader holding the previous one keeps a stable view across the swap.
func (s *signer) set(tokens []Token) {
	accepted := make([]*acceptedToken, 0, len(tokens))
	for _, tok := range tokens {
		accepted = append(accepted, newAcceptedToken(tok))
	}
	s.mu.Lock()
	s.accepted = accepted
	s.mu.Unlock()
}

// snapshot reads the accepted set under the read lock.
func (s *signer) snapshot() []*acceptedToken {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.accepted
}

// empty reports whether this node carries no token and therefore accepts every
// proof, which is correct on loopback and refused on a routable advertisement.
func (s *signer) empty() bool { return len(s.snapshot()) == 0 }

// dialing reports the token this node signs with and derives outbound sessions
// from: the first element of the ordered accepted set. An empty set yields the
// empty token, which still derives a session — see deriveSession for what that
// does and does not buy.
func (s *signer) dialing() Token {
	accepted := s.snapshot()
	if len(accepted) == 0 {
		return ""
	}
	return accepted[0].tok
}

// sign fills the authentication block of buf in place with a fresh nonce, the
// current time and the tag under the DIALING token. It is a no-op when the
// accepted set is empty, which leaves the block zeroed.
//
// THE ALLOCATION SHAPE IS LOAD-BEARING: lane B's landed budget is one allocation
// per encode. sign writes into the buffer the caller already allocated, takes
// its HMAC from the pool rather than constructing one per call, and Sums into a
// capacity-bounded slice of that same buffer so the append cannot escape it.
func (s *signer) sign(buf []byte, idOff int) {
	accepted := s.snapshot()
	if len(accepted) == 0 {
		return
	}
	// crypto/rand.Read never returns an error and always fills its argument
	// entirely; it panics rather than reporting a short read, so there is no
	// partial-nonce case to handle here.
	_, _ = rand.Read(buf[nonceOff:stampOff])
	putStamp(buf, time.Now())
	accepted[0].mac(buf[macOff:macOff:idOff], buf, idOff)
}

// verify recomputes the tag for buf under each accepted token and admits on the
// first that matches, RETURNING THAT TOKEN so the session derived for this
// connection is keyed by the same secret that admitted it. During a rotation
// overlap two peers may be admitted under different tokens on the same node, and
// each session must follow the one its own peer proved.
//
// It admits with the empty token when the accepted set is empty, which is the
// tokenless loopback shape.
//
// THE COMPARISON IS hmac.Equal AND NOTHING ELSE. bytes.Equal returns at the
// first differing byte, so its duration reports how many leading bytes of a
// forged tag were right, which turns forgery into a byte-at-a-time search
// against a gate that admits to the raft log.
func (s *signer) verify(buf []byte, idOff int) (Token, error) {
	accepted := s.snapshot()
	if len(accepted) == 0 {
		return "", nil
	}
	if !withinClockSkew(buf) {
		return "", ErrUnauthenticated
	}
	var scratch [macLen]byte
	for _, a := range accepted {
		if hmac.Equal(a.mac(scratch[:0], buf, idOff), buf[macOff:idOff]) {
			return a.tok, nil
		}
	}
	return "", ErrUnauthenticated
}

// putStamp writes t into the stamp bytes as big-endian unix nanoseconds.
func putStamp(buf []byte, t time.Time) {
	binary.BigEndian.PutUint64(buf[stampOff:macOff], uint64(t.UnixNano()))
}

// withinClockSkew reports whether the preamble's stamp sits inside the window in
// either direction.
//
// A RAW VALUE ABOVE THE int64 RANGE IS REFUSED OUTRIGHT rather than converted.
// Nanoseconds since 1970 do not reach it, so such a stamp is forged or corrupt
// and there is no plausible instant to coerce it to. The refusal is also what
// keeps the arithmetic below honest: every value that reaches time.Since is a
// real instant within a few decades of now, so the Duration cannot overflow and
// wrap a nonsense stamp back into the accepted window.
func withinClockSkew(buf []byte) bool {
	raw := binary.BigEndian.Uint64(buf[stampOff:macOff])
	if raw > math.MaxInt64 {
		return false
	}
	skew := time.Since(time.Unix(0, int64(raw)))
	return skew <= clockSkewWindow && skew >= -clockSkewWindow
}
