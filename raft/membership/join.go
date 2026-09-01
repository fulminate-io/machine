// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/ledger"
)

const (
	// maxJoinRedirects bounds a redirect chain so a cycle of stale leader
	// answers cannot loop forever.
	maxJoinRedirects = 4
	// placeRetryInterval is how long a node waits between attempts to settle a
	// flow it has not yet placed.
	placeRetryInterval = 200 * time.Millisecond
)

// Start opens this node's flows and settles each one against the cluster.
//
// THE LEDGER IS OPENED FIRST, DELIBERATELY. A crash between opening a ledger and
// announcing it leaves a local ledger with no group membership, which the next
// Start re-probes and the ledger's own state makes idempotent. The reverse
// order — announced but not yet open — would make this node a staged member that
// cannot receive replication, which is the worse of the two intermediate states.
func (m *Manager) Start(ctx context.Context) error {
	for _, flow := range m.cfg.Flows {
		l, err := m.openFlow(flow)
		if err != nil {
			return err
		}
		m.addFlow(flow, l)
	}
	if err := m.refreshPeers(ctx); err != nil {
		return err
	}
	for _, flow := range m.cfg.Flows {
		if err := m.placeFlow(ctx, flow); err != nil {
			return err
		}
		m.superviseFlow(flow)
	}
	return nil
}

// openFlow opens ONE flow's ledger and refuses one whose raft server id is not
// this manager's node id.
//
// IT EXISTS BECAUSE THERE ARE TWO OPEN SITES. Start opens the flows this node
// begins with and joinFlow opens the ones it later takes on; a guard written at
// one of them only would leave the other admitting exactly what the first
// refuses, which is the same two-call-sites-computing-one-thing drift SetFlows
// is written to avoid on the flow-set half.
//
// THE REFUSAL NAMES BOTH VALUES because an operator supplied them as two
// separate fields, and a refusal naming neither is one they cannot act on.
func (m *Manager) openFlow(flow string) (*ledger.Ledger, error) {
	l, err := m.cfg.Open(flow)
	if err != nil {
		return nil, fmt.Errorf("membership: opening the ledger for flow %q failed: %w", flow, err)
	}
	id := l.LocalID()
	if id == m.cfg.Node {
		return l, nil
	}
	// THE REFUSED LEDGER IS CLOSED HERE OR BY NOBODY. It never reaches m.flows, so
	// the manager's own Close will not find it, and an open ledger holds the
	// flow's group id bound on the shared mux — a leaked one turns the next
	// attempt at this flow into a bind refusal naming an unrelated cause.
	if cerr := l.Close(); cerr != nil {
		m.logger.Warn("closing a ledger refused for a diverged identity failed",
			"flow", flow, "error", cerr)
	}

	return nil, fmt.Errorf("%w: flow %q ledger reports %q, Config.Node is %q",
		ErrIdentityDiverged, flow, id, m.cfg.Node)
}

// superviseFlow starts the goroutine that runs a flow's autopilot for exactly as
// long as this node leads that flow.
func (m *Manager) superviseFlow(flow string) {
	l, ok := m.Ledger(flow)
	if !ok {
		return
	}
	pilot := newFlowPilot(m, flow, l.Raft())
	m.mu.Lock()
	m.pilots[flow] = pilot
	m.mu.Unlock()
	m.wg.Add(2)
	go m.supervise(flow, pilot, l.Raft())
	// The membership watcher runs on EVERY node, unlike the promoter above,
	// because a follower that could not see a configuration change could not act
	// on one.
	go m.watchMembership(flow, pilot)
}

// supervise starts and stops a flow's autopilot as this node gains and loses
// leadership of it, and joins autopilot's goroutines on the way out.
//
// LEADERSHIP IS OBSERVED WITHOUT HANDING RAFT A SECOND CHANNEL. The ledger
// already owns raft's NotifyCh and drains it in a loop that must never park, so
// this reads the flow's observable state rather than registering a competing
// notification channel that would steal those events.
func (m *Manager) supervise(flow string, pilot *flowPilot, r *raft.Raft) {
	defer m.wg.Done()
	ticker := time.NewTicker(leadershipPollInterval)
	defer ticker.Stop()
	evictions := time.NewTicker(orDuration(m.cfg.Autopilot.ReconcileInterval, defaultEvictInterval))
	defer evictions.Stop()
	running := false
	for {
		running = m.reconcileLeadership(flow, pilot, r, running)
		select {
		case <-m.pilotCtx.Done():
			if running {
				<-pilot.stop()
			}
			return
		case <-pilot.done:
			if running {
				<-pilot.stop()
			}
			return
		case <-evictions.C:
			m.evictRound(m.pilotCtx, flow)
		case <-ticker.C:
		}
	}
}

// reconcileLeadership starts or stops the flow's autopilot to match this node's
// current leadership of it, and reports whether it is now running.
func (m *Manager) reconcileLeadership(flow string, pilot *flowPilot, r *raft.Raft, running bool) bool {
	leading := r.State() == raft.Leader
	switch {
	case leading && !running:
		pilot.start(m.pilotCtx)
		m.logger.Info("started a flow's promoter on acquiring leadership", "flow", flow)
		return true
	case !leading && running:
		<-pilot.stop()
		m.logger.Info("stopped a flow's promoter on losing leadership", "flow", flow)
		return false
	default:
		return running
	}
}

// refreshPeers resolves the one configured address into the instance set and
// tells the client who to ask. With no Peers address the mechanism is absent and
// a single-instance run stays zero-config.
func (m *Manager) refreshPeers(ctx context.Context) error {
	if m.cfg.Peers == "" {
		m.peers.setMembership(nil, m.cfg.Flows)
		return nil
	}
	addrs, err := m.resolve(ctx, m.cfg.Peers)
	if err != nil {
		return err
	}
	m.peers.setMembership(withoutSelf(addrs, m.cfg.Advertise), m.cfg.Flows)
	return nil
}

// withoutSelf drops this node's own advertised address from a resolved set, so a
// node never announces to itself and never counts itself twice.
func withoutSelf(addrs []string, self string) []string {
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if addr != self {
			out = append(out, addr)
		}
	}
	return out
}

// placeFlow settles ONE flow, retrying until ctx expires.
//
// A NODE THAT IS NOT THE LOWEST ID WAITS RATHER THAN CREATING, and waiting is
// the correct action rather than a stall: the lowest id will create the group
// and the next probe finds it. When ctx expires with the flow still unplaced
// that is an ERROR NAMING THE FLOW — never a silent self-bootstrap, because two
// workers each creating a one-voter group for the same flow produce two logs
// that can never merge.
func (m *Manager) placeFlow(ctx context.Context, flow string) error {
	var why string
	for {
		placed, reason := m.placeOnce(ctx, flow)
		if placed {
			return nil
		}
		why = reason
		m.logger.Info("waiting to place a flow", "flow", flow, "reason", reason)
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: flow %q: %s", ErrFlowUnplaced, flow, why)
		case <-time.After(placeRetryInterval):
		}
	}
}

// placeOnce makes one attempt at settling a flow and reports why it did not.
func (m *Manager) placeOnce(ctx context.Context, flow string) (bool, string) {
	if m.alreadyMember(flow) {
		return true, ""
	}
	answers := m.announceRound(ctx, flow)
	for _, reply := range answers {
		if contains(reply.Staged, flow) {
			return true, ""
		}
	}
	return m.createIfRuled(flow, answers)
}

// alreadyMember reports whether this node's ledger for the flow is already in a
// formed group. A ledger that survived a restart, or one this node created, is
// already placed and must not be announced again.
func (m *Manager) alreadyMember(flow string) bool {
	l, ok := m.Ledger(flow)
	if !ok {
		return false
	}
	future := l.Raft().GetConfiguration()
	if future.Error() != nil {
		return false
	}
	return len(future.Configuration().Servers) > 0
}

// announceRound announces this node to every peer for one flow, following each
// redirect to the address it named.
//
// THE CHAIN IS BOUNDED. A redirect names the address raft believes leads the
// flow, and a stale answer can name a node that has since lost leadership, so
// the walk stops after maxJoinRedirects hops rather than following a cycle.
func (m *Manager) announceRound(ctx context.Context, flow string) map[string]announceReply {
	answers := make(map[string]announceReply)
	targets := m.peers.addresses()
	for hop := 0; hop <= maxJoinRedirects && len(targets) > 0; hop++ {
		next := make([]string, 0, len(targets))
		for _, addr := range targets {
			reply, err := m.announceTo(ctx, addr, flow)
			if err != nil {
				m.logger.Warn("an announce did not reach a peer", "peer", addr, "flow", flow, "error", err)
				continue
			}
			answers[addr] = reply
			if to, ok := reply.Redirects[flow]; ok && answers[to].Node == "" {
				next = append(next, to)
			}
		}
		targets = next
	}
	return answers
}

// announceTo sends one announce to one address.
func (m *Manager) announceTo(ctx context.Context, addr, flow string) (announceReply, error) {
	if err := ctx.Err(); err != nil {
		return announceReply{}, err
	}
	reply, err := m.peers.call(addr, announce{
		Node:    m.cfg.Node,
		Address: m.cfg.Advertise,
		Flows:   []string{flow},
	})
	if err != nil {
		return announceReply{}, err
	}
	answer, ok := reply.(*announceReply)
	if !ok {
		return announceReply{}, fmt.Errorf("%w: an announce was answered with %T", ErrUnknownMessage, reply)
	}
	return *answer, nil
}

// createIfRuled applies the count-and-lowest-id rule and reports whether the
// flow is now placed.
//
// THE RULE IS NEVER AN INFERENCE. Nothing here concludes "nobody answered, so I
// must be first": a node that has not heard from Config.Expect instances has
// simply not finished looking, and creating on that evidence is how two logs
// that can never merge get made.
func (m *Manager) createIfRuled(flow string, answers map[string]announceReply) (bool, string) {
	answered := len(answers) + 1
	if m.cfg.Expect > 0 && answered < m.cfg.Expect {
		return false, fmt.Sprintf("no instance hosts it and only %d of %d have answered", answered, m.cfg.Expect)
	}
	if lowest := m.lowestID(answers); !lowest {
		return false, fmt.Sprintf("no instance hosts it, %d have answered, and this node is not the lowest id",
			answered)
	}
	if err := m.createFlow(flow); err != nil {
		return false, fmt.Sprintf("creating it failed: %v", err)
	}
	m.logger.Info("created a flow's group under the count-and-lowest-id rule",
		"flow", flow, "node", m.cfg.Node, "answered", answered)
	return true, ""
}

// lowestID reports whether this node holds the lowest id among those that
// answered, which is what makes the creation decision single-writer without a
// lock anyone has to hold.
func (m *Manager) lowestID(answers map[string]announceReply) bool {
	ids := make([]string, 0, len(answers)+1)
	ids = append(ids, m.cfg.Node)
	for _, reply := range answers {
		if reply.Node != "" {
			ids = append(ids, reply.Node)
		}
	}
	sort.Strings(ids)
	return ids[0] == m.cfg.Node
}

// createFlow bootstraps this node as the single voter of a new group.
func (m *Manager) createFlow(flow string) error {
	l, ok := m.Ledger(flow)
	if !ok {
		return fmt.Errorf("membership: flow %q has no open ledger to create a group in", flow)
	}
	return l.Raft().BootstrapCluster(raft.Configuration{Servers: []raft.Server{{
		Suffrage: raft.Voter,
		ID:       raft.ServerID(m.cfg.Node),
		Address:  raft.ServerAddress(m.cfg.Advertise),
	}}}).Error()
}

// contains reports whether needle is in haystack.
func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
