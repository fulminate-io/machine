package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCloseIsIdempotentAndAClosedLedgerRefusesWithErrClosed(t *testing.T) {
	mux := testMux(t)
	l, err := Open(Config{Flow: "flow-closed", LocalID: "n0", Mux: mux, Bootstrap: true, tuning: fastElections})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	waitLeadership(t, l)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// CONTROL: the ledger serves before the close, so the refusals below are caused
	// by closing it rather than by it never having worked.
	index, err := l.Append(ctx, Entry{Kind: KindSet, Path: "heap/alpha", Value: []byte("v")})
	if err != nil {
		t.Fatalf("CONTROL FAILED: appending to an open ledger: %v", err)
	}
	if index == 0 {
		t.Fatal("CONTROL FAILED: the append reported journal index 0, which no committed entry occupies")
	}

	if err := l.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Idempotent: a second and third call return without error and without panic.
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("third Close: %v", err)
	}

	assertRefusesWithErrClosed(t, ctx, l)
}

// assertRefusesWithErrClosed drives every caller-facing method of a closed ledger
// and requires each to name ErrClosed rather than reach a stopped raft.
func assertRefusesWithErrClosed(t *testing.T, ctx context.Context, l *Ledger) {
	t.Helper()

	if _, err := l.Append(ctx, Entry{Kind: KindSet, Path: "heap/alpha"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Append on a closed ledger gave %v, want ErrClosed", err)
	}
	if _, _, err := l.Get(ctx, "heap/alpha"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get on a closed ledger gave %v, want ErrClosed", err)
	}

	// The seam a caller actually holds refuses too, with the same named error
	// rather than a raft-internal one it cannot interpret.
	store := l.Store()
	if value, ok, err := store.Load(ctx, "heap/alpha"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Store.Load on a closed ledger gave %v present=%v err=%v, want ErrClosed", value, ok, err)
	}
	if err := store.Save(ctx, "heap/alpha", "unreachable"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Store.Save on a closed ledger gave %v, want ErrClosed", err)
	}
	if _, err := store.Update(ctx, "heap/alpha", func(v any) any { return v }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Store.Update on a closed ledger gave %v, want ErrClosed", err)
	}
}
