package ledger

import (
	"errors"
	"strings"
	"testing"

	"github.com/hashicorp/raft"
)

func TestConfigurationCommitsAdvanceTheTrackedAppliedIndex(t *testing.T) {
	f := newFSM()

	// These are the two interfaces raft type-asserts on. ConfigurationStore is the
	// load-bearing one: raft's applySingle returns early for a configuration entry
	// when the state machine does not implement it, advancing nothing, which would
	// park every reader behind that index permanently.
	var _ raft.FSM = f
	store, ok := any(f).(raft.ConfigurationStore)
	if !ok {
		t.Fatal("the ledger state machine does not implement raft.ConfigurationStore")
	}
	if got := f.appliedIndex(); got != 0 {
		t.Fatalf("a fresh state machine reports applied index %d, want 0", got)
	}

	// CONTROL: a command commit advances the tracked index. Without it, a zero
	// after the configuration commit below could not be told apart from a tracker
	// that never moves for anything.
	if resp := f.Apply(commandAt(t, 7, Entry{Kind: KindSet, Path: "heap/alpha", Value: []byte("v")})); resp != nil {
		t.Fatalf("applying a KindSet entry responded %v, want nil", resp)
	}
	if got := f.appliedIndex(); got != 7 {
		t.Fatalf("CONTROL FAILED: a command commit at 7 left the tracked index at %d", got)
	}

	// The property under test: a configuration commit advances it as well.
	store.StoreConfiguration(9, raft.Configuration{})
	if got := f.appliedIndex(); got != 9 {
		t.Fatalf("a configuration commit at 9 left the tracked index at %d; a reader waiting on 9 would never wake", got)
	}

	// A configuration commit journals no value, and does not disturb one.
	entry, ok := f.get("heap/alpha")
	if !ok || string(entry.Value) != "v" {
		t.Fatalf("after a configuration commit heap/alpha reads %+v present=%v, want the committed value", entry, ok)
	}

	// The tracker is monotonic: a late or replayed lower index never walks it back,
	// which would strand a waiter that had already been woken.
	store.StoreConfiguration(4, raft.Configuration{})
	if got := f.appliedIndex(); got != 9 {
		t.Fatalf("a configuration commit at 4 moved the tracked index to %d, want it held at 9", got)
	}
	if resp := f.Apply(commandAt(t, 4, Entry{Kind: KindSet, Path: "heap/beta", Value: []byte("w")})); resp != nil {
		t.Fatalf("applying a replayed command responded %v, want nil", resp)
	}
	if got := f.appliedIndex(); got != 9 {
		t.Fatalf("a replayed command at 4 moved the tracked index to %d, want it held at 9", got)
	}

	// An epoch entry carries no value but must still advance the index; that is the
	// entire reason it exists.
	if resp := f.Apply(commandAt(t, 12, Entry{Kind: KindEpoch})); resp != nil {
		t.Fatalf("applying an epoch entry responded %v, want nil", resp)
	}
	if got := f.appliedIndex(); got != 12 {
		t.Fatalf("an epoch commit at 12 left the tracked index at %d", got)
	}
}

func TestPoisonedApplyAdvancesTheIndexAndReportsThePoison(t *testing.T) {
	f := newFSM()

	// A poisoned entry must still advance the index. If it did not, every reader
	// waiting on that commit would hang rather than learn the ledger is poisoned.
	resp := f.Apply(&raft.Log{Index: 5, Type: raft.LogCommand, Data: mustEncode(t, Entry{Kind: Kind(200), Path: "heap/alpha"})})
	err, isErr := resp.(error)
	if !isErr {
		t.Fatalf("applying an undeclared kind responded %v (%T), want an error", resp, resp)
	}
	if got := f.appliedIndex(); got != 5 {
		t.Fatalf("a poisoned apply at 5 left the tracked index at %d, so a reader would hang instead of failing", got)
	}
	if !errors.Is(err, ErrPoisonedJournal) {
		t.Fatalf("the apply response %v is not a wrapped ErrPoisonedJournal", err)
	}
	if !strings.Contains(err.Error(), "5") {
		t.Fatalf("the refusal %q does not name the log index it failed at", err)
	}
	if f.poison == nil {
		t.Fatal("the state machine recorded no poison, so later reads would not learn the journal is unusable")
	}
	// CONTROL: a clean apply through the same path records no poison and still
	// advances, so the assertions above are about the poison and not about Apply.
	clean := newFSM()
	if resp := clean.Apply(commandAt(t, 5, Entry{Kind: KindSet, Path: "heap/alpha"})); resp != nil {
		t.Fatalf("CONTROL FAILED: a clean apply responded %v, want nil", resp)
	}
	if clean.poison != nil {
		t.Fatalf("CONTROL FAILED: a clean apply recorded the poison %v", clean.poison)
	}
}

func commandAt(t *testing.T, index uint64, entry Entry) *raft.Log {
	t.Helper()

	return &raft.Log{Index: index, Type: raft.LogCommand, Data: mustEncode(t, entry)}
}
