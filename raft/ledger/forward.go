// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ledger

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/hashicorp/raft"
)

// forwardRequestTimeout bounds one forwarded exchange END TO END on the leader: the
// read of the request off the wire, the operation it names, AND the write of the reply
// back. It is applied as ONE connection deadline covering all three, never as a read
// deadline that is set and then cleared.
//
// BOUNDING ONLY THE READ IS NOT ENOUGH, and both halves fail for the same underlying
// reason: the transport hands this connection over with NO deadline on it, deliberately
// — raft owns the per-RPC deadline on its own stream, and this stream has no raft to
// own it.
//
//   - A peer that connects, sends one byte and goes silent parks the handler in Decode.
//     Because handlers join the wait group Close drains, that park is a Close that never
//     returns.
//   - A peer that sends a VALID request and then stops READING parks it in Encode
//     instead. There Close is UNAFFECTED, because the in-flight registry ends the
//     connection, so every close-path gate stays green while one goroutine and one file
//     descriptor accumulate per stalled peer for the ledger's lifetime on an otherwise
//     healthy leader. Measured on the read-only-bound shape: a handler still holding at
//     ten seconds against this five-second bound.
//
// Neither direction needs a hostile peer. A peer SIGSTOPped, paused under a debugger or
// thrashing in GC stops reading with no intent at all, and the group's own members are
// the peers that can complete this handshake.
const forwardRequestTimeout = 5 * time.Second

// Forwarding refusals, declared beside the code that returns them.
var (
	// ErrUndeclaredForwardOp reports a forwarded request naming an operation this
	// build does not implement. It is what a request decoded from zeroed or
	// truncated bytes earns, rather than being read as the first arm.
	ErrUndeclaredForwardOp = errors.New("ledger: forwarded request names an undeclared operation")
	// ErrLeaderUnavailable reports that the node addressed as the flow's leader
	// CANNOT SERVE it at all — its raft has stopped while its transport is still
	// bound, so it answers with something that is neither a refusal nor a success.
	//
	// IT IS RETRYABLE. The forwarding loop keeps going against a re-resolved leader,
	// because the condition repairs itself as soon as the group elects one.
	//
	// IT IS A SEPARATE SENTINEL FROM ErrNotLeader ON PURPOSE. Both are retryable and
	// the loop treats them alike, but they are not the same fact: a node answering
	// ErrNotLeader is deferring to a leader that exists, while this one is gone.
	// Folding the second into the first would leave a caller that logged the
	// difference unable to tell them apart.
	ErrLeaderUnavailable = errors.New("ledger: the node addressed as leader cannot serve the flow")
)

// forwardOp names the operation a forwarded request carries.
type forwardOp uint8

const (
	// opLoad reads one path through the leader's own barrier.
	opLoad forwardOp = iota + 1
	// opSave appends one entry of a forwarding kind through the leader's own journal.
	opSave
	// opList enumerates every entry under a path prefix through the leader's own
	// barrier.
	//
	// IT IS THE THIRD OPERATION, and it is one the derivation below genuinely did not
	// cover. Update decomposes into a Load, the caller's function and a Save, so it
	// needed no operation of its own; an ENUMERATION decomposes into nothing — it is
	// not expressible as a Load, because the caller does not know the keys it is
	// asking for. Recovery reads it to decide whether a datum is orphaned, and a
	// stale answer there means either claiming live work or leaving dead work
	// unclaimed, so it FORWARDS and is linearizable on every node rather than reading
	// this node's own replicated state.
	opList
)

// declared reports whether this build knows how to serve an operation.
//
// THE ZERO VALUE IS DELIBERATELY NOT A MEMBER, exactly as Kind's is not: a request
// decoded from zeroed or truncated bytes names no operation and is refused rather
// than reading as the first arm.
//
// THIS IS THE ONE PLACE THE OPERATION SET IS ENUMERATED, and serveOne decides its
// refusal by asking this rather than by a switch default — which is what keeps the
// enumeration load-bearing instead of decorative.
func (o forwardOp) declared() bool {
	return o == opLoad || o == opSave || o == opList
}

// forwardRequest is one operation on the wire.
//
// TWO OF ITS THREE OPERATIONS ARE DERIVED RATHER THAN CHOSEN. Store.Update is
// already a Load, then the caller's function, then a Save, so once Load and Save
// forward, Update forwards with NO CLOSURE CROSSING THE WIRE. That is what keeps the
// ruled model intact: the datum's own worker computes the update and the leader only
// orders it. A closure on the wire would invert it.
//
// THAT DERIVATION DOES NOT EXTEND TO THE THIRD. It covers operations decomposable
// into Load and Save; an enumeration is not one, because the caller does not know
// the keys it is asking for. opList is therefore an operation genuinely added rather
// than a shape the two already expressed — see its own comment for why it forwards.
//
// THE WIRE CARRIES THE ENTRY'S KIND, and that is what keeps a new kind from being
// leader-local. A kind with no representation here refuses on every follower,
// silently, with nothing to notice it. Kind rides beside the operation rather than
// being reconstructed on the leader, because rewriting one kind into another there
// would be a coercion of bad input.
type forwardRequest struct {
	Op    forwardOp
	Kind  Kind
	Path  string
	Value []byte
}

// forwardReply is one answer on the wire.
//
// Value stays OPAQUE BYTES for the reason the entry vocabulary gives: decoding at
// the READING node makes an unregistered type fail one reader loudly instead of
// poisoning replicated state across peers.
// Entries carries an enumeration's answer. It is a separate field from Value rather
// than a packing of it because the two are different shapes, and a reply that
// smuggled a list through the single-value field would decode as a value on any peer
// that read it as one.
type forwardReply struct {
	Value   []byte
	Entries []Entry
	Present bool
	Index   uint64
	Code    errCode
	Message string
}

// errCode is the refusal identity a reply carries.
//
// IT EXISTS BECAUSE A MESSAGE STRING ALONE WOULD LOSE errors.Is. Forwarded
// operations owe callers the same fail-loud contract local ones give, and without a
// code a caller matching on a sentinel would silently stop matching the moment the
// operation crossed a node boundary.
type errCode uint8

const (
	// codeNone is the successful reply: no refusal to rebuild.
	codeNone errCode = iota
	codeNotLeader
	codeUnavailable
	codePoisonedJournal
	codeReadTimeout
	codeClosed
	// codeClaimHeld reports that this operation LOST a claim race. Without its own
	// identity a lost claim takes codeOther, which behaves correctly — codeOther is
	// already non-retryable — but arrives as a bare message, leaving a caller unable
	// to tell "you lost the race" from "something unclassified went wrong". That is
	// the distinction the whole recovery loop turns on.
	codeClaimHeld
	// codeOther carries an error the leader could not classify. It rebuilds to a
	// plain error and is promoted to no sentinel at all.
	codeOther
)

// retryable reports whether a caller should keep trying against a re-resolved
// leader.
//
// THIS IS THE ONE PLACE THE RETRYABLE SET IS ENUMERATED, and the retry loop asks it
// rather than comparing a code inline. THE INLINE FORM IS THE DEFECT IT REPLACES:
// written as "keep going while the code is codeNotLeader", the loop treats every
// other reply as terminal, so a new condition becomes an invisible consequence of a
// comparison rather than a one-line decision in a named place — which is exactly how
// a node whose raft had stopped killed a forwarded write on its first attempt.
//
// codeClosed is deliberately NOT retryable: a closed ledger never reopens, so
// retrying it would spin to the bound against a condition no retry can repair.
// codePoisonedJournal, codeReadTimeout and codeClaimHeld are the leader's answers
// ABOUT THIS OPERATION rather than statements about who leads, and codeOther is
// unclassified — retrying any of them would be a guess. A lost claim in particular
// is settled: no retry repairs it, and retrying would spin to the bound.
func (c errCode) retryable() bool {
	return c == codeNotLeader || c == codeUnavailable
}

// sentinel returns the declared error a code rebuilds to, or nil when the code names
// none. codeNone is a success and codeOther is deliberately unmapped.
func (c errCode) sentinel() error {
	switch c {
	case codeNotLeader:
		return ErrNotLeader
	case codeUnavailable:
		return ErrLeaderUnavailable
	case codePoisonedJournal:
		return ErrPoisonedJournal
	case codeReadTimeout:
		return ErrReadTimeout
	case codeClosed:
		return ErrClosed
	case codeClaimHeld:
		return ErrClaimHeld
	case codeNone, codeOther:
		return nil
	default:
		return nil
	}
}

// classify maps an error the leader produced onto the wire's refusal identity and
// preserves the leader's own wording.
//
// AN UNCLASSIFIED ERROR TAKES codeOther AND IS PROMOTED TO NOTHING. That is the
// discriminating property of this table: adding a classification arm is a small step
// from promoting everything, and a retry loop made generous would look identical to
// one made correct.
//
// ErrNilContext and ErrConfigIncomplete get no arm and need none — a nil context is
// refused before any wire is reached, and a Config is never forwarded.
func classify(err error) (errCode, string) {
	switch {
	case err == nil:
		return codeNone, ""
	case errors.Is(err, ErrNotLeader):
		return codeNotLeader, err.Error()
	case errors.Is(err, raft.ErrRaftShutdown):
		return codeUnavailable, err.Error()
	case errors.Is(err, ErrPoisonedJournal):
		return codePoisonedJournal, err.Error()
	case errors.Is(err, ErrReadTimeout):
		return codeReadTimeout, err.Error()
	case errors.Is(err, ErrClosed):
		return codeClosed, err.Error()
	case errors.Is(err, ErrClaimHeld):
		return codeClaimHeld, err.Error()
	default:
		return codeOther, err.Error()
	}
}

// rebuild turns a reply's code and message back into an error the caller can match
// with errors.Is, so a sentinel a local call would have returned still matches after
// the operation crossed a node boundary. The leader's own wording is wrapped around
// the sentinel, so both survive.
func (r forwardReply) rebuild() error {
	if r.Code == codeNone {
		return nil
	}

	sentinel := r.Code.sentinel()
	if sentinel == nil {
		if r.Message == "" {
			return errors.New("ledger: the leader refused a forwarded operation without naming a reason")
		}

		return errors.New(r.Message)
	}
	if r.Message == "" {
		return sentinel
	}

	return fmt.Errorf("%s: %w", r.Message, sentinel)
}

// serveForward accepts forwarded connections until Accept fails, which is what the
// group's release does. THE LOOP MUST NEVER PARK ON ANYTHING BUT Accept, for the
// reason the leadership drain must not either: a loop that stops receiving turns a
// healthy-looking node into one whose every operation times out.
func (l *Ledger) serveForward(ln net.Listener) {
	defer l.forwarding.Done()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if !l.trackConn(conn) {
			_ = conn.Close()

			continue
		}
		l.forwarding.Add(1)

		go l.handleForward(conn)
	}
}

// handleForward decodes one request, serves it and encodes the reply. It joins the
// forwarding wait group, so Close does not return while a handler still holds a
// connection.
func (l *Ledger) handleForward(conn net.Conn) {
	defer l.forwarding.Done()
	defer l.releaseConn(conn)

	// ONE DEADLINE FOR THE WHOLE EXCHANGE, set once and never cleared, so it covers
	// the decode, the operation and the encode together.
	//
	// BOTH DIRECTIONS NEED IT AND FOR THE SAME REASON. A peer that connects, sends one
	// byte and goes silent parks this handler in Decode, and because handlers join the
	// wait group Close drains, that park is a Close that never returns. A peer that
	// sends a VALID request and then stops READING parks it in Encode instead — and
	// there Close is UNAFFECTED, because closeInflight ends the connection, so every
	// close-path gate stays green while one goroutine and one file descriptor
	// accumulate per stalled peer for the ledger's lifetime on an otherwise healthy
	// leader. serveForward spawns handlers unbounded, so the accept queue never
	// saturates and no refusal counter moves: the exhaustion lands on the process
	// rather than on this stream.
	//
	// Neither direction needs a hostile peer. A peer SIGSTOPped, paused under a
	// debugger or thrashing in GC stops reading with no intent at all, and the group's
	// own members are the peers that can complete this handshake.
	if err := conn.SetDeadline(time.Now().Add(forwardRequestTimeout)); err != nil {
		return
	}
	var req forwardRequest
	if err := gob.NewDecoder(conn).Decode(&req); err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), forwardRequestTimeout)
	defer cancel()

	if err := gob.NewEncoder(conn).Encode(l.serveOne(ctx, req)); err != nil {
		l.logger.Warn("ledger: writing a forwarded reply failed", "flow", l.cfg.Flow, "error", err)
	}
}

// serveOne is the dispatch, and it executes LOCAL-ONLY in two senses.
//
// IT NEVER CALLS THE FORWARDING-CAPABLE ENTRY POINTS. Those resolve a leader and
// forward, so a handler calling one is relaying rather than serving: an operation
// landing on a node that no longer leads must be REFUSED so the client re-resolves
// under its own bound, because relayed onward, two peers whose leader resolution
// disagrees bounce it between them and no bound governs the chain.
//
// AND IT NEVER READS THROUGH THE STATE MACHINE DIRECTLY. The read arm goes through
// the leader-local read because THAT is where the barrier is: VerifyLeader proves
// the term against a quorum, then the wait carries the state machine to the commit
// index observed after that proof. A handler answering from the state machine has a
// local leadership BELIEF, no quorum proof and no wait at all — it compiles, it vets,
// and it looks careful because it checks raft's state, which is why both prohibitions
// are gated by shape rather than left to review.
//
// THE REFUSAL FOR AN UNDECLARED OPERATION IS DECIDED BY declared(), not by a switch
// default. A default arm satisfies the behavior while calling declared() nowhere,
// the linter then reports declared as unused, and the cheapest route to green from
// there is deleting the one place the operation set is enumerated.
func (l *Ledger) serveOne(ctx context.Context, req forwardRequest) forwardReply {
	if !req.Op.declared() {
		code, message := classify(fmt.Errorf("ledger: operation %d: %w", req.Op, ErrUndeclaredForwardOp))

		return forwardReply{Code: code, Message: message}
	}

	if req.Op == opLoad {
		entry, present, err := l.getLocal(ctx, req.Path)
		code, message := classify(err)

		return forwardReply{Value: entry.Value, Present: present, Code: code, Message: message}
	}

	if req.Op == opList {
		entries, err := l.listLocal(ctx, req.Path)
		code, message := classify(err)

		return forwardReply{Entries: entries, Code: code, Message: message}
	}

	// opSave. declared() admits exactly three operations and the other two are handled
	// above, so this arm needs no default beside it.
	//
	// THE KIND IS GUARDED HERE AND NEVER COERCED, the same way declared() guards the
	// operation. The zero Kind is deliberately not a member, so a request decoded
	// from zeroed or truncated bytes names no kind and is refused rather than
	// silently becoming a KindSet assignment. The guard sits in this arm because
	// this is the arm that consumes the field: opLoad carries no kind.
	if !req.Kind.declared() {
		code, message := classify(fmt.Errorf(
			"ledger: forwarded save for %q names kind %d, which this build does not declare: %w",
			req.Path, uint8(req.Kind), ErrPoisonedJournal))

		return forwardReply{Code: code, Message: message}
	}

	index, err := l.appendLocal(ctx, Entry{Kind: req.Kind, Path: req.Path, Value: req.Value})
	code, message := classify(err)

	return forwardReply{Index: index, Code: code, Message: message}
}

// trackConn registers an accepted connection so Close can END it rather than wait its
// read deadline out, and reports whether it was admitted.
//
// IT RETURNS A BOOLEAN BECAUSE OF ONE REACHABLE STATE: a connection accepted after
// closeInflight has already run would otherwise be held by nobody and closed by
// nobody. Refusing it there is what makes that window correct.
func (l *Ledger) trackConn(conn net.Conn) bool {
	l.inflightMu.Lock()
	defer l.inflightMu.Unlock()

	if l.inflightClosed {
		return false
	}
	if l.inflight == nil {
		l.inflight = map[net.Conn]struct{}{}
	}
	l.inflight[conn] = struct{}{}

	return true
}

// releaseConn deregisters a connection and closes it. The lock is held only around
// the map operation and never across a raft append.
func (l *Ledger) releaseConn(conn net.Conn) {
	l.inflightMu.Lock()
	delete(l.inflight, conn)
	l.inflightMu.Unlock()

	_ = conn.Close()
}

// closeInflight ends every connection a handler is currently holding, which is what
// makes Close PROMPT rather than merely bounded: without it, a handler parked reading
// from a peer that stopped sending holds Close for the whole read deadline.
//
// The connections are closed OUTSIDE the lock, so a handler waking from its read and
// calling releaseConn cannot deadlock against this.
func (l *Ledger) closeInflight() {
	l.inflightMu.Lock()
	l.inflightClosed = true
	conns := make([]net.Conn, 0, len(l.inflight))
	for conn := range l.inflight {
		conns = append(conns, conn)
	}
	l.inflightMu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}
}

// The forwarding client's bounds.
const (
	// forwardRetryInterval is how long the loop waits between attempts.
	forwardRetryInterval = 100 * time.Millisecond
	// defaultForwardTimeout bounds the whole operation when Config.ForwardTimeout
	// is unset.
	//
	// IT IS WALL-CLOCK AND NOT AN ATTEMPT COUNT, and the reason is measured: with
	// raft's production timers one leadership event cost 29 attempts at a 50ms retry
	// interval and 1 attempt at 200ms, so an attempt-count bound means a different
	// thing at every interval.
	//
	// THE SIZE IS DERIVED FROM RAFT'S OWN WORST COMPLIANT WINDOW rather than from a
	// convergence measurement, because a convergence measurement on a fast machine
	// measures the harness's timers rather than this constant. raft randomizes its
	// timeouts between a minimum and twice that minimum, so detection plus campaigning
	// is at worst two heartbeat timeouts plus two election timeouts — 4s under raft's
	// defaults. This covers that twice over.
	defaultForwardTimeout = 10 * time.Second
	// forwardDialTimeout bounds one dial to the resolved leader.
	forwardDialTimeout = 5 * time.Second
)

var (
	// ErrForwardBoundExceeded reports that no leader served the operation before the
	// forwarding bound elapsed. It names the flow, the attempts made, the bound and
	// the last condition observed, because a caller told only that it did not work
	// cannot tell a leadership change in progress from a group that has lost quorum.
	ErrForwardBoundExceeded = errors.New("ledger: no leader served this operation before the forwarding bound elapsed")
	// errLeaderIsSelf reports that leadership resolved to this node, which means it
	// landed here between the local attempt and the resolution. It is a LOCAL RETRY
	// rather than a refusal: the loop re-enters the local arm.
	errLeaderIsSelf = errors.New("ledger: leadership resolved to this node")
	// errNoLeaderKnown reports that this node knows of no leader for the flow yet,
	// which is the ordinary state during an election.
	errNoLeaderKnown = errors.New("ledger: no leader is known for this flow")
)

// forwardTimeout is the bound this ledger applies, defaulting when unconfigured.
func (l *Ledger) forwardTimeout() time.Duration {
	if l.cfg.ForwardTimeout <= 0 {
		return defaultForwardTimeout
	}

	return l.cfg.ForwardTimeout
}

// forwardOnce resolves the flow's leader and serves one attempt against it.
//
// IT DIALS EXACTLY ONCE PER ATTEMPT. A probe-then-send shape would double the
// connection count for no information, which is why the cost gate counts connections
// rather than measuring latency: a retry storm is invisible in a latency band and
// obvious in a connection count.
func (l *Ledger) forwardOnce(req forwardRequest) (forwardReply, error) {
	address, id := l.raft.LeaderWithID()
	if address == "" || id == "" {
		return forwardReply{}, errNoLeaderKnown
	}
	if string(id) == l.cfg.LocalID {
		return forwardReply{}, errLeaderIsSelf
	}

	conn, err := l.group.DialForward(string(address), forwardDialTimeout)
	if err != nil {
		return forwardReply{}, err
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(forwardRequestTimeout)); err != nil {
		return forwardReply{}, err
	}
	if err := gob.NewEncoder(conn).Encode(req); err != nil {
		return forwardReply{}, err
	}
	var reply forwardReply
	if err := gob.NewDecoder(conn).Decode(&reply); err != nil {
		return forwardReply{}, err
	}

	return reply, nil
}

// forward runs one operation to completion against whichever node leads the flow,
// trying locally first and forwarding while the condition is one a retry can repair.
//
// THE LOCAL ARM RUNS FIRST ON EVERY ATTEMPT, which is what gives a leader serving its
// OWN operations a path with no forwarding detour: its local call succeeds and no dial
// ever happens.
//
// THE LOOP CONTINUES WHILE THE REPLY IS RETRYABLE, asking errCode.retryable() rather
// than comparing a code inline. The inline comparison is a MEASURED defect: written as
// "continue while the code is codeNotLeader", a leader stopped with its transport still
// bound replies with a condition the loop calls terminal, and the forwarded write dies
// on its first attempt with the write lost.
//
// EVERY ARM LEAVES THIS LOOP THROUGH pause AND THERE IS NO OTHER WAY ROUND IT —
// including self-resolution, which re-enters the local arm by falling through to pause
// rather than by a bare continue. That is what makes "every arm is bounded" checkable
// by reading one call site per arm instead of trusting each arm to have remembered.
func (l *Ledger) forward(ctx context.Context, req forwardRequest, local func() forwardReply) (forwardReply, error) {
	deadline := time.Now().Add(l.forwardTimeout())
	attempts := 0

	var last error
	for {
		attempts++

		reply := local()
		if !reply.Code.retryable() {
			return reply, nil
		}

		remote, err := l.forwardOnce(req)
		if err == nil && !remote.Code.retryable() {
			return remote, nil
		}
		last = err
		if err == nil {
			last = remote.rebuild()
		}

		if err := l.pause(ctx, deadline, attempts, last); err != nil {
			return forwardReply{}, err
		}
	}
}

// pause is THE ONE PLACE the deadline is compared, the caller's context is selected on,
// and the retry interval is waited out.
//
// KEEPING IT ONE FUNCTION IS THE POINT. A bare continue on any arm skips both the
// deadline and the caller's context: driven against a local arm that keeps reporting
// not-leader while leadership resolves to this node, that shape made over 1.5 billion
// attempts without returning, ignoring both a 2s bound and a context canceled at 300ms.
// Adding only the deadline and context checks while keeping immediate re-entry
// terminated correctly and still made 6.7 million attempts in 302ms, hammering raft.
// Routing every arm through here: 3 attempts in 300ms.
//
// THE CALLER'S CONTEXT IS THE OUTER BOUND: a canceled context returns the context's
// error with the attempt count, never ErrForwardBoundExceeded, because those are
// different facts.
func (l *Ledger) pause(ctx context.Context, deadline time.Time, attempts int, last error) error {
	if !time.Now().Before(deadline) {
		return fmt.Errorf(
			"ledger: flow %q: no leader served this operation in %d attempts over %s, last condition %v: %w",
			l.cfg.Flow, attempts, l.forwardTimeout(), last, ErrForwardBoundExceeded)
	}

	select {
	case <-time.After(forwardRetryInterval):
		return nil
	case <-ctx.Done():
		return fmt.Errorf("ledger: flow %q: %d attempts before the caller's context ended, last condition %v: %w",
			l.cfg.Flow, attempts, last, ctx.Err())
	}
}
