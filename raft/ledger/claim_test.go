// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ledger

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/hashicorp/raft"
)

// claimAt builds a claim command naming owner as the claimant of datum.
func claimAt(t *testing.T, index uint64, datum, owner string) *raft.Log {
	t.Helper()

	return commandAt(t, index, Entry{Kind: KindClaim, Path: datum, Value: []byte(owner)})
}

// claimant reports which owner holds a datum, dropping the presence flag so an
// assertion reads in one line. It delegates to the production reader rather than
// touching the map, so a test never observes claim state through a path production
// does not use.
func (f *fsm) claimant(datum string) string {
	owner, _ := f.claimOwner(datum)

	return owner
}

func TestTheClaimArmRecordsTheFirstOwnerAndRefusesALater(t *testing.T) {
	f := newFSM()

	// CONTROL: an assignment through the SAME state machine responds nil, so a
	// refusal below is the claim arm answering rather than every apply failing.
	if resp := f.Apply(commandAt(t, 1, Entry{Kind: KindSet, Path: "heap/alpha", Value: []byte("v")})); resp != nil {
		t.Fatalf("CONTROL FAILED: a KindSet apply responded %v, want nil", resp)
	}

	if resp := f.Apply(claimAt(t, 2, "datum/7", "worker-a")); resp != nil {
		t.Fatalf("the first claim of datum/7 responded %v, want nil — the first claimant wins", resp)
	}

	// The property: a DIFFERENT owner is refused rather than overwriting, which is
	// what separates a claim from the last-write-wins KindSet arm above.
	resp := f.Apply(claimAt(t, 3, "datum/7", "worker-b"))
	err, ok := resp.(error)
	if !ok || err == nil {
		t.Fatalf("a second owner's claim of datum/7 responded %v, want an error; last-write-wins here would let two workers own one datum", resp)
	}
	if !errors.Is(err, ErrClaimHeld) {
		t.Fatalf("the refusal %v does not wrap ErrClaimHeld, so a caller cannot tell a lost race from an unclassified failure", err)
	}
	// The refusal names both parties, or the loser cannot act on it.
	for _, want := range []string{"datum/7", "worker-a", "worker-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err.Error(), want)
		}
	}

	// The held owner is unchanged by the refused attempt.
	if held := f.claimant("datum/7"); held != "worker-a" {
		t.Fatalf("after a refused claim datum/7 is held by %q, want worker-a", held)
	}
}

func TestReClaimingByTheSameOwnerSucceeds(t *testing.T) {
	// The forwarding loop retries an operation across a leadership change, so a
	// claim that refused its own winner on a retry would deny a worker the datum it
	// already owns.
	f := newFSM()

	if resp := f.Apply(claimAt(t, 1, "datum/7", "worker-a")); resp != nil {
		t.Fatalf("the first claim responded %v, want nil", resp)
	}
	if resp := f.Apply(claimAt(t, 2, "datum/7", "worker-a")); resp != nil {
		t.Fatalf("the winner re-claiming its OWN datum responded %v, want nil; a forwarding retry would lose the datum to its rightful owner", resp)
	}

	// CONTROL: the same state machine still refuses a different owner, so the nil
	// above is idempotence rather than a claim arm that refuses nothing.
	if resp := f.Apply(claimAt(t, 3, "datum/7", "worker-b")); resp == nil {
		t.Fatal("CONTROL FAILED: a different owner's claim responded nil, so this state machine refuses nobody")
	}
}

func TestARefusedClaimStillAdvancesTheTrackedIndex(t *testing.T) {
	// Apply's own doc states the invariant: the tracked index advances on EVERY
	// path. A reader parked on the index of a refused claim must learn its fate
	// rather than hang forever on an index that will never arrive.
	f := newFSM()

	if resp := f.Apply(claimAt(t, 4, "datum/7", "worker-a")); resp != nil {
		t.Fatalf("the first claim responded %v, want nil", resp)
	}
	if got := f.appliedIndex(); got != 4 {
		t.Fatalf("CONTROL FAILED: an ACCEPTED claim at 4 left the tracked index at %d", got)
	}

	if resp := f.Apply(claimAt(t, 9, "datum/7", "worker-b")); resp == nil {
		t.Fatal("the second owner's claim was not refused, so this test is not measuring the refused path")
	}
	if got := f.appliedIndex(); got != 9 {
		t.Fatalf("a REFUSED claim at 9 left the tracked index at %d; every reader parked on 9 hangs forever", got)
	}
}

func TestAClaimSurvivesASnapshotRestore(t *testing.T) {
	origin := newFSM()
	if resp := origin.Apply(claimAt(t, 3, "datum/7", "worker-a")); resp != nil {
		t.Fatalf("the claim responded %v, want nil", resp)
	}

	snapshot, err := origin.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot reported %v", err)
	}
	sink := &memSink{}
	if err := snapshot.Persist(sink); err != nil {
		t.Fatalf("Persist reported %v", err)
	}

	restored := newFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.buf.Bytes()))); err != nil {
		t.Fatalf("Restore reported %v", err)
	}

	// The property: the claim is still held after the restore. A claim that vanished
	// at a snapshot boundary would let a second worker claim a datum already owned.
	if held := restored.claimant("datum/7"); held != "worker-a" {
		t.Fatalf("after a snapshot restore datum/7 is held by %q, want worker-a", held)
	}
	if resp := restored.Apply(claimAt(t, 8, "datum/7", "worker-b")); resp == nil {
		t.Fatal("after a snapshot restore a SECOND owner's claim succeeded; the claim did not survive the boundary and two workers now own one datum")
	}

	// CONTROL: the restored state machine still admits an UNCLAIMED datum, so the
	// refusal above is the surviving claim and not a state machine that refuses all.
	if resp := restored.Apply(claimAt(t, 9, "datum/8", "worker-b")); resp != nil {
		t.Fatalf("CONTROL FAILED: claiming the unclaimed datum/8 responded %v, want nil", resp)
	}

	// DISCLOSURE: report the observation itself, so a restore that carried nothing
	// forward cannot satisfy this test's assertions vacuously.
	t.Logf("the restored claim still names the original winner: datum/7 is held by %q after the snapshot boundary",
		restored.claimant("datum/7"))
}

func TestRestoringASnapshotWithNoClaimsLeavesAWritableMap(t *testing.T) {
	// A snapshot written before claims existed decodes with a nil Claims map. A nil
	// map refuses writes, so the claim arm would panic on the first claim after such
	// a restore; replace installs an empty map instead, mirroring the values guard.
	f := newFSM()
	if err := f.Restore(io.NopCloser(bytes.NewReader(snapshotBytes(t, fsmSnapshot{
		Values:  map[string]Entry{"heap/alpha": {Kind: KindSet, Path: "heap/alpha", Value: []byte("v")}},
		Applied: 12,
	})))); err != nil {
		t.Fatalf("Restore reported %v", err)
	}

	if resp := f.Apply(claimAt(t, 13, "datum/7", "worker-a")); resp != nil {
		t.Fatalf("claiming after restoring a claimless snapshot responded %v, want nil", resp)
	}
	if held := f.claimant("datum/7"); held != "worker-a" {
		t.Fatalf("after the claim datum/7 is held by %q, want worker-a", held)
	}
}
