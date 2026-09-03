// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package recovery

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/checkpoint"
	"github.com/whitaker-io/machine/raft/ledger"
	machine "github.com/whitaker-io/machine/v4"
)

// restampOwner rewrites the one checkpoint in the journal to name a different owner,
// which is what a deleted pod's record looks like to a survivor. It goes through the
// ledger directly rather than through the seam BECAUSE the seam no longer lets a
// caller name an owner — that is the property under test, and a test that could still
// pass one would be testing a surface the product does not have.
func restampOwner(t *testing.T, l *ledger.Ledger, owner string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	entries, err := l.List(ctx, checkpoint.Path(""))
	if err != nil {
		t.Fatalf("listing checkpoints to re-stamp: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want exactly one checkpoint to re-stamp, found %d", len(entries))
	}

	record, err := decodeRecord(entries[0].Path, entries[0].Value)
	if err != nil {
		t.Fatalf("decoding the record to re-stamp: %v", err)
	}
	datum := datumOf(entries[0].Path)
	record.Owner = owner

	value, err := encodeRecord(record)
	if err != nil {
		t.Fatalf("encoding the re-stamped record: %v", err)
	}
	if _, err := l.Append(ctx, ledger.Entry{
		Kind: ledger.KindSet, Path: checkpoint.Path(datum), Value: value,
	}); err != nil {
		t.Fatalf("re-stamping the record: %v", err)
	}

	return datum
}

// TestAClaimCarriesTheLedgersOwnIdentity drives the identity a recovery claim is
// written under. The claimant is compared against the alive set the leader builds
// from the committed configuration, so a claim written in any other namespace is one
// the comparison can never match.
func TestAClaimCarriesTheLedgersOwnIdentity(t *testing.T) {
	nodes := newGroup(t, "flow-claimid", 3)
	leader := awaitLeader(t, nodes)
	view := newView(nodes[0].id, nodes[1].id, nodes[2].id)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// ARM A: TWO NODES, ONE DATUM. Under a shared owner both win and the claim
	// excludes nobody, which is mutual exclusion simply absent.
	first := New(nodes[0].ledger, view, "flow-claimid")
	second := New(nodes[1].ledger, view, "flow-claimid")

	firstWon, err := first.Claim(ctx, "flow-claimid", "d1")
	if err != nil {
		t.Fatalf("ARM A: the first claim errored: %v", err)
	}
	secondWon, err := second.Claim(ctx, "flow-claimid", "d1")
	if err != nil {
		t.Fatalf("ARM A: the second claim errored; a lost race is false, never an error: %v", err)
	}
	t.Logf("ARM A (two nodes, one datum): first=%v second=%v", firstWon, secondWon)
	if !firstWon || secondWon {
		t.Fatalf("ARM A: first=%v second=%v, want true then FALSE. A claim that admits every survivor "+
			"excludes nobody", firstWon, secondWon)
	}

	// ARM A CONTROL, THE WINNER'S RETRY. Without it an implementation that refused
	// every second claim passes the arm above while denying a holder the datum it
	// already owns across a leadership change.
	retry, err := first.Claim(ctx, "flow-claimid", "d1")
	if err != nil {
		t.Fatalf("ARM A control: the winner's retry errored: %v", err)
	}
	t.Logf("ARM A control (the winner retries): retry=%v", retry)
	if !retry {
		t.Fatal("ARM A control: the holder's own retry was refused, which denies a worker the datum it holds")
	}

	// THE CLAIMANT IS READ BACK, and it must be the claiming node's own ledger id.
	// Everything else rests on this: a claimant that is not a configured server id
	// is never in the alive set.
	claimant, held, err := leader.ledger.Claimant(ctx, checkpoint.ClaimPath("d1"))
	if err != nil {
		t.Fatalf("reading the claim state: %v", err)
	}
	t.Logf("ARM A claim state: claimant=%q held=%v", claimant, held)
	if !held || claimant != nodes[0].id {
		t.Fatalf("claimant=%q held=%v, want %q held — a claim under any other name is in a namespace "+
			"the alive set cannot match", claimant, held, nodes[0].id)
	}

	// ARM B: THE LEADER WITHHOLDS A LIVE CLAIMANT'S DATUM. The writer departed, so
	// the datum is an orphan by the absence arm, but a LIVE node holds the claim and
	// the datum is already being recovered.
	journalCheckpoint(t, leader.ledger, "writer-gone", "d2", "progress")
	if _, err := first.Claim(ctx, "flow-claimid", "d2"); err != nil {
		t.Fatalf("ARM B: claiming d2 for a live node: %v", err)
	}
	detector := New(leader.ledger, view, "flow-claimid")
	shortCtx, cancelShort := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancelShort()
	orphans, err := detector.Orphans(shortCtx, "flow-claimid")
	t.Logf("ARM B (claimant is a live node): orphans=%v err=%v", datums(orphans), err)
	if len(orphans) != 0 {
		t.Fatalf("ARM B: %v offered though a LIVE node holds the claim; the datum is already being "+
			"recovered and offering it puts a second writer beside the first", datums(orphans))
	}
	claimant, held, err = leader.ledger.Claimant(ctx, checkpoint.ClaimPath("d2"))
	if err != nil {
		t.Fatalf("ARM B: re-reading the claim state: %v", err)
	}
	t.Logf("ARM B claim survived the scan: claimant=%q held=%v", claimant, held)
	if !held || claimant != nodes[0].id {
		t.Fatalf("ARM B: the live node's claim did not survive the scan: claimant=%q held=%v", claimant, held)
	}

	// ARM B CONTROL, THE DEPARTED HOLDER. Without it an implementation that withheld
	// EVERY claimed datum passes arm B and strands every stranded claim.
	journalCheckpoint(t, leader.ledger, "writer-gone", "d3", "progress")
	claimOwner(t, leader.ledger, "d3", "departed-node")
	orphans, err = detector.Orphans(ctx, "flow-claimid")
	t.Logf("ARM B control (claimant departed): orphans=%v err=%v", datums(orphans), err)
	if err != nil {
		t.Fatalf("ARM B control: Orphans errored: %v", err)
	}
	if len(orphans) != 1 || orphans[0].Datum != "d3" {
		t.Fatalf("ARM B control: orphans=%v, want exactly d3 re-offered after its departed holder's claim "+
			"was retired", datums(orphans))
	}
}

// TestTheProductsOwnClaimPathIsExercisedEndToEnd drives a real Machine over the real
// Detector, which is the leg an ast census showed nothing landed exercises: every
// test site passed a node identity while the product passed the machine's name.
func TestTheProductsOwnClaimPathIsExercisedEndToEnd(t *testing.T) {
	nodes := newGroup(t, "flow-product", 3)
	leader := awaitLeader(t, nodes)

	// THE VIEW MUST KEEP DELIVERING, and that is a correction from running this: the
	// harness view hands back ONE batch and then blocks until its context ends, so a
	// resume loop that scanned before the record was re-stamped parked in Watch and
	// never looked again. The product's real membership manager publishes on every
	// configuration commit; a fake that goes silent measures the fake.
	view := &pollingView{servers: []raft.ServerID{
		raft.ServerID(nodes[0].id), raft.ServerID(nodes[1].id), raft.ServerID(nodes[2].id),
	}}

	detector := New(leader.ledger, view, "flow-product")

	gate := make(chan struct{})
	var closeOnce sync.Once
	release := func() { closeOnce.Do(func() { close(gate) }) }
	defer release()

	var mutex sync.Mutex
	entries := 0

	// THE MACHINE IS NAMED THE WAY THE SMOKE FIXTURE NAMES ITS OWN, so if the claim
	// ever carried the machine's name again this arm would read it verbatim.
	m := machine.New("smokehost-flow-product", machine.OptionJournal(detector), machine.OptionFIFO)
	src, ingest := m.Source[string]("src")
	worked := src.Map("worker", func(f machine.Frame[string]) string {
		mutex.Lock()
		entries++
		mutex.Unlock()
		// PARK INSIDE THE FUNC, which is what holds the recovery window open long
		// enough for the claim to be read while it is still held.
		<-gate

		return f.Value()
	}, machine.WithCheckpoint[string](machine.GobCodec[string]{}), machine.WithIdempotent[string]())
	worked.Output("sink", machine.WithCheckpoint[string](machine.GobCodec[string]{}))

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("starting the flow: %v", err)
	}
	if err := ingest(ctx, "unit-of-work"); err != nil {
		t.Fatalf("ingesting: %v", err)
	}

	// The ARRIVAL record is written by the MACHINE, before the node runs.
	var datum string
	for range 60 {
		list, err := leader.ledger.List(ctx, checkpoint.Path(""))
		if err == nil && len(list) == 1 {
			datum = datumOf(list[0].Path)

			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if datum == "" {
		t.Fatal("the machine never journaled an arrival record; there is nothing for recovery to find")
	}
	mutex.Lock()
	firstPass := entries
	mutex.Unlock()

	// PRE-CHECK: the record the MACHINE wrote is an arrival record for this node,
	// stamped with the ledger's own id. If it were anything else the arms below
	// would be measuring a record this test built rather than one the product did.
	list, err := leader.ledger.List(ctx, checkpoint.Path(""))
	if err != nil {
		t.Fatalf("listing the journal: %v", err)
	}
	written, err := decodeRecord(list[0].Path, list[0].Value)
	if err != nil {
		t.Fatalf("decoding the machine's own record: %v", err)
	}
	t.Logf("PRODUCT PATH pre-check: the machine journaled datum=%q node=%q anchor=%q owner=%q",
		datum, written.Node, written.Anchor, written.Owner)
	if written.Node != "worker" || written.Anchor != machine.AnchorArrival ||
		written.Owner != leader.ledger.LocalID() {
		t.Fatalf("PRE-CHECK FAILED: the machine journaled node=%q anchor=%q owner=%q, want the "+
			"checkpointed node's arrival record owned by this ledger", written.Node, written.Anchor, written.Owner)
	}

	// Re-stamp the record with a DEPARTED owner, which is how a deleted pod's
	// checkpoint looks to the survivor that inherits it.
	restampOwner(t, leader.ledger, "departed-node")

	// PRE-CHECK: the re-stamped record really is detectable as an orphan. Without
	// it, a claim that never appears below could mean the datum was never offered
	// rather than that the resume loop failed to claim it.
	seed := New(leader.ledger, view, "flow-product")
	seedCtx, cancelSeed := context.WithTimeout(context.Background(), 10*time.Second)
	seen, seedErr := seed.Orphans(seedCtx, "flow-product")
	cancelSeed()
	t.Logf("PRODUCT PATH pre-check: an independent scan offers %v err=%v", datums(seen), seedErr)
	if len(seen) != 1 || seen[0].Datum != datum {
		t.Fatalf("PRE-CHECK FAILED: an independent scan offered %v, want the re-stamped datum; the "+
			"claim assertion below would be vacuous", datums(seen))
	}

	// The resume loop is already parked in Orphans. Wait for it to claim.
	var claimant string
	var held bool
	for range 80 {
		var err error
		claimant, held, err = leader.ledger.Claimant(ctx, checkpoint.ClaimPath(datum))
		if err == nil && held {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	mutex.Lock()
	total := entries
	mutex.Unlock()
	t.Logf("PRODUCT PATH: entries=%d (first pass %d) claimant=%q held=%v", total, firstPass, claimant, held)
	if !held || claimant != leader.ledger.LocalID() {
		t.Fatalf("PRODUCT PATH: claimant=%q held=%v, want the ledger id %q held. The product's own "+
			"resume path is the only caller of Claim and nothing landed exercises the identity it passes",
			claimant, held, leader.ledger.LocalID())
	}

	// THE BOUND IS THE CLAIM, NOT AN ENTRY COUNT. A FIFO worker parked inside its
	// func queues the re-run rather than entering again, so the count is a ceiling;
	// what stops the next round is a claim held by a live node. Under the defect the
	// claim is retired in the same scan and the loop re-offers without limit.
	probe := New(leader.ledger, view, "flow-product")
	boundCtx, cancelBound := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancelBound()
	offered, _ := probe.Orphans(boundCtx, "flow-product")
	t.Logf("PRODUCT PATH bound: a further scan offered %v", datums(offered))
	if len(offered) != 0 {
		t.Fatalf("PRODUCT PATH bound: a further scan offered %v while the datum is claimed by a live "+
			"node and still running; the loop is unbounded", datums(offered))
	}

	release()
}
