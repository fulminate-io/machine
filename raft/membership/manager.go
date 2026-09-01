// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/whitaker-io/machine/raft/transport"
)

// Manager refusals, declared beside the code that returns them.
var (
	// ErrConfigMissing refuses a required configuration field left empty, naming
	// the field. Bad input errors here; it is never defaulted.
	ErrConfigMissing = errors.New("membership: required configuration field is empty")
	// ErrUnservedMessage refuses a DECLARED kind this node has no arm for — a
	// reply kind arriving at an acceptor, for instance. It is distinct from
	// ErrUnknownMessage, which refuses a kind no build declares.
	ErrUnservedMessage = errors.New("membership: control message kind has no handler here")
)

const (
	// controlReadTimeout is the TIME bound on every control read, on both sides.
	//
	// IT IS THE ONLY BOUND THAT COVERS A PEER WHICH STOPS SENDING. The connection
	// this package receives carries no deadline of its own: the transport's
	// handshake clears it before handing the socket over, deliberately, because
	// raft sets a per-RPC deadline only when its own Timeout is positive and a
	// deadline left behind would expire mid-RPC. An io.LimitedReader bounds
	// BYTES and does nothing about silence, so without this a silent peer holds
	// a goroutine and a file descriptor for the process lifetime.
	controlReadTimeout = 2 * time.Second
	// statsInterval is how long a stats view is served before a fresh round runs.
	statsInterval = 2 * time.Second
)

// Config carries what the control channel needs to serve and to dial.
type Config struct {
	// Node is this node's raft server id.
	//
	// EPHEMERAL BY CONSTRUCTION: under a plain Deployment no per-replica value
	// survives a restart, so a restarted instance is a NEW member and the old one
	// is reconciled by eviction rather than resumed.
	Node string
	// Advertise is what peers dial — the pod IP, because only a StatefulSet has
	// per-pod DNS and the design must work under a plain Deployment.
	Advertise string
	// Mux is the shared listener this node's control channel binds on.
	Mux *transport.Mux
	// Logger receives control-channel logs.
	Logger hclog.Logger
}

// validate refuses an empty required field BY NAME.
func (c Config) validate() error {
	if c.Node == "" {
		return fmt.Errorf("%w: Config.Node", ErrConfigMissing)
	}
	if c.Advertise == "" {
		return fmt.Errorf("%w: Config.Advertise", ErrConfigMissing)
	}
	if c.Mux == nil {
		return fmt.Errorf("%w: Config.Mux", ErrConfigMissing)
	}
	return nil
}

// Manager owns this node's membership control channel: the acceptor that serves
// peers and the client that asks them.
type Manager struct {
	cfg    Config
	logger hclog.Logger
	link   *transport.MembershipLink
	peers  *peers

	// wg covers the serve loop and every handler goroutine, so Close can join
	// them rather than leaving state to be torn down underneath a live handler.
	wg sync.WaitGroup

	mu         sync.Mutex
	closed     bool
	inflight   map[net.Conn]struct{}
	localStats func(flow string) (FlowStats, bool)
	// onHandlerExit is an OBSERVATION SEAM, called as a handler leaves. The
	// shutdown gate needs to see that a handler actually exited rather than
	// inferring it: a Close that abandoned its handlers would otherwise be the
	// fastest implementation and would satisfy any timing ceiling perfectly.
	onHandlerExit func()
}

// InFlight reports how many control connections this node is currently serving.
// It is what makes a parked handler observable from outside, both to an operator
// and to the gate that proves a silent peer is reaped.
func (m *Manager) InFlight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.inflight)
}

// New binds this node's control channel and starts serving it.
func New(cfg Config) (*Manager, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	link, err := cfg.Mux.BindMembership()
	if err != nil {
		return nil, err
	}
	logger := cfg.Logger
	if logger == nil {
		logger = hclog.NewNullLogger()
	}
	m := &Manager{
		cfg:      cfg,
		logger:   logger,
		link:     link,
		peers:    newPeers(link, logger),
		inflight: map[net.Conn]struct{}{},
	}
	m.wg.Add(1)
	go m.serve()
	return m, nil
}

// SetLocalStats installs what this node answers a stats request with. The
// promoter and the ledgers supply it; until they do, this node reports nothing
// about itself rather than reporting a zero value that would read as a peer at
// term zero with no contact.
func (m *Manager) SetLocalStats(fn func(flow string) (FlowStats, bool)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.localStats = fn
}

// SetPeers records who this node asks and what it asks about.
func (m *Manager) SetPeers(addrs, flows []string) { m.peers.setMembership(addrs, flows) }

// Stats reports every peer's progress on one flow, served from a view refreshed
// at most once per interval and shared by every caller.
func (m *Manager) Stats(flow string) map[string]FlowStats { return m.peers.statsFor(flow) }

// PeerFailures reports the peers the last stats round could not reach, with the
// error each one failed on. A failed call is REPORTED rather than retried, and
// this is where the report is read.
func (m *Manager) PeerFailures() map[string]error { return m.peers.failuresSnapshot() }

// serve does nothing but Accept and dispatch.
//
// THE ACCEPTOR MUST NOT SLOW THE MUX. The transport hands a connection to the
// binding's queue and returns; a handler that ran inline on this loop would let
// one slow peer delay every other connection's dispatch, so handle runs on its
// own goroutine and this loop never reads a byte.
func (m *Manager) serve() {
	defer m.wg.Done()
	for {
		conn, err := m.link.Accept()
		if err != nil {
			return
		}
		if !m.register(conn) {
			_ = conn.Close()
			return
		}
		m.wg.Add(1)
		go m.handle(conn)
	}
}

// register adds conn to the in-flight set so Close can reach it, refusing once
// Close has run so a connection accepted in that race is not served after the
// door shut.
func (m *Manager) register(conn net.Conn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.inflight[conn] = struct{}{}
	return true
}

// noteExit fires the handler-exit seam, if one is installed.
func (m *Manager) noteExit() {
	m.mu.Lock()
	fn := m.onHandlerExit
	m.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// deregister drops conn from the in-flight set.
func (m *Manager) deregister(conn net.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inflight, conn)
}

// handle serves one connection and then closes it.
func (m *Manager) handle(conn net.Conn) {
	defer m.wg.Done()
	defer m.noteExit()
	defer func() { _ = conn.Close() }()
	defer m.deregister(conn)
	if err := m.exchange(conn); err != nil {
		m.logger.Warn("a membership control exchange failed", "remote", conn.RemoteAddr(), "error", err)
	}
}

// exchange reads exactly ONE request and writes exactly one reply, which is the
// COUNT bound: there is no loop here for a peer to hold open.
//
// The read deadline is set BEFORE the read, which is the TIME bound. It reaches
// the socket because the session connection the transport hands us EMBEDS
// net.Conn and promotes SetReadDeadline to it; an implementation that overrode
// that method — to buffer it, to no-op it, or to apply it per record — would
// make this bound inert while every other assertion still passed.
func (m *Manager) exchange(conn net.Conn) error {
	if err := conn.SetReadDeadline(time.Now().Add(controlReadTimeout)); err != nil {
		return err
	}
	kind, body, err := readMessage(conn)
	if err != nil {
		return err
	}
	return m.answer(conn, kind, body)
}

// answer dispatches one request to the arm its kind names.
func (m *Manager) answer(conn net.Conn, kind msgKind, body []byte) error {
	switch kind {
	case msgStats:
		var req statsRequest
		if err := decodeMessage(body, &req); err != nil {
			return err
		}
		if err := conn.SetWriteDeadline(time.Now().Add(controlReadTimeout)); err != nil {
			return err
		}
		return writeMessage(conn, msgStatsReply, m.statsReplyFor(req))
	case msgAnnounce, msgLeave, msgAnnounceReply, msgStatsReply, msgLeaveReply:
		return fmt.Errorf("%w: kind %d", ErrUnservedMessage, uint8(kind))
	default:
		return fmt.Errorf("%w: kind %d", ErrUnknownMessage, uint8(kind))
	}
}

// statsReplyFor answers with what this node knows about itself. A flow it does
// not host is OMITTED rather than reported as a zero value, which would read as
// a member at term zero that has never been contacted.
func (m *Manager) statsReplyFor(req statsRequest) statsReply {
	m.mu.Lock()
	fn := m.localStats
	m.mu.Unlock()
	out := statsReply{PerFlow: make(map[string]FlowStats, len(req.Flows))}
	if fn == nil {
		return out
	}
	for _, flow := range req.Flows {
		if stats, ok := fn(flow); ok {
			out.PerFlow[flow] = stats
		}
	}
	return out
}

// Close shuts the control channel down in a PRESCRIBED ORDER, and each step is
// safe only because the one before it ran.
//
//  1. close the membership link, so no new connection is accepted
//  2. close every in-flight connection, so every parked read returns NOW rather
//     than waiting out its deadline
//  3. wait for the serve loop and every handler goroutine
//
// Both readings of an unspecified order are defects: a Close that joined without
// first closing the connections would hang on one silent peer, and a Close that
// never joined would go on to tear down state underneath a live handler.
//
// It is idempotent.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	conns := make([]net.Conn, 0, len(m.inflight))
	for conn := range m.inflight {
		conns = append(conns, conn)
	}
	m.inflight = map[net.Conn]struct{}{}
	m.mu.Unlock()
	err := m.link.Close()
	for _, conn := range conns {
		_ = conn.Close()
	}
	m.wg.Wait()
	return err
}
