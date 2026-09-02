// Package loader - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package loader

import "testing"

// TestOneDeriverAnswersEachSiteIndependentlyTerminatesAndReusesItsMemo gates the
// three things the memo is for.
//
// It is the only test in this module that REUSES a Deriver across calls, which
// is the shape a consumer actually has: one Deriver held for a whole generation
// run. Every other test constructs its subject fresh, so without this one the
// memo's behaviour under reuse is entirely unmeasured.
func TestOneDeriverAnswersEachSiteIndependentlyTerminatesAndReusesItsMemo(t *testing.T) {
	loaded, err := Load(subjectDir, []string{"./..."})
	if err != nil {
		t.Fatalf("the subject fixture module did not load: %v", err)
	}

	plain, err := loaded.Resolve(subjectPath, "Plain")
	if err != nil {
		t.Fatalf("the fixture type Plain did not resolve: %v", err)
	}

	// CONTROL: the fixture must span the threshold. If Plain answered the same
	// at both sites, a site-blind memo key would be undetectable here and a
	// green would mean nothing.
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

	t.Run("a self-referential type terminates and still reports what is inside the cycle", func(t *testing.T) {
		rec, err := loaded.Resolve(subjectPath, "Rec")
		if err != nil {
			t.Fatalf("the recursive fixture type Rec did not resolve: %v", err)
		}

		found := NewDeriver().Serializable(rec, SiteConcrete)
		t.Logf("recursive walk terminated with %d findings: %v", len(found), found)

		if len(found) == 0 {
			t.Fatal("the recursive walk terminated but reported nothing, so the chan buried in the cycle was lost")
		}
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
