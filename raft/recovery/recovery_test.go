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
	"sync"
	"testing"
	"time"

	"github.com/whitaker-io/machine/raft/checkpoint"
	"github.com/whitaker-io/machine/raft/ledger"
	"github.com/whitaker-io/machine/raft/membership"
	machine "github.com/whitaker-io/machine/v4"
)

func TestADeadWorkersDatumIsClaimedOnceAndCompleted(t *testing.T) {
	// A worker checkpointed a datum and then left the configuration. A survivor must
	// find it, take it, finish it from that checkpoint, and retire it.
	nodes := newGroup(t, "flow-dead", 3)
	leader := awaitLeader(t, nodes)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The dead worker's progress, journaled before it died.
	journalCheckpoint(t, leader.ledger, "n9", "datum-1", "halfway")

	// NO CLAIM EXISTS. A claim is a RECOVERY ownership record, taken by a survivor
	// when it picks work up; the runtime does not claim a datum it is merely
	// processing. So a worker that dies mid-flight leaves a checkpoint naming itself
	// as the writer and no claim at all, and the first survivor to claim it wins.

	survivors := newView(nodes[0].id, nodes[1].id, nodes[2].id)
	detector := New(leader.ledger, survivors, "flow-dead")

	orphans, err := detector.Orphans(ctx, "flow-dead")
	if err != nil {
		t.Fatalf("detecting orphans: %v", err)
	}
	if got := datums(orphans); !slices.Equal(got, []string{"datum-1"}) {
		t.Fatalf("the detector offered %v, want exactly the dead worker's datum", got)
	}

	// EXACTLY ONE survivor takes it.
	won, err := detector.Claim(ctx, "flow-dead", "datum-1")
	if err != nil {
		t.Fatalf("claiming the orphan: %v", err)
	}
	if !won {
		t.Fatal("the only claimant did not win the datum")
	}

	// It completes from its last checkpoint: the payload the dead worker journaled
	// is what the survivor resumes from.
	if len(orphans) != 1 || string(orphans[0].Data) != "halfway" {
		t.Fatalf("the offered record carries %q, want the dead worker's last checkpoint", orphans[0].Data)
	}
	if orphans[0].Owner != "n9" {
		t.Fatalf("the offered record names owner %q, want the dead worker n9", orphans[0].Owner)
	}
	t.Logf("the datum was re-executed from its last checkpoint by a surviving worker: %s resumed datum-1 from %q "+
		"(written by the departed %s)", nodes[0].id, orphans[0].Data, orphans[0].Owner)

	// And retiring it drops both halves, so it is never offered again.
	if err := detector.Retire(ctx, "flow-dead", "datum-1"); err != nil {
		t.Fatalf("retiring the completed datum: %v", err)
	}
	if _, present, err := leader.ledger.Get(ctx, checkpoint.Path("datum-1")); err != nil || present {
		t.Fatalf("after retirement the checkpoint is present=%v (err %v)", present, err)
	}
}

func TestTwoSurvivorsRacingForOneDatumProduceExactlyOneWinner(t *testing.T) {
	// COMPARE-AND-CLAIM THROUGH LOG ORDERING. Two survivors drive the same datum at
	// the same time through their OWN ledgers, so the race is settled by the
	// replicated log rather than by anything in one process.
	nodes := newGroup(t, "flow-race", 3)
	awaitLeader(t, nodes)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	view := newView(nodes[0].id, nodes[1].id, nodes[2].id)
	first := New(nodes[0].ledger, view, "flow-race")
	second := New(nodes[1].ledger, view, "flow-race")

	var wg sync.WaitGroup
	results := make([]bool, 2)
	errs := make([]error, 2)

	// EACH DETECTOR STAMPS ITS OWN LEDGER'S IDENTITY, so there is no owner to hand
	// in and no way for this test to model two nodes that the product does not also
	// take. That equivalence is the point: while the seam accepted an owner, this
	// test passed two distinct node ids and stayed green while the product passed
	// one shared machine name to every replica.
	wg.Add(2)
	for i, detector := range []*Detector{first, second} {
		go func() {
			defer wg.Done()
			results[i], errs[i] = detector.Claim(ctx, "flow-race", "datum-1")
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("claimant %d reported %v; a LOST race is reported as false, not as an error", i, err)
		}
	}

	winners := 0
	for _, won := range results {
		if won {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d of 2 racing survivors won the datum, want exactly 1: single-writer-per-datum is what the "+
			"whole design rests on", winners)
	}

	// The loser's refusal carries the claim sentinel underneath, which is what lets a
	// caller tell a lost race from an unclassified failure.
	_, err := nodes[2].ledger.Append(ctx, ledger.Entry{
		Kind: ledger.KindClaim, Path: checkpoint.ClaimPath("datum-1"), Value: []byte(nodes[2].id),
	})
	if !errors.Is(err, ledger.ErrClaimHeld) {
		t.Fatalf("a third claimant got %v, want ErrClaimHeld", err)
	}
}

func TestAnAlreadyClaimedOrphanIsNotOfferedASecondTime(t *testing.T) {
	// THE FILTER IS ONLY REAL IF IT READS THROUGH THE CLAIM ACCESSOR. A filter
	// written against the value read observes nothing — a held claim reports ABSENT
	// there — so it would drop nothing while every other leg still passed.
	nodes := newGroup(t, "flow-filter", 3)
	leader := awaitLeader(t, nodes)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	journalCheckpoint(t, leader.ledger, "n9", "datum-taken", "progress")
	journalCheckpoint(t, leader.ledger, "n9", "datum-free", "progress")

	// datum-taken is held by a survivor; datum-free is held by nobody. Both were
	// checkpointed by a worker that is gone.
	claimOwner(t, leader.ledger, "datum-taken", nodes[1].id)

	view := newView(nodes[0].id, nodes[1].id, nodes[2].id)
	detector := New(leader.ledger, view, "flow-filter")

	orphans, err := detector.Orphans(ctx, "flow-filter")
	if err != nil {
		t.Fatalf("detecting orphans: %v", err)
	}

	got := datums(orphans)
	if slices.Contains(got, "datum-taken") {
		t.Fatalf("the detector offered datum-taken, which is already claimed: %v", got)
	}
	if !slices.Contains(got, "datum-free") {
		t.Fatalf("the detector did not offer the UNCLAIMED datum-free: %v; the filter is dropping everything "+
			"rather than dropping what is held", got)
	}

	// THE DISCLOSURE: the claim really was readable. Without this an inert filter
	// that observed nothing would satisfy every assertion above.
	owner, held, err := leader.ledger.Claimant(ctx, checkpoint.ClaimPath("datum-taken"))
	if err != nil || !held {
		t.Fatalf("reading the claim back gave held=%v err=%v", held, err)
	}
	t.Logf("the already-claimed orphan was read back as claimed and dropped from the offer: "+
		"datum-taken is held by %q, offered set is %v", owner, got)
}

func TestAnIdempotentNodesDatumIsReRunAndAnUnmarkedNodesIsReInjectedPastIt(t *testing.T) {
	// BOTH ANCHORS, IN TWO SEPARATE FLOWS. They cannot share one: re-running a marked
	// node feeds its successor, so a chained pair would show the downstream node
	// running again for a reason that has nothing to do with its own anchor. A
	// single-anchor implementation fails exactly one of these whichever it chose.
	markedRuns, markedDelivered := anchorFlow(t, "flow-marked", true)
	unmarkedRuns, unmarkedDelivered := anchorFlow(t, "flow-unmarked", false)

	// The MARKED node ran again: its record is its INPUT and recovery hands it back.
	if markedRuns < 2 {
		t.Fatalf("the idempotent-marked node ran %d times, want it re-run on resume: an arrival record is the "+
			"node's input and the node runs again", markedRuns)
	}
	// The UNMARKED node did NOT: its record is its OUTPUT and resume re-injects past
	// it, which is what keeps a non-idempotent node's side effects from repeating.
	if unmarkedRuns != 1 {
		t.Fatalf("the unmarked node ran %d times, want exactly 1: a completion record is re-injected into the "+
			"SUCCESSORS and the node itself is never run again", unmarkedRuns)
	}
	// CONTROL: the unmarked flow's datum DID reach the successor on resume, so the
	// single run above is re-injection rather than a resume that did nothing at all.
	if unmarkedDelivered < 2 {
		t.Fatalf("the unmarked node's successor received %d datums, want the resumed one too; nothing was "+
			"re-injected and this arm proves nothing", unmarkedDelivered)
	}
	if markedDelivered < 2 {
		t.Fatalf("the marked node's successor received %d datums, want the resumed one too", markedDelivered)
	}

	t.Logf("the marked node re-ran and the unmarked node did not: marked ran %d times, unmarked ran %d; "+
		"both successors received %d and %d datums", markedRuns, unmarkedRuns, markedDelivered, unmarkedDelivered)
}

// anchorFlow runs one datum through a one-node flow, replays what was journaled, and
// reports how many times the node ran and how many datums reached its successor.
func anchorFlow(t *testing.T, name string, idempotent bool) (runs, delivered int) {
	t.Helper()

	journal := &recordingJournal{}
	m := machine.New(name, machine.OptionJournal(journal), machine.OptionFIFO)

	var mutex sync.Mutex
	ran := 0

	opts := []machine.NodeOption[string]{machine.WithCheckpoint[string](machine.GobCodec[string]{})}
	if idempotent {
		opts = append(opts, machine.WithIdempotent[string]())
	}

	src, ingest := m.Source[string]("src")
	worked := src.Map("worker", func(f machine.Frame[string]) string {
		mutex.Lock()
		ran++
		mutex.Unlock()

		return f.Value()
	}, opts...)
	out := worked.Output("sink", machine.WithCheckpoint[string](machine.GobCodec[string]{}))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("starting %s: %v", name, err)
	}
	if err := ingest(ctx, "datum"); err != nil {
		t.Fatalf("ingesting into %s: %v", name, err)
	}
	awaitOne(t, out)
	delivered++

	journal.replay()
	awaitOne(t, out)
	delivered++

	// Let the resumed traversal settle before sampling, so a node that runs again
	// slightly later is not read as one that never did.
	time.Sleep(500 * time.Millisecond)

	mutex.Lock()
	defer mutex.Unlock()

	return ran, delivered
}

func TestTheDuplicateWindowIsDisclosedNotMasked(t *testing.T) {
	// AT-LEAST-ONCE IS THE SEMANTIC, and this test ENTERS the window rather than
	// documenting one it never reached: work performed after the last checkpoint and
	// before the death is executed a SECOND time on resume.
	journal := &recordingJournal{}
	m := machine.New("flow-duplicate", machine.OptionJournal(journal), machine.OptionFIFO)

	var mutex sync.Mutex
	var performed []string

	src, ingest := m.Source[string]("src")
	worked := src.Map("worker", func(f machine.Frame[string]) string {
		mutex.Lock()
		performed = append(performed, f.Value())
		mutex.Unlock()

		return f.Value()
	}, machine.WithCheckpoint[string](machine.GobCodec[string]{}), machine.WithIdempotent[string]())
	out := worked.Output("sink", machine.WithCheckpoint[string](machine.GobCodec[string]{}))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("starting the flow: %v", err)
	}
	if err := ingest(ctx, "unit-of-work"); err != nil {
		t.Fatalf("ingesting: %v", err)
	}
	awaitOne(t, out)

	journal.replay()
	awaitOne(t, out)

	mutex.Lock()
	defer mutex.Unlock()
	if len(performed) < 2 {
		t.Fatalf("the work ran %d times, so this test never ENTERED the duplicate window it documents", len(performed))
	}
	t.Logf("work between the last checkpoint and the death was executed twice: %v", performed)
}

func TestARefusedCursorRebuildsFromMembershipRatherThanStalling(t *testing.T) {
	// A REFUSED CURSOR IS NOT AN ERROR TO RETRY. Both refusal kinds — the retention
	// overrun and the foreign incarnation — share one sentinel and one response:
	// rebuild from the committed membership and resume from zero.
	nodes := newGroup(t, "flow-cursor", 3)
	leader := awaitLeader(t, nodes)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	journalCheckpoint(t, leader.ledger, "n9", "datum-1", "progress")

	view := newView(nodes[0].id, nodes[1].id, nodes[2].id)
	// The FOREIGN INCARNATION form, which wraps the retention sentinel.
	view.err = membership.ErrCursorForeignIncarnation
	detector := New(leader.ledger, view, "flow-cursor")

	// It does not stall and it does not surface the refusal: the orphan is found on
	// the same round the cursor was refused.
	orphans, err := detector.Orphans(ctx, "flow-cursor")
	if err != nil {
		t.Fatalf("a refused cursor surfaced as an error instead of rebuilding: %v", err)
	}
	if got := datums(orphans); !slices.Equal(got, []string{"datum-1"}) {
		t.Fatalf("after rebuilding from membership the detector offered %v, want the orphan", got)
	}

	// CONTROL: a refusal that is NOT the cursor sentinel still reaches the caller,
	// so the arm above is a defined response rather than a blanket swallow. The datum
	// is first handed to a LIVE owner so nothing is offered and the round actually
	// reaches the membership read — with an orphan available it would return before
	// ever consulting the signal log, and the control would prove nothing.
	claimOwner(t, leader.ledger, "datum-1", nodes[0].id)

	other := newView(nodes[0].id, nodes[1].id, nodes[2].id)
	other.err = errors.New("the signal log is broken")

	bounded, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	if _, err := New(leader.ledger, other, "flow-cursor").Orphans(bounded, "flow-cursor"); err == nil {
		t.Fatal("CONTROL FAILED: an unrelated signal-log failure was swallowed too")
	}
}

func TestOrphansOnANonLeaderRefusesLoudlyRatherThanReturningNothing(t *testing.T) {
	// THE INERTNESS CLASS THIS PREVENTS. A leader-local claim read refuses on a
	// follower; a detector that swallowed that would return an empty set and a nil
	// error, which is indistinguishable from "no orphans" at every call site above
	// it — recovery reporting healthy while doing nothing.
	nodes := newGroup(t, "flow-follower", 3)
	leader := awaitLeader(t, nodes)
	follower := followerOf(t, nodes, leader)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	journalCheckpoint(t, leader.ledger, "n9", "datum-1", "progress")

	view := newView(nodes[0].id, nodes[1].id, nodes[2].id)

	orphans, err := New(follower.ledger, view, "flow-follower").Orphans(ctx, "flow-follower")
	if err == nil {
		t.Fatalf("a detector on a FOLLOWER returned %v and no error; an empty set with a nil error is "+
			"indistinguishable from 'no orphans' and leaves recovery silently inert", orphans)
	}
	if !errors.Is(err, ledger.ErrNotLeader) {
		t.Fatalf("the refusal %v does not wrap ErrNotLeader", err)
	}
	if orphans != nil {
		t.Fatalf("the refusal also returned %v; a refusal returns no orphan set at all", orphans)
	}
	if !strings.Contains(err.Error(), "flow-follower") {
		t.Errorf("the refusal %q does not name the flow it refused for", err)
	}

	// CONTROL: the very same view on the LEADER answers, so the refusal is about
	// leadership rather than about a detector that refuses everything.
	if _, err := New(leader.ledger, view, "flow-follower").Orphans(ctx, "flow-follower"); err != nil {
		t.Fatalf("CONTROL FAILED: the same detector on the leader also refused: %v", err)
	}
	t.Logf("the non-leader detector refused with ErrNotLeader and returned no orphan set: %v", err)
}

// claimOwner records a claim directly, standing in for a worker that took a datum.
func claimOwner(t *testing.T, l *ledger.Ledger, datum, owner string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := l.Append(ctx, ledger.Entry{
		Kind: ledger.KindClaim, Path: checkpoint.ClaimPath(datum), Value: []byte(owner),
	}); err != nil {
		t.Fatalf("claiming %s for %s: %v", datum, owner, err)
	}
}

// awaitOne reads one packet or fails.
func awaitOne[T any](t *testing.T, out <-chan machine.Packet[T]) machine.Packet[T] {
	t.Helper()

	select {
	case p := <-out:
		return p
	case <-time.After(30 * time.Second):
		t.Fatal("no packet left the flow within thirty seconds")

		return machine.Packet[T]{}
	}
}

// recordingJournal keeps what the runtime journaled and can offer it back once,
// which is what a survivor picking up a dead worker's datum observes.
type recordingJournal struct {
	mutex   sync.Mutex
	records []machine.CheckpointRecord
	offer   []machine.CheckpointRecord
	ready   chan struct{}
	once    sync.Once
}

func (j *recordingJournal) Checkpoint(_ context.Context, record machine.CheckpointRecord) error {
	j.mutex.Lock()
	defer j.mutex.Unlock()

	j.records = append(j.records, record)

	return nil
}

func (j *recordingJournal) Claim(context.Context, string, string) (bool, error) {
	return true, nil
}

func (j *recordingJournal) Retire(context.Context, string, string) error { return nil }

func (j *recordingJournal) Orphans(ctx context.Context, _ string) ([]machine.CheckpointRecord, error) {
	j.gate()
	select {
	case <-j.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// THE LOCK IS RELEASED BEFORE ANYTHING BLOCKS. Holding it across the park below
	// would wedge every Checkpoint the resumed node goes on to make — which is a
	// deadlock in this double rather than in the runtime, and it presents as a flow
	// that simply stops delivering.
	j.mutex.Lock()
	batch := j.offer
	j.offer = nil
	j.mutex.Unlock()

	if len(batch) == 0 {
		// Nothing more to replay: park until the machine ends rather than spinning.
		<-ctx.Done()

		return nil, ctx.Err()
	}

	return batch, nil
}

func (j *recordingJournal) gate() {
	j.once.Do(func() {
		j.mutex.Lock()
		defer j.mutex.Unlock()
		if j.ready == nil {
			j.ready = make(chan struct{})
		}
	})
}

// replay offers back everything journaled so far, once.
func (j *recordingJournal) replay() {
	j.gate()

	j.mutex.Lock()
	j.offer = append([]machine.CheckpointRecord(nil), j.records...)
	ready := j.ready
	j.mutex.Unlock()

	close(ready)
}

// AwaitLeadership returns at once: this recorder never withholds leadership.
func (j *recordingJournal) AwaitLeadership(context.Context, string) error { return nil }
