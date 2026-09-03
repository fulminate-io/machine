// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package recovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/ledger"
	machine "github.com/whitaker-io/machine/v4"
)

// TestTheDetectorParksOnLeadershipAndNamesTheRootsSentinel drives the two halves of
// the seam the root's resume loop rides: the refusal it can NAME, and the wait it
// does with it.
func TestTheDetectorParksOnLeadershipAndNamesTheRootsSentinel(t *testing.T) {
	nodes := newGroup(t, "alpha", 3)
	leader := awaitLeader(t, nodes)
	follower := followerOf(t, nodes, leader)

	stable := newView(nodes[0].id, nodes[1].id, nodes[2].id)

	// ARM 1: the refusal a FOLLOWER returns carries BOTH sentinels. The root module
	// may not import the ledger, so a refusal wrapping only the ledger's is a
	// refusal the machine cannot tell from a disk failure — it would exit.
	followerDetector := New(follower.ledger, stable, "alpha")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := followerDetector.Orphans(ctx, "alpha")
	t.Logf("ARM 1 (follower): Orphans err=%v", err)
	if !errors.Is(err, ledger.ErrNotLeader) {
		t.Fatalf("ARM 1: the refusal does not wrap ledger.ErrNotLeader: %v", err)
	}
	if !errors.Is(err, machine.ErrNotLeader) {
		t.Fatalf("ARM 1: the refusal does not wrap machine.ErrNotLeader, so the root module "+
			"cannot tell it from a real failure and the resume loop exits: %v", err)
	}

	// ARM 2: on the LEADER, AwaitLeadership returns at once.
	leaderDetector := New(leader.ledger, stable, "alpha")
	quick, cancelQuick := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelQuick()
	if err := leaderDetector.AwaitLeadership(quick, "alpha"); err != nil {
		t.Fatalf("ARM 2: AwaitLeadership refused on the leader: %v", err)
	}
	t.Log("ARM 2 (leader): AwaitLeadership returned nil")

	// ARM 3, THE DISCRIMINATING CONTROL for arm 2: on a FOLLOWER it PARKS. Without
	// this, an AwaitLeadership that returned nil unconditionally passes arm 2 and
	// re-creates the spin the design refuses.
	parked, cancelParked := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancelParked()
	err = followerDetector.AwaitLeadership(parked, "alpha")
	t.Logf("ARM 3 (follower parks): AwaitLeadership err=%v", err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ARM 3: AwaitLeadership returned %v on a follower, want a park ending in the "+
			"context deadline", err)
	}

	// ARM 4: a follower that WINS leadership is released. This is the behavior the
	// whole re-arm exists for, and it is the one neither arm above can show.
	released := make(chan error, 1)
	win, cancelWin := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelWin()
	go func() { released <- followerDetector.AwaitLeadership(win, "alpha") }()

	select {
	case err := <-released:
		t.Fatalf("ARM 4: AwaitLeadership returned %v before leadership moved", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := leader.ledger.Raft().LeadershipTransferToServer(
		raft.ServerID(follower.id), raft.ServerAddress(follower.mux.Addr().String()),
	).Error(); err != nil {
		t.Fatalf("ARM 4: transferring leadership: %v", err)
	}
	select {
	case err := <-released:
		t.Logf("ARM 4 (follower wins leadership): AwaitLeadership err=%v", err)
		if err != nil {
			t.Fatalf("ARM 4: AwaitLeadership returned %v after leadership arrived, want nil", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("ARM 4: AwaitLeadership never returned after leadership moved to this node")
	}
}
