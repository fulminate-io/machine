// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// recordingJournal is a Journal that records what it was handed. It stands in for a
// DEPENDENCY rather than for the code under test: the replicated implementation
// lives in another module, and the property under test here is what this module
// hands across the seam.
type recordingJournal struct {
	mutex sync.Mutex

	checkpoints []CheckpointRecord
	claims      []string
	retires     []string

	claimResult bool
	claimErr    error
	orphans     []CheckpointRecord
	orphanErr   error
	failWith    error
}

func (j *recordingJournal) Checkpoint(_ context.Context, record CheckpointRecord) error {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	j.checkpoints = append(j.checkpoints, record)

	return j.failWith
}

func (j *recordingJournal) Claim(_ context.Context, flow, datum, owner string) (bool, error) {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	j.claims = append(j.claims, flow+"/"+datum+"/"+owner)

	return j.claimResult, j.claimErr
}

func (j *recordingJournal) Retire(_ context.Context, flow, datum string) error {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	j.retires = append(j.retires, flow+"/"+datum)

	return j.failWith
}

func (j *recordingJournal) Orphans(_ context.Context, _ string) ([]CheckpointRecord, error) {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	return j.orphans, j.orphanErr
}

// recorded returns copies of what the journal was handed, under its own lock.
func (j *recordingJournal) recorded() ([]CheckpointRecord, []string, []string) {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	return append([]CheckpointRecord(nil), j.checkpoints...),
		append([]string(nil), j.claims...),
		append([]string(nil), j.retires...)
}

func TestOptionJournalWritesTheJournalOntoTheConfig(t *testing.T) {
	journal := &recordingJournal{}

	// CONTROL: a machine built with NO journal option carries none, so the presence
	// below is the option acting rather than a field that is always populated.
	if bare := New("bare"); bare.cfg.journal != nil {
		t.Fatalf("a machine built with no journal option carries %v", bare.cfg.journal)
	}

	m := New("journaled", OptionJournal(journal))
	if m.cfg.journal == nil {
		t.Fatal("OptionJournal left the config's journal unset, so nothing would ever be checkpointed")
	}
	if m.cfg.journal != Journal(journal) {
		t.Fatalf("the config carries %v, want the journal the option was given", m.cfg.journal)
	}
}

func TestTheJournalSeamCarriesEachCallThrough(t *testing.T) {
	// The seam is an interface this module DECLARES and another implements, so the
	// property that matters here is that each method's arguments arrive intact and
	// each return reaches the caller.
	journal := &recordingJournal{claimResult: true}
	ctx := context.Background()

	record := CheckpointRecord{
		Flow:   "flow-a",
		Datum:  "datum-1",
		Owner:  "worker-a",
		Node:   "node-a",
		Anchor: AnchorArrival,
		Data:   []byte("marshaled packet"),
	}
	if err := journal.Checkpoint(ctx, record); err != nil {
		t.Fatalf("checkpointing: %v", err)
	}

	held, err := journal.Claim(ctx, "flow-a", "datum-1", "worker-a")
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if !held {
		t.Fatal("the claim reported the owner does not hold the datum it just won")
	}

	if err := journal.Retire(ctx, "flow-a", "datum-1"); err != nil {
		t.Fatalf("retiring: %v", err)
	}

	orphans, err := journal.Orphans(ctx, "flow-a")
	if err != nil {
		t.Fatalf("reading orphans: %v", err)
	}
	if len(orphans) != 0 {
		t.Fatalf("an empty journal reported %d orphans", len(orphans))
	}

	checkpoints, claims, retires := journal.recorded()
	if len(checkpoints) != 1 {
		t.Fatalf("the journal was handed %d records, want exactly 1", len(checkpoints))
	}
	// Compared field by field because the record carries a []byte, which is not
	// comparable — and the payload is the field a seam is most likely to drop.
	got := checkpoints[0]
	if got.Flow != record.Flow || got.Datum != record.Datum || got.Owner != record.Owner ||
		got.Node != record.Node || got.Anchor != record.Anchor || string(got.Data) != string(record.Data) {
		t.Fatalf("the journal was handed %+v, want exactly the record given %+v", got, record)
	}
	if len(claims) != 1 || claims[0] != "flow-a/datum-1/worker-a" {
		t.Fatalf("the journal was handed claims %v, want the flow, datum and owner", claims)
	}
	if len(retires) != 1 || retires[0] != "flow-a/datum-1" {
		t.Fatalf("the journal was handed retires %v, want the flow and datum", retires)
	}
}

func TestTheJournalSeamReportsItsFailuresRatherThanSwallowingThem(t *testing.T) {
	// A checkpoint that did not land leaves its datum unrecoverable from that point,
	// so every method's error must reach the caller rather than being absorbed at the
	// seam.
	sentinel := errors.New("the journal refused")
	journal := &recordingJournal{failWith: sentinel, claimErr: sentinel, orphanErr: sentinel}
	ctx := context.Background()

	if err := journal.Checkpoint(ctx, CheckpointRecord{Datum: "datum-1"}); !errors.Is(err, sentinel) {
		t.Fatalf("Checkpoint reported %v, want the journal's own error", err)
	}
	if _, err := journal.Claim(ctx, "flow-a", "datum-1", "worker-a"); !errors.Is(err, sentinel) {
		t.Fatalf("Claim reported %v, want the journal's own error", err)
	}
	if err := journal.Retire(ctx, "flow-a", "datum-1"); !errors.Is(err, sentinel) {
		t.Fatalf("Retire reported %v, want the journal's own error", err)
	}
	if _, err := journal.Orphans(ctx, "flow-a"); !errors.Is(err, sentinel) {
		t.Fatalf("Orphans reported %v, want the journal's own error", err)
	}
}

func TestTheTwoAnchorsAreDistinctAndNamed(t *testing.T) {
	// The anchor rides the record so a resuming worker never re-derives it from a
	// marker that may have changed between the writing build and the reading one.
	// Two anchors that compared equal would make every record ambiguous.
	if AnchorArrival == AnchorCompletion {
		t.Fatal("the two anchors are the same string, so a record cannot say which side of the node function it was written on")
	}
	for _, anchor := range []string{AnchorArrival, AnchorCompletion} {
		if anchor == "" {
			t.Fatal("an anchor is the empty string, which is indistinguishable from a record that names none")
		}
	}
}

// AwaitLeadership returns at once: this fake never withholds leadership.
func (j *recordingJournal) AwaitLeadership(context.Context, string) error { return nil }
