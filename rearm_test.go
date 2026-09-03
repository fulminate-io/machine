// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// leadershipJournal refuses a scripted set of Orphans rounds with ErrNotLeader and
// releases AwaitLeadership only when the script says this node now leads. It is the
// shape a node takes as leadership moves to it, away from it, and back.
type leadershipJournal struct {
	mu       sync.Mutex
	calls    int
	refuse   map[int]bool // Orphans round -> refuse with ErrNotLeader
	fatalOn  int          // Orphans round -> refuse with a NON-leadership error
	offerOn  map[int]CheckpointRecord
	claimed  []string
	awaited  int
	awaitErr error
}

func (j *leadershipJournal) Checkpoint(context.Context, CheckpointRecord) error { return nil }
func (j *leadershipJournal) Retire(context.Context, string, string) error       { return nil }

func (j *leadershipJournal) Claim(_ context.Context, _, datum, owner string) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.claimed = append(j.claimed, datum+"/"+owner)
	return true, nil
}

func (j *leadershipJournal) Orphans(ctx context.Context, flow string) ([]CheckpointRecord, error) {
	j.mu.Lock()
	j.calls++
	n := j.calls
	refuse, fatal := j.refuse[n], n == j.fatalOn
	record, offering := j.offerOn[n]
	j.mu.Unlock()

	switch {
	case fatal:
		return nil, fmt.Errorf("journaling flow %q: %w", flow, errors.New("disk is gone"))
	case refuse:
		return nil, fmt.Errorf("recovery: detection on flow %q runs on the leader: %w", flow, ErrNotLeader)
	case offering:
		return []CheckpointRecord{record}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (j *leadershipJournal) AwaitLeadership(ctx context.Context, _ string) error {
	j.mu.Lock()
	j.awaited++
	err := j.awaitErr
	j.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-time.After(5 * time.Millisecond):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (j *leadershipJournal) observed() (calls, awaited int, claimed []string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.calls, j.awaited, append([]string(nil), j.claimed...)
}

// runResume wires a one-node checkpointed flow around a journal and lets its resume
// loop run. THE LOOP IS THE SUBJECT; nothing here calls resume directly.
func runResume(t *testing.T, journal *leadershipJournal) (int, int, []string) {
	t.Helper()
	m := New("flow-rearm", OptionJournal(journal), OptionFIFO)
	src, _ := m.Source[string]("src")
	mapped := src.Map("worker", func(f Frame[string]) string { return f.Value() },
		WithCheckpoint[string](GobCodec[string]{}), WithIdempotent[string](),
		WithErrorHandler[string](func(NodeError[string]) {}))
	mapped.Output("sink", WithCheckpoint[string](GobCodec[string]{}))
	startFlow(t, m)
	time.Sleep(600 * time.Millisecond)
	return journal.observed()
}

func TestTheResumeLoopReArmsWhenLeadershipArrives(t *testing.T) {
	offer := recordFor(t, "worker", AnchorArrival, "datum-1", "recovered")

	// ARM 1: a FOLLOWER at wiring that wins leadership. Round 1 refuses; the loop
	// must wait for leadership and come back rather than exiting.
	j1 := &leadershipJournal{
		refuse:  map[int]bool{1: true},
		offerOn: map[int]CheckpointRecord{2: offer},
	}
	calls, awaited, claimed := runResume(t, j1)
	t.Logf("ARM 1 (follower wins leadership): Orphans calls=%d AwaitLeadership calls=%d claims=%v",
		calls, awaited, claimed)
	if awaited == 0 {
		t.Fatalf("ARM 1: the loop never awaited leadership after a refusal (calls=%d)", calls)
	}
	if len(claimed) != 1 || claimed[0] != "datum-1/flow-rearm" {
		t.Fatalf("ARM 1: claims=%v, want the stranded datum claimed after leadership arrived", claimed)
	}

	// ARM 2: a LEADER that loses the term and regains it. Round 1 offers, round 2
	// refuses, round 3 offers again — the loop must survive the middle refusal.
	j2 := &leadershipJournal{
		refuse:  map[int]bool{2: true},
		offerOn: map[int]CheckpointRecord{1: offer, 3: recordFor(t, "worker", AnchorArrival, "datum-2", "again")},
	}
	calls, awaited, claimed = runResume(t, j2)
	t.Logf("ARM 2 (leader loses and regains): Orphans calls=%d AwaitLeadership calls=%d claims=%v",
		calls, awaited, claimed)
	if len(claimed) != 2 {
		t.Fatalf("ARM 2: claims=%v, want both datums — the loop did not survive losing the term", claimed)
	}

	// ARM 3, THE DISCRIMINATING CONTROL. A NON-leadership error must still END the
	// loop. Without this arm, a resume loop that retried EVERY error would pass arms
	// 1 and 2 while being the catch-and-continue this design is not.
	j3 := &leadershipJournal{
		fatalOn: 1,
		offerOn: map[int]CheckpointRecord{2: offer},
	}
	calls, awaited, claimed = runResume(t, j3)
	t.Logf("ARM 3 (a non-leadership error): Orphans calls=%d AwaitLeadership calls=%d claims=%v",
		calls, awaited, claimed)
	if calls != 1 {
		t.Fatalf("ARM 3: Orphans was called %d times after a disk error, want exactly 1: "+
			"the loop is retrying an error it cannot fix", calls)
	}
	if len(claimed) != 0 {
		t.Fatalf("ARM 3: claims=%v after a disk error, want none", claimed)
	}

	// ARM 4, THE SECOND CONTROL. AwaitLeadership's own failure ends the loop rather
	// than spinning on it.
	j4 := &leadershipJournal{
		refuse:   map[int]bool{1: true},
		awaitErr: errors.New("membership is gone"),
		offerOn:  map[int]CheckpointRecord{2: offer},
	}
	calls, awaited, claimed = runResume(t, j4)
	t.Logf("ARM 4 (AwaitLeadership fails): Orphans calls=%d AwaitLeadership calls=%d claims=%v",
		calls, awaited, claimed)
	if calls != 1 || awaited != 1 || len(claimed) != 0 {
		t.Fatalf("ARM 4: calls=%d awaited=%d claims=%v, want 1/1/none", calls, awaited, claimed)
	}
}
