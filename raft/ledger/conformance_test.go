package ledger

import (
	"context"
	"testing"
	"time"

	machine "github.com/whitaker-io/machine/v4"
)

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
