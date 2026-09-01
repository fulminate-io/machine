// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package transport

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
)

// Mux lifecycle refusals, declared beside the code that returns them.
var (
	ErrClosed          = errors.New("transport: mux is closed")
	ErrGroupBound      = errors.New("transport: group id is already bound")
	ErrNotAdvertisable = errors.New("transport: bind address is not advertisable")
)

// Config carries the mux's listener and the per-group NetworkTransport knobs.
type Config struct {
	// BindAddr is the host:port the shared listener binds.
	BindAddr string
	// Advertise overrides the address reported to peers; it is required when
	// BindAddr is unspecified, exactly as raft's own TCP transport requires.
	Advertise net.Addr
	// Logger receives mux and per-group transport logs.
	Logger hclog.Logger
	// HandshakeTimeout bounds how long a connection may take to announce its
	// group before it is closed.
	HandshakeTimeout time.Duration
	// AcceptQueueDepth bounds the connections held for one group awaiting
	// Accept. Beyond it, a connection waits HandshakeTimeout and is then
	// refused and counted rather than held.
	AcceptQueueDepth int
	// MaxPool and RPCTimeout are handed to every group's NetworkTransport.
	MaxPool    int
	RPCTimeout time.Duration
}

// Stats reports what the mux accepted and refused. Every refusal is counted:
// no connection is dropped without a number moving. The set is closed rather
// than grown — if an arm is added to deliver, it gets a counter in the same
// breath.
type Stats struct {
	Handshakes               uint64
	ForwardHandshakes        uint64
	RejectedUnknownGroup     uint64
	RejectedMalformed        uint64
	RejectedQueueFull        uint64
	RejectedForwardQueueFull uint64
	RejectedUnknownKind      uint64
	RejectedGroupClosed      uint64
	AcceptErrors             uint64
}

// Mux owns one net.Listener shared by every raft group on this node.
type Mux struct {
	ln        net.Listener
	advertise net.Addr
	logger    hclog.Logger
	cfg       Config

	mu     sync.RWMutex
	groups map[GroupID]*groupStream
	closed bool

	stats Stats
}

// New binds the shared listener and starts the accept loop.
func New(cfg Config) (*Mux, error) {
	ln, err := net.Listen("tcp", cfg.BindAddr)
	if err != nil {
		return nil, err
	}
	m, err := newMux(ln, cfg)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	go m.accept()
	return m, nil
}

// newMux wires a mux over an already-bound listener and does NOT start the
// accept loop. Tests drive this to inject listeners whose Accept fails on
// demand, and to own the moment the loop starts.
func newMux(ln net.Listener, cfg Config) (*Mux, error) {
	m := &Mux{
		ln:        ln,
		advertise: cfg.Advertise,
		logger:    cfg.Logger,
		cfg:       withDefaults(cfg),
		groups:    map[GroupID]*groupStream{},
	}
	if m.logger == nil {
		m.logger = hclog.NewNullLogger()
	}
	if err := m.checkAdvertisable(); err != nil {
		return nil, err
	}
	return m, nil
}

// withDefaults fills the knobs a caller left zero.
func withDefaults(cfg Config) Config {
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 5 * time.Second
	}
	if cfg.AcceptQueueDepth <= 0 {
		cfg.AcceptQueueDepth = 4
	}
	if cfg.MaxPool <= 0 {
		cfg.MaxPool = 3
	}
	if cfg.RPCTimeout <= 0 {
		cfg.RPCTimeout = 10 * time.Second
	}
	return cfg
}

// checkAdvertisable refuses an address peers could not dial back, mirroring
// raft's own newTCPTransport check. Without it a node bound to 0.0.0.0 hands
// peers a ServerAddress of 0.0.0.0:port and the cluster silently cannot form.
func (m *Mux) checkAdvertisable() error {
	addr, ok := m.Addr().(*net.TCPAddr)
	if !ok {
		return ErrNotAdvertisable
	}
	if addr.IP == nil || addr.IP.IsUnspecified() {
		return ErrNotAdvertisable
	}
	return nil
}

// Addr reports the one address every group on this mux is reached at. Group
// identity is carried by the handshake, never by the address.
func (m *Mux) Addr() net.Addr {
	if m.advertise != nil {
		return m.advertise
	}
	return m.ln.Addr()
}

// Stats returns a snapshot of the mux counters.
func (m *Mux) Stats() Stats {
	return Stats{
		Handshakes:               atomic.LoadUint64(&m.stats.Handshakes),
		ForwardHandshakes:        atomic.LoadUint64(&m.stats.ForwardHandshakes),
		RejectedUnknownGroup:     atomic.LoadUint64(&m.stats.RejectedUnknownGroup),
		RejectedMalformed:        atomic.LoadUint64(&m.stats.RejectedMalformed),
		RejectedQueueFull:        atomic.LoadUint64(&m.stats.RejectedQueueFull),
		RejectedForwardQueueFull: atomic.LoadUint64(&m.stats.RejectedForwardQueueFull),
		RejectedUnknownKind:      atomic.LoadUint64(&m.stats.RejectedUnknownKind),
		RejectedGroupClosed:      atomic.LoadUint64(&m.stats.RejectedGroupClosed),
		AcceptErrors:             atomic.LoadUint64(&m.stats.AcceptErrors),
	}
}

// accept owns the shared listener. It never reads from a connection and never
// blocks on a group, so one group can neither stall another's inbound path nor
// the acceptance of new connections.
//
// A FAILING Accept IS NOT ASSUMED TO MEAN THE LISTENER CLOSED. TCPListener.Accept
// also reports process- and system-wide file-descriptor exhaustion (EMFILE,
// ENFILE), and the runtime retries only EINTR and ECONNABORTED — so returning on
// the first error would let one transient condition silently retire the node's
// whole inbound path while the socket stayed healthy. This mirrors raft's own
// NetworkTransport.listen: back off, keep serving, and exit only when we are
// actually shut down.
func (m *Mux) accept() {
	const baseDelay = 5 * time.Millisecond
	const delayCeiling = time.Second
	var delay time.Duration
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			atomic.AddUint64(&m.stats.AcceptErrors, 1)
			if m.isClosed() {
				return
			}
			delay = nextDelay(delay, baseDelay, delayCeiling)
			m.logger.Error("accept failed; retrying", "error", err, "retry-in", delay)
			time.Sleep(delay)
			continue
		}
		delay = 0
		go m.route(conn)
	}
}

// nextDelay doubles the accept backoff up to a ceiling. The third parameter is
// named ceiling rather than max because revive's redefines-builtin-id is on at
// severity error and Go 1.21 made max a builtin.
func nextDelay(cur, base, ceiling time.Duration) time.Duration {
	if cur == 0 {
		return base
	}
	if cur*2 > ceiling {
		return ceiling
	}
	return cur * 2
}

// isClosed reports whether Close has run, which is the only condition under
// which the accept loop is allowed to stop.
func (m *Mux) isClosed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.closed
}

// route reads one handshake and hands the connection to the arm its announced
// stream kind names.
//
// THE KIND IS DELIVERY VOCABULARY AND NOTHING ELSE: the group lookup and the
// refusal for an unbound group are shared by both arms, so a forwarding
// connection for a group this node does not host is refused exactly as a raft
// one is, on the same counter.
func (m *Mux) route(conn net.Conn) {
	id, kind, err := readPreamble(conn, m.cfg.HandshakeTimeout)
	if err != nil {
		m.refuseHandshake(conn, err)
		return
	}
	m.countHandshake(kind)
	m.mu.RLock()
	s := m.groups[id]
	m.mu.RUnlock()
	if s == nil {
		atomic.AddUint64(&m.stats.RejectedUnknownGroup, 1)
		m.logger.Warn("refusing connection for an unbound group", "group", string(id), "remote", conn.RemoteAddr())
		_ = conn.Close()
		return
	}
	if kind == KindForward {
		m.deliverForward(s, conn)
		return
	}
	m.deliver(s, conn)
}

// refuseHandshake counts and closes a connection whose handshake this node will
// not deliver. It distinguishes a kind this build does not declare from a
// malformed head because they mean different things to an operator: the first is
// a peer from a version that speaks something newer, the second is a stray
// client that is not speaking this protocol at all.
func (m *Mux) refuseHandshake(conn net.Conn, err error) {
	if errors.Is(err, ErrBadStreamKind) {
		atomic.AddUint64(&m.stats.RejectedUnknownKind, 1)
		m.logger.Warn("refusing an undeclared stream kind", "remote", conn.RemoteAddr(), "error", err)
		_ = conn.Close()
		return
	}
	atomic.AddUint64(&m.stats.RejectedMalformed, 1)
	m.logger.Warn("refusing connection with a malformed handshake", "remote", conn.RemoteAddr(), "error", err)
	_ = conn.Close()
}

// countHandshake counts an accepted handshake against the arm that will deliver
// it, so the two arms stay separately observable instead of summing into one
// number that cannot tell forwarding load from raft load.
func (m *Mux) countHandshake(kind StreamKind) {
	if kind == KindForward {
		atomic.AddUint64(&m.stats.ForwardHandshakes, 1)
		return
	}
	atomic.AddUint64(&m.stats.Handshakes, 1)
}

// deliver hands conn to s, refusing rather than holding it when s is backlogged.
// Refusing a genuinely backlogged group is backpressure rather than a fallback:
// it repairs itself, because raft's replication reconnects and the group's own
// listen loop drains the queue as soon as it is running, and it is loud. All
// three arms count.
func (m *Mux) deliver(s *groupStream, conn net.Conn) {
	select {
	case s.acceptCh <- conn:
	case <-s.doneCh:
		atomic.AddUint64(&m.stats.RejectedGroupClosed, 1)
		m.logger.Warn("refusing connection for a group unbound mid-handshake", "group", string(s.id))
		_ = conn.Close()
	case <-time.After(m.cfg.HandshakeTimeout):
		atomic.AddUint64(&m.stats.RejectedQueueFull, 1)
		m.logger.Warn("refusing connection for a backlogged group", "group", string(s.id))
		_ = conn.Close()
	}
}

// deliverForward hands conn to s's forwarding queue, refusing rather than
// holding it when that queue is backlogged. It mirrors deliver arm for arm and
// for the same reason given there: refusing a backlogged group is backpressure
// rather than a fallback, because it repairs itself as the group's forwarding
// server drains the queue, and it is loud. A group released mid-handshake counts
// against the same RejectedGroupClosed the raft arm uses, because it is the same
// condition on the same door.
func (m *Mux) deliverForward(s *groupStream, conn net.Conn) {
	select {
	case s.forwardCh <- conn:
	case <-s.doneCh:
		atomic.AddUint64(&m.stats.RejectedGroupClosed, 1)
		m.logger.Warn("refusing a forwarding connection for a group unbound mid-handshake", "group", string(s.id))
		_ = conn.Close()
	case <-time.After(m.cfg.HandshakeTimeout):
		atomic.AddUint64(&m.stats.RejectedForwardQueueFull, 1)
		m.logger.Warn("refusing a forwarding connection for a backlogged group", "group", string(s.id))
		_ = conn.Close()
	}
}

// bindStream registers id and returns the raw stream layer, WITHOUT starting a
// NetworkTransport over it. Bind builds on it, and tests use it to drive the
// routing seam directly — once a NetworkTransport exists it owns every Accept,
// so a test that also called Accept would park forever.
func (m *Mux) bindStream(id GroupID) (*groupStream, error) {
	if len(id) == 0 || len(id) > MaxGroupIDLen {
		return nil, ErrGroupIDRange
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if _, dup := m.groups[id]; dup {
		return nil, fmt.Errorf("%w: %q", ErrGroupBound, string(id))
	}
	s := newGroupStream(m, id)
	m.groups[id] = s
	return s, nil
}

// unbind removes a group and releases anything waiting on it. The release runs
// outside the lock.
func (m *Mux) unbind(id GroupID) {
	m.mu.Lock()
	s := m.groups[id]
	delete(m.groups, id)
	m.mu.Unlock()
	if s != nil {
		s.release()
	}
}

// Close unbinds every group and closes the shared listener. Every group's
// Accept returns an error, which is what lets raft's listen goroutine exit.
func (m *Mux) Close() error {
	m.mu.Lock()
	m.closed = true
	ids := make([]GroupID, 0, len(m.groups))
	for id := range m.groups {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.unbind(id)
	}
	return m.ln.Close()
}
