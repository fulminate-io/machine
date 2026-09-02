// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/loader"
)

// The serialization fixtures: real .flow files whose declared spellings are
// resolved against a REAL Go module on disk, because a spelling that resolves to
// nothing produces a refusal that has nothing to do with the contract under
// test.
const (
	serializationDir     = "testdata/serialization"
	serializationSubject = "testdata/serialization/subject"
	serializationPkg     = "example.com/serialization/subject"
)

// loadSerializationSubject loads the fixture module.
//
// IT ASSERTS pkgs.Errors() IS EMPTY AS A CONTROL. A fixture module that does not
// type-check would make every verdict below meaningless: every spelling would
// refuse to resolve, and a test asserting a refusal would pass for the wrong
// reason.
func loadSerializationSubject(t *testing.T) *loader.Packages {
	t.Helper()

	pkgs, err := loader.Load(serializationSubject, []string{"./..."})
	if err != nil {
		t.Fatalf("the serialization fixture module did not load: %v", err)
	}

	if problems := pkgs.Errors(); len(problems) != 0 {
		t.Fatalf("CONTROL FAILED: the fixture module does not type-check, so no verdict below means anything: %v",
			problems)
	}

	return pkgs
}

// deriveFixture runs the contract over one fixture file THROUGH THE DRIVER, and
// hands back the run's own state alongside its result and diagnostics.
//
// The run is returned because the cost shape is a property of the run rather
// than of its output: how many resolutions it performed is not visible in
// Registrations.
func deriveFixture(t *testing.T, name string) (*serializationRun, *Registrations, []Diagnostic) {
	t.Helper()

	src := loadSource(t, filepath.Join(serializationDir, name))
	run := newSerializationRun(loadSerializationSubject(t), serializationPkg)

	got, diags := resultOf(t, run.analyzer(), src)

	table, ok := got.(*Registrations)
	if !ok {
		t.Fatalf("the serialization analyzer produced %T, want *Registrations", got)
	}

	return run, table, withCode(diags, serializationName)
}

// subjectOf renders the phrase a diagnostic about one declaration leads with,
// which is how a test names the declaration it means.
func subjectOf(kind, name, flow string) string {
	return declaredType{kind: kind, name: name, flow: flow}.subject()
}

// countFor counts the hints and the errors reported about one declaration.
func countFor(diags []Diagnostic, subject string) (int, int) {
	hints, errs := 0, 0

	for _, d := range diags {
		if !strings.HasPrefix(d.Message, subject+" declares ") {
			continue
		}

		if d.Severity == SeverityHint {
			hints++
		}

		if d.Severity == SeverityError {
			errs++
		}
	}

	return hints, errs
}

// messagesFor is every message reported about one declaration.
func messagesFor(diags []Diagnostic, subject string) []string {
	var out []string

	for _, d := range diags {
		if strings.HasPrefix(d.Message, subject+" declares ") {
			out = append(out, d.Message)
		}
	}

	return out
}

// registeredFor is every registration recorded for one declaration name.
func registeredFor(table *Registrations, kind, name string) []Registration {
	var out []Registration

	for _, one := range table.Required {
		if one.Kind == kind && one.Name == name {
			out = append(out, one)
		}
	}

	return out
}

// TestTheSameTypeIsRefusedAtOneSiteAndRequiredAtTheOther is the contract's
// central claim: the site is a parameter of the question, not a property of the
// type.
//
// Mixed is declared twice in one flow — once as a state entry, which the ledger
// encodes through an interface slot, and once as a declared output, which rides
// a checkpointed packet's typed payload. The structural refusal holds at both;
// the registration REQUIREMENT holds only at the erased one. An implementation
// answering a single boolean per type cannot produce this pair, which is why
// this is the leg that catches the ticket's own named defect.
func TestTheSameTypeIsRefusedAtOneSiteAndRequiredAtTheOther(t *testing.T) {
	_, table, diags := deriveFixture(t, "site-dependence.flow")

	state := subjectOf(kindStateField, "tally", "Screening")
	output := subjectOf(kindOutput, "ok", "Screening")

	for _, message := range append(messagesFor(diags, state), messagesFor(diags, output)...) {
		t.Log(message)
	}

	stateHints, stateErrors := countFor(diags, state)
	outputHints, outputErrors := countFor(diags, output)

	if stateHints != 1 {
		t.Errorf("the state entry reported %d registration hints, want 1; the interface site needs one", stateHints)
	}

	if outputHints != 0 {
		t.Errorf("the declared output reported %d registration hints, want 0; a typed payload needs none", outputHints)
	}

	if stateErrors == 0 || outputErrors == 0 {
		t.Errorf("the structural refusal holds at BOTH sites, but the run reported %d state errors and %d output errors",
			stateErrors, outputErrors)
	}

	if got := registeredFor(table, kindStateField, "tally"); len(got) != 1 {
		t.Errorf("the state entry recorded %d registrations, want 1", len(got))
	}

	if got := registeredFor(table, kindOutput, "ok"); len(got) != 0 {
		t.Errorf("the declared output recorded %d registrations, want 0", len(got))
	}
}

// TestTheHatchSuppressesTheWalkAndNotRegistration pins the corrected hatch rule
// finding da8b34d7 measured.
//
// A type supplying both halves of the gob hatch owns its own bytes, so the chan
// beneath it is no longer a drop — and it is STILL unreconstructable at an
// interface slot until it is registered, exactly as an ordinary named type is.
// The tempting implementation returns early on the hatch before the site is
// consulted; it compiles, reads well, and ships the runtime failure this
// derivation exists to prevent.
func TestTheHatchSuppressesTheWalkAndNotRegistration(t *testing.T) {
	_, table, diags := deriveFixture(t, "hatch.flow")

	state := subjectOf(kindStateField, "tally", "Screening")
	output := subjectOf(kindOutput, "ok", "Screening")

	stateHints, stateErrors := countFor(diags, state)
	outputHints, outputErrors := countFor(diags, output)

	t.Logf("the hatch at the interface site: hints=%d errors=%d; at the concrete site: hints=%d errors=%d",
		stateHints, stateErrors, outputHints, outputErrors)

	if stateErrors != 0 {
		t.Errorf("the hatch did not suppress the structural walk: %v", messagesFor(diags, state))
	}

	if stateHints != 1 {
		t.Errorf("the hatch bought an exemption from registration it does not buy: %d hints, want 1", stateHints)
	}

	if outputHints != 0 || outputErrors != 0 {
		t.Errorf("a hatched type at the concrete site is clean, but the run reported %v",
			messagesFor(diags, output))
	}

	if got := registeredFor(table, kindStateField, "tally"); len(got) != 1 {
		t.Errorf("the hatched state entry recorded %d registrations, want 1", len(got))
	}
}

// TestTheDiagnosticNamesTheFieldPathInsideTheType proves the seam this contract
// sits on is actually composed.
//
// loader.Finding carries a field chain inside a type and NO source position; the
// declaration carries a position and names only the outer type. A diagnostic
// naming Nested without saying .Inner.C sends an author to a struct with nothing
// visibly wrong with it.
func TestTheDiagnosticNamesTheFieldPathInsideTheType(t *testing.T) {
	_, _, diags := deriveFixture(t, "nested-path.flow")

	state := subjectOf(kindStateField, "holder", "Nesting")

	found := false

	for _, message := range messagesFor(diags, state) {
		t.Log(message)

		if strings.Contains(message, ".Inner.C") {
			found = true
		}
	}

	if !found {
		t.Error("no diagnostic named the field chain .Inner.C inside the declared type")
	}
}

// TestAnUnresolvableSpellingIsReportedRatherThanSkipped keeps a typo from
// reading as a clean declaration.
func TestAnUnresolvableSpellingIsReportedRatherThanSkipped(t *testing.T) {
	_, _, diags := deriveFixture(t, "unresolvable.flow")

	state := subjectOf(kindStateField, "tally", "Screening")

	reported := messagesFor(diags, state)
	if len(reported) != 1 {
		t.Fatalf("an unresolvable spelling produced %d diagnostics, want 1: %v", len(reported), reported)
	}

	t.Log(reported[0])

	if !strings.Contains(reported[0], "does not resolve to a Go type") {
		t.Error("the refusal does not say the spelling names no Go type")
	}

	if _, errs := countFor(diags, state); errs != 1 {
		t.Errorf("the refusal was reported at %d errors, want 1", errs)
	}
}

// TestEveryDeclarationKindNamesItsOwnBoundary drives ONE Go type declared four
// ways and logs the site mapping per kind.
//
// THE HINT COUNT IS THE SITE MAPPING. The two erased kinds carry a registration
// hint on top of the same refusal and the two concrete kinds do not, so a kind
// wired to the wrong loader.Site moves the numbers while every message stays
// recognisable. Sealed is the spelling because it produces exactly one
// structural refusal at either site.
func TestEveryDeclarationKindNamesItsOwnBoundary(t *testing.T) {
	_, _, diags := deriveFixture(t, "four-kind.flow")

	want := []struct {
		kind   string
		name   string
		hints  int
		errors int
	}{
		{kindStateField, "cell", 1, 1},
		{kindVar, "alpha", 1, 1},
		{kindOutput, "ok", 0, 1},
		{kindInput, implicitInput, 0, 1},
	}

	for _, one := range want {
		subject := subjectOf(one.kind, one.name, "Screening")
		hints, errs := countFor(diags, subject)

		t.Logf("kind=%s subject=%q hints=%d errors=%d", one.kind, subject, hints, errs)

		if hints != one.hints || errs != one.errors {
			t.Errorf("the %s kind reported hints=%d errors=%d, want hints=%d errors=%d; its loader.Site is wrong",
				one.kind, hints, errs, one.hints, one.errors)
		}
	}
}

// TestABoundarylessDeclarationIsSkippedRatherThanGuessed pins the one silence
// this contract is allowed.
//
// A flow declaring no signature declares no input type and no output types, so
// there is nothing to examine and nothing is guessed — what such a flow carries
// is inference over the flow graph, a different analyzer's subject. The state
// entry IS examined, which is the control that keeps this from passing on a run
// that examined nothing at all.
func TestABoundarylessDeclarationIsSkippedRatherThanGuessed(t *testing.T) {
	run, table, diags := deriveFixture(t, "boundaryless.flow")

	if run.declarations != 1 {
		t.Fatalf("CONTROL FAILED: the run examined %d declarations, want the 1 state entry; a run that examined "+
			"nothing would pass every leg below vacuously", run.declarations)
	}

	for _, kind := range []string{kindInput, kindOutput} {
		if got := len(messagesFor(diags, subjectOf(kind, implicitInput, "Screening"))); got != 0 {
			t.Errorf("a flow with no signature produced %d %s diagnostics", got, kind)
		}

		for _, one := range table.Required {
			if one.Kind == kind {
				t.Errorf("a flow with no signature recorded a %s registration for %s", kind, one.Name)
			}
		}
	}

	if got := registeredFor(table, kindStateField, "tally"); len(got) != 1 {
		t.Errorf("the state entry recorded %d registrations, want 1", len(got))
	}
}
