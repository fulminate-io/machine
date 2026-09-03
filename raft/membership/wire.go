// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"time"
)

// Control-channel refusals, declared beside the code that returns them.
var (
	// ErrUnknownMessage refuses a message naming a kind this build does not
	// declare, rather than defaulting it onto the first arm.
	ErrUnknownMessage = errors.New("membership: control message names an undeclared kind")
	// ErrMessageTooLarge refuses a message, or an announced length, above the
	// byte ceiling — before that length sizes any allocation.
	ErrMessageTooLarge = errors.New("membership: control message exceeds the size ceiling")
)

// THE CEILING IS DECLARED IN THREE DIMENSIONS, AND NAMING ALL THREE IS THE
// POINT. A bound stated as "the ceiling" invites the reader to assume the other
// two are covered, which is exactly how a byte bound gets mistaken for
// protection against a peer that stops sending.
//
//	BYTES   maxControlMessage, enforced by an io.LimitedReader on every read and
//	        by a check on the announced length before it sizes an allocation.
//	        Generous against the largest legitimate message — a stats reply is
//	        tens of bytes per flow — because its job is to stop a peer-supplied
//	        length from sizing an allocation, not to be a tight fit.
//	TIME    controlReadTimeout, a read deadline set before EVERY message read.
//	        It is NOT enforced in this file and it is NOT optional: the
//	        connection this package receives carries no deadline of its own,
//	        because the transport's handshake clears it before handing the socket
//	        over. It lives on the acceptor and the client.
//	COUNT   one request and one reply per connection per exchange. The handler
//	        reads exactly one message and answers; there is no loop for a peer to
//	        hold open.
//
// An io.LimitedReader does nothing about a peer that stops sending, and a read
// deadline does nothing about a peer that sends a gigabyte. They are different
// bounds against different adversaries, and both are named here because this is
// where a reader looks for them.
const (
	maxControlMessage = 1 << 20
	// controlHeadLen is the kind byte plus the four-byte big-endian body length.
	controlHeadLen = 5
)

// msgKind tags what a control message asks for. The zero value is deliberately
// not a member, for the reason the ledger's Kind gives: a message decoded from
// zeroed or truncated bytes names no kind and is refused rather than silently
// reading as the first arm.
type msgKind uint8

const (
	msgAnnounce msgKind = iota + 1
	msgAnnounceReply
	msgStats
	msgStatsReply
	msgLeave
	msgLeaveReply
)

// declared reports whether this build knows how to interpret a kind. THIS IS THE
// ONE PLACE THE DECLARED SET IS ENUMERATED, so a kind cannot be admitted on the
// write path while the read path refuses it.
func (k msgKind) declared() bool {
	switch k {
	case msgAnnounce, msgAnnounceReply, msgStats, msgStatsReply, msgLeave, msgLeaveReply:
		return true
	default:
		return false
	}
}

// announce asks a peer to admit this node to the flows it names.
type announce struct {
	Node    string
	Address string
	Flows   []string
	// Generation is the announcer's deployment epoch.
	//
	// IT IS ON THE WIRE BECAUSE A DYING GROUP MUST NOT ADMIT THE NEXT
	// DEPLOYMENT'S MEMBER. Measured on a rolling update: a replacement pod
	// reached a still-leading outgoing pod and was staged into the outgoing
	// group, which then terminated, leaving three dead voters and one live
	// nonvoter with no quorum and no path back. An acceptor compares this
	// against its own and refuses any difference by name.
	//
	// VERSION SKEW RUNS IN BOTH DIRECTIONS AND BOTH ARE DISCLOSED, because the
	// one that matters is the one an operator is not told about. A NEW ANNOUNCER
	// REACHING AN OLD ACCEPTOR: gob decodes the extra field with no error and the
	// old binary ignores it, so an acceptor that predates this field has no
	// refusal path and stages the next generation's joiner exactly as before.
	// THE PROTECTION IS THEREFORE INERT DURING THE FIRST ROLLOUT THAT INTRODUCES
	// IT and effective from the next one; that is inherent, since a gate cannot
	// run in a binary that does not contain it, and what carries that first
	// rollout is the readmission fix rather than this refusal. AN OLD ANNOUNCER
	// REACHING A NEW ACCEPTOR: the absent field decodes as zero, which a new
	// acceptor refuses against any non-zero generation — correct, because an
	// announcer that predates the field belongs to the outgoing deployment.
	//
	// GOB DOES NOT ZERO A FIELD ABSENT FROM THE STREAM — it leaves whatever the
	// decode target already held, with no error. Nothing is wrong today because
	// answerJoin decodes into a fresh announce value; a future reader that
	// REUSED a decode target would silently inherit a stale generation here and
	// admit a foreign announcer, so the decode target stays fresh.
	Generation uint64
}

// announceReply answers an announce.
//
// REFUSALS ARE NAMED, NEVER DROPPED. Refused maps a flow to the reason it was
// not staged, and Redirects maps a flow to the address of the member that should
// be asked instead. A silent omission would be indistinguishable from a lost
// message and would leave a worker believing it had joined a flow it had not.
type announceReply struct {
	// Node identifies the answering member.
	//
	// IT IS CARRIED BECAUSE THE CREATION RULE NEEDS IT. When no instance hosts a
	// flow yet, the node that creates its group is the one holding the lowest id
	// among those that answered — and an answer that did not say who sent it
	// could not be compared. Nothing else reads it.
	Node string
	// Generation is the ANSWERING node's deployment epoch.
	//
	// THE ANNOUNCER READS IT RATHER THAN PARSING A REFUSAL STRING. Refused is
	// free text for an operator; a joiner deciding whether an answer counts
	// toward the count-and-lowest-id rule needs a value it can compare, and a
	// substring match on a human sentence is not one.
	Generation uint64
	Staged     []string
	Redirects  map[string]string
	Refused    map[string]string
}

// statsRequest asks a peer to report its own progress on each flow it names. The
// request carries a LIST so one round trip serves every flow a peer shares with
// us, rather than one connection per flow per interval.
type statsRequest struct {
	Flows []string
}

// statsReply carries the answering node's view of itself, flow by flow.
type statsReply struct {
	PerFlow map[string]FlowStats
}

// FlowStats is what a member reports about ONE of its flows. It is the member's
// OWN view: raft exposes no per-follower progress on the leader — Stats() is
// local and there is no exported match index — so the only node that can answer
// these is the one being asked.
//
// It is exported because it is the seam, not the wire: the promoter reads it to
// decide whether a staged joiner has caught up, and the failure signal reads it
// to decide whether a peer has gone quiet. The message types that carry it stay
// unexported.
type FlowStats struct {
	Term        uint64
	LastIndex   uint64
	LastContact time.Duration
	Voter       bool
	Leader      bool
}

// leave asks a peer to remove this node from the flows it names.
type leave struct {
	Node  string
	Flows []string
}

// leaveReply answers a leave, naming every refusal for the reason announceReply
// gives.
type leaveReply struct {
	Removed []string
	Refused map[string]string
}

// writeMessage frames one control message: the kind byte, a four-byte
// big-endian body length, then the gob value.
//
// IT IS ONE Write. The connection underneath is a session conn that seals each
// Write into its own record, so framing the head separately would put every
// message on the wire as two records for no reason.
func writeMessage(w io.Writer, kind msgKind, payload any) error {
	if !kind.declared() {
		return fmt.Errorf("%w: %d", ErrUnknownMessage, uint8(kind))
	}
	var body bytes.Buffer
	if err := gob.NewEncoder(&body).Encode(payload); err != nil {
		return fmt.Errorf("membership: encoding a control message of kind %d failed: %w", uint8(kind), err)
	}
	// The two-sided comparison against the ceiling is what gosec's G115
	// recognizes as a bound on the conversion below; the ceiling itself is the
	// policy cap.
	n := body.Len()
	if n < 0 || n > maxControlMessage {
		return fmt.Errorf("%w: encoding produced %d bytes", ErrMessageTooLarge, n)
	}
	out := make([]byte, controlHeadLen, controlHeadLen+n)
	out[0] = byte(kind)
	binary.BigEndian.PutUint32(out[1:controlHeadLen], uint32(n))
	out = append(out, body.Bytes()...)
	_, err := w.Write(out)
	return err
}

// readMessage reads one framed control message and returns its kind and its
// undecoded body.
//
// THE ANNOUNCED LENGTH IS BOUNDED BEFORE IT SIZES ANYTHING, and the whole read
// additionally runs through an io.LimitedReader, so a peer that lies in four
// bytes can neither make this process allocate a gigabyte nor read past the
// message it declared.
func readMessage(r io.Reader) (msgKind, []byte, error) {
	bounded := &io.LimitedReader{R: r, N: maxControlMessage + controlHeadLen}
	var head [controlHeadLen]byte
	if _, err := io.ReadFull(bounded, head[:]); err != nil {
		return 0, nil, err
	}
	kind := msgKind(head[0])
	if !kind.declared() {
		return 0, nil, fmt.Errorf("%w: %d", ErrUnknownMessage, head[0])
	}
	announced := binary.BigEndian.Uint32(head[1:controlHeadLen])
	if announced > maxControlMessage {
		return 0, nil, fmt.Errorf("%w: %d bytes announced", ErrMessageTooLarge, announced)
	}
	body := make([]byte, int(announced))
	if _, err := io.ReadFull(bounded, body); err != nil {
		return 0, nil, err
	}
	return kind, body, nil
}

// decodeMessage rebuilds a control message body into out.
func decodeMessage(body []byte, out any) error {
	if err := gob.NewDecoder(bytes.NewReader(body)).Decode(out); err != nil {
		return fmt.Errorf("membership: decoding a control message body failed: %w", err)
	}
	return nil
}
