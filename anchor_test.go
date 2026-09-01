// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// orderedJournal records the ORDER of journal calls against the node functions
// running beside them, which is the only instrument that can separate the two
// anchors: on three of the four runner kinds the received and produced payloads are
// the same value by construction, so a payload comparison cannot tell them apart.
type orderedJournal struct {
	mutex    sync.Mutex
	events   []string
	records  []CheckpointRecord
	failWith error
}

func (j *orderedJournal) Checkpoint(_ context.Context, record CheckpointRecord) error {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	j.events = append(j.events, "journal:"+record.Anchor)
	j.records = append(j.records, record)

	return j.failWith
}

func (j *orderedJournal) Claim(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (j *orderedJournal) Retire(context.Context, string, string) error { return nil }
func (j *orderedJournal) Orphans(ctx context.Context, _ string) ([]CheckpointRecord, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}

// note records a node function running, into the same ordered log the journal writes.
func (j *orderedJournal) note(event string) {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	j.events = append(j.events, event)
}

func (j *orderedJournal) observed() ([]string, []CheckpointRecord) {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	return append([]string(nil), j.events...), append([]CheckpointRecord(nil), j.records...)
}

// awaitPacket reads one packet or fails, so a wiring mistake reports itself instead
// of hanging the suite.
func awaitPacket[T any](t *testing.T, out <-chan Packet[T]) Packet[T] {
	t.Helper()

	select {
	case p := <-out:
		return p
	case <-time.After(10 * time.Second):
		t.Fatal("no packet left the flow within ten seconds")

		return Packet[T]{}
	}
}

// startFlow starts a machine and registers its shutdown.
func startFlow(t *testing.T, m *Machine) context.Context {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := m.Start(ctx); err != nil {
		t.Fatalf("starting the machine: %v", err)
	}

	return ctx
}

func TestAnIdempotentNodeJournalsBeforeItsFunctionRuns(t *testing.T) {
	journal := &orderedJournal{}
	m := New("flow-arrival", OptionJournal(journal), OptionFIFO)

	src, ingest := m.Source[string]("src")
	// MARKED, so the anchor is ARRIVAL. The function CHANGES THE VALUE, which is what
	// makes the record's payload evidence at all: with an identity function the two
	// anchors journal identical bytes and no arm could discriminate.
	mapped := src.Map("worker", func(f Frame[string]) string {
		journal.note("fn")

		return "PRODUCED:" + f.Value()
	}, WithCheckpoint[string](GobCodec[string]{}), WithIdempotent[string]())
	out := mapped.Output("sink")

	ctx := startFlow(t, m)
	if err := ingest(ctx, "received"); err != nil {
		t.Fatalf("ingesting: %v", err)
	}
	awaitPacket(t, out)

	events, records := journal.observed()
	if len(records) != 1 {
		t.Fatalf("the marked node wrote %d records, want exactly 1: %+v", len(records), events)
	}
	if records[0].Anchor != AnchorArrival {
		t.Fatalf("the marked node journaled at anchor %q, want %q", records[0].Anchor, AnchorArrival)
	}

	// THE ORDER IS THE ASSERTION.
	if len(events) < 2 || events[0] != "journal:arrival" || events[1] != "fn" {
		t.Fatalf("the observed order was %v, want the journal call BEFORE the node function ran", events)
	}
	t.Logf("the marked node journaled BEFORE its function ran: observed order %v", events)

	// The record holds the node's INPUT, decoded rather than assumed.
	packet, err := GobCodec[string]{}.Unmarshal(records[0].Data)
	if err != nil {
		t.Fatalf("decoding the arrival record: %v", err)
	}
	if packet.Value() != "received" {
		t.Fatalf("the arrival record decoded to %q, want the node's INPUT %q", packet.Value(), "received")
	}
	if packet.Value() == "PRODUCED:received" {
		t.Fatal("the arrival record holds the PRODUCED value, so it was written on the wrong side of the node function")
	}
	t.Log("the node function changed the value, so received and produced are distinguishable")
}

func TestAnUnmarkedNodeJournalsAfterItsFunctionReturns(t *testing.T) {
	journal := &orderedJournal{}
	m := New("flow-completion", OptionJournal(journal), OptionFIFO)

	src, ingest := m.Source[string]("src")
	// UNMARKED, so the anchor is COMPLETION, which is the default.
	mapped := src.Map("worker", func(f Frame[string]) string {
		journal.note("fn")

		return "PRODUCED:" + f.Value()
	}, WithCheckpoint[string](GobCodec[string]{}))
	// The successor supplies the codec the completion record is marshaled with.
	out := mapped.Output("sink", WithCheckpoint[string](GobCodec[string]{}))

	ctx := startFlow(t, m)
	if err := ingest(ctx, "received"); err != nil {
		t.Fatalf("ingesting: %v", err)
	}
	awaitPacket(t, out)

	events, records := journal.observed()
	if len(records) != 1 {
		t.Fatalf("the unmarked node wrote %d records, want exactly 1: %+v", len(records), events)
	}
	if records[0].Anchor != AnchorCompletion {
		t.Fatalf("the unmarked node journaled at anchor %q, want %q", records[0].Anchor, AnchorCompletion)
	}

	if len(events) < 2 || events[0] != "fn" || events[1] != "journal:completion" {
		t.Fatalf("the observed order was %v, want the journal call AFTER the node function returned", events)
	}
	t.Logf("the unmarked node journaled AFTER its function returned: observed order %v", events)

	packet, err := GobCodec[string]{}.Unmarshal(records[0].Data)
	if err != nil {
		t.Fatalf("decoding the completion record: %v", err)
	}
	if packet.Value() != "PRODUCED:received" {
		t.Fatalf("the completion record decoded to %q, want the node's OUTPUT", packet.Value())
	}
	t.Log("the node function changed the value, so received and produced are distinguishable")
}

func TestTheAnchorHoldsOnANodeThatForwardsRatherThanTransforms(t *testing.T) {
	// THIS ARM EXISTS SO A COMPLETION ANCHOR WIRED ONLY AT THE PRODUCED-VALUE SITE
	// CANNOT PASS. A route forwards the frame it received and produces nothing new,
	// so an implementation that journals only in worker.transform writes nothing here
	// while every other arm stays green.
	journal := &orderedJournal{}
	m := New("flow-route", OptionJournal(journal), OptionFIFO)

	src, ingest := m.Source[string]("src")
	left, right := src.If("router", func(f Frame[string]) bool {
		journal.note("fn")

		return strings.HasPrefix(f.Value(), "yes")
	}, WithCheckpoint[string](GobCodec[string]{}))

	// BOTH branches supply a codec: a completion-anchored router journals down
	// whichever branch the filter chose, so either could be the one that runs.
	taken := left.Output("taken", WithCheckpoint[string](GobCodec[string]{}))
	right.Output("untaken", WithCheckpoint[string](GobCodec[string]{}))

	ctx := startFlow(t, m)
	if err := ingest(ctx, "yes-please"); err != nil {
		t.Fatalf("ingesting: %v", err)
	}
	awaitPacket(t, taken)

	events, records := journal.observed()
	if len(records) != 1 {
		t.Fatalf("the router wrote %d records, want exactly 1: %+v", len(records), events)
	}
	if records[0].Anchor != AnchorCompletion {
		t.Fatalf("the router journaled at anchor %q, want %q", records[0].Anchor, AnchorCompletion)
	}
	// A COMPLETION RECORD NAMES THE EMITTER IT WAS WRITTEN AT, which on a branching
	// node is the branch rather than the bare node name. That is what makes the
	// record self-placing on resume: a route journals at whichever branch its filter
	// chose, and a record naming only "router" could not say which outlet to
	// re-inject it into.
	if records[0].Node != "router"+leftSuffix {
		t.Fatalf("the record names node %q, want the branch the filter chose", records[0].Node)
	}
	if len(events) < 2 || events[0] != "fn" || events[1] != "journal:completion" {
		t.Fatalf("the observed order was %v, want the journal call AFTER the filter ran", events)
	}
	t.Logf("the anchor was observed on a node kind that forwards rather than produces: order %v on a route", events)
}

func TestAJournalFailureIsReportedThroughTheNodesErrorHandler(t *testing.T) {
	// A checkpoint that did not land must be VISIBLE. It routes through the same
	// funnel every node failure passes, so it reaches the node's typed handler; the
	// datum still proceeds, because refusing it would convert a durability failure
	// into a liveness failure, and that trade is the caller's.
	sentinel := errors.New("the journal refused")

	for _, arm := range []struct {
		name       string
		idempotent bool
		anchor     string
	}{
		{name: "arrival", idempotent: true, anchor: AnchorArrival},
		{name: "completion", idempotent: false, anchor: AnchorCompletion},
	} {
		t.Run(arm.name, func(t *testing.T) {
			journal := &orderedJournal{failWith: sentinel}

			var mutex sync.Mutex
			var seen []error

			m := New("flow-failure-"+arm.name, OptionJournal(journal), OptionFIFO)
			src, ingest := m.Source[string]("src")

			opts := []NodeOption[string]{
				WithCheckpoint[string](GobCodec[string]{}),
				WithErrorHandler[string](func(e NodeError[string]) {
					mutex.Lock()
					seen = append(seen, e.Err)
					mutex.Unlock()
				}),
			}
			if arm.idempotent {
				opts = append(opts, WithIdempotent[string]())
			}

			mapped := src.Map("worker", func(f Frame[string]) string { return f.Value() }, opts...)
			out := mapped.Output("sink", WithCheckpoint[string](GobCodec[string]{}))

			ctx := startFlow(t, m)
			if err := ingest(ctx, "received"); err != nil {
				t.Fatalf("ingesting: %v", err)
			}
			// The datum still traverses: a journal failure is reported, not fatal.
			awaitPacket(t, out)

			mutex.Lock()
			defer mutex.Unlock()
			if len(seen) == 0 {
				t.Fatalf("the %s journal failure reached no handler; a checkpoint that did not land is invisible", arm.anchor)
			}
			if !errors.Is(seen[0], sentinel) {
				t.Fatalf("the handler received %v, which does not wrap the journal's own error", seen[0])
			}
			if !strings.Contains(seen[0].Error(), "worker") {
				t.Fatalf("the reported failure %q does not name the node it happened on", seen[0])
			}
		})
	}
}

// failingCodec refuses to marshal, which is the other way a checkpoint fails to
// land: not the journal rejecting it, but the payload never becoming bytes.
type failingCodec[T any] struct{ err error }

func (c failingCodec[T]) Marshal(Packet[T]) ([]byte, error)   { return nil, c.err }
func (c failingCodec[T]) Unmarshal([]byte) (Packet[T], error) { return Packet[T]{}, c.err }

func TestAMarshalFailureIsReportedRatherThanJournalingNothing(t *testing.T) {
	// A record that could not be MARSHALED is as unrecoverable as one the journal
	// refused, and it must be as visible. Silently writing nothing would leave the
	// author believing the datum was checkpointed.
	sentinel := errors.New("the codec refused")

	for _, arm := range []struct {
		name       string
		idempotent bool
	}{
		{name: "arrival", idempotent: true},
		{name: "completion", idempotent: false},
	} {
		t.Run(arm.name, func(t *testing.T) {
			journal := &orderedJournal{}

			var mutex sync.Mutex
			var seen []error

			m := New("flow-marshal-"+arm.name, OptionJournal(journal), OptionFIFO)
			src, ingest := m.Source[string]("src")

			// On ARRIVAL the node marshals with its OWN codec; on COMPLETION it
			// marshals with its SUCCESSOR's. So the failing codec goes on whichever
			// of the two the arm exercises.
			nodeCodec := Codec[string](GobCodec[string]{})
			sinkCodec := Codec[string](GobCodec[string]{})
			if arm.idempotent {
				nodeCodec = failingCodec[string]{err: sentinel}
			} else {
				sinkCodec = failingCodec[string]{err: sentinel}
			}

			opts := []NodeOption[string]{
				WithCheckpoint[string](nodeCodec),
				WithErrorHandler[string](func(e NodeError[string]) {
					mutex.Lock()
					seen = append(seen, e.Err)
					mutex.Unlock()
				}),
			}
			if arm.idempotent {
				opts = append(opts, WithIdempotent[string]())
			}

			mapped := src.Map("worker", func(f Frame[string]) string { return f.Value() }, opts...)
			out := mapped.Output("sink", WithCheckpoint[string](sinkCodec))

			ctx := startFlow(t, m)
			if err := ingest(ctx, "received"); err != nil {
				t.Fatalf("ingesting: %v", err)
			}
			awaitPacket(t, out)

			mutex.Lock()
			defer mutex.Unlock()
			if len(seen) == 0 {
				t.Fatal("a marshal failure reached no handler; the datum was silently not checkpointed")
			}
			if !errors.Is(seen[0], sentinel) {
				t.Fatalf("the handler received %v, which does not wrap the codec's own error", seen[0])
			}
			if !strings.Contains(seen[0].Error(), "marshaling") {
				t.Fatalf("the reported failure %q does not say the marshal step failed", seen[0])
			}

			// CONTROL: nothing was journaled, so the record really is absent rather
			// than written from bytes the codec never produced.
			if _, records := journal.observed(); len(records) != 0 {
				t.Fatalf("a failed marshal still wrote %d records", len(records))
			}
		})
	}
}

func TestACheckpointDeclaredWithNoCodecOrNoJournalIsRefused(t *testing.T) {
	// Both are declaration-time programmer errors, and both error rather than being
	// defaulted: a substituted codec would journal bytes the reading build may not
	// decode, and a machine with no journal has nowhere to put the record at all.
	t.Run("nil codec", func(t *testing.T) {
		m := New("flow-nilcodec", OptionJournal(&orderedJournal{}))
		src, _ := m.Source[string]("src")
		mapped := src.Map("worker", func(f Frame[string]) string { return f.Value() },
			WithCheckpoint[string](nil))
		mapped.Output("sink", WithCheckpoint[string](GobCodec[string]{}))

		err := m.Start(context.Background())
		if err == nil {
			t.Fatal("a checkpoint declared with a nil codec was accepted")
		}
		for _, want := range []string{"worker", "nil codec"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the refusal %q does not name %q", err, want)
			}
		}
	})

	t.Run("no journal", func(t *testing.T) {
		// CONTROL for the arm above: the same declaration with a VALID codec still
		// refuses here, so this arm is about the missing journal specifically.
		m := New("flow-nojournal")
		src, _ := m.Source[string]("src")
		mapped := src.Map("worker", func(f Frame[string]) string { return f.Value() },
			WithCheckpoint[string](GobCodec[string]{}))
		mapped.Output("sink", WithCheckpoint[string](GobCodec[string]{}))

		err := m.Start(context.Background())
		if err == nil {
			t.Fatal("a checkpointed node on a machine with no journal was accepted")
		}
		for _, want := range []string{"worker", "no journal", "OptionJournal"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the refusal %q does not name %q", err, want)
			}
		}
	})
}

func TestACheckpointedMapJournalsItsProducedValueThroughTheSuccessorsCodec(t *testing.T) {
	// THE TYPES ARE DELIBERATELY DIFFERENT. A Map from string to int cannot marshal
	// its own output with its own codec, because a node's options carry its INPUT
	// type. The codec that CAN is its successor's, whose input type IS the produced
	// type — which is the whole resolution this test pins.
	journal := &orderedJournal{}
	m := New("flow-codec", OptionJournal(journal), OptionFIFO)

	src, ingest := m.Source[string]("src")
	mapped := src.Map("mapper", func(f Frame[string]) int {
		return len(f.Value())
	}, WithCheckpoint[string](GobCodec[string]{}))
	out := mapped.Output("sink", WithCheckpoint[int](GobCodec[int]{}))

	ctx := startFlow(t, m)
	if err := ingest(ctx, "twelve chars"); err != nil {
		t.Fatalf("ingesting: %v", err)
	}
	awaitPacket(t, out)

	_, records := journal.observed()
	if len(records) != 1 {
		t.Fatalf("the mapper wrote %d records, want exactly 1", len(records))
	}
	if records[0].Anchor != AnchorCompletion {
		t.Fatalf("the mapper journaled at anchor %q, want completion", records[0].Anchor)
	}

	// THE BYTES ARE DECODED, not merely counted. A completion anchor journaling the
	// node's INPUT is type-clean and would satisfy any leg that only checked a
	// journal happened — and that is the resolution this design rejects. Decoding
	// with the INT codec is itself the proof the successor's codec was used: the
	// input bytes were written by a string codec and would not decode here.
	packet, err := GobCodec[int]{}.Unmarshal(records[0].Data)
	if err != nil {
		t.Fatalf("decoding the completion record with the SUCCESSOR's codec: %v; the record was marshaled with the wrong codec", err)
	}
	if packet.Value() != len("twelve chars") {
		t.Fatalf("the record decoded to %d, want the PRODUCED value %d", packet.Value(), len("twelve chars"))
	}
	t.Logf("the journaled bytes decoded to the value the node PRODUCED, through the successor's codec: %d", packet.Value())
}

func TestACheckpointedNodeWhoseSuccessorDeclaresNoCodecIsRefusedAtStart(t *testing.T) {
	// A completion-anchored node with no successor codec has nothing to marshal
	// with. That must refuse OBSERVABLY rather than silently skipping the journal,
	// because a checkpoint the author declared and never got is exactly the datum
	// they believed was recoverable.
	journal := &orderedJournal{}
	m := New("flow-refusal", OptionJournal(journal), OptionFIFO)

	src, ingest := m.Source[string]("src")
	mapped := src.Map("mapper", func(f Frame[string]) int {
		return len(f.Value())
	}, WithCheckpoint[string](GobCodec[string]{}))
	mapped.Output("sink") // declares NO codec

	_ = ingest
	err := m.Start(context.Background())
	if err == nil {
		t.Fatal("Start accepted a completion-anchored checkpoint whose successor declares no codec; the journal would silently write nothing")
	}

	message := err.Error()
	for _, want := range []string{"mapper", "sink", "WithCheckpoint", "idempotent"} {
		if !strings.Contains(message, want) {
			t.Fatalf("the refusal %q does not name %q; an author told only that something is wrong cannot act on it", message, want)
		}
	}
	t.Logf("Start refused a checkpointed node whose successor declares no codec, naming node, successor and fix: %v", err)
}

func TestTheCompletionCodecBindsWhenTheSuccessorIsDeclared(t *testing.T) {
	// THE TIMING IS WHAT MAKES THE DESIGN POSSIBLE AT ALL. An emitter is bound only
	// once the downstream node is declared, so the completion codec cannot be read at
	// node construction — an implementation that tried would find nothing there. This
	// observes the before and the after rather than only the end state.
	journal := &orderedJournal{}
	m := New("flow-timing", OptionJournal(journal), OptionFIFO)

	src, ingest := m.Source[string]("src")
	mapped := src.Map("mapper", func(f Frame[string]) int {
		return len(f.Value())
	}, WithCheckpoint[string](GobCodec[string]{}))

	// BEFORE: the successor does not exist yet.
	if mapped.out.codec != nil {
		t.Fatal("the emitter carried a codec before its consumer was declared; the codec cannot come from the producing node")
	}
	if mapped.out.consumer != "" {
		t.Fatalf("the emitter names consumer %q before one was declared", mapped.out.consumer)
	}

	out := mapped.Output("sink", WithCheckpoint[int](GobCodec[int]{}))

	// AFTER: declaring the successor bound its codec onto the producer's emitter.
	if mapped.out.codec == nil {
		t.Fatal("declaring the successor left the emitter with no codec, so a completion checkpoint has nothing to marshal with")
	}
	if mapped.out.consumer != "sink" {
		t.Fatalf("the emitter names consumer %q, want sink", mapped.out.consumer)
	}
	t.Log("the codec was absent at node construction and present after the successor was declared")

	// And the bound codec is the one that actually gets used.
	ctx := startFlow(t, m)
	if err := ingest(ctx, "twelve chars"); err != nil {
		t.Fatalf("ingesting: %v", err)
	}
	awaitPacket(t, out)

	_, records := journal.observed()
	if len(records) != 1 {
		t.Fatalf("the mapper wrote %d records, want exactly 1", len(records))
	}
	if _, err := (GobCodec[int]{}).Unmarshal(records[0].Data); err != nil {
		t.Fatalf("the record does not decode with the successor's codec: %v", err)
	}
}
