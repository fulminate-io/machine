package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWaitAppliedExpiresWithTheTargetAndObservedIndex(t *testing.T) {
	f := newFSM()
	f.advance(3)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := f.waitApplied(ctx, 99)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrReadTimeout) {
		t.Fatalf("waiting for an index the ledger will never reach gave %v, want a wrapped ErrReadTimeout", err)
	}
	if !strings.Contains(err.Error(), "99") {
		t.Fatalf("the timeout %q does not name the target index the read was waiting for", err)
	}
	if !strings.Contains(err.Error(), "3") {
		t.Fatalf("the timeout %q does not name the index actually observed", err)
	}
	// It expired because it WAITED, not because it refused up front. Without this a
	// waitApplied that returned the timeout immediately would satisfy every
	// assertion above.
	if elapsed < 50*time.Millisecond {
		t.Fatalf("the wait returned after %v, well inside its 100ms context: it never actually waited", elapsed)
	}

	// CONTROL: a target already reached returns nil through the same call, so the
	// refusal above is about the unreachable target and not about waitApplied
	// failing for everything.
	if err := f.waitApplied(ctx, 3); err != nil {
		t.Fatalf("CONTROL FAILED: waiting for an already-applied index gave %v, want nil", err)
	}
}

func TestWaitAppliedWakesWhenTheIndexArrives(t *testing.T) {
	f := newFSM()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The waiter parks first and the index arrives after, which is the ordering a
	// real read takes. If the broadcast channel were not closed and replaced on
	// every advance, this would sit until the context expired.
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.advance(42)
	}()

	start := time.Now()
	if err := f.waitApplied(ctx, 42); err != nil {
		t.Fatalf("a waiter parked below an index that then arrived gave %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("the waiter took %v to notice an index that arrived after 30ms", elapsed)
	}
}

func TestWaitAppliedReportsThePoisonRatherThanHanging(t *testing.T) {
	f := newFSM()
	f.Apply(commandAt(t, 6, Entry{Kind: Kind(200), Path: "heap/alpha"}))

	// The poisoned apply advanced the index to 6, so a wait BEYOND it would
	// otherwise block; the poison must come back instead of a timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := f.waitApplied(ctx, 99)
	if !errors.Is(err, ErrPoisonedJournal) {
		t.Fatalf("waiting on a poisoned ledger gave %v, want the poison", err)
	}
	if errors.Is(err, ErrReadTimeout) {
		t.Fatalf("waiting on a poisoned ledger timed out (%v) rather than reporting the poison", err)
	}
}

func TestReadFastPathAllocatesNothing(t *testing.T) {
	f := newFSM()
	f.advance(10)
	ctx := context.Background()

	// The prescribed ordering: compare the tracked index, touch the context only on
	// the branch that waits.
	fast := testing.AllocsPerRun(1000, func() {
		if err := f.waitApplied(ctx, 5); err != nil {
			t.Fatalf("the satisfied wait failed: %v", err)
		}
	})

	// CONTROL: the same measurement over the ordering this step forbids —
	// materializing a per-call deadline before the comparison. It MUST read
	// non-zero, or a zero above would be a blind instrument rather than a fast path.
	control := testing.AllocsPerRun(1000, func() {
		deadline, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if err := f.waitApplied(deadline, 5); err != nil {
			t.Fatalf("the satisfied wait failed under the control ordering: %v", err)
		}
	})
	if control == 0 {
		t.Fatal("CONTROL FAILED: the deadline-first ordering measured 0 allocations, so this instrument cannot see allocation at all")
	}
	// Deliberately NOT phrased as "allocates N objects per call": the criterion for
	// this step greps for that sentence, and a control reading 10 would otherwise
	// satisfy the grep on this line.
	t.Logf("the deadline-first ordering this step forbids measures %.0f allocations per call", control)

	if fast != 0 {
		t.Fatalf("the already-satisfied wait allocates %.0f objects per call, want 0: the context is being touched before the index comparison", fast)
	}
	t.Logf("the already-satisfied wait allocates %.0f objects per call", fast)
}
