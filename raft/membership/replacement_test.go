// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// caughtUpWith makes a node report itself level with the leader, which is what a
// healthy member of a formed group reports and what the promoter reads.
func caughtUpWith(node *clusterNode, leaderRaft *raft.Raft) {
	reportsSelfAs(node, func() FlowStats {
		return FlowStats{
			Term: leaderRaft.CurrentTerm(), LastIndex: leaderRaft.LastIndex(),
			LastContact: time.Millisecond,
		}
	})
}

// TestAReplacementIsPromotedAndTheDeadVoterEvictedWithoutHandFedPeers observes
// the whole composition a pod replacement puts in motion, on the LEADER, with
// nothing hand-fed.
//
// THE LEADER IS THE SUBJECT AND THAT IS THE POINT. A leader is always present in
// its own committed configuration, so it never enters the announce path — and it
// is the node that runs the promoter. The peer set it holds has THREE consumers:
// the announce round dials it, the stats round polls it, and FetchServerStats
// projects that view into autopilot, which omits any server it has no entry for
// and leaves it at term zero, permanently unhealthy. A refresh that fired only
// on the announce path would leave this node frozen for the life of the process.
//
// THE CHAIN UNDER TEST, in order: the registry reports the replacement -> the
// leader's refresh round writes it into the peer set -> the stats round polls it
// -> the promoter sees a caught-up member and promotes it to Voter -> the voter
// count rises -> eviction's bound 2 stops refusing -> the dead voter is removed.
// Every link is another subsystem obeying its own contract; the composition is
// what strands a replacement when the first link is missing.
//
// THE SHAPE IS THE MEASURED ONE: three voters, one of them replaced. Two live
// voters are required rather than decorative — a single live leader beside one
// dead voter has no quorum, cannot commit, and steps down, which is a different
// failure entirely and was the first draft of this test.
func TestAReplacementIsPromotedAndTheDeadVoterEvictedWithoutHandFedPeers(t *testing.T) {
	leader := newClusterNode(t, "a-leader", []string{"alpha"}, 0)
	leader.mgr.cfg.Autopilot = promotionTuning
	leader.start(t)
	leader.awaitLeader(t, "alpha")
	leaderRaft := leader.raftFor(t, "alpha")
	reportsSelfAs(leader, func() FlowStats {
		return FlowStats{
			Term: leaderRaft.CurrentTerm(), LastIndex: leaderRaft.LastIndex(),
			LastContact: time.Millisecond, Voter: true, Leader: true,
		}
	})

	// THE SECOND LIVE VOTER. It joins for real and is then given a vote, which is
	// the state a promoted member reaches and the state the eviction bounds are
	// written against. It is what keeps a quorum available once a voter dies.
	live := newClusterNode(t, "b-live", []string{"alpha"}, 2)
	live.mgr.cfg.Autopilot = promotionTuning
	live.peering(leader.addr)
	caughtUpWith(live, leaderRaft)
	live.start(t)
	addStaleVoter(t, leaderRaft, "b-live", live.addr)
	growLog(t, leader, "alpha", 60)

	// THE POD THAT DIED: a voter at an address nothing answers on. Planted rather
	// than run, because what matters is that the configuration carries a voter
	// the registry no longer lists.
	dead := deadPeerAddress(t)
	addStaleVoter(t, leaderRaft, "c-dead", dead)

	// THE POD THAT REPLACED IT. It joins through the real announce path and
	// reports itself caught up, which is what a healthy replacement does.
	replacement := newClusterNode(t, "d-new", []string{"alpha"}, 3)
	replacement.mgr.cfg.Autopilot = promotionTuning
	replacement.peering(leader.addr)
	caughtUpWith(replacement, leaderRaft)
	replacement.start(t)

	if got, ok := suffrageIn(t, leaderRaft, "d-new"); !ok || got != raft.Nonvoter {
		t.Fatalf("CONTROL FAILED: the replacement is %v present=%v, want a staged Nonvoter; "+
			"without a staged replacement this test observes nothing about promotion", got, ok)
	}
	t.Logf("before the registry reports the replacement, the configuration is %v",
		memberIDs(t, leaderRaft))

	// THE KNOWN-POSITIVE, SAME INSTRUMENT, TAKEN BEFORE THE REGISTRY MOVES: the
	// leader's stats view carries NO entry for the replacement, because its peer
	// set is empty. This is the state the promoter starves in, and reading it
	// through Manager.Stats — the exact map FetchServerStats projects — is what
	// makes the after-reading below mean something.
	if _, present := leader.mgr.Stats("alpha")[replacement.addr]; present {
		t.Fatalf("KNOWN-POSITIVE FAILED: the leader already has stats for the replacement before the "+
			"registry reports it, so this test cannot observe the refresh supplying them; view is %v",
			leader.mgr.Stats("alpha"))
	}
	t.Log("the leader's stats view carries no entry for the replacement while its peer set is empty")

	// THE POD REPLACEMENT, EXPRESSED THE ONLY WAY A DEPLOYMENT EXPRESSES IT: the
	// registry now resolves to the live instances. NOTHING HAND-FEEDS THE PEER
	// SET. The landed promotion test drives SetPeers directly with a comment
	// saying discovery does this in production; this test refuses that shortcut,
	// because whether discovery actually does it is the thing under measurement.
	leader.mgr.cfg.Peers = "peers.invalid:0"
	leader.mgr.cfg.Expect = 3
	leader.mgr.resolve = func(context.Context, string) ([]string, error) {
		return []string{leader.addr, live.addr, replacement.addr}, nil
	}

	// LINK ONE: the refresh round writes the registry's answer into the peer set
	// the stats round polls.
	if !awaitStats(t, leader.mgr, "alpha", replacement.addr, 20*time.Second) {
		got, _ := suffrageIn(t, leaderRaft, "d-new")
		// THE WHOLE STRANDED COMPOSITION IS REPORTED HERE, not just the missing
		// link, because the point of this gate is that each subsystem is obeying
		// its own contract while the composition strands the datum.
		t.Fatalf("the leader never acquired stats for the replacement at %s: its peer set is %v and its "+
			"stats view is %v — the refresh never reached the field the promoter reads through. "+
			"Downstream, as measured in the same run: the replacement is still %v, the configuration is "+
			"%v with %d voters, and eviction's bound 2 refuses the dead voter every round",
			replacement.addr, leader.mgr.peers.addresses(), leader.mgr.Stats("alpha"),
			got, memberIDs(t, leaderRaft), voters(configOf(t, leaderRaft)))
	}
	t.Log("the refresh round put the replacement into the peer set and the stats round polled it")

	// LINK TWO: the promoter promotes a caught-up member to Voter.
	if !awaitSuffrage(t, leaderRaft, "d-new", raft.Voter, 30*time.Second) {
		got, _ := suffrageIn(t, leaderRaft, "d-new")
		t.Fatalf("the replacement never reached Voter; it is still %v with configuration %v — "+
			"the voter count cannot rise and eviction's bound 2 can never permit the removal",
			got, memberIDs(t, leaderRaft))
	}
	t.Logf("the replacement reached Voter; the configuration is now %v", memberIDs(t, leaderRaft))

	// LINK THREE: with the voter count risen, the eviction round stops refusing.
	if !awaitAbsent(t, leaderRaft, "c-dead", 30*time.Second) {
		t.Fatalf("the dead voter was never evicted; the configuration is %v — bound 2 is still refusing "+
			"because the voter count did not rise", memberIDs(t, leaderRaft))
	}
	t.Logf("the dead voter was evicted; the configuration is %v", memberIDs(t, leaderRaft))
}

// awaitStats waits for a peer's stats to appear in a manager's view for a flow.
func awaitStats(t *testing.T, m *Manager, flow, addr string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, present := m.Stats(flow)[addr]; present {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// configOf returns a raft group's committed configuration servers.
func configOf(t *testing.T, r *raft.Raft) []raft.Server {
	t.Helper()
	future := r.GetConfiguration()
	if err := future.Error(); err != nil {
		t.Fatalf("GetConfiguration: %v", err)
	}
	return future.Configuration().Servers
}
