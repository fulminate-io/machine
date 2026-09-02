// Package loader - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package loader

import (
	"go/types"
	"testing"
	"time"
)

// TestOneDeriverAnswersEachSiteIndependentlyTerminatesAndReusesItsMemo gates the
// memo and the depth ceiling together, because their relationship is the thing
// most easily got wrong.
//
// It is the only test in this module that REUSES a Deriver across calls, which
// is the shape a consumer actually has: one Deriver held for a whole generation
// run. Every other test constructs its subject fresh.
//
// THE SEED AND THE CEILING ARE TWO INDEPENDENT MECHANISMS. The memo's nil seed is
// what makes a legitimately recursive type ANSWER — terminate and still report
// what is inside the cycle. The ceiling is a backstop that makes an UNTERMINATED
// walk fail loud and bounded. A leg asserting only that a recursive walk
// terminates would pass with the seed removed, because the ceiling would rescue
// it — so the termination leg asserts termination AND the absence of any depth
// refusal.
func TestOneDeriverAnswersEachSiteIndependentlyTerminatesAndReusesItsMemo(t *testing.T) {
	loaded, err := Load(subjectDir, []string{"./..."})
	if err != nil {
		t.Fatalf("the subject fixture module did not load: %v", err)
	}

	resolve := func(name string) types.Type {
		t.Helper()

		typ, err := loaded.Resolve(subjectPath, name)
		if err != nil {
			t.Fatalf("the fixture type %s did not resolve: %v", name, err)
		}

		return typ
	}

	plain := resolve("Plain")

	// CONTROL: the fixture must span the threshold. If Plain answered the same
	// at both sites, a site-blind memo key would be undetectable here.
	concreteWant := len(NewDeriver().Serializable(plain, SiteConcrete))
	interfaceWant := len(NewDeriver().Serializable(plain, SiteInterface))

	if concreteWant != 0 || interfaceWant != 1 {
		t.Fatalf("CONTROL FAILED: Plain answers %d at concrete and %d at interface; the fixture must answer 0 and 1 for this test to discriminate",
			concreteWant, interfaceWant)
	}

	t.Run("one Deriver answers each site independently", func(t *testing.T) {
		shared := NewDeriver()

		if got := len(shared.Serializable(plain, SiteConcrete)); got != concreteWant {
			t.Fatalf("the shared Deriver's concrete answer is %d, want %d", got, concreteWant)
		}

		got := len(shared.Serializable(plain, SiteInterface))
		if got != interfaceWant {
			t.Fatalf("SHARED DERIVER GAVE A DIFFERENT ANSWER: interface position returned %d findings after a concrete query, but %d from a fresh Deriver",
				got, interfaceWant)
		}
	})

	t.Run("a recursive type terminates VIA THE MEMO SEED, not via the ceiling", func(t *testing.T) {
		rec := resolve("Rec")

		start := time.Now()
		found := NewDeriver().Serializable(rec, SiteConcrete)
		elapsed := time.Since(start)

		t.Logf("recursive walk terminated via the memo in %s with %d findings: %v",
			elapsed.Round(time.Microsecond), len(found), found)

		if len(found) == 0 {
			t.Fatal("the recursive walk terminated but reported nothing, so the chan buried in the cycle was lost")
		}

		// THE DISCRIMINATING ASSERTION. A walk rescued by the ceiling also
		// terminates, so termination alone would pass with the seed removed and
		// the backstop would hide the defect this leg exists to catch.
		for _, one := range found {
			if one.Reason == ReasonDepthExceeded {
				t.Fatalf("the recursive walk was stopped by the depth ceiling rather than by the memo, so the memo is not terminating the cycle: %d findings, deepest %s",
					len(found), one.Type)
			}
		}
	})

	t.Run("a legitimately deep finite type is NOT refused", func(t *testing.T) {
		under := resolve("UnderCeiling0")

		natural := naturalDepth(t, under, SiteConcrete)
		if natural > MaxDepth {
			t.Fatalf("the under-control's natural depth is %d frames, past the %d ceiling — it is not a control, it is a second over-fixture",
				natural, MaxDepth)
		}

		found := NewDeriver().Serializable(under, SiteConcrete)
		for _, one := range found {
			if one.Reason == ReasonDepthExceeded {
				t.Fatalf("a legitimate %d-frame type was refused by the %d-frame ceiling", natural, MaxDepth)
			}
		}

		// It must REPORT through its whole depth, not merely survive it.
		if len(found) == 0 {
			t.Fatalf("the under-control walked clean but reported nothing, so the chan at the bottom of %d frames was lost", natural)
		}

		t.Logf("under-control: naturalDepth=%d frames (%.0f%% of the %d ceiling), walks clean and still reports %v",
			natural, 100*float64(natural)/float64(MaxDepth), MaxDepth, found)
	})

	t.Run("a type nested past the ceiling is refused by name in bounded time", func(t *testing.T) {
		// STEP ONE: measure the fixture's NATURAL depth with the bound lifted.
		// Under the real ceiling every fixture at or past the boundary reads the
		// same capped depth, so only this step proves the fixture genuinely
		// EXCEEDS the bound rather than merely reaching it.
		past := resolve("PastCeiling0")

		natural := naturalDepth(t, past, SiteConcrete)
		if natural <= MaxDepth {
			t.Fatalf("the over-fixture's natural depth is %d frames, which does NOT exceed the %d ceiling — it cannot gate the refusal",
				natural, MaxDepth)
		}

		// STEP TWO: assert the behaviour under the real ceiling.
		start := time.Now()
		found := NewDeriver().Serializable(past, SiteConcrete)
		elapsed := time.Since(start)

		refused := ""

		for _, one := range found {
			if one.Reason == ReasonDepthExceeded {
				refused = one.Type
			}
		}

		if refused == "" {
			t.Fatalf("a type nested past MaxDepth=%d was not refused by the ceiling: %v", MaxDepth, found)
		}

		t.Logf("over-fixture: naturalDepth=%d frames (%d past the ceiling); ceiling refused in %s naming %s",
			natural, natural-MaxDepth, elapsed.Round(time.Microsecond), refused)
	})

	t.Run("a repeated query is served from the memo rather than re-walked", func(t *testing.T) {
		deriver := NewDeriver()

		first := deriver.Serializable(plain, SiteInterface)
		if len(first) == 0 {
			t.Fatal("CONTROL FAILED: this leg compares backing arrays and needs a non-empty result to compare")
		}

		second := deriver.Serializable(plain, SiteInterface)

		// A re-walk allocates a fresh slice, so sharing the backing array is
		// what distinguishes a memo hit from an identical recomputation.
		if &first[0] != &second[0] {
			t.Fatal("the second query for the same type and site allocated a new result, so the memo did not serve it")
		}
	})
}

// naturalDepth reports the frame depth a type's walk reaches with the ceiling
// lifted, MEASURED rather than derived from a frames-per-level ratio.
//
// It is the smallest ceiling at which the walk completes with no depth refusal,
// found by bisection. That uses nothing but the walk's own production behaviour
// plus the Deriver's unexported bound, so it adds no measurement machinery to
// the shipping type.
func naturalDepth(t *testing.T, typ types.Type, site Site) int {
	t.Helper()

	refused := func(ceiling int) bool {
		probe := NewDeriver()
		probe.ceiling = ceiling

		for _, one := range probe.Serializable(typ, site) {
			if one.Reason == ReasonDepthExceeded {
				return true
			}
		}

		return false
	}

	lo, hi := 1, 1<<12
	if refused(hi) {
		t.Fatalf("the walk still refuses at a ceiling of %d frames, so this type's natural depth is not measurable here", hi)
	}

	for lo < hi {
		mid := lo + (hi-lo)/2
		if refused(mid) {
			lo = mid + 1

			continue
		}

		hi = mid
	}

	return lo
}
