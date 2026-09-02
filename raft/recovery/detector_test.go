// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package recovery

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/whitaker-io/machine/raft/checkpoint"
	"github.com/whitaker-io/machine/raft/ledger"
	machine "github.com/whitaker-io/machine/v4"
)

func TestADetectorReportsTheIdentityItsLedgerReports(t *testing.T) {
	// THE DETECTOR TAKES NO IDENTITY PARAMETER, which is what this asserts from the
	// outside: there is no second field to disagree with the ledger's, so the
	// two-stamper divergence is unrepresentable here rather than merely refused.
	// The refusal itself belongs to the membership open sites and is gated there;
	// this test does not restate it.
	nodes := newGroup(t, "flow-identity", 3)
	awaitLeader(t, nodes)

	for _, member := range nodes {
		detector := New(member.ledger, newView(member.id), "flow-identity")
		if got := detector.LocalID(); got != member.ledger.LocalID() {
			t.Fatalf("the detector reports %q while its ledger reports %q; an orphan's owner would be "+
				"compared against a different authority than the configuration carries",
				got, member.ledger.LocalID())
		}
		if got := detector.LocalID(); got != member.id {
			t.Fatalf("the detector reports %q, want the node's own id %q", got, member.id)
		}
	}
}

func TestCheckpointJournalsTheWholeRecordAndStampsItsWriter(t *testing.T) {
	// THE WHOLE RECORD IS PERSISTED, not just its payload. Detection needs the OWNER
	// to decide whose worker is gone, and resume needs the ANCHOR and the NODE to
	// decide where the record goes back — a journal storing bytes alone leaves both
	// unanswerable at the only moment they matter.
	nodes := newGroup(t, "flow-write", 3)
	leader := awaitLeader(t, nodes)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	detector := New(leader.ledger, newView(nodes[0].id, nodes[1].id, nodes[2].id), "flow-write")

	if err := detector.Checkpoint(ctx, machine.CheckpointRecord{
		Flow: "flow-write", Datum: "datum-1", Node: "worker",
		Anchor: machine.AnchorArrival, Data: []byte("payload"),
	}); err != nil {
		t.Fatalf("journaling: %v", err)
	}

	entry, present, err := leader.ledger.Get(ctx, checkpoint.Path("datum-1"))
	if err != nil || !present {
		t.Fatalf("reading the record back gave present=%v err=%v", present, err)
	}

	record, err := decodeRecord(checkpoint.Path("datum-1"), entry.Value)
	if err != nil {
		t.Fatalf("decoding the record: %v", err)
	}
	if record.Node != "worker" || record.Anchor != machine.AnchorArrival {
		t.Fatalf("the stored record reads node=%q anchor=%q, want the ones journaled", record.Node, record.Anchor)
	}
	if string(record.Data) != "payload" {
		t.Fatalf("the stored record carries %q, want the journaled payload", record.Data)
	}
	// THE WRITER IS STAMPED FROM THE LEDGER, not left to the caller: it is the same
	// authority the configuration carries, so detection compares against one fact.
	if record.Owner != leader.ledger.LocalID() {
		t.Fatalf("the stored record names owner %q, want the ledger's own id %q",
			record.Owner, leader.ledger.LocalID())
	}

	// CONTROL: an owner the caller DID supply is preserved rather than overwritten,
	// so the stamp above fills a gap instead of clobbering a decision.
	if err := detector.Checkpoint(ctx, machine.CheckpointRecord{
		Datum: "datum-2", Owner: "someone-else", Data: []byte("payload"),
	}); err != nil {
		t.Fatalf("journaling datum-2: %v", err)
	}
	second, _, err := leader.ledger.Get(ctx, checkpoint.Path("datum-2"))
	if err != nil {
		t.Fatalf("reading datum-2 back: %v", err)
	}
	kept, err := decodeRecord(checkpoint.Path("datum-2"), second.Value)
	if err != nil {
		t.Fatalf("decoding datum-2: %v", err)
	}
	if kept.Owner != "someone-else" {
		t.Fatalf("a caller-supplied owner was overwritten with %q", kept.Owner)
	}
}

func TestRetireDropsTheCheckpointSoItIsNeverOfferedAgain(t *testing.T) {
	nodes := newGroup(t, "flow-retire", 3)
	leader := awaitLeader(t, nodes)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	journalCheckpoint(t, leader.ledger, "n9", "datum-1", "progress")
	detector := New(leader.ledger, newView(nodes[0].id, nodes[1].id, nodes[2].id), "flow-retire")

	// CONTROL: it IS offered before the retire, so the absence afterwards is the
	// retirement acting rather than a detector that offers nothing.
	before, err := detector.Orphans(ctx, "flow-retire")
	if err != nil {
		t.Fatalf("detecting before the retire: %v", err)
	}
	if !slices.Contains(datums(before), "datum-1") {
		t.Fatalf("CONTROL FAILED: datum-1 was not offered before the retire: %v", datums(before))
	}

	if err := detector.Retire(ctx, "flow-retire", "datum-1"); err != nil {
		t.Fatalf("retiring: %v", err)
	}

	bounded, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	after, err := detector.Orphans(bounded, "flow-retire")
	if err == nil && slices.Contains(datums(after), "datum-1") {
		t.Fatalf("a retired datum was offered again: %v", datums(after))
	}
}

func TestAnUnreadableRecordIsReportedRatherThanSkipped(t *testing.T) {
	// Silently dropping a record that will not decode would hide a datum from
	// recovery, which is the same silence this package exists to prevent.
	nodes := newGroup(t, "flow-garbage", 3)
	leader := awaitLeader(t, nodes)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := leader.ledger.Append(ctx, ledger.Entry{
		Kind: ledger.KindSet, Path: checkpoint.Path("datum-bad"), Value: []byte("not a record"),
	}); err != nil {
		t.Fatalf("writing the undecodable entry: %v", err)
	}

	detector := New(leader.ledger, newView(nodes[0].id, nodes[1].id, nodes[2].id), "flow-garbage")
	_, err := detector.Orphans(ctx, "flow-garbage")
	if err == nil {
		t.Fatal("an undecodable record was skipped rather than reported; the datum under it is invisible to recovery")
	}
	if !strings.Contains(err.Error(), "decoding the record") {
		t.Fatalf("the failure %q does not name the decode step", err)
	}
}

func TestClaimReportsAJournalFailureRatherThanALostRace(t *testing.T) {
	// A LOST RACE and a BROKEN JOURNAL are different facts. The first is the protocol
	// working and reports false; the second must reach the caller.
	nodes := newGroup(t, "flow-claimerr", 3)
	leader := awaitLeader(t, nodes)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	detector := New(leader.ledger, newView(nodes[0].id), "flow-claimerr")

	// CONTROL: an ordinary claim succeeds through this same path.
	won, err := detector.Claim(ctx, "flow-claimerr", "datum-1", nodes[0].id)
	if err != nil || !won {
		t.Fatalf("CONTROL FAILED: an uncontested claim gave won=%v err=%v", won, err)
	}

	// A LOST race: reported as false, never as an error.
	won, err = detector.Claim(ctx, "flow-claimerr", "datum-1", nodes[1].id)
	if err != nil {
		t.Fatalf("a lost race reported an error %v; losing is the protocol working", err)
	}
	if won {
		t.Fatal("a second owner won a datum another worker already holds")
	}

	// A CLOSED ledger is a real failure and reaches the caller.
	if err := leader.ledger.Close(); err != nil {
		t.Fatalf("closing the ledger: %v", err)
	}
	if _, err := detector.Claim(ctx, "flow-claimerr", "datum-2", nodes[0].id); !errors.Is(err, ledger.ErrClosed) {
		t.Fatalf("claiming on a closed ledger gave %v, want the ledger's own refusal", err)
	}
}
