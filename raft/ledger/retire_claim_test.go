// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

// THE PATHS HERE ARE THE PRODUCTION ONES. checkpoint.Path(d) is "checkpoint/"+d and
// checkpoint.ClaimPath(d) is "claim/"+d, and the two spaces are DISJOINT BY
// CONSTRUCTION. They are spelled as literals because raft/checkpoint imports this
// package, so importing it back would be a cycle. A test that keyed both halves at one
// path would collapse the two spaces and could not see which map an arm reached.
var (
	probeCheckpointPath = ckPath("datum-7")
	probeClaimPath      = clPath("datum-7")
)

func TestRetireClaimDropsTheClaimAndKeepsTheCheckpoint(t *testing.T) {
	f := newFSM()

	if resp := f.Apply(commandAt(t, 1, Entry{Kind: KindSet, Path: probeCheckpointPath, Value: []byte("progress")})); resp != nil {
		t.Fatalf("checkpointing responded %v, want nil", resp)
	}
	if resp := f.Apply(commandAt(t, 2, Entry{Kind: KindClaim, Path: probeClaimPath, Value: []byte("worker-a")})); resp != nil {
		t.Fatalf("claiming responded %v, want nil", resp)
	}

	// CONTROL: both halves are present before the retire-claim, so an absence
	// afterwards is the arm acting rather than a state that was never established.
	if _, ok := f.get(probeCheckpointPath); !ok {
		t.Fatal("CONTROL FAILED: no checkpoint before the retire-claim")
	}
	if held := f.claimant(probeClaimPath); held != "worker-a" {
		t.Fatalf("CONTROL FAILED: the claim is held by %q before the retire-claim, want worker-a", held)
	}

	f.Apply(commandAt(t, 3, Entry{Kind: KindRetireClaim, Path: probeClaimPath}))

	if held := f.claimant(probeClaimPath); held != "" {
		t.Fatalf("the claim SURVIVED the retire-claim, still held by %q; every survivor is refused forever", held)
	}
	// THE CHECKPOINT MUST SURVIVE. The datum is unowned, not finished — deleting its
	// progress here destroys exactly what the survivor is about to resume from.
	if _, ok := f.get(probeCheckpointPath); !ok {
		t.Fatal("the retire-claim ALSO deleted the checkpoint; the datum's progress is gone and resume has nothing to read")
	}

	// The datum is claimable again, by a DIFFERENT owner, which is the whole point.
	if resp := f.Apply(commandAt(t, 4, Entry{Kind: KindClaim, Path: probeClaimPath, Value: []byte("worker-b")})); resp != nil {
		t.Fatalf("a survivor claiming the retired claim responded %v, want nil", resp)
	}

	// DISCLOSURE: an arm that deleted nothing, against state that was never
	// established, would satisfy the absence assertion above.
	t.Log("the claim was held before the retire-claim and absent after, and the checkpoint survived it")
}

func TestARetireClaimOnAnUnclaimedDatumIsANoOpThatStillAdvancesTheIndex(t *testing.T) {
	// The leader re-observes a departure whenever its membership cursor rebuilds, so a
	// repeat must reach the same post-state rather than fail. A reader parked on the
	// index must still learn its fate.
	f := newFSM()

	if resp := f.Apply(commandAt(t, 5, Entry{Kind: KindRetireClaim, Path: "claim/never-claimed"})); resp != nil {
		t.Fatalf("retiring a claim nobody holds responded %v, want nil", resp)
	}
	if got := f.appliedIndex(); got != 5 {
		t.Fatalf("a no-op retire-claim at 5 left the tracked index at %d; a reader parked on 5 hangs forever", got)
	}
}

func TestARetireClaimOnAFollowerRefusesRatherThanForwarding(t *testing.T) {
	// ONLY THE LEADER MAY JUDGE THAT A HOLDER DEPARTED, because only the leader holds
	// the authoritative membership view. A forwarded retire-claim would let a DEMOTED
	// leader retire a live worker's claim off a view that has gone stale.
	muxes := newMuxes(t, 3)
	nodes := newClusterOn(t, "flow-retireclaim", muxes)
	leader := waitClusterLeader(t, nodes)
	follower := followerOf(t, nodes, leader)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// CONTROL: the SAME follower's KindRetire DOES forward and land, so the refusal
	// below is about this kind rather than about a follower that forwards nothing.
	if _, err := follower.ledger.Append(ctx, Entry{Kind: KindRetire, Path: ckPath("control"), Value: []byte(clPath("control"))}); err != nil {
		t.Fatalf("CONTROL FAILED: a follower's KindRetire did not forward: %v", err)
	}

	_, err := follower.ledger.Append(ctx, Entry{Kind: KindRetireClaim, Path: probeClaimPath})
	if !errors.Is(err, ErrNotLeader) {
		t.Fatalf("a follower's retire-claim reported %v, want ErrNotLeader; a forwarded retire-claim lets a "+
			"demoted leader retire a live worker's claim off a stale membership view", err)
	}

	// The LEADER's own retire-claim lands, so the refusal above is the forwarding
	// disposition rather than a kind nothing can append at all.
	if _, err := leader.ledger.Append(ctx, Entry{Kind: KindRetireClaim, Path: probeClaimPath}); err != nil {
		t.Fatalf("the leader's own retire-claim reported %v, want nil", err)
	}

	t.Logf("the follower's KindRetire forwarded and landed, its retire-claim was refused with %v, "+
		"and the leader's own retire-claim landed", err)
}
