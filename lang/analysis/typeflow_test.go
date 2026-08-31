// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"path/filepath"
	"testing"
)

// typeflowDir holds this module's own declared-type fixtures.
var typeflowDir = filepath.Join("testdata", "typeflow")

// TestTypeflowStructuralDisagreementAndUntypedSilence covers all three legs.
//
// WHAT THE STRAWMAN LEG DOES AND DOES NOT PROVE. Under the structural-first
// ruling a type identity comes only from a declared spelling, and no strawman
// declares a flow signature — their fan-ins join plain node outputs, so every
// strawman edge is UNTYPED. Their silence is evidence for the untyped leg, NOT
// for the disagreement check. The disagreement leg is exercised solely by the
// authored fixture, and a real conflict between two genuinely declared Go types
// cannot arise until go/types resolution lands with the loader.
func TestTypeflowStructuralDisagreementAndUntypedSilence(t *testing.T) {
	// LEG ONE: a decidable disagreement.
	conflict := loadSource(t, filepath.Join(typeflowDir, "declared-type-disagreement.flow"))
	diags := withCode(analyze(t, TypeflowAnalyzer, conflict), TypeflowAnalyzer.Name)
	if len(diags) != 1 {
		t.Fatalf("the conflicting fan-in produced %d typeflow diagnostics, want 1: %v", len(diags), messages(diags))
	}
	if !containsAll(diags[0].Message, "declared types disagree", "ErrResult", "OkResult") {
		t.Errorf("the diagnostic does not name the disagreeing spellings: %s", diags[0].Message)
	}
	t.Logf("declared disagreement: %v", messages(diags))

	// LEG TWO: an untyped fan-in is SILENCE, not agreement. Reported separately
	// from the strawman sweep so the property is asserted on a fixture built to
	// isolate it.
	untyped := loadSource(t, filepath.Join(typeflowDir, "untyped-fan-in.flow"))
	if got := withCode(analyze(t, TypeflowAnalyzer, untyped), TypeflowAnalyzer.Name); len(got) != 0 {
		t.Errorf("an untyped fan-in was reported; silence is correct where nothing declares a type: %v",
			messages(got))
	}

	// THE DISCRIMINATING SHAPE FOR LEG TWO. A fan-in where EVERY input is
	// untyped stays silent under both readings, because absence-as-identity
	// still yields one identity. Only a MIXED fan-in separates them: one
	// declared spelling plus one untyped input is a single identity under the
	// correct rule and two under the wrong one. This case was added after a
	// mutation showed the all-untyped fixture above could not tell them apart.
	mixed := loadSource(t, filepath.Join(typeflowDir, "mixed-typed-and-untyped-fan-in.flow"))
	if got := withCode(analyze(t, TypeflowAnalyzer, mixed), TypeflowAnalyzer.Name); len(got) != 0 {
		t.Errorf("a fan-in mixing a declared input with an untyped one was reported as a disagreement: %v",
			messages(got))
	}

	// LEG THREE: the corpus sweep.
	for name, got := range sweepCorpus(t, TypeflowAnalyzer, strawmanDir) {
		if len(got) != 0 {
			t.Errorf("strawman %s produced typeflow diagnostics: %v", name, messages(got))
		}
	}

	// THE FAN-INS ARE REAL, which is what keeps the silence above from being the
	// silence of an analyzer that found nothing to look at.
	var fanIns int
	for _, name := range strawmanFiles {
		src := loadSource(t, filepath.Join(strawmanDir, name))
		set, _ := graphsOf(t, src)
		for _, file := range set.Files {
			for i := range file.Graphs {
				for _, node := range file.Graphs[i].Nodes {
					if len(node.Inputs) > 1 {
						fanIns++
					}
				}
			}
		}
	}
	if fanIns == 0 {
		t.Fatal("no strawman carries a multi-input fan-in, so their typeflow silence proves nothing")
	}
	t.Logf("the strawmen carry %d multi-input fan-ins, all of them untyped", fanIns)
}

// TestTypeflowAgreesWhenSpellingsMatch pins the other side of the comparison: a
// fan-in whose declared inputs carry the SAME spelling is not reported.
//
// Without it, "a disagreement is reported" is satisfied by an analyzer that
// reports every fan-in touching a declared name.
func TestTypeflowAgreesWhenSpellingsMatch(t *testing.T) {
	src := parseSource(t, "agree.flow",
		"flow screening (Order) -> ok Result, bad Result\n"+
			"branch check fraud.Clean from in -> ok, bad\n"+
			"sink merge audit.Store from ok, bad\n")
	if got := withCode(analyze(t, TypeflowAnalyzer, src), TypeflowAnalyzer.Name); len(got) != 0 {
		t.Errorf("a fan-in whose declared spellings match was reported as a disagreement: %v", messages(got))
	}
}
