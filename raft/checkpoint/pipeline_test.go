// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package checkpoint

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whitaker-io/machine/raft/ledger"
)

// blockingJournal parks every append on a release channel and records the PEAK
// number of appends parked at once.
//
// THE INSTRUMENT IS A PEAK COUNTER RATHER THAN A RATE, deliberately. A wall-clock
// rate on a fast machine measures the harness, not the pipeline; the property the
// measurement actually demands is that many appends are outstanding for one group at
// the same moment, and a serial per-datum writer cannot exceed one of those however
// fast the disk is.
type blockingJournal struct {
	release chan struct{}

	live atomic.Int64
	peak atomic.Int64

	admitted sync.WaitGroup
}

func (j *blockingJournal) Append(_ context.Context, _ ledger.Entry) (uint64, error) {
	live := j.live.Add(1)
	for {
		peak := j.peak.Load()
		if live <= peak || j.peak.CompareAndSwap(peak, live) {
			break
		}
	}
	j.admitted.Done()

	<-j.release
	j.live.Add(-1)

	return 1, nil
}

func TestCheckpointAppendsStayConcurrentPerGroup(t *testing.T) {
	// THE BURST IS SIZED SO THE THRESHOLD IS EXPRESSIBLE. Offering fewer datums than
	// the floor would make a low peak unreachable by construction, which is the
	// fixture-input trap: an assertion whose input space cannot express the property
	// passes against a broken implementation.
	const burst = 64

	journal := &blockingJournal{release: make(chan struct{})}
	journal.admitted.Add(burst)

	pipeline, err := New(Config{
		Journal: func(string) (Journal, error) { return journal, nil },
		Failure: func(flow, datum string, err error) { t.Errorf("append of %s on %s failed: %v", datum, flow, err) },
	})
	if err != nil {
		t.Fatalf("building the pipeline: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// THE SUBMISSION LOOP IS BOUNDED, and that bound is an assertion rather than a
	// convenience. A pipeline that awaited its own future would park in the FIRST
	// Append against this deliberately-blocking journal and never return, so without
	// a bound the wrong implementation fails as a whole-suite hang with no message.
	// Bounded, it names what happened.
	submitted := make(chan error, 1)
	go func() {
		for i := range burst {
			if err := pipeline.Append(ctx, "flow-burst", datumID(i), []byte("progress")); err != nil {
				submitted <- err

				return
			}
		}
		submitted <- nil
	}()

	select {
	case err := <-submitted:
		if err != nil {
			t.Fatalf("submitting the burst: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("only some of the %d submissions returned within twenty seconds against a journal that parks every append: the submit path is awaiting its own future, so a flow is capped at one checkpoint per fsync", burst)
	}

	// Every submitted append reached the journal. This is what makes the peak below
	// a measurement of the pipeline rather than a race with the test's own timing.
	waitOrFail(t, &journal.admitted, "the journal never received all %d appends: the pipeline is awaiting them one at a time rather than submitting them", burst)

	peak := journal.peak.Load()

	// DISCLOSURE: report the observed peak. A run that measured nothing cannot
	// satisfy the concurrency claim by silence.
	t.Logf("peak in-flight appends per group: %d", peak)

	// CONTROL: the counter moved at all, so a zero below would be a broken
	// instrument rather than a serial pipeline.
	if peak == 0 {
		t.Fatal("CONTROL FAILED: the journal recorded no appends at all, so this instrument cannot see concurrency")
	}
	if peak < 16 {
		t.Fatalf("the pipeline held only %d appends in flight for one group; a serial per-datum await peaks at 1 and the measurement requires at least 16", peak)
	}

	close(journal.release)
	if err := pipeline.Close(); err != nil {
		t.Fatalf("closing the pipeline: %v", err)
	}
}

func TestCheckpointAppendsAreNeverAwaitedOneDatumAtATime(t *testing.T) {
	// THE SECOND LEG IS WHAT MAKES THE FIRST ONE'S PEAK REACHABLE AT ALL. If Append
	// awaited its own future, the caller would be parked inside the journal and the
	// second submission would never be issued — so this asserts the submit path
	// RETURNS while its append is still outstanding.
	journal := &blockingJournal{release: make(chan struct{})}
	journal.admitted.Add(1)

	pipeline, err := New(Config{
		Journal: func(string) (Journal, error) { return journal, nil },
		Failure: func(flow, datum string, err error) { t.Errorf("append of %s on %s failed: %v", datum, flow, err) },
	})
	if err != nil {
		t.Fatalf("building the pipeline: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	returned := make(chan error, 1)
	go func() { returned <- pipeline.Append(ctx, "flow-await", "datum-1", []byte("progress")) }()

	// The append is INSIDE the journal and parked there.
	waitOrFail(t, &journal.admitted, "the journal never received the append")

	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("the submission reported %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Append had not returned five seconds after its journal call parked: the submit path is awaiting its own future, which caps a flow at one checkpoint per fsync")
	}

	// CONTROL: the append really was still outstanding when Append returned. Without
	// this, a journal that returned immediately would satisfy the timing above.
	if live := journal.live.Load(); live != 1 {
		t.Fatalf("CONTROL FAILED: %d appends were live when Append returned, want 1; the journal did not park and this leg measured nothing", live)
	}

	close(journal.release)
	if err := pipeline.Close(); err != nil {
		t.Fatalf("closing the pipeline: %v", err)
	}
}

func TestASubmittedAppendThatFailsReachesTheFailureHandler(t *testing.T) {
	// NO FAILURE IS SWALLOWED. A checkpoint that did not land leaves its datum
	// unrecoverable from that point, and only the caller can decide what that means
	// for the flow — so it must reach the handler rather than being logged here.
	sentinel := errors.New("the journal refused")

	var mutex sync.Mutex
	var seen []string

	pipeline, err := New(Config{
		Journal: func(string) (Journal, error) { return failingJournal{err: sentinel}, nil },
		Failure: func(_, datum string, err error) {
			if !errors.Is(err, sentinel) {
				t.Errorf("the reported failure %v does not wrap the journal's own error", err)
			}
			mutex.Lock()
			seen = append(seen, datum)
			mutex.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("building the pipeline: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The SUBMISSION succeeds: the failure happens later and cannot come back here.
	if err := pipeline.Append(ctx, "flow-fail", "datum-1", []byte("progress")); err != nil {
		t.Fatalf("submitting: %v", err)
	}

	closeErr := pipeline.Close()
	if !errors.Is(closeErr, sentinel) {
		t.Fatalf("Close reported %v, want the joined journal failure", closeErr)
	}

	mutex.Lock()
	defer mutex.Unlock()
	if len(seen) != 1 || seen[0] != "datum-1" {
		t.Fatalf("the failure handler saw %v, want exactly [datum-1]", seen)
	}
}

func TestThePipelineRefusesAnIncompleteConfigAndAClosedAppend(t *testing.T) {
	// Bad input errors here rather than being defaulted: a pipeline with no journal
	// resolver can append nothing, and one with no failure handler would have
	// nowhere to report a checkpoint that did not land.
	if _, err := New(Config{Failure: func(string, string, error) {}}); err == nil {
		t.Fatal("a Config with no Journal resolver was accepted")
	}
	if _, err := New(Config{Journal: func(string) (Journal, error) { return nil, nil }}); err == nil {
		t.Fatal("a Config with no Failure handler was accepted")
	}

	// CONTROL: a complete Config IS accepted, so the refusals above are the guards
	// rather than a constructor that refuses everything.
	pipeline, err := New(Config{
		Journal: func(string) (Journal, error) { return failingJournal{err: errors.New("unused")}, nil },
		Failure: func(string, string, error) {},
	})
	if err != nil {
		t.Fatalf("CONTROL FAILED: a complete Config was refused: %v", err)
	}

	if err := pipeline.Close(); err != nil {
		t.Fatalf("closing an idle pipeline: %v", err)
	}
	// Close is idempotent, mirroring the ledger's own.
	if err := pipeline.Close(); err != nil {
		t.Fatalf("closing twice: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pipeline.Append(ctx, "flow-closed", "datum-1", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("appending to a closed pipeline gave %v, want ErrClosed", err)
	}
}

// failingJournal refuses every append with a fixed error.
type failingJournal struct{ err error }

func (j failingJournal) Append(context.Context, ledger.Entry) (uint64, error) { return 0, j.err }

// datumID names the i-th datum of a burst.
func datumID(i int) string { return "datum-" + string(rune('a'+i%26)) + "-" + itoa(i) }

// itoa avoids pulling strconv in for one call.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}

	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}

	return string(digits)
}

// waitOrFail fails the test when a wait group does not drain inside a bound, rather
// than hanging until the whole suite times out with no message.
func waitOrFail(t *testing.T, group *sync.WaitGroup, format string, args ...any) {
	t.Helper()

	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf(format, args...)
	}
}
