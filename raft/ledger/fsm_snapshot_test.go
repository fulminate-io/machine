package ledger

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// snapshotBytes encodes a snapshot envelope exactly as Persist does, so a test can
// hand Restore a journal no honest Snapshot on this build would produce.
func snapshotBytes(t *testing.T, snapshot fsmSnapshot) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(&snapshot); err != nil {
		t.Fatalf("encoding a snapshot envelope: %v", err)
	}

	return buf.Bytes()
}

func TestRestoreValidatesTheKindsItInstalls(t *testing.T) {
	// A kind arriving by SNAPSHOT is no more interpretable than the same kind
	// arriving by log. If Apply poisons on it, Restore must too — otherwise the
	// fail-loud invariant inverts on arrival path and the node serves an entry it
	// cannot interpret.
	poisoned := newFSM()
	err := poisoned.Restore(io.NopCloser(bytes.NewReader(snapshotBytes(t, fsmSnapshot{
		Values:  map[string]Entry{"heap/unknown": {Kind: Kind(200), Path: "heap/unknown"}},
		Applied: 42,
	}))))
	if err != nil {
		t.Fatalf("Restore reported %v; it installs the snapshot and records a poison rather than refusing the restore outright", err)
	}
	if poisoned.poison == nil {
		t.Fatal("a snapshot carrying kind 200 restored with no poison recorded; the same kind arriving through Apply poisons, so the fail-loud invariant inverted on arrival path")
	}
	if !errors.Is(poisoned.poison, ErrPoisonedJournal) {
		t.Fatalf("the recorded poison %v is not a wrapped ErrPoisonedJournal", poisoned.poison)
	}

	// The unreadable entry is not served: a read reports the poison.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := poisoned.waitApplied(ctx, 42); !errors.Is(err, ErrPoisonedJournal) {
		t.Fatalf("a read against the restored journal gave %v, want the poison", err)
	}

	// CONTROL: a snapshot carrying only declared kinds restores clean through the
	// same path, so the poison above is about the kind rather than about Restore
	// poisoning everything it touches.
	clean := newFSM()
	if err := clean.Restore(io.NopCloser(bytes.NewReader(snapshotBytes(t, fsmSnapshot{
		Values:  map[string]Entry{"heap/known": {Kind: KindSet, Path: "heap/known", Value: []byte("v")}},
		Applied: 7,
	})))); err != nil {
		t.Fatalf("CONTROL FAILED: restoring a declared-kind snapshot: %v", err)
	}
	if clean.poison != nil {
		t.Fatalf("CONTROL FAILED: a snapshot of declared kinds recorded the poison %v", clean.poison)
	}
}

func TestRestoreClearsAPoisonAgainstTheDiscardedJournal(t *testing.T) {
	f := newFSM()
	// Poison this node against a journal it is about to throw away.
	f.Apply(commandAt(t, 3, Entry{Kind: Kind(200), Path: "heap/bad"}))
	if f.poison == nil {
		t.Fatal("CONTROL FAILED: the seeding apply recorded no poison, so there is nothing for the restore to clear")
	}

	// An authoritative snapshot carrying only declared kinds replaces that journal
	// entirely. The poison described state this node no longer holds, and it named
	// a log index the restore discarded.
	if err := f.Restore(io.NopCloser(bytes.NewReader(snapshotBytes(t, fsmSnapshot{
		Values:  map[string]Entry{"heap/known": {Kind: KindSet, Path: "heap/known", Value: []byte("authoritative")}},
		Applied: 90,
	})))); err != nil {
		t.Fatalf("restoring an authoritative journal: %v", err)
	}
	if f.poison != nil {
		t.Fatalf("a poison recorded against the discarded journal survived an authoritative clean restore: %v; a rejoining peer would stay read-dead on correct data", f.poison)
	}

	// CONTROL: the restore actually installed the authoritative journal, so the
	// cleared poison is not the result of a restore that did nothing.
	entry, ok := f.get("heap/known")
	if !ok || string(entry.Value) != "authoritative" {
		t.Fatalf("CONTROL FAILED: after the restore heap/known reads %+v present=%v", entry, ok)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.waitApplied(ctx, 90); err != nil {
		t.Fatalf("a read against the healed journal gave %v, want it to serve", err)
	}
}

// memSink is a raft.SnapshotSink that keeps the snapshot in memory and records
// which of Close and Cancel raft's contract got.
type memSink struct {
	buf      bytes.Buffer
	failWith error
	closed   bool
	canceled bool
}

var _ raft.SnapshotSink = (*memSink)(nil)

func (s *memSink) Write(p []byte) (int, error) {
	if s.failWith != nil {
		return 0, s.failWith
	}

	return s.buf.Write(p)
}

func (s *memSink) Close() error  { s.closed = true; return nil }
func (s *memSink) Cancel() error { s.canceled = true; return nil }
func (*memSink) ID() string      { return "test-snapshot" }

func TestLedgerRestoresValuesAndAppliedIndexFromASnapshot(t *testing.T) {
	source := newFSM()
	source.Apply(commandAt(t, 40, Entry{Kind: KindSet, Path: "heap/kept", Value: []byte("snapshot value")}))
	source.advance(42)

	snapshot, err := source.Snapshot()
	if err != nil {
		t.Fatalf("taking a snapshot: %v", err)
	}
	sink := &memSink{}
	if err := snapshot.Persist(sink); err != nil {
		t.Fatalf("persisting a snapshot: %v", err)
	}
	if !sink.closed || sink.canceled {
		t.Fatalf("a successful persist left closed=%v canceled=%v, want the sink closed and not canceled", sink.closed, sink.canceled)
	}
	// raft calls Release unconditionally, so it runs here exactly as it would in a
	// real cycle.
	snapshot.Release()

	target := newFSM()
	// A key that is ABSENT from the snapshot, seeded before the restore. This is the
	// key the discard assertion below is about.
	target.Apply(commandAt(t, 1, Entry{Kind: KindSet, Path: "heap/stale", Value: []byte("pre-restore")}))

	// A reader parked below the snapshot's index must be woken BY the restore. A
	// Restore that replaced the map without advancing would strand it.
	parked := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		parked <- target.waitApplied(ctx, 42)
	}()
	time.Sleep(20 * time.Millisecond)

	if err := target.Restore(io.NopCloser(bytes.NewReader(sink.buf.Bytes()))); err != nil {
		t.Fatalf("restoring a snapshot: %v", err)
	}

	// CONTROL: the snapshot's own key is present after the restore. Without this,
	// the stale key being gone below could equally mean the restore wiped
	// everything and installed nothing.
	entry, ok := target.get("heap/kept")
	if !ok || string(entry.Value) != "snapshot value" {
		t.Fatalf("CONTROL FAILED: after the restore heap/kept reads %+v present=%v, want the snapshot's value", entry, ok)
	}
	if got := target.appliedIndex(); got != 42 {
		t.Fatalf("the restored state machine reports applied index %d, want the snapshot's 42", got)
	}

	// THE DISCARD ASSERTION: a key held before the restore and absent from the
	// snapshot must be gone. A Restore that merges instead of discarding satisfies
	// every other clause in this test and still diverges peers.
	if stale, ok := target.get("heap/stale"); ok {
		t.Fatalf("heap/stale survived the restore as %+v; Restore merged over the live journal instead of discarding it", stale)
	}

	if err := <-parked; err != nil {
		t.Fatalf("a reader parked at index 42 was not woken by the restore: %v", err)
	}
}

func TestPersistCancelsTheSinkWhenItCannotWrite(t *testing.T) {
	f := newFSM()
	f.Apply(commandAt(t, 3, Entry{Kind: KindSet, Path: "heap/alpha", Value: []byte("v")}))

	snapshot, err := f.Snapshot()
	if err != nil {
		t.Fatalf("taking a snapshot: %v", err)
	}
	failure := errors.New("sink is out of space")
	sink := &memSink{failWith: failure}

	err = snapshot.Persist(sink)
	if !errors.Is(err, failure) {
		t.Fatalf("persisting to a failing sink gave %v, want the sink's own error wrapped", err)
	}
	if !sink.canceled {
		t.Fatal("a failed persist did not cancel the sink, so raft would keep a half-written snapshot")
	}
	if sink.closed {
		t.Fatal("a failed persist closed the sink, which tells raft the snapshot is complete")
	}
	// Release is safe on a snapshot that was never successfully persisted; raft
	// calls it on exactly this path.
	snapshot.Release()
}

func TestSnapshotCopiesTheJournalRatherThanAliasingIt(t *testing.T) {
	f := newFSM()
	f.Apply(commandAt(t, 1, Entry{Kind: KindSet, Path: "heap/alpha", Value: []byte("first")}))

	snapshot, err := f.Snapshot()
	if err != nil {
		t.Fatalf("taking a snapshot: %v", err)
	}

	// raft runs Apply concurrently with Persist, so a write landing between the two
	// must not reach the snapshot that was already taken.
	f.Apply(commandAt(t, 2, Entry{Kind: KindSet, Path: "heap/beta", Value: []byte("second")}))

	sink := &memSink{}
	if err := snapshot.Persist(sink); err != nil {
		t.Fatalf("persisting a snapshot: %v", err)
	}
	restored := newFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.buf.Bytes()))); err != nil {
		t.Fatalf("restoring a snapshot: %v", err)
	}

	// CONTROL: the key present when the snapshot was taken survived it.
	if _, ok := restored.get("heap/alpha"); !ok {
		t.Fatal("CONTROL FAILED: the key present at snapshot time is missing from the persisted snapshot")
	}
	if entry, ok := restored.get("heap/beta"); ok {
		t.Fatalf("a write applied after the snapshot was taken reached it as %+v; Snapshot aliased the live journal", entry)
	}
}
