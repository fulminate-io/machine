// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"math"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/whitaker-io/machine/lang/ast"
)

// TestGuidanceScopeRespectsDeclareBeforeUse pins that the facts are correct at a
// position inside a canonical program.
//
// BOTH DIRECTIONS OF MEMBERSHIP ARE ASSERTED. A scope function returning every
// name in the file would satisfy a positive-membership check while being useless
// for constraining a decoder — the whole value of the surface is what it LEAVES
// OUT, so a name declared later has to be absent.
func TestGuidanceScopeRespectsDeclareBeforeUse(t *testing.T) {
	src := loadSource(t, filepath.Join(strawmanDir, "payments.flow"))
	table, err := BuildGuidance([]Source{src})
	if err != nil {
		t.Fatalf("building guidance failed: %v", err)
	}

	// `branch screen ... from payments` is the third statement. At that point
	// `events` and `payments` are declared; `enriched`, `flaky` and `wh` are
	// declared by statements below it.
	flow := firstFlow(t, src)
	screen := statementNamed(t, flow, "screen")
	got, ok := table.At(src, screen.Pos())
	if !ok {
		t.Fatalf("no guidance at %s", screen.Pos())
	}
	if got.Flow != "payments" {
		t.Errorf("the guidance names flow %q, want payments", got.Flow)
	}

	for _, name := range []string{"events", "payments"} {
		if !slices.Contains(got.Producers, name) {
			t.Errorf("%q is declared above screen but is not in scope: %v", name, got.Producers)
		}
	}
	for _, name := range []string{"enriched", "flaky", "wh", "live"} {
		if slices.Contains(got.Producers, name) {
			t.Errorf("%q is declared BELOW screen but is in scope: %v", name, got.Producers)
		}
	}

	// Storage and imports match the flow's own declarations.
	for _, name := range []string{"attempt", "span", "processed", "by_type"} {
		if !slices.Contains(got.Storage, name) {
			t.Errorf("%q is declared by payments but is not offered as storage: %v", name, got.Storage)
		}
	}
	if len(got.Imports) != 5 {
		t.Errorf("payments declares 5 imports but guidance offers %d: %v", len(got.Imports), got.Imports)
	}
	t.Logf("at screen: producers %v, storage %v", got.Producers, got.Storage)
}

// TestGuidanceAccessorIsPrebuiltAndSizeIndependent gates the per-keystroke
// contract, which the correctness test above cannot see.
//
// STRUCTURAL LEG: the accessor answers correctly after ONE driver run and
// without re-running it, which is only true if the table was built during that
// run. An implementation that walked on demand would pass a correctness-only
// test, so this leg is about WHEN the work happens rather than whether the
// answer is right.
//
// COST LEG: many accessor calls against one run, over the largest and the
// smallest strawman, asserting the per-call cost RATIO. The threshold is 1.5x
// and it is measured rather than chosen — a compliant prebuilt lookup runs at
// 0.60-0.75x, while a per-call re-walk runs at 1.80-1.92x, tracking the
// statement-count ratio as it must. A "small constant multiple" read as 2x would
// sit ABOVE the violating band's floor and separate nothing. The assertion is a
// ratio rather than an absolute figure because an absolute nanosecond threshold
// measured on one machine is a flake on another.
func TestGuidanceAccessorIsPrebuiltAndSizeIndependent(t *testing.T) {
	large := loadSource(t, filepath.Join(strawmanDir, "payments.flow"))
	small := loadSource(t, filepath.Join(strawmanDir, "enrichment.flow"))

	largeTable, err := BuildGuidance([]Source{large})
	if err != nil {
		t.Fatalf("building guidance for the larger source failed: %v", err)
	}
	smallTable, serr := BuildGuidance([]Source{small})
	if serr != nil {
		t.Fatalf("building guidance for the smaller source failed: %v", serr)
	}

	// STRUCTURAL: correct answers with no further Run.
	largePos := statementNamed(t, firstFlow(t, large), "backoff").Pos()
	if got, ok := largeTable.At(large, largePos); !ok || len(got.Producers) == 0 {
		t.Fatalf("the accessor answered nothing after a single run at %s (ok=%t)", largePos, ok)
	}

	largeStmts := len(firstFlow(t, large).Body)
	smallStmts := len(firstFlow(t, small).Body)
	if largeStmts <= smallStmts {
		t.Fatalf("the larger source has %d statements and the smaller %d; the ratio measures nothing",
			largeStmts, smallStmts)
	}

	perLarge := measureAccessor(t, largeTable, large, largePos)
	perSmall := measureAccessor(t, smallTable, small, statementNamed(t, firstFlow(t, small), "backoff").Pos())
	ratio := perLarge / perSmall

	t.Logf("guidance per-call ratio: %.2fx (limit 1.5x) over %d and %d statements, %.2fns and %.2fns per call",
		ratio, largeStmts, smallStmts, perLarge, perSmall)
	if ratio > 1.5 {
		t.Errorf("per-call cost tracks source size at %.2fx, over the 1.5x limit; the accessor is walking, not looking up",
			ratio)
	}
}

// measureAccessor times one accessor call in NANOSECONDS, reporting the best of
// several rounds.
//
// The MINIMUM across rounds is the estimator, not the mean: timing noise is
// one-sided — scheduling and cache pressure only ever make a round slower — so
// the fastest observation is closest to the real cost and far more stable across
// machines than an average.
//
// The result is a float rather than a time.Duration because a call costs about
// ten nanoseconds, and truncating that to a whole-nanosecond Duration quantizes
// the measurement at ten percent, which shows up as a ratio that wanders between
// runs for reasons that have nothing to do with the algorithm.
func measureAccessor(t *testing.T, table *GuidanceTable, src Source, pos ast.Position) float64 {
	t.Helper()

	const (
		calls  = 200000
		rounds = 5
	)

	best := math.Inf(1)
	for r := 0; r < rounds; r++ {
		start := time.Now()
		for i := 0; i < calls; i++ {
			if _, ok := table.At(src, pos); !ok {
				t.Fatalf("the accessor stopped answering at %s during measurement", pos)
			}
		}
		if per := float64(time.Since(start).Nanoseconds()) / calls; per < best {
			best = per
		}
	}
	return best
}

// TestGuidanceOffersNothingOutsideAKnownSource pins that an unknown path is a
// miss rather than an empty-but-successful answer, which a caller could not tell
// from a flow with nothing in scope.
func TestGuidanceOffersNothingOutsideAKnownSource(t *testing.T) {
	src := loadSource(t, filepath.Join(strawmanDir, "toy.flow"))
	table, err := BuildGuidance([]Source{src})
	if err != nil {
		t.Fatalf("building guidance failed: %v", err)
	}

	if _, ok := table.At(Source{Path: "never-analyzed.flow"}, position(0)); ok {
		t.Error("the accessor answered for a source that was never analyzed")
	}
	if _, ok := table.At(src, ast.Position{Offset: -1}); ok {
		t.Error("the accessor answered for a position before the first scope")
	}
}

// statementNamed finds a statement by its node name.
func statementNamed(t *testing.T, flow ast.FlowDecl, name string) ast.Stmt {
	t.Helper()

	for _, stmt := range flow.Body {
		if _, ident, ok := namedClauses(stmt); ok && ident.Name == name {
			return stmt
		}
	}
	t.Fatalf("flow %s declares no statement named %s", flow.Name.Name, name)
	return nil
}
