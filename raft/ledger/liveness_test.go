package ledger

import (
	"context"
	"testing"
	"time"
)

// establishedReadCeiling separates "returns" from "never returns".
//
// IT IS NOT A LATENCY BUDGET, and it must not be tightened into one. The compliant
// band is microseconds — a steady-state read measures around 2.7µs and a first read
// that includes establishment around 298µs — while this ceiling is seconds. That gap
// is deliberate: the VIOLATING band is unbounded, so any finite ceiling separates the
// two categories, and the number is chosen for margin against loaded-CI noise rather
// than to measure speed. A read that is merely slow must not red this gate; a read
// that never returns must.
const establishedReadCeiling = 5 * time.Second

func TestEstablishedReadCompletesUnderTheCeiling(t *testing.T) {
	// THIS IS THE CONVERGENCE HALF, and it exists because the round that produced it
	// moved every other read gate onto the VALUE. Asserting the value was correct: a
	// barrier that answers absent returns a wrong answer, and completion-checking is
	// blind to it. But a value gate is structurally blind to the complement — a read
	// that never returns produces no value to be wrong about. The defect this lane
	// started from was a STALL, so a suite made entirely of value gates would meet a
	// reintroduced hang as a suite timeout, with nothing naming the read path.
	l := openTestLedger(t, Config{Flow: "flow-liveness", LocalID: "n0", Mux: testMux(t), Bootstrap: true})
	waitLeadership(t, l)
	awaitEstablished(t, l)

	// The read's own context carries NO deadline, so nothing but the ledger can end
	// it: the violating band stays genuinely unbounded and the ceiling below is the
	// only thing separating the categories.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := l.Append(ctx, Entry{Kind: KindSet, Path: fencedPath, Value: []byte(fencedValue)}); err != nil {
		t.Fatalf("committing the value to read back: %v", err)
	}

	type outcome struct {
		entry Entry
		ok    bool
		err   error
		took  time.Duration
	}
	// The read runs on ITS OWN GOROUTINE and is raced against the ceiling, so a hang
	// fails this assertion rather than parking the test binary — a gate proving
	// something does not hang must not hang to do it.
	done := make(chan outcome, 1)
	go func() {
		start := time.Now()
		entry, ok, err := l.Get(ctx, fencedPath)
		done <- outcome{entry: entry, ok: ok, err: err, took: time.Since(start)}
	}()

	select {
	case got := <-done:
		switch {
		case got.err != nil:
			t.Fatalf("a linearizable read on an established term failed after %s: %v", got.took.Round(time.Microsecond), got.err)
		case !got.ok:
			// DISTINCT ON PURPOSE. A read that completes and reports absent is the
			// VALUE defect, which the sibling gates own — the fresh-leader window gate
			// and the per-term read gates. Reporting it as a liveness failure would
			// send a reader to the wrong mechanism entirely.
			t.Fatalf("the read COMPLETED in %s but reported %q absent: this is the stale-read VALUE defect, not a liveness failure — the fresh-leader and per-term read gates are the ones that own it",
				got.took.Round(time.Microsecond), fencedPath)
		case string(got.entry.Value) != fencedValue:
			t.Fatalf("the read COMPLETED in %s but returned %q, want %q: this is the VALUE defect the sibling read gates own, not a liveness failure",
				got.took.Round(time.Microsecond), got.entry.Value, fencedValue)
		}
		t.Logf("a linearizable read on an established term completed in %s, under the %s ceiling",
			got.took.Round(time.Microsecond), establishedReadCeiling)
	case <-time.After(establishedReadCeiling):
		t.Fatalf("a linearizable read on an established term did not return within %s: the read path hangs, and no value gate can see this because a read that never returns produces no value to be wrong about",
			establishedReadCeiling)
	}
}
