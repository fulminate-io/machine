// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/whitaker-io/machine/raft/transport"
)

// peers is this node's control-channel client.
type peers struct {
	logger   hclog.Logger
	timeout  time.Duration
	interval time.Duration
	// dial is the ONE dial path, held as a field so a test can count the
	// connections a round opens without reaching into the transport.
	dial func(address string, timeout time.Duration) (net.Conn, error)

	mu       sync.Mutex
	addrs    []string
	flows    []string
	view     map[string]map[string]FlowStats
	failures map[string]error
	viewAt   time.Time
}

// newPeers builds the client over this node's control channel.
func newPeers(link *transport.MembershipLink, logger hclog.Logger) *peers {
	return &peers{
		logger:   logger,
		timeout:  controlReadTimeout,
		interval: statsInterval,
		dial:     link.Dial,
	}
}

// setMembership records who to ask and what to ask about, and invalidates the
// current view so the next read reflects the new set rather than the old one.
func (p *peers) setMembership(addrs, flows []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.addrs = append([]string(nil), addrs...)
	p.flows = append([]string(nil), flows...)
	p.view = nil
}

// statsFor reports every peer's progress on ONE flow, projected out of the
// shared view.
//
// THE SHARED VIEW IS WHY N LED FLOWS COST ONE ROUND. One promoter instance runs
// per flow and each asks on its own schedule; without a view shared across them,
// a node leading fifty flows would issue fifty independent rounds to every peer
// per interval, and the flow LIST in the request — which makes one round cover
// every flow — would be defeated by the fan-out above it.
func (p *peers) statsFor(flow string) map[string]FlowStats {
	view := p.statsView()
	out := make(map[string]FlowStats, len(view))
	for addr, byFlow := range view {
		if stats, ok := byFlow[flow]; ok {
			out[addr] = stats
		}
	}
	return out
}

// statsView returns the current per-peer view, running one round when the view
// is older than the interval.
//
// THE LOCK IS HELD ACROSS THE ROUND DELIBERATELY, and that is the coalescing
// mechanism rather than an oversight: a second caller arriving mid-round waits
// and then reads the view the first one just filled, instead of finding it stale
// and dialing again. Releasing the lock to do the I/O would let N concurrent
// per-flow callers each start their own round, which is the exact defect the
// shared view exists to prevent.
func (p *peers) statsView() map[string]map[string]FlowStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.view != nil && time.Since(p.viewAt) < p.interval {
		return p.view
	}
	p.view, p.failures = p.round()
	p.viewAt = time.Now()
	return p.view
}

// addresses reports the peer set this node asks.
func (p *peers) addresses() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.addrs...)
}

// failuresSnapshot reports the peers the last round could not reach.
func (p *peers) failuresSnapshot() map[string]error {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]error, len(p.failures))
	for addr, err := range p.failures {
		out[addr] = err
	}
	return out
}

// round asks every peer once, carrying the whole flow list. The caller holds the
// lock.
//
// A PEER THAT CANNOT BE REACHED IS RECORDED, NOT RETRIED. It is left out of the
// view and named in the failures map, because an unreachable peer is exactly the
// condition the failure signal exists to report — a retry loop here would hide
// it, and a silent omission would be indistinguishable from a peer that answered
// with nothing.
func (p *peers) round() (map[string]map[string]FlowStats, map[string]error) {
	view := make(map[string]map[string]FlowStats, len(p.addrs))
	failed := make(map[string]error)
	for _, addr := range p.addrs {
		reply, err := p.call(addr, statsRequest{Flows: p.flows})
		if err != nil {
			failed[addr] = err
			p.logger.Warn("a stats round did not reach a peer", "peer", addr, "error", err)
			continue
		}
		answer, ok := reply.(*statsReply)
		if !ok {
			failed[addr] = fmt.Errorf("%w: a stats request was answered with %T", ErrUnknownMessage, reply)
			continue
		}
		view[addr] = answer.PerFlow
	}
	return view, failed
}

// call sends one request to addr and returns the reply.
//
// ONE CONNECTION, ONE EXCHANGE. The acceptor reads exactly one message and
// answers, so there is no connection to hold open between rounds; what makes a
// round cheap is that the request carries a flow LIST, so one dial serves every
// flow this node shares with the peer rather than one dial per flow.
//
// A FAILED CALL IS REPORTED, NOT RETRIED. The connection is discarded and the
// error returned; the next round dials fresh.
func (p *peers) call(addr string, req any) (any, error) {
	kind, reply, err := replyFor(req)
	if err != nil {
		return nil, err
	}
	conn, err := p.dial(addr, p.timeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if err := p.exchange(conn, kind, req, reply); err != nil {
		return nil, err
	}
	return reply, nil
}

// exchange writes the request and reads the reply, bounding both in time.
//
// THE READ DEADLINE IS SET BEFORE THE READ on this side too. A peer that accepts
// the request and then goes silent must not park this caller, and the connection
// carries no deadline of its own.
func (p *peers) exchange(conn net.Conn, kind msgKind, req, reply any) error {
	if err := conn.SetWriteDeadline(time.Now().Add(p.timeout)); err != nil {
		return err
	}
	if err := writeMessage(conn, kind, req); err != nil {
		return err
	}
	if err := conn.SetReadDeadline(time.Now().Add(p.timeout)); err != nil {
		return err
	}
	answered, body, err := readMessage(conn)
	if err != nil {
		return err
	}
	if want := replyKind(kind); answered != want {
		return fmt.Errorf("%w: a kind %d request was answered with kind %d, want %d",
			ErrUnknownMessage, uint8(kind), uint8(answered), uint8(want))
	}
	return decodeMessage(body, reply)
}

// replyFor pairs a request with the kind that carries it and the value its reply
// decodes into. THE PAIRING LIVES IN ONE PLACE so a request kind cannot be sent
// under one name and read back under another.
func replyFor(req any) (msgKind, any, error) {
	switch req.(type) {
	case announce:
		return msgAnnounce, &announceReply{}, nil
	case statsRequest:
		return msgStats, &statsReply{}, nil
	case leave:
		return msgLeave, &leaveReply{}, nil
	default:
		return 0, nil, fmt.Errorf("%w: %T is not a control request", ErrUnknownMessage, req)
	}
}

// replyKind reports the kind that answers a request kind.
func replyKind(kind msgKind) msgKind {
	switch kind {
	case msgAnnounce:
		return msgAnnounceReply
	case msgStats:
		return msgStatsReply
	case msgLeave:
		return msgLeaveReply
	case msgAnnounceReply, msgStatsReply, msgLeaveReply:
		return 0
	default:
		return 0
	}
}
