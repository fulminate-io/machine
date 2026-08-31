package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/transport"
)

func TestOpenRefusesAnIncompleteConfigNamingTheField(t *testing.T) {
	mux := testMux(t)
	complete := Config{Flow: "flow-validate", LocalID: "n0", Mux: mux, Bootstrap: true}

	for _, testCase := range []struct {
		name  string
		cfg   Config
		field string
	}{
		{"no flow", Config{LocalID: "n0", Mux: mux}, "Flow"},
		{"no local id", Config{Flow: "flow-validate", Mux: mux}, "LocalID"},
		{"no mux", Config{Flow: "flow-validate", LocalID: "n0"}, "Mux"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			l, err := Open(testCase.cfg)
			if !errors.Is(err, ErrConfigIncomplete) {
				t.Fatalf("Open gave %v, want a wrapped ErrConfigIncomplete", err)
			}
			if l != nil {
				t.Fatal("Open returned a ledger alongside its refusal")
			}
			// The refusal NAMES the field, so an operator is not left diffing a
			// config against a doc comment to find which one is missing.
			if !strings.Contains(err.Error(), testCase.field) {
				t.Fatalf("the refusal %q does not name the missing field %s", err, testCase.field)
			}
		})
	}

	// CONTROL: the same config with every field present opens, so the refusals
	// above are about the missing field rather than about this config being
	// unusable for another reason.
	l := openTestLedger(t, complete)
	waitLeadership(t, l)
}

func TestFailedRaftStartupReleasesEverythingItOpened(t *testing.T) {
	mux := testMux(t)
	dir := t.TempDir()

	// raft refuses a SnapshotInterval below 5ms in its own config validation, so
	// this fails inside NewRaft — AFTER the group is bound and the bolt store is
	// open, which is the window the cleanup path exists for.
	l, err := Open(Config{
		Flow: "flow-startup-fail", LocalID: "n0", Mux: mux, Dir: dir, Bootstrap: true,
		tuning: func(c *raft.Config) { c.SnapshotInterval = time.Nanosecond },
	})
	if err == nil {
		_ = l.Close()
		t.Fatal("Open accepted a raft config raft itself refuses")
	}
	if l != nil {
		t.Fatal("Open returned a ledger alongside its error")
	}

	// Nothing leaked: the group id is free again...
	group, bindErr := mux.Bind(transport.GroupID("flow-startup-fail"))
	if bindErr != nil {
		t.Fatalf("the failed startup leaked its transport binding: %v", bindErr)
	}
	if err := group.Close(); err != nil {
		t.Fatalf("closing the probe binding: %v", err)
	}

	// ...and the bolt file is closed, so a fresh ledger can open the SAME Dir. A
	// leaked handle would fail here on bolt's own file lock.
	second := openTestLedger(t, Config{
		Flow: "flow-startup-fail", LocalID: "n0", Mux: mux, Dir: dir, Bootstrap: true,
	})
	waitLeadership(t, second)
}

func TestSaveRefusesAValueGobCannotEncode(t *testing.T) {
	l := openTestLedger(t, Config{Flow: "flow-unencodable", LocalID: "n0", Mux: testMux(t), Bootstrap: true})
	waitLeadership(t, l)
	store := l.Store()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// A map inside an interface is not one of gob's built-ins and was never
	// registered, so the WRITER fails loudly here rather than putting a value into
	// the journal that no reader could decode.
	before := l.raft.LastIndex()
	err := store.Save(ctx, "heap/unencodable", map[string]int{"a": 1})
	if err == nil {
		t.Fatal("Save accepted a value gob cannot encode; an undecodable value would have reached the journal")
	}
	if !strings.Contains(err.Error(), "encoding a heap value") {
		t.Fatalf("the refusal %q does not name the encode step", err)
	}
	if after := l.raft.LastIndex(); after != before {
		t.Fatalf("the refused Save still appended %d entries", after-before)
	}

	// CONTROL: a registered type saves through the same path.
	if err := store.Save(ctx, "heap/encodable", heapValue{Count: 1}); err != nil {
		t.Fatalf("CONTROL FAILED: saving a registered type: %v", err)
	}
}

func TestLoadReportsAValueItCannotDecodeRatherThanGuessing(t *testing.T) {
	l := openTestLedger(t, Config{Flow: "flow-undecodable", LocalID: "n0", Mux: testMux(t), Bootstrap: true})
	waitLeadership(t, l)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Journal bytes that are not a gob-encoded heap value. This is reachable in
	// practice by a peer running a build whose encoding has moved on.
	if _, err := l.Append(ctx, Entry{Kind: KindSet, Path: "heap/raw", Value: []byte("not a gob stream")}); err != nil {
		t.Fatalf("appending raw bytes: %v", err)
	}

	value, ok, err := l.Store().Load(ctx, "heap/raw")
	if err == nil {
		t.Fatal("Load returned no error for bytes it cannot decode")
	}
	// A failed read reports NOTHING: it did not answer, which is a different
	// outcome from answering that the path is absent.
	if value != nil || ok {
		t.Fatalf("a failed Load still returned %v present=%v", value, ok)
	}
	if !strings.Contains(err.Error(), "decoding a heap value") {
		t.Fatalf("the failure %q does not name the decode step", err)
	}

	// CONTROL: the entry really is in the journal — the ledger read it and failed
	// at decode, rather than the path being absent.
	if entry, present, getErr := l.Get(ctx, "heap/raw"); getErr != nil || !present || string(entry.Value) != "not a gob stream" {
		t.Fatalf("CONTROL FAILED: the raw entry is not in the journal (present=%v err=%v)", present, getErr)
	}
}

func TestBarrierReportsAPoisonedJournalUnderAReadTimeout(t *testing.T) {
	l := openTestLedger(t, Config{
		Flow: "flow-poisoned-read", LocalID: "n0", Mux: testMux(t), Bootstrap: true, ReadTimeout: 30 * time.Second,
	})
	waitLeadership(t, l)

	// Poison the state machine directly: this is the state a peer reaches when it
	// meets a journal entry its build cannot interpret.
	l.fsm.Apply(commandAt(t, l.raft.LastIndex()+1, Entry{Kind: Kind(200), Path: "heap/bad"}))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _, err := l.Get(ctx, "heap/anything")
	if !errors.Is(err, ErrPoisonedJournal) {
		t.Fatalf("a read on a poisoned ledger gave %v, want the poison", err)
	}
	// It reports the poison rather than waiting out the ReadTimeout.
	if errors.Is(err, ErrReadTimeout) {
		t.Fatalf("the read timed out (%v) instead of reporting the poison it could already see", err)
	}
}

func TestEnqueueTimeoutDerivesFromTheCallersDeadline(t *testing.T) {
	// No deadline means no bound, which is raft's own convention for this argument.
	if got := enqueueTimeout(context.Background()); got != 0 {
		t.Fatalf("a context with no deadline gave %v, want 0", got)
	}

	live, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if got := enqueueTimeout(live); got <= 0 || got > time.Minute {
		t.Fatalf("a live deadline gave %v, want a positive bound at or under a minute", got)
	}

	// An ALREADY-EXPIRED deadline must not read as "no bound", which is what a
	// plain subtraction would produce: it would turn an expired context into an
	// unbounded enqueue.
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancelExpired()
	if got := enqueueTimeout(expired); got <= 0 {
		t.Fatalf("an expired deadline gave %v, which raft reads as no timeout at all", got)
	}
}

func TestConfigLoggerDefaultsToDiscardingRatherThanNil(t *testing.T) {
	if logger := (Config{}).logger(); logger == nil {
		t.Fatal("a Config with no Logger produced a nil logger, which every log call would panic on")
	}
	named := hclog.New(&hclog.LoggerOptions{Name: "supplied"})
	if logger := (Config{Logger: named}).logger(); logger != named {
		t.Fatal("a supplied Logger was replaced")
	}
}
