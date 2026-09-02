// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"strings"
	"testing"
)

// The two fixtures differ in ONE token — the declared output's spelling — which is
// what makes the pair decide the property rather than merely exercise it.
//
// The shape follows the corpus's own declared-type-disagreement.flow: a branch
// introduces the declared output from the implicit input, and a sink joins the
// input with that output. `bad` is deliberately undeclared, so it is UNTYPED and
// contributes no identity, exactly as an ordinary edge does.
const (
	inputDisagreementFlow = `note """analysis check: a fan-in joining the signature's declared INPUT with a declared OUTPUT whose spelling differs. The language states both types, so this is decidable without go/types."""
flow Screening (Alpha) -> ok Beta
import "acme.dev/flows/audit"
branch check audit.Clean from in -> ok, bad
sink merge audit.Store from in, ok
`

	inputAgreementFlow = `note """analysis check: NOT a diagnostic. The same fan-in, with the declared output spelled exactly as the declared input. One identity, no disagreement."""
flow Screening (Alpha) -> ok Alpha
import "acme.dev/flows/audit"
branch check audit.Clean from in -> ok, bad
sink merge audit.Store from in, ok
`
)

// TestTheSignaturesDeclaredInputCarriesItsType gates the half of typeflow's own
// Doc promise its body did not keep.
//
// The Doc says the analyzer gives each name "the structural type IDENTITY the
// language itself states — a flow signature's declared input and output types".
// declaredTypes tabled the OUTPUTS only, so the implicit `in` carried no identity
// and a decidable disagreement read as agreement — silence, which is the
// direction a consumer over-reads.
//
// BOTH DIRECTIONS ARE IN ONE TEST BECAUSE EITHER ALONE IS SATISFIABLE BY A WRONG
// BODY: an analyzer reporting EVERY fan-in passes the disagreement leg, and one
// reporting NONE passes the agreement leg. Neither existing typeflow test can see
// this property at all — both their fixtures join two OUTPUTS.
func TestTheSignaturesDeclaredInputCarriesItsType(t *testing.T) {
	disagreeing := typeflowOver(t, "declared-input-disagreement.flow", inputDisagreementFlow)

	reported := ""
	if len(disagreeing) > 0 {
		reported = disagreeing[0].Message
	}

	t.Logf("declared input: %d diagnostic naming both spellings: %s", len(disagreeing), reported)

	assertDisagreementNamesBoth(t, disagreeing, reported)

	// THE CONTROL RUNS UNCONDITIONALLY, and the leg above uses Errorf rather than
	// Fatalf for that reason: a red on the first leg must not suppress the second,
	// or a run that reports every fan-in would look like a single failure rather
	// than the two it is.
	agreeing := typeflowOver(t, "declared-input-agreement.flow", inputAgreementFlow)

	t.Logf("declared input agreeing with the output: %d diagnostics", len(agreeing))

	if len(agreeing) != 0 {
		t.Errorf("the input and the output declare the SAME spelling, so there is one identity and nothing to "+
			"report, but typeflow said %v", messages(agreeing))
	}
}

// assertDisagreementNamesBoth checks the reported count and that the message
// carries both declared spellings.
//
// NAMING BOTH IS THE POINT rather than decoration: a diagnostic saying only that
// a fan-in disagrees sends an author to a node and leaves them to work out which
// two declarations are in conflict.
func assertDisagreementNamesBoth(t *testing.T, got []Diagnostic, reported string) {
	t.Helper()

	if len(got) != 1 {
		t.Errorf("the fan-in joining the declared input with a declared output produced %d typeflow "+
			"diagnostics, want 1: %v", len(got), messages(got))

		return
	}

	if !containsAll(reported, "Alpha", "Beta") {
		t.Errorf("the diagnostic does not name both declared spellings: %s", reported)
	}

	if !strings.Contains(reported, "the sink merge in flow Screening") {
		t.Errorf("the diagnostic does not name the fan-in it is about: %s", reported)
	}
}

// typeflowOver runs the typeflow analyzer over one inline fixture and keeps only
// what that analyzer reported.
//
// The fixtures are inline because they exist to decide ONE property and reading
// them beside the assertion is what makes the single-token difference between
// them visible.
func typeflowOver(t *testing.T, path, src string) []Diagnostic {
	t.Helper()

	return withCode(analyze(t, TypeflowAnalyzer, parseSource(t, path, src)), TypeflowAnalyzer.Name)
}
