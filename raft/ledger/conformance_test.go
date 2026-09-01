package ledger

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	machine "github.com/whitaker-io/machine/v4"
)

// ledgerSurface is the published Ledger shape, written once and used both by the
// compile-time pin below and by the test that exercises it.
//
// The machine.Store pin further down was already here and it caught real drift; a
// Ledger pin was missing, and that is how Append shipped returning only an error
// while the phase-4 seam statement published (uint64, error). A lane author reading
// the overview and one reading the code would have built against different
// contracts, and nothing would have said so until one of them failed.
//
// It is an ANONYMOUS interface deliberately. A named exported interface would become
// part of the seam and invite implementations; the point is to pin the concrete
// type's shape, not to offer an abstraction.
// Restore is pinned here alongside the rest because it is now published surface a
// consumer is REQUIRED to use: a snapshot delivered through Raft().Restore skips the
// epoch epilogue and leaves every read on that node timing out. A method the seam
// tells callers they must use is exactly the kind that must not drift silently.
// LocalID is pinned on the same terms: a caller that carries its own copy of this
// node's identity reads this one to check the two agree, and a caller that cannot
// read it can only assume they do.
// List and Claimant are pinned on those same terms, and for a sharper reason: the
// recovery path is REQUIRED to use both. Enumeration is how an orphan is found at
// all, and Claimant is the only way to observe a held claim — Get reports one
// ABSENT, so an already-claimed filter reading through Get is inert and hands every
// claimed datum out a second time. A method whose absence makes a filter silently
// pass everything is exactly the kind that must not drift.
type ledgerSurface = interface {
	Close() error
	Store() *Store
	Raft() *raft.Raft
	Flow() string
	LocalID() string
	Append(context.Context, Entry) (uint64, error)
	Get(context.Context, string) (Entry, bool, error)
	List(context.Context, string) ([]Entry, error)
	Claimant(context.Context, string) (string, bool, error)
	Restore(*raft.SnapshotMeta, io.Reader, time.Duration) error
}

var _ ledgerSurface = (*Ledger)(nil)

func TestLedgerSurfaceMatchesThePublishedSeam(t *testing.T) {
	l := openTestLedger(t, Config{Flow: "flow-surface", LocalID: "n0", Mux: testMux(t), Bootstrap: true})
	waitLeadership(t, l)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Held through the pinned shape, so the assertion is load-bearing here rather
	// than a declaration nothing reads.
	var surface ledgerSurface = l

	if surface.Flow() != "flow-surface" {
		t.Fatalf("Flow() reported %q, want the configured flow", surface.Flow())
	}
	if surface.LocalID() != "n0" {
		t.Fatalf("LocalID() reported %q, want the configured raft server id", surface.LocalID())
	}
	if surface.Raft() == nil {
		t.Fatal("Raft() returned nil on an open ledger")
	}
	if surface.Store() == nil {
		t.Fatal("Store() returned nil on an open ledger")
	}

	index, err := surface.Append(ctx, Entry{Kind: KindSet, Path: "heap/alpha", Value: []byte("v")})
	if err != nil {
		t.Fatalf("Append through the published surface: %v", err)
	}
	if index == 0 {
		t.Fatal("Append reported journal index 0, which no committed entry occupies")
	}
	entry, ok, err := surface.Get(ctx, "heap/alpha")
	if err != nil || !ok || string(entry.Value) != "v" {
		t.Fatalf("Get through the published surface gave %+v present=%v err=%v", entry, ok, err)
	}

	// The reported index is a journal position, not a count of this ledger's calls.
	if last := l.raft.LastIndex(); index > last {
		t.Fatalf("Append reported index %d beyond the log's last index %d", index, last)
	}
}

// The ledger Store is a machine heap store. This assertion is what makes a drift in
// that seam a COMPILE failure here rather than a surprise at the first site that
// tries to wire the two together.
//
// It is not redundant with the methods simply existing. Every method of the seam
// names only stdlib types, so *Store would satisfy it STRUCTURALLY without this
// module importing the root module at all — and a widened or narrowed seam would
// then break nothing here and surface only in some future caller.
//
// It lives in a test file deliberately: the linter does not analyze test files, so a
// production-file import would drag the whole root package into the analysis of a
// transport-and-consensus module for no benefit, while the module's own `go test`
// still compiles this file on every push.
var _ machine.Store = (*Store)(nil)

func TestStoreSatisfiesTheWidenedMachineSeam(t *testing.T) {
	l := openTestLedger(t, Config{Flow: "flow-conformance", LocalID: "n0", Mux: testMux(t), Bootstrap: true})
	waitLeadership(t, l)

	// Held as the INTERFACE, not the concrete type, so this exercise depends on the
	// assertion above rather than merely calling three methods that happen to exist.
	var seam machine.Store = l.Store()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := seam.Save(ctx, "heap/alpha", "through-the-seam"); err != nil {
		t.Fatalf("Save through machine.Store: %v", err)
	}
	value, ok, err := seam.Load(ctx, "heap/alpha")
	if err != nil || !ok || value != "through-the-seam" {
		t.Fatalf("Load through machine.Store gave %v present=%v err=%v", value, ok, err)
	}
	updated, err := seam.Update(ctx, "heap/alpha", func(any) any { return "updated-through-the-seam" })
	if err != nil || updated != "updated-through-the-seam" {
		t.Fatalf("Update through machine.Store gave %v err=%v", updated, err)
	}
}
