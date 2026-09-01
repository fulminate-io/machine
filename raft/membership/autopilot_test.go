package membership

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/ledger"
)

// promotionTuning makes the promoter observable inside a test window. The
// thresholds themselves are the production ones in spirit — a stabilization
// period, a trailing-log ceiling — only scaled down so a run takes seconds
// rather than the ten seconds the shipped stabilization time would need.
var promotionTuning = AutopilotTuning{
	LastContactThreshold:    2 * time.Second,
	MaxTrailingLogs:         10,
	MinQuorum:               1,
	ServerStabilizationTime: 200 * time.Millisecond,
	UpdateInterval:          100 * time.Millisecond,
	ReconcileInterval:       100 * time.Millisecond,
}

// reportsSelfAs installs what a node answers about one of its own flows, which
// is exactly what its peers learn about it over the control channel and what the
// promoter reads.
func reportsSelfAs(node *clusterNode, stats func() FlowStats) {
	node.mgr.SetLocalStats(func(string) (FlowStats, bool) { return stats(), true })
}

// growLog appends entries until the leader's log is comfortably longer than
// MaxTrailingLogs.
//
// WITHOUT IT THE TRAILING ARM IS VACUOUS, and that was MEASURED rather than
// anticipated: a freshly bootstrapped single-voter group sits at a last index of
// a handful, so a joiner reporting itself five hundred behind clamps to zero and
// zero plus a ten-log allowance is still ahead of a leader at index four. The
// arm reported "promoted anyway" against an implementation that was in fact
// conditional. The threshold has to be EXPRESSIBLE before a test can assert it.
func growLog(t *testing.T, node *clusterNode, flow string, entries int) {
	t.Helper()
	l, ok := node.mgr.Ledger(flow)
	if !ok {
		t.Fatalf("node %s does not host flow %q", node.id, flow)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := 0; i < entries; i++ {
		entry := ledger.Entry{Kind: ledger.KindSet, Path: fmt.Sprintf("grow/%d", i), Value: []byte("v")}
		if _, err := l.Append(ctx, entry); err != nil {
			t.Fatalf("growing the log: %v", err)
		}
	}
}

func TestAStagedJoinerIsPromotedOnceCaughtUpAndNotWhileBehind(t *testing.T) {
	// The two arms share EVERY other input: the same tuning, the same cluster
	// shape, the same staging path, the same window. Only what the joiner reports
	// about its own progress differs.
	run := func(t *testing.T, behind uint64) (raft.ServerSuffrage, bool) {
		t.Helper()
		leader := newClusterNode(t, "a-leader", []string{"alpha"}, 0)
		leader.mgr.cfg.Autopilot = promotionTuning
		leader.start(t)
		leader.awaitLeader(t, "alpha")
		leaderRaft := leader.raftFor(t, "alpha")
		growLog(t, leader, "alpha", 60)
		if floor := promotionTuning.MaxTrailingLogs + 10; leaderRaft.LastIndex() <= floor {
			t.Fatalf("CONTROL FAILED: the leader is at index %d, not clear of the %d-log trailing allowance, "+
				"so a joiner cannot be far enough behind for the threshold to mean anything",
				leaderRaft.LastIndex(), floor)
		}
		reportsSelfAs(leader, func() FlowStats {
			return FlowStats{
				Term: leaderRaft.CurrentTerm(), LastIndex: leaderRaft.LastIndex(),
				LastContact: time.Millisecond, Voter: true, Leader: true,
			}
		})

		joiner := newClusterNode(t, "b-joiner", []string{"alpha"}, 2)
		joiner.peering(leader.addr)
		// THE ONLY DIFFERENCE BETWEEN THE ARMS. The joiner reports its own
		// progress relative to the leader's, either level with it or behind it by
		// more than MaxTrailingLogs.
		reportsSelfAs(joiner, func() FlowStats {
			index := leaderRaft.LastIndex()
			if index > behind {
				index -= behind
			} else {
				index = 0
			}
			return FlowStats{
				Term: leaderRaft.CurrentTerm(), LastIndex: index, LastContact: time.Millisecond,
			}
		})
		joiner.start(t)
		// The leader has to know where to ask; discovery drives this in
		// production and the test drives it directly.
		leader.mgr.SetPeers([]string{joiner.addr}, []string{"alpha"})

		// A LEG THAT MAKES EITHER ANSWER MEAN SOMETHING: both arms must have
		// formed a committed configuration carrying the joiner at all, so a group
		// that never formed cannot read as "not promoted".
		if _, ok := suffrageIn(t, leaderRaft, "b-joiner"); !ok {
			t.Fatal("CONTROL FAILED: the joiner never reached the committed configuration, so this arm " +
				"observes nothing about promotion")
		}
		promoted := awaitSuffrage(t, leaderRaft, "b-joiner", raft.Voter, 8*time.Second)
		got, _ := suffrageIn(t, leaderRaft, "b-joiner")
		return got, promoted
	}

	t.Run("caught up is promoted", func(t *testing.T) {
		got, promoted := run(t, 0)
		if !promoted {
			t.Fatalf("a caught-up staged joiner was not promoted within the window; it is still %v: "+
				"promotion is not happening at all", got)
		}
	})

	// THE CONTROL ARM. An integration that promoted unconditionally — or one
	// whose stats source silently returned nothing while some other code promoted
	// — passes the first arm alone. This is what makes the first arm mean
	// "promotion is conditional" rather than merely "promotion happens".
	t.Run("trailing is not promoted", func(t *testing.T) {
		got, promoted := run(t, 500)
		if promoted {
			t.Fatal("a joiner reporting itself far behind the leader was promoted anyway: " +
				"promotion is unconditional")
		}
		if got != raft.Nonvoter {
			t.Fatalf("the trailing joiner is recorded at suffrage %v, want Nonvoter", got)
		}
	})
}
