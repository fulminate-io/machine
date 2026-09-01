// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"context"
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
}

// signalLog is the retained window and the broadcast channel readers park on.
//
// IT IS A CURSOR, NOT A CHANNEL, and that is what makes a lost signal
// unrepresentable: a reader asks for everything since its cursor, so a slow
// reader falls behind without anything blocking or dropping. A channel we owned
// could drop, and any channel raft sends on can wedge it.
type signalLog struct {
	mu       sync.Mutex
	wake     chan struct{}
	retained []Signal
	next     uint64
}

func newSignalLog() *signalLog {
	return &signalLog{wake: make(chan struct{}), next: 1}
}

// publish records one signal and wakes every parked reader.
func (s *signalLog) publish(sig Signal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sig.Index = s.next
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
	if len(s.retained) > 0 {
		oldest := s.retained[0].Index
		// The reader's next expected signal is cursor+1. If the window no longer
		// holds it, signals were dropped between them and saying so is the only
		// honest answer.
		if cursor+1 < oldest {
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
func (f *flowPilot) noteHealth(state *autopilotState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, healthy := range state.health {
		was, seen := f.healthy[id]
		if seen && was == healthy {
			continue
		}
		f.healthy[id] = healthy
		if !seen && healthy {
			continue
		}
		kind := SignalPeerReturned
		if !healthy {
			kind = SignalPeerUnreachable
		}
		f.mgr.signals.publish(Signal{Kind: kind, Flow: f.flow, Node: string(id), Since: time.Now()})
	}
}

// autopilotState is the slice of autopilot's published state this package reads.
type autopilotState struct {
	health map[raft.ServerID]bool
}
