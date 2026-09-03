// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/ledger"
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
	// ErrConfigRange refuses a configuration value outside the range this package
	// can act on, naming the field and the value.
	ErrConfigRange = errors.New("membership: configuration value is out of range")
	// ErrFlowUnplaced refuses to start a flow that could neither be found through
	// the peers address nor created under the count-and-lowest-id rule.
	//
	// IT IS AN ERROR RATHER THAN A SELF-BOOTSTRAP, and that is the whole point:
	// two workers each creating a one-voter group for the same flow produce two
	// logs that can never merge, so while the rule is unsatisfied the only safe
	// action is none.
	ErrFlowUnplaced = errors.New("membership: flow is neither reachable nor creatable under the rule")
	// ErrNotStaged reports a join whose node never appeared in the leader's
	// committed configuration.
	ErrNotStaged = errors.New("membership: the joiner is absent from the committed configuration")
	// ErrIdentityDiverged refuses a ledger whose raft server id is not this
	// manager's node id, naming both values.
	//
	// IT CANNOT BE A validate REFUSAL, and that is why it is declared here rather
	// than beside ErrConfigMissing's use: at validate time there is no LocalID to
	// compare against, because Config.Open has not run and this package reaches a
	// ledger's configuration through nothing else.
	//
	// TWO AUTHORITIES STAMP ONE IDENTITY. Config.Node stamps every signal this
	// package publishes; the ledger's LocalID stamps the raft server id and
	// therefore every entry in the configuration. Diverged, this node bootstraps a
	// group under one and evaluates its own membership under the other — so the
	// self-exclusion in noteHealth never matches and the leader publishes peer
	// signals naming itself, and the placement rule fails later with a message
	// that points at suffrage rather than at the identity.
	ErrIdentityDiverged = errors.New("membership: the ledger's raft server id is not this manager's node id")
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
	// leadershipPollInterval is how often a flow's supervisor re-reads whether
	// this node leads it.
	leadershipPollInterval = 50 * time.Millisecond
	// defaultEvictInterval is how often a leader runs one eviction round.
	defaultEvictInterval = 10 * time.Second
)

// Config carries what the control channel needs to serve and to dial.
type Config struct {
	// Node is this node's raft server id.
	//
	// EPHEMERAL BY CONSTRUCTION: under a plain Deployment no per-replica value
	// survives a restart, so a restarted instance is a NEW member and the old one
	// is reconciled by eviction rather than resumed.
	//
	// IT MUST EQUAL THE LocalID OF EVERY LEDGER Open RETURNS. The two are supplied
	// separately and stamp different things — this one stamps the signals this
	// package publishes, the ledger's stamps the raft configuration — so a
	// disagreement is refused with ErrIdentityDiverged rather than run.
	Node string
	// Advertise is what peers dial — the pod IP, because only a StatefulSet has
	// per-pod DNS and the design must work under a plain Deployment.
	Advertise string
	// Mux is the shared listener this node's control channel binds on.
	Mux *transport.Mux
	// Logger receives control-channel logs.
	Logger hclog.Logger
	// Flows is the flow set this worker hosts. THE FLOW SET NAMES THE GROUPS:
	// derivation is the whole mechanism, and there is no separate group list.
	Flows []string
	// Peers is ONE address that reaches the other instances — a headless
	// Service, a service-discovery name, a DNS round-robin over VMs. It carries
	// the MACHINE_CLUSTER_PEERS value. Unset means the mechanism is absent and a
	// single-instance run stays zero-config.
	Peers string
	// Expect is how many instances this deployment runs, carried by
	// MACHINE_CLUSTER_EXPECT. It is REQUIRED whenever Peers is set and refused
	// when it is below one, so a cluster deployment cannot start in an ambiguous
	// state where a node cannot tell "nobody hosts this flow yet" from "I have
	// not heard from everyone yet".
	Expect int
	// Open opens one flow's ledger. Required whenever Flows is non-empty, and the
	// ledger it returns must report Node as its raft server id.
	Open OpenFunc
	// Autopilot overrides the promotion thresholds and the reconcile cadence.
	Autopilot AutopilotTuning
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
	if len(c.Flows) > 0 && c.Open == nil {
		return fmt.Errorf("%w: Config.Open, which opens each flow's ledger", ErrConfigMissing)
	}
	// A PEERS ADDRESS WITHOUT AN EXPECTED COUNT IS AN ERROR, NOT A DEFAULT. The
	// count is what tells a node that has found no group for a flow whether it
	// has heard from everyone; defaulting it would let a cluster start in the
	// one state where a wrong answer creates two logs that can never merge.
	if c.Peers != "" && c.Expect < 1 {
		return fmt.Errorf("%w: Config.Expect is %d, and a Peers address requires the instance count",
			ErrConfigRange, c.Expect)
	}
	return nil
}

// Manager owns this node's membership control channel: the acceptor that serves
// peers and the client that asks them.
type Manager struct {
	cfg     Config
	logger  hclog.Logger
	link    *transport.MembershipLink
	peers   *peers
	signals *signalLog

	// admit is the staging call, held as a field for ONE reason: the availability
	// proof has to be able to run the wrong shape. The mutant it exists to catch
	// is a join that admits with AddVoter, and a test that could not point this at
	// the voter form could not show the group losing leadership. Production never
	// reassigns it, and this package never calls AddVoter — a corpus check
	// enforces that as a shape across the module.
	admit func(r *raft.Raft, id raft.ServerID, addr raft.ServerAddress) raft.IndexFuture
	// resolve is the discovery seam: one configured address in, every instance
	// behind it out. It is a field so a test can supply a set DNS cannot express,
	// several instances on distinct ports of one loopback host.
	resolve func(ctx context.Context, peers string) ([]string, error)

	// wg covers the serve loop and every handler goroutine, so Close can join
	// them rather than leaving state to be torn down underneath a live handler.
	wg sync.WaitGroup

	// pilotCtx bounds every flow's reconcile loop to this manager's lifetime.
	// Autopilot's Start takes a context and runs until it is canceled, so the
	// loops cannot be tied to the context of whichever call happened to start
	// them.
	pilotCtx    context.Context
	pilotCancel context.CancelFunc

	mu sync.Mutex
	// resolvedAt is when the announce target set was last re-resolved. It is
	// what bounds the refresh to the eviction round's cadence while the place
	// loop retries every placeRetryInterval.
	resolvedAt time.Time
	closed     bool
	inflight   map[net.Conn]struct{}
	flows      map[string]*ledger.Ledger
	pilots     map[string]*flowPilot
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
		signals:  newSignalLog(),
		inflight: map[net.Conn]struct{}{},
		flows:    map[string]*ledger.Ledger{},
		pilots:   map[string]*flowPilot{},
		resolve:  resolvePeers,
	}
	m.pilotCtx, m.pilotCancel = context.WithCancel(context.Background())
	// ADMISSION IS AddNonvoter AND NOTHING ELSE. A joiner is replaying when it
	// is admitted, and raising quorum to include a member that cannot yet answer
	// costs the leader its leadership — measured, and not as a stall: the join
	// call itself returns nil in zero seconds while the NEXT write fails.
	m.admit = func(r *raft.Raft, id raft.ServerID, addr raft.ServerAddress) raft.IndexFuture {
		return r.AddNonvoter(id, addr, 0, 0)
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
		return m.answerStats(conn, body)
	case msgAnnounce:
		return m.answerJoin(conn, body)
	case msgLeave:
		return m.answerDeparture(conn, body)
	case msgAnnounceReply, msgStatsReply, msgLeaveReply:
		return fmt.Errorf("%w: kind %d", ErrUnservedMessage, uint8(kind))
	default:
		return fmt.Errorf("%w: kind %d", ErrUnknownMessage, uint8(kind))
	}
}

// answerStats serves a stats request from what this node knows about itself.
func (m *Manager) answerStats(conn net.Conn, body []byte) error {
	var req statsRequest
	if err := decodeMessage(body, &req); err != nil {
		return err
	}
	return m.reply(conn, msgStatsReply, m.statsReplyFor(req))
}

// answerJoin serves an announce.
func (m *Manager) answerJoin(conn net.Conn, body []byte) error {
	var req announce
	if err := decodeMessage(body, &req); err != nil {
		return err
	}
	return m.reply(conn, msgAnnounceReply, m.answerAnnounce(req))
}

// answerDeparture serves a leave.
func (m *Manager) answerDeparture(conn net.Conn, body []byte) error {
	var req leave
	if err := decodeMessage(body, &req); err != nil {
		return err
	}
	return m.reply(conn, msgLeaveReply, m.answerLeave(req))
}

// reply bounds the write in time and sends one message. The write deadline
// matters for the same reason the read one does: a peer that stops READING must
// not park a handler either.
func (*Manager) reply(conn net.Conn, kind msgKind, payload any) error {
	if err := conn.SetWriteDeadline(time.Now().Add(controlReadTimeout)); err != nil {
		return err
	}
	return writeMessage(conn, kind, payload)
}

// statsReplyFor answers with what this node knows about itself. A flow it does
// not host is OMITTED rather than reported as a zero value, which would read as
// a member at term zero that has never been contacted.
func (m *Manager) statsReplyFor(req statsRequest) statsReply {
	out := statsReply{PerFlow: make(map[string]FlowStats, len(req.Flows))}
	for _, flow := range req.Flows {
		if stats, ok := m.localFlowStats(flow); ok {
			out.PerFlow[flow] = stats
		}
	}
	return out
}

// localFlowStats reports what this node knows about ONE of its own flows. It is
// the one place the installed reporter is read, so the acceptor answering a peer
// and the promoter answering autopilot see the same value.
func (m *Manager) localFlowStats(flow string) (FlowStats, bool) {
	m.mu.Lock()
	fn := m.localStats
	m.mu.Unlock()
	if fn == nil {
		return FlowStats{}, false
	}
	return fn(flow)
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
	conns, flows, already := m.beginClose()
	if already {
		return nil
	}
	// EVERY RECONCILE LOOP STOPS FIRST. Canceling here is what lets the flow
	// supervisors below join autopilot's own goroutines, so no reconcile round
	// can be in flight against a ledger this Close is about to tear down.
	m.pilotCancel()
	err := m.link.Close()
	for _, conn := range conns {
		_ = conn.Close()
	}
	m.wg.Wait()
	if cerr := closeLedgers(flows); cerr != nil && err == nil {
		err = cerr
	}
	return err
}

// beginClose marks the manager closed and takes everything Close must release,
// reporting whether Close had already run.
func (m *Manager) beginClose() ([]net.Conn, map[string]*ledger.Ledger, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, nil, true
	}
	m.closed = true
	conns := make([]net.Conn, 0, len(m.inflight))
	for conn := range m.inflight {
		conns = append(conns, conn)
	}
	flows := m.flows
	m.inflight = map[net.Conn]struct{}{}
	m.flows = map[string]*ledger.Ledger{}
	m.pilots = map[string]*flowPilot{}
	return conns, flows, false
}

// closeLedgers closes every open flow and reports the first failure.
//
// IT RUNS LAST, after the handlers have been joined. A handler serving an
// announce reaches into a flow's raft handle, so closing the ledgers while one
// was still running would tear that state down underneath it — which is the
// second of the two defects Close's ordering exists to prevent, and the one that
// is only safe because the join step preceded it.
func closeLedgers(flows map[string]*ledger.Ledger) error {
	var err error
	for flow, l := range flows {
		if cerr := l.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("membership: closing the ledger for flow %q failed: %w", flow, cerr)
		}
	}
	return err
}
