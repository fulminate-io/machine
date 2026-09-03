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
	m.peers.setFlows(m.cfg.Flows)
	// START STILL REFUSES ON A RESOLVE ERROR. An EMPTY but successful resolution
	// is tolerated and always was: the place loop's count-and-lowest-id rule is
	// what stops an empty answer being read as "nobody is out there", and under
	// the per-round refresh below the next round finds the peers that were not
	// published yet.
	if err := m.refreshPeers(ctx); err != nil {
		return err
	}
	m.stampResolved()
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
			// THE REFRESH RUNS FIRST AND IT RUNS ON EVERY NODE, INCLUDING THE
			// LEADER, and that ordering is the whole reason it is here rather
			// than only inside the announce path. p.addrs has THREE consumers,
			// not one: the announce round dials it, the stats round polls it,
			// and the promoter reads the stats round's view through
			// FetchServerStats. A leader is always present in its own
			// configuration, so it never enters the announce path at all — and
			// the leader is precisely the node that runs the promoter. Refreshed
			// only from announceRound, the one node whose peer set decides
			// whether a replacement is ever promoted would stay frozen for the
			// life of the process.
			m.refreshTargets(m.pilotCtx, flow)
			m.evictRound(m.pilotCtx, flow)
			// THE RE-PLACE ROUND RUNS ON EVERY NODE, unlike the eviction round
			// beside it, which returns immediately unless this node leads. A
			// node that has fallen out of a configuration is by definition not
			// the one that removed it.
			m.replaceRound(m.pilotCtx, flow)
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
//
// IT RESOLVES THROUGH resolveLive, WHICH IS THE EVICTION ROUND'S OWN CALL, so
// the announce path and the eviction path share ONE refresh policy and one
// resolver. They did not before: this side resolved exactly once, inside Start,
// and held that answer for the life of the process while the leader's side
// re-resolved every round. Measured consequence of that asymmetry: a replacement
// pod announced to the previous generation's addresses for the whole of its life
// and never reached the live group.
func (m *Manager) refreshPeers(ctx context.Context) error {
	addrs, err := m.resolveLive(ctx)
	if err != nil {
		return err
	}
	m.peers.setAddresses(withoutSelf(addrs, m.cfg.Advertise))
	return nil
}

// refreshInterval is how often the announce path re-resolves. IT IS THE EVICTION
// ROUND'S OWN CADENCE, read from the same expression supervise reads, because
// one refresh policy means one number as much as it means one resolver.
func (m *Manager) refreshInterval() time.Duration {
	return orDuration(m.cfg.Autopilot.ReconcileInterval, defaultEvictInterval)
}

// refreshDue reports whether the announce target set is older than the refresh
// interval. The place loop retries every placeRetryInterval, which is fifty
// times faster than the refresh, so without this the registry would be resolved
// fifty times per interval per unplaced flow.
func (m *Manager) refreshDue() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resolvedAt.IsZero() || time.Since(m.resolvedAt) >= m.refreshInterval()
}

// stampResolved records that a resolution SUCCEEDED.
//
// IT IS STAMPED ON SUCCESS AND NEVER ON THE ATTEMPT. Stamping the attempt would
// make a failed resolution hold the interval open, so a registry that came back
// half a second later would go unread for the rest of it.
func (m *Manager) stampResolved() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resolvedAt = time.Now()
}

// refreshTargets re-resolves the peer set when it is due, and reports whether
// the caller's round may proceed.
//
// IT WRITES THROUGH peers.setAddresses INTO p.addrs, NEVER INTO A LOCAL SLICE,
// and that is a correctness requirement rather than a style choice. p.addrs is
// read by three consumers: announceRound dials it, peers.round polls it for
// stats, and flowPilot.FetchServerStats projects that view into autopilot,
// which omits any server with no entry and leaves it at term zero, permanently
// unhealthy and never promoted. A refresh that resolved into a per-round local
// slice would fix the announce path and leave promotion broken forever.
//
// A FAILED RESOLUTION SKIPS THE ROUND RATHER THAN ANNOUNCING TO THE PREVIOUS
// ANSWER, which is the same choice evictRound makes on the same failure and for
// the same reason: acting on a set the registry no longer vouches for is the
// defect, not the mitigation. The place loop retries in placeRetryInterval.
func (m *Manager) refreshTargets(ctx context.Context, flow string) bool {
	if !m.refreshDue() {
		return true
	}
	if err := m.refreshPeers(ctx); err != nil {
		m.logger.Warn("skipping an announce round: the registry did not resolve",
			"flow", flow, "error", err)
		return false
	}
	m.stampResolved()
	return true
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

// alreadyMember reports whether this node is IN its flow's committed
// configuration. A ledger that survived a restart, or one this node created, is
// already placed and must not be announced again.
//
// IT ASKS WHETHER THIS NODE IS THERE, NOT WHETHER THE CONFIGURATION IS
// NON-EMPTY, and the difference is the whole of the re-place round below. A
// removed FOLLOWER is told nothing it acts on — leaveFlow records the
// measurement — so a node evicted from a group it can still see reads a
// perfectly populated configuration that no longer names it. Under the
// non-empty test that node is "already placed" forever.
func (m *Manager) alreadyMember(flow string) bool {
	l, ok := m.Ledger(flow)
	if !ok {
		return false
	}
	return confirmStaged(l.Raft(), raft.ServerID(m.cfg.Node)) == nil
}

// replaceRound re-announces a flow this node has fallen out of, on the same
// ticker the eviction round runs on.
//
// THIS IS THE NEXT PROBE eviction's live-nonvoter residual is justified by. That
// justification was false when it was written: placeFlow had exactly two
// one-shot callers, Start and joinFlow, and nothing periodic ever called the
// announce path again, so an evicted member stayed out until its process
// restarted. Re-resolving the announce target set does not repair that on its
// own — it makes the targets current, not the probe recurrent — so the probe is
// here.
//
// IT COSTS ONE LOCAL CONFIGURATION READ PER FLOW PER ROUND in the steady state
// and dials nothing: a node that is in its configuration returns at
// alreadyMember without touching the network.
//
// THREE TRANSIENT WINDOWS, ENUMERATED, because each one makes this round
// announce when nothing is wrong. A joiner staged on the LEADER cannot see
// itself until the configuration entry reaches its own log, so it re-announces
// once per round for the length of that replication lag; the announce is
// idempotent, because a leader staging an already-present member has no effect.
// A node whose GetConfiguration errors is treated as not-present and announces,
// on the same idempotence, and the next round re-reads. And the removal DOES
// reach a removed node's own log — measured converging within two seconds — so
// this round sees an eviction after that lag rather than never.
func (m *Manager) replaceRound(ctx context.Context, flow string) {
	if _, ok := m.Ledger(flow); !ok {
		return
	}
	if m.alreadyMember(flow) {
		return
	}
	placed, reason := m.placeOnce(ctx, flow)
	if placed {
		m.logger.Warn("re-placed a flow this node had fallen out of", "flow", flow, "node", m.cfg.Node)
		return
	}
	m.logger.Warn("a flow this node has fallen out of is not re-placed yet",
		"flow", flow, "node", m.cfg.Node, "reason", reason)
}

// announceRound announces this node to every peer for one flow, following each
// redirect to the address it named.
//
// THE CHAIN IS BOUNDED. A redirect names the address raft believes leads the
// flow, and a stale answer can name a node that has since lost leadership, so
// the walk stops after maxJoinRedirects hops rather than following a cycle.
func (m *Manager) announceRound(ctx context.Context, flow string) map[string]announceReply {
	answers := make(map[string]announceReply)
	if !m.refreshTargets(ctx, flow) {
		return answers
	}
	targets := m.peers.addresses()
	for hop := 0; hop <= maxJoinRedirects && len(targets) > 0; hop++ {
		next := make([]string, 0, len(targets))
		for _, addr := range targets {
			reply, err := m.announceTo(ctx, addr, flow)
			if err != nil {
				m.logger.Warn("an announce did not reach a peer", "peer", addr, "flow", flow, "error", err)
				continue
			}
			// A FOREIGN-GENERATION ANSWER IS NOT AN ANSWER. It is dropped here,
			// once, rather than filtered at each of its three readers: it must
			// not count toward Config.Expect, because a node that has heard only
			// from the outgoing deployment has not finished looking; it must not
			// enter the lowest-id comparison, because the creation decision is
			// single-writer only among nodes that can form the group; and it
			// must not be followed as a redirect to a leader that is leaving.
			//
			// COUNTING IT TOWARD Expect WHILE EXCLUDING IT FROM lowestID IS THE
			// SPECIFIC WRONG-BUT-REASONABLE VARIANT: three new pods would each
			// satisfy Expect from the outgoing deployment alone and each find
			// itself the lowest id among a set of one, producing three
			// single-voter groups that can never merge — precisely the
			// split-brain createIfRuled exists to prevent.
			if reply.Generation != m.cfg.Generation {
				m.logger.Warn("an announce reached a peer of another deployment generation",
					"peer", addr, "flow", flow,
					"generation", m.cfg.Generation, "peer_generation", reply.Generation)
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
		Node:       m.cfg.Node,
		Address:    m.cfg.Advertise,
		Flows:      []string{flow},
		Generation: m.cfg.Generation,
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
