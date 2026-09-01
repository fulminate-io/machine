// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ledger

import (
	"bytes"
	"io"
	"testing"

	"github.com/hashicorp/raft"
)

// ckPath and clPath are the PRODUCTION path spaces a datum occupies, spelled as
// literals because raft/checkpoint imports this package and importing it back is a
// cycle. They mirror checkpoint.Path and checkpoint.ClaimPath, and the two are
// DISJOINT BY CONSTRUCTION.
//
// EVERY RETIRE TEST BELOW USES BOTH, and that is the whole point rather than a
// detail. Keying a checkpoint and its claim at ONE bare string collapses the two
// spaces, and a fixture that collapses them cannot see WHICH map an arm reached —
// which is exactly how an arm that deletes no claim at all passed this file.
func ckPath(datum string) string { return "checkpoint/" + datum }
func clPath(datum string) string { return "claim/" + datum }

// retireAt builds a retire command for a datum. The entry names the datum's
// CHECKPOINT path and carries its CLAIM path as the value, because the state
// machine cannot derive one from the other: the path vocabulary belongs to
// raft/checkpoint, which imports this package.
func retireAt(t *testing.T, index uint64, datum string) *raft.Log {
	t.Helper()

	return commandAt(t, index, Entry{
		Kind: KindRetire, Path: ckPath(datum), Value: []byte(clPath(datum)),
	})
}

func TestRetireDeletesTheCheckpointAndTheClaimTogether(t *testing.T) {
	f := newFSM()

	// A datum under recovery: a checkpoint at its path and a claim naming its owner.
	if resp := f.Apply(commandAt(t, 1, Entry{Kind: KindSet, Path: ckPath("datum-7"), Value: []byte("progress")})); resp != nil {
		t.Fatalf("checkpointing datum-7 responded %v, want nil", resp)
	}
	if resp := f.Apply(claimAt(t, 2, clPath("datum-7"), "worker-a")); resp != nil {
		t.Fatalf("claiming datum-7 responded %v, want nil", resp)
	}

	// CONTROL: both halves are present before the retire, so an absence afterwards
	// is the retire arm acting rather than a state that was never established.
	if _, ok := f.get(ckPath("datum-7")); !ok {
		t.Fatal("CONTROL FAILED: datum-7 carries no checkpoint before the retire")
	}
	if held := f.claimant(clPath("datum-7")); held != "worker-a" {
		t.Fatalf("CONTROL FAILED: datum-7 is held by %q before the retire, want worker-a", held)
	}

	f.Apply(retireAt(t, 3, "datum-7"))

	// BOTH halves leave. Either one surviving alone is silently wrong: a surviving
	// checkpoint under a dropped claim makes a completed datum re-claimable, and a
	// surviving claim over a dropped checkpoint names nothing forever.
	if entry, ok := f.get(ckPath("datum-7")); ok {
		t.Fatalf("after retirement datum-7 still carries the checkpoint %+v; a completed datum's progress is still being replayed", entry)
	}
	if held := f.claimant(clPath("datum-7")); held != "" {
		t.Fatalf("after retirement datum-7 is still held by %q; the already-claimed filter will honor a claim naming nothing forever", held)
	}

	// A retired datum is claimable again ONLY in the sense that nothing refuses it —
	// which is why the checkpoint had to go with it.
	if resp := f.Apply(claimAt(t, 4, clPath("datum-7"), "worker-b")); resp != nil {
		t.Fatalf("claiming the retired datum-7 responded %v, want nil", resp)
	}

	// DISCLOSURE: report the pre- and post-states this test actually observed. An
	// arm that deleted nothing, against a datum that was never established, would
	// otherwise satisfy every absence assertion above.
	t.Log("both the checkpoint and the claim were present before the retire and absent after")
}

func TestRetiringLeavesEveryOtherDatumAlone(t *testing.T) {
	// The retire arm deletes by path. A retire that cleared the maps would satisfy
	// every assertion above.
	f := newFSM()

	for _, datum := range []string{"datum-7", "datum-8"} {
		if resp := f.Apply(commandAt(t, 1, Entry{Kind: KindSet, Path: ckPath(datum), Value: []byte("progress")})); resp != nil {
			t.Fatalf("checkpointing %s responded %v, want nil", datum, resp)
		}
		if resp := f.Apply(claimAt(t, 2, clPath(datum), "worker-a")); resp != nil {
			t.Fatalf("claiming %s responded %v, want nil", datum, resp)
		}
	}

	f.Apply(retireAt(t, 3, "datum-7"))

	if _, ok := f.get(ckPath("datum-8")); !ok {
		t.Fatal("retiring datum-7 dropped datum-8's checkpoint as well")
	}
	if held := f.claimant(clPath("datum-8")); held != "worker-a" {
		t.Fatalf("retiring datum-7 left datum-8 held by %q, want worker-a", held)
	}
}

func TestRetiringADatumThatWasNeverCheckpointedIsANoOp(t *testing.T) {
	// Completion drives retirement and fires for every datum whether or not its flow
	// declared a checkpoint. Making absence an error would turn the ordinary case
	// into a failure.
	f := newFSM()

	if resp := f.Apply(retireAt(t, 5, "never-seen")); resp != nil {
		t.Fatalf("retiring a datum that was never checkpointed responded %v, want nil", resp)
	}
	if got := f.appliedIndex(); got != 5 {
		t.Fatalf("a no-op retire at 5 left the tracked index at %d; a reader parked on 5 hangs forever", got)
	}
}

func TestARetiredDatumIsAbsentFromASnapshot(t *testing.T) {
	// This is what bounds both accumulation paths: without it every claim and every
	// checkpoint ever made is carried forward by every snapshot for the life of the
	// flow.
	origin := newFSM()
	if resp := origin.Apply(commandAt(t, 1, Entry{Kind: KindSet, Path: ckPath("datum-7"), Value: []byte("progress")})); resp != nil {
		t.Fatalf("checkpointing datum-7 responded %v, want nil", resp)
	}
	if resp := origin.Apply(claimAt(t, 2, clPath("datum-7"), "worker-a")); resp != nil {
		t.Fatalf("claiming datum-7 responded %v, want nil", resp)
	}
	// CONTROL: a datum that is NOT retired survives the same snapshot, so an absence
	// below is retirement rather than a snapshot that carries nothing.
	if resp := origin.Apply(commandAt(t, 3, Entry{Kind: KindSet, Path: ckPath("datum-8"), Value: []byte("progress")})); resp != nil {
		t.Fatalf("checkpointing datum-8 responded %v, want nil", resp)
	}
	if resp := origin.Apply(claimAt(t, 4, clPath("datum-8"), "worker-b")); resp != nil {
		t.Fatalf("claiming datum-8 responded %v, want nil", resp)
	}

	origin.Apply(retireAt(t, 5, "datum-7"))

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

	if _, ok := restored.get(ckPath("datum-7")); ok {
		t.Fatal("the retired datum-7 came back through the snapshot; every snapshot carries every datum ever processed")
	}
	if held := restored.claimant(clPath("datum-7")); held != "" {
		t.Fatalf("the retired datum-7's claim came back through the snapshot held by %q", held)
	}
	if _, ok := restored.get(ckPath("datum-8")); !ok {
		t.Fatal("CONTROL FAILED: the live datum-8 did not survive the snapshot, so the absence above proves nothing")
	}
	if held := restored.claimant(clPath("datum-8")); held != "worker-b" {
		t.Fatalf("CONTROL FAILED: the live datum-8 is held by %q after the snapshot, want worker-b", held)
	}
}
