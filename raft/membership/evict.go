// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"context"
	"time"

	"github.com/hashicorp/raft"
)

// evictRound runs ONE eviction pass for a flow, on the leader only.
//
// EVICTION IS WHAT MAKES A PLAIN DEPLOYMENT SURVIVABLE. Under the ruled
// ephemeral-identity model every restart is a new member, so a group that never
// removes the old ones wedges permanently — measured on a three-replica
// Deployment, which reached a lone Candidate in a four-member configuration of
// mostly-dead identities after three restarts.
//
// IT KEYS ON ABSENCE FROM THE ORCHESTRATOR'S REGISTRY, NOT ON UNREACHABILITY,
// which is what makes it safe to be more aggressive than autopilot's own cleanup
// — and autopilot's cleanup is off for a measured reason rather than a
// jurisdictional one, recorded on AutopilotConfig.
//
// IT IS PREVENTIVE AND NEVER A RECOVERY MECHANISM. A configuration that has
// already lost quorum cannot be repaired here at all, because removing a member
// is itself a configuration change that needs a quorum to commit.
func (m *Manager) evictRound(ctx context.Context, flow string) {
	l, ok := m.Ledger(flow)
	if !ok {
		return
	}
	r := l.Raft()
	if r.State() != raft.Leader {
		return
	}
	live, err := m.resolveLive(ctx)
	if err != nil {
		m.logger.Warn("skipping an eviction round: the registry did not resolve",
			"flow", flow, "error", err)
		return
	}
	m.evictOne(flow, r, live)
}

// resolveLive resolves the peers address into the FULL live set, this node
// included. Eviction compares against the whole registry, so it must not use the
// peer set the client dials, which has this node stripped out of it.
func (m *Manager) resolveLive(ctx context.Context) ([]string, error) {
	if m.cfg.Peers == "" {
		return nil, nil
	}
	return m.resolve(ctx, m.cfg.Peers)
}

// evictOne removes AT MOST ONE configured member whose address the registry no
// longer lists, under all four bounds.
func (m *Manager) evictOne(flow string, r *raft.Raft, live []string) {
	future := r.GetConfiguration()
	if future.Error() != nil {
		return
	}
	servers := future.Configuration().Servers
	// THE VICTIM IS CHOSEN BEFORE THE BOUNDS ARE APPLIED, so bound 2 can charge
	// the cost this particular removal actually carries. Applying the bounds
	// first meant subtracting a vote from a victim that might hold none, which
	// made a stale NONVOTER un-evictable whenever the voter count equalled the
	// live count — the steady state under ephemeral identity, so stale nonvoters
	// accumulated without bound.
	victim, found := absentMember(servers, live)
	if !found {
		return
	}
	if !m.evictionPermitted(flow, servers, live, victim) {
		return
	}
	// THE REACHABILITY FACT IS TAKEN HERE, after the bounds and before the
	// removal, so it describes the member at the moment it was evicted and is
	// paid only on the path that actually evicts.
	reachable := m.reachableNow(flow, string(victim.Address))
	if err := r.RemoveServer(victim.ID, 0, 0).Error(); err != nil {
		m.logger.Warn("an eviction failed", "flow", flow, "node", string(victim.ID), "error", err)
		return
	}
	m.logger.Warn("evicted a member absent from the registry",
		"flow", flow, "node", string(victim.ID), "address", string(victim.Address),
		"live", len(live), "reachable", reachable)
	m.signals.publish(Signal{
		Kind: SignalPeerEvicted, Flow: flow, Node: string(victim.ID), Since: time.Now(),
		EvictedWhileReachable: reachable,
		ReadmissionExpectedBy: time.Now().Add(2 * m.refreshInterval()),
	})
}

// reachableNow reports whether a member ANSWERS A CONTROL DIAL right now.
//
// IT IS A DIAL AND NOT A READ OF THE REGISTRY, and it has to be. The victim of
// an eviction is by construction absent from the resolution — absentMember
// returns nothing else — so every instrument keyed on that resolution says the
// member is gone, including the peer set and the stats view derived from it.
// The one thing that can distinguish a member the orchestrator has retired from
// a member a right-size wrong-membership resolution merely failed to mention is
// asking the member itself.
//
// THE COST IS ONE DIAL PER EVICTION, NOT PER ROUND. It runs after all four
// bounds have passed and immediately before the removal, so it is paid only on
// the path that is about to change the configuration — at most once per round,
// and in the steady state never. A probe taken before the bounds would be paid
// on every round that finds an absent member and then declines to remove it,
// which is the common case under a rolling update. It is bounded by
// controlReadTimeout like every other control exchange.
func (m *Manager) reachableNow(flow, addr string) bool {
	_, err := m.peers.call(addr, statsRequest{Flows: []string{flow}})
	return err == nil
}

// evictionPermitted applies the bounds that do not depend on which member would
// go. TWO OF THESE EXIST BECAUSE THE FIRST DRAFT'S BOUNDS WERE MEASURED
// INSUFFICIENT: a resolution that was non-empty, contained this node and was a
// strict SUBSET of the live set passed them all and evicted a live follower, and
// across rounds the one-per-round bound compounded rather than capped — taking a
// five-voter group to a single voter in four rounds with every member alive.
func (m *Manager) evictionPermitted(flow string, servers []raft.Server, live []string, victim raft.Server) bool {
	// BOUND 4: never on an empty or failed resolution, and never on one that does
	// not contain this node. A registry that cannot see us is not a registry we
	// should be pruning membership from.
	if len(live) == 0 || !containsAddr(live, m.cfg.Advertise) {
		return false
	}
	// BOUND 1, COMPLETENESS: only act on an answer from the whole deployment.
	// This is what distinguishes an incomplete answer from a genuinely smaller
	// cluster, and it is the bound a strict-subset resolution defeats without it.
	if m.cfg.Expect > 0 && len(live) < m.cfg.Expect {
		m.logger.Debug("skipping an eviction round: the resolution is incomplete",
			"flow", flow, "resolved", len(live), "expect", m.cfg.Expect)
		return false
	}
	// BOUND 2, NEVER BELOW THE LIVE COUNT. This closes the right-size-wrong-
	// membership case bound 1 alone does not: a resolution of the RIGHT SIZE but
	// the WRONG membership passes completeness while still naming a live member
	// as absent. It also subsumes a separate quorum bound, because a configuration
	// never reduced below the number of live instances always has a quorum
	// available among live members.
	//
	// THE VOTE IS SUBTRACTED ONLY WHEN THE VICTIM HOLDS ONE. Removing a nonvoter
	// costs the group no voting power, so charging it a vote refuses evictions
	// that were never a risk — and specifically makes a stale nonvoter permanently
	// un-evictable in the steady state, which is where they accumulate.
	//
	// AND THE CONSEQUENCE THAT BUYS, stated here because a reader of this code
	// should see the trade and not only the fix: a LIVE nonvoter can be evicted
	// under a right-size wrong-membership resolution, where the unconditional
	// subtraction happened to refuse it. The trade is deliberate and bounded — no
	// vote is lost, the configuration never falls below the live count or below
	// quorum, and the joiner re-announces on the next re-place round, which runs
	// on every node on this same ticker and fires exactly when a node is absent
	// from its flow's committed configuration — and it replaces a strictly worse
	// state, stale nonvoters accumulating without bound.
	//
	// THAT LAST CLAUSE WAS FALSE WHEN IT WAS FIRST WRITTEN, and it is recorded
	// here rather than quietly corrected. The sentence read "the joiner
	// re-announces on its next probe", and there was no next probe: the announce
	// path had exactly two one-shot callers, Start and joinFlow, and nothing
	// periodic ever called it again, so an evicted member stayed out until its
	// process restarted and the recovery this trade is justified by could not
	// occur at all. The re-place round exists to make the sentence true rather
	// than to make it read better. A reader of this file should be able to see
	// that a disclosed trade was once justified by a mechanism that did not
	// exist, because that is the failure this file is most likely to repeat.
	cost := 0
	if victim.Suffrage == raft.Voter {
		cost = 1
	}
	if voters(servers)-cost < len(live) {
		m.logger.Debug("skipping an eviction round: it would shrink the configuration below the live count",
			"flow", flow, "victim", string(victim.ID), "suffrage", victim.Suffrage.String(),
			"voters", voters(servers), "resolved", len(live))
		return false
	}
	return true
}

// absentMember reports the first configured member whose address the registry
// does not list. BOUND 3, ONE PER ROUND, is this returning at most one.
func absentMember(servers []raft.Server, live []string) (raft.Server, bool) {
	for _, server := range servers {
		if !containsAddr(live, string(server.Address)) {
			return server, true
		}
	}
	return raft.Server{}, false
}

// voters counts the members that carry a vote.
func voters(servers []raft.Server) int {
	count := 0
	for _, server := range servers {
		if server.Suffrage == raft.Voter {
			count++
		}
	}
	return count
}

// containsAddr reports whether addr is in the resolved set.
func containsAddr(live []string, addr string) bool {
	for _, item := range live {
		if item == addr {
			return true
		}
	}
	return false
}
