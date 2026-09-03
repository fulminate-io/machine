// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

// ErrCursorTooOld refuses a reader whose cursor has fallen off the retained
// window, NAMING THE OLDEST INDEX STILL HELD.
//
// It is a loud refusal rather than a quiet reset to the oldest available signal,
// because being served from the oldest available signal would look like a
// complete history while being a gap. A consumer's correct response is to rebuild
// from Membership, which it can only do if it is told.
var ErrCursorTooOld = errors.New("membership: signal cursor is older than the retained window")

// ErrCursorForeignIncarnation refuses a cursor minted by a DIFFERENT incarnation
// of this log — one from before a restart, or one carried over from another node.
//
// IT WRAPS ErrCursorTooOld ON PURPOSE. The consumer's correct response is
// identical to the retention case and already published: rebuild from
// Membership. Wrapping means a consumer keyed on that sentinel keeps working,
// while one that wants to tell a restart from a retention overrun still can.
var ErrCursorForeignIncarnation = fmt.Errorf(
	"%w: it was minted by a different incarnation of this signal log", ErrCursorTooOld)

// The cursor is one uint64 carrying two fields: this log's incarnation in the
// high bits, the sequence in the low.
//
// THE DISCRIMINATOR GOES INSIDE THE PUBLISHED uint64 rather than beside it,
// because Watch's signature and Signal.Index are seam surfaces this plan
// declares and pins against drift. Packing gives the discriminator every
// property it needs without touching that contract.
const (
	signalIncarnationShift = 32
	signalSequenceMask     = uint64(1)<<signalIncarnationShift - 1
)

// maxRetainedSignals bounds the signal window.
//
// WHICH DIMENSION THIS BOUNDS, and what the other two do. COUNT is the bound
// doing the work: at most this many signals are retained and the oldest are
// dropped. BYTES follow as a CONSEQUENCE of the count, and the reason is not that
// a Signal is fixed size — it carries two strings. It is that every field is
// LOCALLY SOURCED and never wire-supplied: Flow comes from this node's own
// configured flow set and Node from a raft ServerID already in the committed
// configuration, so no peer can inflate one. A peer-supplied string reaching this
// struct would break that reasoning and is the thing to watch for. TIME is
// deliberately unbounded and safe here, because nothing on this path reads from a
// socket: a stale signal costs a slot, not a parked goroutine, which is the
// opposite of the control channel's situation.
const maxRetainedSignals = 1024

// SignalKind names what a membership signal reports.
type SignalKind uint8

const (
	// SignalMembershipChanged rides the state machine's configuration commit and
	// is therefore available on EVERY node.
	SignalMembershipChanged SignalKind = iota + 1
	// SignalPeerUnreachable is LEADER-ONLY: raft's peer and failed-heartbeat
	// observations fire in leader replication code and a follower receives none.
	SignalPeerUnreachable
	// SignalPeerReturned reports a previously unreachable peer answering again.
	SignalPeerReturned
	// SignalPeerEvicted reports a member removed from a flow's configuration
	// because the orchestrator's registry no longer lists it.
	SignalPeerEvicted
)

// String renders a signal kind for logs and failure messages.
func (k SignalKind) String() string {
	switch k {
	case SignalMembershipChanged:
		return "membership-changed"
	case SignalPeerUnreachable:
		return "peer-unreachable"
	case SignalPeerReturned:
		return "peer-returned"
	case SignalPeerEvicted:
		return "peer-evicted"
	default:
		return fmt.Sprintf("signal(%d)", uint8(k))
	}
}

// Signal is one membership event. Index is this node's own monotonic cursor
// position, not a raft index.
type Signal struct {
	Kind  SignalKind
	Flow  string
	Node  string
	Since time.Time
	Index uint64
	// EvictedWhileReachable reports that the evicted member ANSWERED A CONTROL
	// DIAL at the moment it was removed. IT IS DEFINED ONLY ON
	// SignalPeerEvicted and is false on every other kind, which is why it is
	// named for the eviction rather than for reachability: read on any other
	// kind, false is the true statement that this is not an eviction of a
	// reachable member.
	//
	// TRUE AND FALSE ARE NOT SYMMETRIC, AND A CONSUMER MUST KNOW WHICH IT HAS.
	// True is positive evidence: the member answered. FALSE IS THE ABSENCE OF
	// EVIDENCE, not evidence of absence — it means the member did not answer ONE
	// dial from THIS leader, bounded by controlReadTimeout, so a member that is
	// alive but partitioned from the leader at that instant reads false and a
	// consumer treating absence as death will treat it as gone.
	//
	// THAT IS NOT A REGRESSION AND IT IS NOT THIS FIELD'S EXPOSURE. A consumer
	// deciding orphanhood from the leader's view already reads a partitioned
	// member as dead, because the leader's view is the only liveness authority
	// available to it; this field agrees with that view rather than contradicting
	// it, and narrows the case where it is wrong rather than widening it. The
	// field is a ONE-DIAL FACT and is meant to be consumed as one. A second dial
	// would not settle it either, and no consumer has asked for one.
	//
	// IT EXISTS BECAUSE EVICTION AND DEATH ARE NOT THE SAME EVENT. Eviction
	// keys on absence from the orchestrator's registry, and a resolution of the
	// right size but the wrong membership can omit a member that is perfectly
	// alive — the residual evictionPermitted discloses. A consumer that reads an
	// eviction as "this member is gone" would offer a live member's work beside
	// its still-running owner, which is a single-writer violation; with this
	// fact it can treat a reachable member's eviction as a SUSPENSION and wait
	// for the re-place round to readmit it.
	//
	// THE REGISTRY CANNOT SUPPLY THIS FACT, and that is worth stating where the
	// field is declared. absentMember returns only members whose address the
	// resolution does NOT list, so the victim is absent from the live set BY
	// CONSTRUCTION and the live set says the opposite of what a consumer needs.
	// The value is a direct dial, taken at the moment of removal.
	EvictedWhileReachable bool
	// ReadmissionExpectedBy is the instant after which a readmission of this
	// evicted member is no longer in flight. IT IS STAMPED ON EVERY EVICTION
	// SIGNAL, reachable or not — a member that did not answer one dial may still
	// return and re-place itself, so the instant is meaningful for it too, and a
	// consumer that reads the fact as gone simply has no use for it. It is the
	// zero value on every other kind.
	//
	// IT IS NOT AUTOPILOT'S STABILIZATION WINDOW, and the difference matters to
	// the consumer rather than to this package. ServerStabilizationTime is how
	// long autopilot waits before it will call a member healthy; it says nothing
	// about how long a readmission takes. The quantity a consumer needs, to know
	// how long an eviction of a REACHABLE member might still be undone, is the
	// re-place round's own cadence: an evicted member observes its absence on
	// its next round and re-announces on that same round, so two rounds cover
	// the one in which it notices and the one in which it acts.
	//
	// CARRYING THE DEADLINE RATHER THAN THE DURATION IS DELIBERATE. A consumer
	// that held the duration would hold a second copy of a cadence this package
	// owns, and the two would drift; a consumer given an instant holds nothing
	// and asks no question about which clock started it.
	ReadmissionExpectedBy time.Time
}

// signalLog is the retained window and the broadcast channel readers park on.
//
// IT IS A CURSOR, NOT A CHANNEL, and that is what makes a lost signal
// unrepresentable: a reader asks for everything since its cursor, so a slow
// reader falls behind without anything blocking or dropping. A channel we owned
// could drop, and any channel raft sends on can wedge it.
type signalLog struct {
	mu sync.Mutex
	// incarnation distinguishes THIS log from every other one a consumer could
	// hold a cursor from. Without it a cursor is meaningful only by accident: a
	// restart renumbers from the beginning, and a consumer that reconnects to a
	// DIFFERENT node applies one instance's numbering to another's — which under
	// ephemeral identity is the designed steady state rather than an exception.
	incarnation uint64
	wake        chan struct{}
	retained    []Signal
	next        uint64
}

func newSignalLog() *signalLog {
	return &signalLog{incarnation: newIncarnation(), wake: make(chan struct{}), next: 1}
}

// newIncarnation mints a non-zero instance discriminator.
//
// ZERO IS EXCLUDED because zero is how a caller spells "from the beginning", so
// a log whose incarnation were zero could not tell that request from a cursor.
// This is an instance discriminator rather than a security value: the only
// property it needs is that two logs are overwhelmingly unlikely to share one.
//
// IT DRAWS FROM crypto/rand ANYWAY. A weak source would satisfy the property,
// but gosec refuses one on sight and the module's lint gate admits no findings —
// and this package has no hot path here to protect, since an incarnation is
// minted once per log. crypto/rand.Read never returns an error and always fills
// its argument entirely; it panics rather than reporting a short read.
func newIncarnation() uint64 {
	for {
		var raw [4]byte
		_, _ = rand.Read(raw[:])
		if n := uint64(binary.BigEndian.Uint32(raw[:])); n != 0 {
			return n
		}
	}
}

// roll starts a fresh numbering when the sequence would otherwise carry into the
// incarnation bits.
//
// ROLLING IS THE RIGHT RESPONSE RATHER THAN A LAST RESORT: the numbering has
// restarted, so every outstanding cursor SHOULD be refused — which is exactly
// what a restart already gets. The retained window is dropped with it because it
// belongs to the old numbering, and leaving it would break the sequence
// comparison in since, which assumes one numbering.
func (s *signalLog) roll() {
	s.incarnation = newIncarnation()
	s.next = 1
	s.retained = nil
}

// publish records one signal and wakes every parked reader.
func (s *signalLog) publish(sig Signal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next > signalSequenceMask {
		s.roll()
	}
	sig.Index = s.incarnation<<signalIncarnationShift | s.next
	s.next++
	s.retained = append(s.retained, sig)
	if len(s.retained) > maxRetainedSignals {
		s.retained = s.retained[len(s.retained)-maxRetainedSignals:]
	}
	close(s.wake)
	s.wake = make(chan struct{})
}

// signalBatch is everything a reader needs from one look at the log, taken
// together under one lock so the wake channel accounts for the window it was
// read beside.
type signalBatch struct {
	signals []Signal
	cursor  uint64
	wake    chan struct{}
	err     error
}

// since collects every retained signal after cursor.
func (s *signalLog) since(cursor uint64) signalBatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := signalBatch{cursor: cursor, wake: s.wake}
	// THE INCARNATION IS CHECKED FIRST, AND THE RETENTION COMPARISON BELOW DEPENDS
	// ON IT HAVING RUN. Comparing sequences across two different numberings is
	// meaningless, so that comparison is sound only once the high bits are known
	// equal. A zero cursor is exempt: it is how a caller spells "from the
	// beginning" and belongs to no incarnation.
	if cursor != 0 && cursor>>signalIncarnationShift != s.incarnation {
		batch.err = fmt.Errorf("%w: cursor %d carries incarnation %d, this log is %d",
			ErrCursorForeignIncarnation, cursor, cursor>>signalIncarnationShift, s.incarnation)
		return batch
	}
	if len(s.retained) > 0 {
		oldest := s.retained[0].Index
		// The reader's next expected signal is cursor+1. If the window no longer
		// holds it, signals were dropped between them and saying so is the only
		// honest answer.
		//
		// THE COMPARISON IS ON THE SEQUENCE, NEVER ON THE PACKED VALUE. Packed, the
		// oldest retained index is an enormous number and even a zero cursor — the
		// most common call there is — reads as older than the window.
		if (cursor&signalSequenceMask)+1 < oldest&signalSequenceMask {
			batch.err = fmt.Errorf("%w: cursor %d, oldest retained %d", ErrCursorTooOld, cursor, oldest)
			return batch
		}
	}
	for _, sig := range s.retained {
		if sig.Index > cursor {
			batch.signals = append(batch.signals, sig)
			batch.cursor = sig.Index
		}
	}
	return batch
}

// Watch blocks until at least one signal newer than since exists, and returns
// every retained signal after it together with the new cursor.
func (m *Manager) Watch(ctx context.Context, since uint64) ([]Signal, uint64, error) {
	for {
		batch := m.signals.since(since)
		if batch.err != nil {
			return nil, 0, batch.err
		}
		if len(batch.signals) > 0 {
			return batch.signals, batch.cursor, nil
		}
		select {
		case <-batch.wake:
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
}

// Membership reports a flow's applied configuration and the index it landed at.
// It is what a consumer rebuilds from when its cursor is refused.
func (m *Manager) Membership(flow string) (raft.Configuration, uint64, bool) {
	l, ok := m.Ledger(flow)
	if !ok {
		return raft.Configuration{}, 0, false
	}
	configuration, index := l.Configuration()
	return configuration, index, true
}

// watchMembership publishes a signal for every configuration this node's state
// machine applies for a flow.
//
// IT RUNS ON EVERY NODE, not only the leader, which is the whole point of
// sourcing it from the FSM: raft's own membership events are leader-only, and a
// follower that could not see a membership change could not act on one.
func (m *Manager) watchMembership(flow string, pilot *flowPilot) {
	defer m.wg.Done()
	l, ok := m.Ledger(flow)
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(m.pilotCtx)
	defer cancel()
	go func() {
		select {
		case <-pilot.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	var since uint64
	for {
		_, index, err := l.WatchConfiguration(ctx, since)
		if err != nil {
			return
		}
		since = index
		m.signals.publish(Signal{
			Kind: SignalMembershipChanged, Flow: flow, Node: m.cfg.Node,
			Since: time.Now(),
		})
	}
}

// noteHealth turns autopilot's own view of a flow's members into unreachable and
// returned signals.
//
// THESE ARE SOURCED FROM AUTOPILOT'S STATE rather than from a second observer
// registration, so this package hands raft no additional channel — and they are
// LEADER-ONLY by construction, because autopilot only runs where a flow is led.
//
// SignalPeerUnreachable MEANS "WAS REACHABLE, NOW IS NOT", AND THE FIRST
// SIGHTING IS WHERE THAT MEANING IS EARNED. Autopilot reports every member
// unhealthy until it has been stable for ServerStabilizationTime, so a node
// taking leadership for the first time publishes a first state in which every
// member is unhealthy — for a group in which nothing is wrong. Publishing on
// that sighting told a consumer that every live peer was unreachable for the
// whole stabilization window after each first-time failover, and a consumer
// reading the signal as liveness would offer every live peer's work as orphaned.
// MEASURED: a fresh pilot's first all-unhealthy state published three
// unreachable signals for three live peers.
//
// THE ENCODING, per peer, and each clause exists because the one before it is
// not sufficient:
//
//	FIRST SIGHTING PUBLISHES NOTHING, in either direction. There is no prior
//	state for a transition to be a transition FROM.
//	HEALTHY THEN UNHEALTHY publishes SignalPeerUnreachable at once. This is the
//	case the signal is named for and it is not delayed.
//	STILL UNHEALTHY SINCE A FIRST SIGHTING publishes SignalPeerUnreachable once
//	the stabilization window has passed since that sighting. WITHOUT THIS CLAUSE
//	A GENUINELY DEAD PEER AT A FRESH LEADER'S START WOULD NEVER BE REPORTED AT
//	ALL, because it never becomes healthy and so never makes the transition
//	above. The cost is a bounded and named latency rather than a lost report:
//	at most one stabilization window, which is the same window autopilot needs
//	before it would call that peer healthy anyway.
//	UNHEALTHY THEN HEALTHY publishes SignalPeerReturned only when an unreachable
//	was actually published for the episode it closes, so a returned signal
//	always has an opening and a consumer never has to reconcile a return it
//	never saw a departure for.
func (f *flowPilot) noteHealth(state *autopilotState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for id, healthy := range state.health {
		// THE LOCAL NODE IS EXCLUDED UNCONDITIONALLY. Autopilot's published state
		// carries every member including this one, and it reports every member
		// unhealthy until it has been stable for ServerStabilizationTime — the
		// leader included — so without this the leader's own first sighting
		// publishes SignalPeerUnreachable naming itself. A node reporting itself
		// unreachable is definitionally reachable, and a consumer reads that
		// signal to decide a peer's datums may be orphaned, so it would treat the
		// live leader's own work as orphaned.
		//
		// EXCLUDING BEATS SEEDING f.healthy WITH SELF, which was the other
		// candidate: seeding suppresses only the FIRST sighting, and self marked
		// unhealthy after having been seen healthy takes the seen-and-changed path
		// and publishes anyway. The exclusion makes the invariant unconditional —
		// no path through this loop can publish a peer signal naming the
		// publishing node.
		//
		// IT IS SCOPED TO THE THREE PEER KINDS. SignalMembershipChanged carries
		// this node's own id BY DESIGN, because it is this node reporting that IT
		// applied a configuration, which is the every-node half of the seam.
		if string(id) == f.mgr.cfg.Node {
			continue
		}
		f.notePeerHealth(id, healthy, now)
	}
}

// notePeerHealth settles ONE peer's health against what this pilot last saw.
func (f *flowPilot) notePeerHealth(id raft.ServerID, healthy bool, now time.Time) {
	was, seen := f.healthy[id]
	if !seen {
		f.healthy[id] = healthy
		f.firstSeen[id] = now
		return
	}
	f.healthy[id] = healthy
	switch {
	case !healthy && !f.announced[id] && (was || now.Sub(f.firstSeen[id]) >= f.stabilization()):
		f.announced[id] = true
		f.publishPeer(SignalPeerUnreachable, id)
	case healthy && f.announced[id]:
		f.announced[id] = false
		f.publishPeer(SignalPeerReturned, id)
	}
}

// stabilization is the window a peer first seen unhealthy is given before it is
// reported unreachable. It is READ FROM THE SAME EXPRESSION AutopilotConfig uses
// so the suppression window and autopilot's own stabilization period cannot
// drift apart.
func (f *flowPilot) stabilization() time.Duration {
	return orDuration(f.mgr.cfg.Autopilot.ServerStabilizationTime, defaultServerStabilizationTime)
}

// publishPeer records one peer-kind signal.
func (f *flowPilot) publishPeer(kind SignalKind, id raft.ServerID) {
	f.mgr.signals.publish(Signal{Kind: kind, Flow: f.flow, Node: string(id), Since: time.Now()})
}

// autopilotState is the slice of autopilot's published state this package reads.
type autopilotState struct {
	health map[raft.ServerID]bool
}
