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

// resumeJournal hands out one batch of orphans and then parks, which is how the real
// journal behaves: Orphans BLOCKS until there is something to claim or the context
// ends, so the loop parks in that one call rather than reading a flag and then
// waiting on something taken separately.
type resumeJournal struct {
	mutex sync.Mutex

	offer     []CheckpointRecord
	offered   bool
	claimWon  bool
	claimErr  error
	orphanErr error

	claimed []string
	retired []string
	reports chan error
}

func newResumeJournal(offer []CheckpointRecord, won bool) *resumeJournal {
	return &resumeJournal{offer: offer, claimWon: won, reports: make(chan error, 16)}
}

func (j *resumeJournal) Checkpoint(context.Context, CheckpointRecord) error { return nil }

func (j *resumeJournal) Claim(_ context.Context, _, datum, owner string) (bool, error) {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	j.claimed = append(j.claimed, datum+"/"+owner)

	return j.claimWon, j.claimErr
}

func (j *resumeJournal) Retire(_ context.Context, _, datum string) error {
	j.mutex.Lock()
	j.retired = append(j.retired, datum)
	j.mutex.Unlock()

	return nil
}

func (j *resumeJournal) Orphans(ctx context.Context, _ string) ([]CheckpointRecord, error) {
	j.mutex.Lock()
	if j.orphanErr != nil {
		j.mutex.Unlock()

		return nil, j.orphanErr
	}
	if !j.offered {
		j.offered = true
		batch := j.offer
		j.mutex.Unlock()

		return batch, nil
	}
	j.mutex.Unlock()

	<-ctx.Done()

	return nil, ctx.Err()
}

func (j *resumeJournal) observed() ([]string, []string) {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	return append([]string(nil), j.claimed...), append([]string(nil), j.retired...)
}

// recordFor builds a record carrying a marshaled payload.
func recordFor(t *testing.T, node, anchor, datum, payload string) CheckpointRecord {
	t.Helper()

	// A packet carrying real frame state, so the record round-trips through the
	// codec the way a journaled one does.
	frame := newFrame("origin", payload, NewMemStore())
	data, err := GobCodec[string]{}.Marshal(packetOf(frame))
	if err != nil {
		t.Fatalf("marshaling the fixture record: %v", err)
	}

	return CheckpointRecord{Flow: "flow-resume", Datum: datum, Node: node, Anchor: anchor, Data: data}
}

func TestResumeHandsAnArrivalRecordBackToItsOwnNodeAndACompletionRecordToTheSuccessors(t *testing.T) {
	// THE TWO ANCHORS DIVERGE HERE, and getting it wrong is the whole point of
	// carrying the anchor on the record. An arrival record is the node's INPUT and
	// the node runs again; a completion record is its OUTPUT and the node is never
	// re-run.
	for _, arm := range []struct {
		name       string
		anchor     string
		node       string
		idempotent bool
		wantRan    bool
	}{
		{name: "arrival re-runs the node", anchor: AnchorArrival, node: "worker", idempotent: true, wantRan: true},
		{name: "completion skips the node", anchor: AnchorCompletion, node: "worker", idempotent: false, wantRan: false},
	} {
		t.Run(arm.name, func(t *testing.T) {
			journal := newResumeJournal([]CheckpointRecord{
				recordFor(t, arm.node, arm.anchor, "datum-1", "recovered"),
			}, true)

			var mutex sync.Mutex
			var ran []string

			m := New("flow-resume", OptionJournal(journal), OptionFIFO)
			src, _ := m.Source[string]("src")

			opts := []NodeOption[string]{WithCheckpoint[string](GobCodec[string]{})}
			if arm.idempotent {
				opts = append(opts, WithIdempotent[string]())
			}
			mapped := src.Map("worker", func(f Frame[string]) string {
				mutex.Lock()
				ran = append(ran, f.Value())
				mutex.Unlock()

				return f.Value()
			}, opts...)
			out := mapped.Output("sink", WithCheckpoint[string](GobCodec[string]{}))

			startFlow(t, m)

			// The datum arrives at the sink either way — what differs is whether it
			// passed THROUGH the node function on the way.
			packet := awaitPacket(t, out)
			if packet.Value() != "recovered" {
				t.Fatalf("the resumed datum carried %q, want the journaled payload", packet.Value())
			}

			mutex.Lock()
			defer mutex.Unlock()
			if arm.wantRan && len(ran) == 0 {
				t.Fatal("an ARRIVAL record did not re-run its node; an idempotent node's work is lost")
			}
			if !arm.wantRan && len(ran) != 0 {
				t.Fatalf("a COMPLETION record re-ran its node (%v); a non-idempotent node's side effects just happened twice", ran)
			}
		})
	}
}

func TestResumeIgnoresADatumAnotherWorkerWon(t *testing.T) {
	// A lost claim is the recovery protocol WORKING, not a failure: another survivor
	// won the datum. It is not reported and nothing is re-placed.
	journal := newResumeJournal([]CheckpointRecord{
		recordFor(t, "worker", AnchorArrival, "datum-1", "recovered"),
	}, false) // claim is LOST

	var mutex sync.Mutex
	var ran []string
	var reported []error

	m := New("flow-resume", OptionJournal(journal), OptionFIFO)
	src, _ := m.Source[string]("src")
	mapped := src.Map("worker", func(f Frame[string]) string {
		mutex.Lock()
		ran = append(ran, f.Value())
		mutex.Unlock()

		return f.Value()
	}, WithCheckpoint[string](GobCodec[string]{}), WithIdempotent[string](),
		WithErrorHandler[string](func(e NodeError[string]) {
			mutex.Lock()
			reported = append(reported, e.Err)
			mutex.Unlock()
		}))
	mapped.Output("sink", WithCheckpoint[string](GobCodec[string]{}))

	startFlow(t, m)

	// CONTROL: the claim was ATTEMPTED, so "nothing re-placed" is a lost race rather
	// than a resume loop that never ran at all.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if claimed, _ := journal.observed(); len(claimed) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	claimed, _ := journal.observed()
	if len(claimed) == 0 {
		t.Fatal("CONTROL FAILED: the resume loop never attempted a claim, so this test measured nothing")
	}

	mutex.Lock()
	defer mutex.Unlock()
	if len(ran) != 0 {
		t.Fatalf("a datum whose claim was LOST was re-placed anyway (%v); two workers are now running one datum", ran)
	}
	if len(reported) != 0 {
		t.Fatalf("a lost claim was reported as a failure: %v; losing a race is the protocol working", reported)
	}
}

func TestResumeReportsAFailedClaimAndAnUnreadableRecord(t *testing.T) {
	// A claim that ERRORED is a different fact from a claim that was lost, and an
	// undecodable record must fail loudly at the reader rather than poisoning the
	// flow with a packet nobody can rebuild.
	t.Run("claim error", func(t *testing.T) {
		sentinel := errors.New("the journal could not claim")
		journal := newResumeJournal([]CheckpointRecord{
			recordFor(t, "worker", AnchorArrival, "datum-1", "recovered"),
		}, false)
		journal.claimErr = sentinel

		seen := collectReports(t, journal, sentinel)
		if !strings.Contains(seen, "claiming datum") {
			t.Fatalf("the reported failure %q does not name the claim step", seen)
		}
	})

	t.Run("undecodable record", func(t *testing.T) {
		journal := newResumeJournal([]CheckpointRecord{
			{Flow: "flow-resume", Datum: "datum-1", Node: "worker", Anchor: AnchorArrival, Data: []byte("not gob")},
		}, true)

		seen := collectReports(t, journal, nil)
		if !strings.Contains(seen, "rebuilding the arrival record") {
			t.Fatalf("the reported failure %q does not name the rebuild step", seen)
		}
	})

	t.Run("orphans error", func(t *testing.T) {
		sentinel := errors.New("the journal refused the orphan read")
		journal := newResumeJournal(nil, true)
		journal.orphanErr = sentinel

		seen := collectReports(t, journal, sentinel)
		if !strings.Contains(seen, "reading orphans") {
			t.Fatalf("the reported failure %q does not name the orphan read", seen)
		}
	})
}

// collectReports stands a checkpointed flow on the journal and returns the first
// failure its handler saw.
func collectReports(t *testing.T, journal *resumeJournal, wrapped error) string {
	t.Helper()

	reports := make(chan error, 8)
	m := New("flow-resume", OptionJournal(journal), OptionFIFO,
		OptionErrorHandler(func(e NodeError[any]) {
			select {
			case reports <- e.Err:
			default:
			}
		}))
	src, _ := m.Source[string]("src")
	mapped := src.Map("worker", func(f Frame[string]) string { return f.Value() },
		WithCheckpoint[string](GobCodec[string]{}), WithIdempotent[string]())
	mapped.Output("sink", WithCheckpoint[string](GobCodec[string]{}))

	startFlow(t, m)

	select {
	case err := <-reports:
		if wrapped != nil && !errors.Is(err, wrapped) {
			t.Fatalf("the handler received %v, which does not wrap the journal's own error", err)
		}

		return err.Error()
	case <-time.After(10 * time.Second):
		t.Fatal("no failure reached the handler within ten seconds; the resume loop swallowed it")

		return ""
	}
}

func TestResumeIgnoresARecordThatNamesAnotherNode(t *testing.T) {
	// Orphans reports the whole flow's records, so every node's loop sees every
	// record. A loop that re-placed one naming a different node would inject a datum
	// into the wrong place in the graph entirely.
	journal := newResumeJournal([]CheckpointRecord{
		recordFor(t, "some-other-node", AnchorArrival, "datum-1", "not mine"),
	}, true)

	var mutex sync.Mutex
	var ran []string

	m := New("flow-resume", OptionJournal(journal), OptionFIFO)
	src, _ := m.Source[string]("src")
	mapped := src.Map("worker", func(f Frame[string]) string {
		mutex.Lock()
		ran = append(ran, f.Value())
		mutex.Unlock()

		return f.Value()
	}, WithCheckpoint[string](GobCodec[string]{}), WithIdempotent[string]())
	mapped.Output("sink", WithCheckpoint[string](GobCodec[string]{}))

	startFlow(t, m)
	time.Sleep(200 * time.Millisecond)

	// It was never even claimed: the filter runs BEFORE the claim, so a foreign
	// record costs no ownership round trip.
	if claimed, _ := journal.observed(); len(claimed) != 0 {
		t.Fatalf("a record naming another node was claimed anyway: %v", claimed)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(ran) != 0 {
		t.Fatalf("a record naming another node was re-placed here: %v", ran)
	}
}

func TestResumeReportsACompletionRecordItCannotRebuildOrDeliver(t *testing.T) {
	// The completion side has its own rebuild and its own send, and both fail
	// differently from the arrival side's. A resume that reported only the arrival
	// half would drop these in silence.
	t.Run("undecodable completion record", func(t *testing.T) {
		journal := newResumeJournal([]CheckpointRecord{
			{Flow: "flow-resume", Datum: "datum-1", Node: "worker", Anchor: AnchorCompletion, Data: []byte("not gob")},
		}, true)

		seen := collectReports(t, journal, nil)
		if !strings.Contains(seen, "rebuilding the completion record") {
			t.Fatalf("the reported failure %q does not name the completion rebuild step", seen)
		}
	})

	t.Run("undeliverable completion record", func(t *testing.T) {
		// The successor's edge refuses every send, so the re-injection fails.
		refused := errors.New("the successor's edge refused")
		journal := newResumeJournal([]CheckpointRecord{
			recordFor(t, "worker", AnchorCompletion, "datum-1", "recovered"),
		}, true)

		reports := make(chan error, 8)
		m := New("flow-resume", OptionJournal(journal), OptionFIFO,
			OptionErrorHandler(func(e NodeError[any]) {
				select {
				case reports <- e.Err:
				default:
				}
			}))
		src, _ := m.Source[string]("src")
		mapped := src.Map("worker", func(f Frame[string]) string { return f.Value() },
			WithCheckpoint[string](GobCodec[string]{}))
		mapped.Output("sink", WithCheckpoint[string](GobCodec[string]{}),
			WithEdge[string](func(string, Report) (Edge[string], error) {
				return &failingEdge[string]{ch: make(chan Packet[string]), err: refused}, nil
			}))

		startFlow(t, m)

		select {
		case err := <-reports:
			if !errors.Is(err, refused) {
				t.Fatalf("the handler received %v, want the edge's refusal", err)
			}
			if !strings.Contains(err.Error(), "re-injecting datum") {
				t.Fatalf("the reported failure %q does not name the re-injection", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("an undeliverable completion record reached no handler")
		}
	})

	t.Run("undeliverable arrival record", func(t *testing.T) {
		// The node's OWN edge refuses, so the re-run cannot be delivered.
		refused := errors.New("the node's edge refused")
		journal := newResumeJournal([]CheckpointRecord{
			recordFor(t, "worker", AnchorArrival, "datum-1", "recovered"),
		}, true)

		reports := make(chan error, 8)
		m := New("flow-resume", OptionJournal(journal), OptionFIFO,
			OptionErrorHandler(func(e NodeError[any]) {
				select {
				case reports <- e.Err:
				default:
				}
			}))
		src, _ := m.Source[string]("src")
		mapped := src.Map("worker", func(f Frame[string]) string { return f.Value() },
			WithCheckpoint[string](GobCodec[string]{}), WithIdempotent[string](),
			WithEdge[string](func(string, Report) (Edge[string], error) {
				return &failingEdge[string]{ch: make(chan Packet[string]), err: refused}, nil
			}))
		mapped.Output("sink", WithCheckpoint[string](GobCodec[string]{}))

		startFlow(t, m)

		select {
		case err := <-reports:
			if !errors.Is(err, refused) {
				t.Fatalf("the handler received %v, want the edge's refusal", err)
			}
			if !strings.Contains(err.Error(), "re-running datum") {
				t.Fatalf("the reported failure %q does not name the re-run", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("an undeliverable arrival record reached no handler")
		}
	})
}

func TestADatumLeavingTheFlowIsRetiredAtBothTerminals(t *testing.T) {
	// Retirement fires where a datum LEAVES the flow, and there are two such places.
	// It fires unconditionally rather than being gated on whether a checkpoint was
	// ever written, because retiring a datum that was never checkpointed is a no-op
	// and gating it would need per-datum state this path does not have.
	for _, arm := range []struct {
		name     string
		terminal string
	}{
		{name: "drop", terminal: "drop"},
		{name: "output", terminal: "output"},
	} {
		t.Run(arm.name, func(t *testing.T) {
			journal := newResumeJournal(nil, true)
			m := New("flow-retire", OptionJournal(journal), OptionFIFO)
			src, ingest := m.Source[string]("src")

			var out <-chan Packet[string]
			if arm.terminal == "drop" {
				src.Drop("sink")
			} else {
				out = src.Output("sink")
			}

			ctx := startFlow(t, m)
			if err := ingest(ctx, "leaving"); err != nil {
				t.Fatalf("ingesting: %v", err)
			}
			if out != nil {
				awaitPacket(t, out)
			}

			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if _, retired := journal.observed(); len(retired) > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			_, retired := journal.observed()
			if len(retired) == 0 {
				t.Fatalf("a datum that left the flow through %s was never retired; its record and claim outlive it", arm.terminal)
			}
		})
	}
}

func TestTheRetiringOutputChannelStopsWhenTheMachineDoes(t *testing.T) {
	// A caller that stops reading must not pin the forwarding goroutine past the
	// machine's own life. The datum is retired and then offered; if nobody takes it
	// and the machine ends, the goroutine leaves rather than parking forever.
	journal := newResumeJournal(nil, true)
	m := New("flow-retire-shutdown", OptionJournal(journal), OptionFIFO)
	src, ingest := m.Source[string]("src")
	out := src.Output("sink")

	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("starting the machine: %v", err)
	}
	if err := ingest(ctx, "unread"); err != nil {
		t.Fatalf("ingesting: %v", err)
	}

	// The datum is retired on its way out even though the caller never reads it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, retired := journal.observed(); len(retired) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, retired := journal.observed(); len(retired) == 0 {
		t.Fatal("CONTROL FAILED: the unread datum was never retired, so the goroutine never reached the handoff and this test measures nothing")
	}

	cancel()

	// GIVE THE GOROUTINE TIME TO LEAVE WITH NO READER PRESENT. This is the whole
	// point: a reader arriving here would make the handoff ready too, and the select
	// could take either arm. With nobody reading, the ended context is the only ready
	// case, so the goroutine takes it and closes the channel on its way out.
	time.Sleep(500 * time.Millisecond)

	select {
	case _, open := <-out:
		if open {
			t.Fatal("the forwarding channel delivered a packet after the machine ended rather than closing")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the forwarding channel did not close after the machine ended; its goroutine is parked on a caller that stopped reading")
	}
}

func TestRetirementReportsAJournalFailure(t *testing.T) {
	// A retire that did not land leaves a completed datum re-claimable, so it must be
	// visible rather than dropped.
	sentinel := errors.New("the journal refused the retire")
	journal := &failingRetireJournal{err: sentinel}

	reports := make(chan error, 8)
	m := New("flow-retire-fail", OptionJournal(journal), OptionFIFO,
		OptionErrorHandler(func(e NodeError[any]) {
			select {
			case reports <- e.Err:
			default:
			}
		}))
	src, ingest := m.Source[string]("src")
	src.Drop("sink")

	ctx := startFlow(t, m)
	if err := ingest(ctx, "leaving"); err != nil {
		t.Fatalf("ingesting: %v", err)
	}

	select {
	case err := <-reports:
		if !errors.Is(err, sentinel) {
			t.Fatalf("the handler received %v, which does not wrap the journal's error", err)
		}
		if !strings.Contains(err.Error(), "retiring datum") {
			t.Fatalf("the reported failure %q does not name the retire step", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a failed retire reached no handler; a completed datum stays re-claimable in silence")
	}
}

// failingRetireJournal refuses every retire and parks every orphan read.
type failingRetireJournal struct{ err error }

func (j *failingRetireJournal) Checkpoint(context.Context, CheckpointRecord) error { return nil }
func (j *failingRetireJournal) Claim(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (j *failingRetireJournal) Retire(context.Context, string, string) error { return j.err }
func (j *failingRetireJournal) Orphans(ctx context.Context, _ string) ([]CheckpointRecord, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}

// AwaitLeadership returns at once: this fake never withholds leadership.
func (j *resumeJournal) AwaitLeadership(context.Context, string) error { return nil }

// AwaitLeadership returns at once: this fake never withholds leadership.
func (j *failingRetireJournal) AwaitLeadership(context.Context, string) error { return nil }
