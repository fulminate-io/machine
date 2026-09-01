// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package recovery

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/whitaker-io/machine/raft/checkpoint"
)

func TestADepartedHoldersClaimIsRetiredAndTheDatumIsReClaimedByASurvivor(t *testing.T) {
	// THE END-TO-END THE DEAD-WORKER TEST CANNOT REACH. That test drives the common
	// case: a checkpoint naming a departed writer with NO claim. This drives the case
	// a survivor already picked the datum up and then died itself — the claim names a
	// departed node, and first-writer-wins refuses every later claimant forever.
	nodes := newGroup(t, "flow-stranded", 3)
	leader := awaitLeader(t, nodes)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	journalCheckpoint(t, leader.ledger, "n9", "datum-stranded", "progress")
	claimOwner(t, leader.ledger, "datum-stranded", "n8")

	// CONTROL: the claim really is held by a node that is NOT in the view below, so
	// its absence later is the retire acting rather than a claim that never landed.
	owner, held, err := leader.ledger.Claimant(ctx, checkpoint.ClaimPath("datum-stranded"))
	if err != nil || !held {
		t.Fatalf("CONTROL FAILED: reading the claim back gave owner=%q held=%v err=%v", owner, held, err)
	}

	// Both the writer (n9) and the holder (n8) have left the flow.
	view := newView(leader.id)
	detector := New(leader.ledger, view, "flow-stranded")

	orphans, err := detector.Orphans(ctx, "flow-stranded")
	if err != nil {
		t.Fatalf("detecting orphans: %v", err)
	}
	if got := datums(orphans); !slices.Contains(got, "datum-stranded") {
		t.Fatalf("the detector did not offer datum-stranded: %v", got)
	}

	// THE CLAIM IS GONE, AND A LIVE SURVIVOR WINS IT. Before the retire-claim entry
	// existed this returned won=false with the datum offered on every round forever.
	won, err := detector.Claim(ctx, "flow-stranded", "datum-stranded", leader.id)
	if err != nil {
		t.Fatalf("a survivor claiming the re-offered datum: %v", err)
	}
	if !won {
		t.Fatal("the survivor LOST the claim on a datum whose holder has departed; the datum is unrecoverable")
	}

	// THE CHECKPOINT SURVIVED the retirement of the claim, which is what the survivor
	// resumes from. A retire-claim that took the checkpoint too would leave it nothing.
	if _, present, err := leader.ledger.Get(ctx, checkpoint.Path("datum-stranded")); err != nil || !present {
		t.Fatalf("the datum's checkpoint did not survive the claim retirement: present=%v err=%v", present, err)
	}

	// DISCLOSURE: report the transition this test observed, so a run in which the
	// stranded state was never established cannot read as a pass.
	t.Logf("the claim was held by the departed %q before detection and won by the live %q after: "+
		"the leader retired it, nobody stole it", owner, leader.id)
}

func TestALiveHoldersClaimIsNeverRetired(t *testing.T) {
	// THE CONTROL ON THE TRIGGER. A retire-claim that fired on liveness it did not
	// check would take work away from a healthy survivor mid-flight, which is exactly
	// the steal that first-writer-wins exists to prevent.
	nodes := newGroup(t, "flow-liveholder", 3)
	leader := awaitLeader(t, nodes)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	journalCheckpoint(t, leader.ledger, "n9", "datum-taken", "progress")
	// A SECOND, UNCLAIMED ORPHAN. Orphans blocks until something is claimable, so
	// without it a correct implementation would park rather than answer.
	journalCheckpoint(t, leader.ledger, "n9", "datum-free", "progress")
	claimOwner(t, leader.ledger, "datum-taken", nodes[1].id)

	// nodes[1] is IN the view: the holder is alive.
	view := newView(nodes[0].id, nodes[1].id, nodes[2].id)
	detector := New(leader.ledger, view, "flow-liveholder")

	orphans, err := detector.Orphans(ctx, "flow-liveholder")
	if err != nil {
		t.Fatalf("detecting orphans: %v", err)
	}
	got := datums(orphans)
	if slices.Contains(got, "datum-taken") {
		t.Fatalf("the detector offered datum-taken, whose holder is alive: %v", got)
	}
	if !slices.Contains(got, "datum-free") {
		t.Fatalf("CONTROL FAILED: the unclaimed datum-free was not offered either (%v), so the detector "+
			"withheld everything rather than withholding what a live worker holds", got)
	}

	// THE CLAIM SURVIVES THE ROUND. This is the assertion the offer-filter alone does
	// not make: a trigger that retired every claim it saw would still withhold the
	// datum this round and hand it away on the next one.
	owner, held, err := leader.ledger.Claimant(ctx, checkpoint.ClaimPath("datum-taken"))
	if err != nil {
		t.Fatalf("reading the claim back: %v", err)
	}
	if !held || owner != nodes[1].id {
		t.Fatalf("after a detection round the LIVE holder's claim reads held=%v owner=%q, want held by %q; "+
			"the leader retired a claim whose holder never left", held, owner, nodes[1].id)
	}

	t.Logf("the live holder %q still holds datum-taken after a detection round that offered %v", owner, got)
}

func TestRetiringThroughTheDetectorDropsTheClaimAndNotOnlyTheCheckpoint(t *testing.T) {
	// THE PRODUCER HALF. The journal's retire arm deletes the claim key the ENTRY
	// names, so an arm that is correct and a producer that names only the checkpoint
	// path combine into a retirement that drops no claim at all — and every fsm-level
	// test still passes, because it builds the entry itself. This drives the real
	// Detector.Retire so both bodies are exercised.
	nodes := newGroup(t, "flow-retireproducer", 3)
	leader := awaitLeader(t, nodes)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	journalCheckpoint(t, leader.ledger, leader.id, "datum-done", "progress")
	claimOwner(t, leader.ledger, "datum-done", leader.id)

	// CONTROL: both halves are established before the retire, through the same
	// accessors the assertions below read.
	if _, present, err := leader.ledger.Get(ctx, checkpoint.Path("datum-done")); err != nil || !present {
		t.Fatalf("CONTROL FAILED: no checkpoint before the retire: present=%v err=%v", present, err)
	}
	if owner, held, err := leader.ledger.Claimant(ctx, checkpoint.ClaimPath("datum-done")); err != nil || !held {
		t.Fatalf("CONTROL FAILED: no claim before the retire: owner=%q held=%v err=%v", owner, held, err)
	}

	if err := (&Detector{ledger: leader.ledger, manager: newView(leader.id), flow: "flow-retireproducer"}).
		Retire(ctx, "flow-retireproducer", "datum-done"); err != nil {
		t.Fatalf("retiring datum-done: %v", err)
	}

	if _, present, err := leader.ledger.Get(ctx, checkpoint.Path("datum-done")); err != nil || present {
		t.Fatalf("the checkpoint survived the retire: present=%v err=%v", present, err)
	}
	owner, held, err := leader.ledger.Claimant(ctx, checkpoint.ClaimPath("datum-done"))
	if err != nil {
		t.Fatalf("reading the claim back: %v", err)
	}
	if held {
		t.Fatalf("the CLAIM survived the retire, still held by %q; every claim ever taken during recovery "+
			"outlives its datum and rides every snapshot for the life of the flow", owner)
	}

	t.Log("retiring through the detector removed the checkpoint AND the claim, both read back through their own accessors")
}
