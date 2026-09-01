package membership

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/ledger"
)

// externalSnapshot mirrors the ledger's own snapshot payload BY FIELD NAME.
//
// That is what lets a test outside the ledger package mint an authoritative
// snapshot at all: gob matches on exported field names, so this encodes into
// exactly what the ledger's own Restore decodes. It is the real artifact rather
// than a fixture shaped like one.
type externalSnapshot struct {
	Values  map[string]ledger.Entry
	Applied uint64
}

// mintSnapshot renders an authoritative snapshot and the metadata raft needs to
// take it on.
func mintSnapshot(t *testing.T, r *raft.Raft, values map[string]ledger.Entry) (*raft.SnapshotMeta, io.Reader) {
	t.Helper()
	applied := r.LastIndex()
	var body bytes.Buffer
	if err := gob.NewEncoder(&body).Encode(externalSnapshot{Values: values, Applied: applied}); err != nil {
		t.Fatalf("encoding the snapshot: %v", err)
	}
	future := r.GetConfiguration()
	if err := future.Error(); err != nil {
		t.Fatalf("GetConfiguration: %v", err)
	}
	meta := &raft.SnapshotMeta{
		Version:            raft.SnapshotVersionMax,
		ID:                 fmt.Sprintf("external-%d", applied),
		Index:              applied,
		Term:               r.CurrentTerm(),
		Configuration:      future.Configuration(),
		ConfigurationIndex: future.Index(),
		Size:               int64(body.Len()),
	}
	return meta, bytes.NewReader(body.Bytes())
}

// poisonJournal replicates an entry whose kind this build does not declare,
// which is what the state machine refuses and records a poison for.
func poisonJournal(t *testing.T, l *ledger.Ledger) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// The append itself is expected to surface the refusal; what matters is that
	// the entry reached the log and the state machine recorded a poison for it.
	_, _ = l.Append(ctx, ledger.Entry{Kind: ledger.Kind(99), Path: "poison", Value: []byte("x")})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, err := l.Get(ctx, "anything"); errors.Is(err, ledger.ErrPoisonedJournal) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the journal never became poisoned, so the heal below would prove nothing")
}

func TestAPoisonedPeerHealsThroughADeliveredSnapshotAndReadsWithoutAnInterveningWrite(t *testing.T) {
	node := newClusterNode(t, "a-node", []string{"alpha"}, 0)
	node.start(t)
	node.awaitLeader(t, "alpha")
	l, ok := node.mgr.Ledger("alpha")
	if !ok {
		t.Fatal("the node does not host the flow")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := l.Append(ctx, ledger.Entry{Kind: ledger.KindSet, Path: "k", Value: []byte("before")}); err != nil {
		t.Fatalf("seeding the journal: %v", err)
	}
	poisonJournal(t, l)

	// AN AUTHORITATIVE SNAPSHOT CARRYING ONLY DECLARED KINDS, delivered through
	// THE DOOR the ledger publishes. Ledger.Restore, never Raft().Restore: the
	// raw handle restores the state machine and dispatches a trailing no-op no
	// state machine ever sees, leaving a healed journal that is READ-DEAD for the
	// rest of the term. The door appends the term's epoch after raft.Restore
	// returns, which orders it after that no-op by construction.
	meta, reader := mintSnapshot(t, l.Raft(), map[string]ledger.Entry{
		"k": {Kind: ledger.KindSet, Path: "k", Value: []byte("healed")},
	})
	if err := l.Restore(meta, reader, 30*time.Second); err != nil {
		t.Fatalf("Ledger.Restore: %v", err)
	}

	// NO INTERVENING WRITE. The shape is non-negotiable and stays even though the
	// arm is green: a restored ledger was measured to recover on a subsequent
	// write and on a new leadership term but never on its own, so a test that
	// wrote before reading would pass with the mechanism absent.
	entry, found, err := l.Get(ctx, "k")
	if err != nil {
		t.Fatalf("the healed ledger did not serve a linearizable read: %v — the restore healed the state and "+
			"left it read-dead", err)
	}
	if !found || string(entry.Value) != "healed" {
		t.Fatalf("the healed read returned found=%v value=%q, want the restored value", found, entry.Value)
	}
	t.Log("read taken with no intervening write after the delivered snapshot")
}

func TestADeliveredSnapshotCarryingAnUndeclaredKindPoisonsTheReceivingPeer(t *testing.T) {
	node := newClusterNode(t, "a-node", []string{"alpha"}, 0)
	node.start(t)
	node.awaitLeader(t, "alpha")
	l, ok := node.mgr.Ledger("alpha")
	if !ok {
		t.Fatal("the node does not host the flow")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// THE CONTROL: the journal is healthy first, so the poison below is one the
	// RESTORE earned rather than one inherited from the log.
	if _, err := l.Append(ctx, ledger.Entry{Kind: ledger.KindSet, Path: "k", Value: []byte("fine")}); err != nil {
		t.Fatalf("seeding the journal: %v", err)
	}
	if _, _, err := l.Get(ctx, "k"); err != nil {
		t.Fatalf("CONTROL FAILED: the journal was not readable before the restore: %v", err)
	}

	meta, reader := mintSnapshot(t, l.Raft(), map[string]ledger.Entry{
		"k":        {Kind: ledger.KindSet, Path: "k", Value: []byte("fine")},
		"undeclar": {Kind: ledger.Kind(99), Path: "undeclar", Value: []byte("x")},
	})
	if err := l.Restore(meta, reader, 30*time.Second); err != nil {
		t.Logf("Ledger.Restore reported: %v", err)
	}

	_, _, err := l.Get(ctx, "k")
	if !errors.Is(err, ledger.ErrPoisonedJournal) {
		t.Fatalf("err = %v, want a wrapped ErrPoisonedJournal: a kind arriving by SNAPSHOT is no more "+
			"interpretable than the same kind arriving by log", err)
	}
	// THE REFUSAL NAMES THE RESTORED ENTRY rather than a log index, which is what
	// distinguishes a poison the restore earned from one inherited from the log.
	if !bytes.Contains([]byte(err.Error()), []byte("restored entry")) {
		t.Fatalf("the refusal %q does not name a restored entry", err)
	}
	t.Logf("the receiving peer refuses with a poison naming a restored entry: %v", err)
}
