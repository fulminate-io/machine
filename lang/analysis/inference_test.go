// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"go/types"
	"path/filepath"
	"testing"

	"github.com/whitaker-io/machine/lang/loader"
)

// The inference fixtures: real .flow files typed against a REAL Go module on
// disk, which is the only way any of this is testable. No pre-existing fixture
// can be typed — every import path in the corpus is fictional.
const (
	inferenceDir     = "testdata/inference"
	inferenceSubject = "testdata/inference/subject"
	inferencePkg     = "example.com/inference/subject/orderpkg/v2"
)

// loadInferenceSubject loads the fixture module once.
//
// IT ASSERTS pkgs.Errors() IS EMPTY AS A CONTROL. A fixture module that does not
// type-check would make every inference result below meaningless — references
// would refuse for a reason that has nothing to do with the code under test, and
// a test asserting a refusal would pass for the wrong reason.
func loadInferenceSubject(t *testing.T) *loader.Packages {
	t.Helper()

	pkgs, err := loader.Load(inferenceSubject, []string{"./..."})
	if err != nil {
		t.Fatalf("the inference fixture module did not load: %v", err)
	}

	if problems := pkgs.Errors(); len(problems) != 0 {
		t.Fatalf("CONTROL FAILED: the fixture module does not type-check, so no inference below means anything: %v",
			problems)
	}

	return pkgs
}

// inferFixture runs the inference over one fixture file.
func inferFixture(t *testing.T, name string) (*InferredTypes, []Diagnostic) {
	t.Helper()

	src := loadSource(t, filepath.Join(inferenceDir, name))

	table, diags, err := BuildInferredTypes([]Source{src}, loadInferenceSubject(t), "")
	if err != nil {
		t.Fatalf("the inference over %s failed: %v", name, err)
	}

	return table, withCode(diags, inferenceName)
}

// assertInferred asserts one flow-level name carries exactly the expected type.
func assertInferred(t *testing.T, table *InferredTypes, flow, name, want string) {
	t.Helper()

	got, known := table.Name(flow, name)
	if !known {
		t.Errorf("%s carries no inferred type at all, want %s", name, want)

		return
	}

	if rendered := types.TypeString(got, nil); rendered != want {
		t.Errorf("%s inferred %s, want %s", name, rendered, want)
	}
}

// TestInferenceTypesEveryReferenceShapeAgainstRealPackages is the capability
// test: an exported, signature-less flow types end to end across a module
// boundary, through all three reference shapes the corpus census found.
//
// THE BRANCH TARGETS ARE THE DISCRIMINATOR. A branch routes on a PREDICATE and
// does not convert the datum, so `clean` and `suspect` carry the branch's INPUT
// type. The plausible-but-incorrect implementation an honest engineer writes
// first — every node taking its own reference's result type — compiles, builds
// clean, passes every other assertion here, and types both branch targets bool.
func TestInferenceTypesEveryReferenceShapeAgainstRealPackages(t *testing.T) {
	table, diags := inferFixture(t, "Screening.flow")

	if len(diags) != 0 {
		t.Errorf("the fully resolvable fixture produced inference diagnostics: %v", messages(diags))
	}

	flows := table.Flows()
	if len(flows) != 1 || flows[0] != "Screening" {
		t.Fatalf("the run inferred flows %v, want exactly [Screening]", flows)
	}

	// COMPOUND: a generic instantiation call, already applied, so its type is the
	// value it yields rather than a signature.
	assertInferred(t, table, "Screening", "ingest", inferencePkg+".Order")
	// BARE: a reference naming the func this file declares.
	assertInferred(t, table, "Screening", "enriched", inferencePkg+".Order")
	// SIMPLE QUALIFIED: a two-result signature whose error result is not the datum.
	assertInferred(t, table, "Screening", "scored", inferencePkg+".Scored")
	// PASS-THROUGH past a predicate, in BOTH directions.
	assertInferred(t, table, "Screening", "clean", inferencePkg+".Scored")
	assertInferred(t, table, "Screening", "suspect", inferencePkg+".Scored")

	names, ok := table.Flow("Screening")
	if !ok {
		t.Fatal("the flow Screening is absent from the table it was just inferred into")
	}
	t.Logf("Screening inferred %d names", len(names))
}

// TestInferenceTypesABareReferenceThroughTheFileDeclaredFunc pins the bare shape
// on its own, because it is 49 of the corpus's 154 references and it is the one
// shape that resolves through the FILE rather than through an import.
func TestInferenceTypesABareReferenceThroughTheFileDeclaredFunc(t *testing.T) {
	table, diags := inferFixture(t, "local-func.flow")

	if len(diags) != 0 {
		t.Errorf("the local-func fixture produced inference diagnostics: %v", messages(diags))
	}

	assertInferred(t, table, "locals", "scored", inferencePkg+".Scored")
}

// TestInferenceReportsADisagreeingFanInOverRealTypes is the deepening the
// STRUCTURAL typeflow analyzer cannot do: nothing in this fixture declares a type
// spelling, so there are no spellings to compare and only real inferred
// identities disagree.
//
// ITS KNOWN POSITIVE IS IN THE SAME PACKAGE: Screening.flow's branch is also a
// multi-input shape and is asserted silent above, so a check that reported every
// fan-in would fail there.
func TestInferenceReportsADisagreeingFanInOverRealTypes(t *testing.T) {
	_, diags := inferFixture(t, "disagreeing-fan-in.flow")

	errs := errorsIn(diags)
	if len(errs) != 1 {
		t.Fatalf("the disagreeing fan-in produced %d errors, want 1: %v", len(errs), messages(diags))
	}

	if !containsAll(errs[0].Message, "joins inputs whose inferred types disagree",
		inferencePkg+".Order", inferencePkg+".Scored") {
		t.Errorf("the diagnostic does not name both disagreeing types: %s", errs[0].Message)
	}
	t.Logf("disagreeing fan-in: %s", errs[0].Message)
}

// TestInferenceRefusesRatherThanGuessesAnUnresolvableReference pins that an
// unresolvable reference is REPORTED and its name left untyped, rather than
// handed back as a guess beside a nil error.
//
// ITS KNOWN POSITIVE IS IN THE SAME RUN: `ingest`, whose reference resolves, is
// asserted to carry a real type, so a run in which the inference had simply
// stopped working reads differently from this one.
func TestInferenceRefusesRatherThanGuessesAnUnresolvableReference(t *testing.T) {
	table, diags := inferFixture(t, "unresolvable-ref.flow")

	if len(diags) != 1 {
		t.Fatalf("the unresolvable reference produced %d diagnostics, want 1: %v", len(diags), messages(diags))
	}

	if diags[0].Severity != SeverityWarning {
		t.Errorf("the refusal carries severity %s, want warning", diags[0].Severity)
	}

	if !containsAll(diags[0].Message, "NotDeclaredAnywhere", "missing", "scored") {
		t.Errorf("the refusal does not name the node, the flow and the reference: %s", diags[0].Message)
	}

	if got, known := table.Name("missing", "scored"); known {
		t.Errorf("the unresolvable node was typed %s instead of being left untyped", types.TypeString(got, nil))
	}

	// THE KNOWN POSITIVE, same run: the resolvable node still typed.
	assertInferred(t, table, "missing", "ingest", inferencePkg+".Order")
	t.Logf("refusal: %s", diags[0].Message)
}

// TestInferenceReportsASignatureTheBodyContradicts pins that a declared output
// type the body does not deliver is reported over REAL types rather than over
// spellings, naming both the declared type and what the body infers.
//
// THE SIGNATURE IS THE AUTHOR'S STATED CONTRACT and the body must satisfy it, so
// the diagnostic names both sides rather than silently preferring either.
//
// THE AGREEING CONTROL MATTERS AS MUCH AS THE REPORT and runs here in the same
// test: signature-agrees.flow is identical but for the declared spelling and must
// produce NOTHING, or "reports a disagreement" is satisfied by an implementation
// that reports every declared output it sees.
func TestInferenceReportsASignatureTheBodyContradicts(t *testing.T) {
	_, diags := inferFixture(t, "signature-disagrees.flow")

	errs := errorsIn(diags)
	if len(errs) != 1 {
		t.Fatalf("the contradicted signature produced %d errors, want 1: %v", len(errs), messages(diags))
	}

	if !containsAll(errs[0].Message, "scored", inferencePkg+".Receipt", inferencePkg+".Scored") {
		t.Errorf("the diagnostic does not name the output, the declared type and the inferred one: %s",
			errs[0].Message)
	}
	t.Logf("signature disagreement: %s", errs[0].Message)

	// THE AGREEING CONTROL, same shape, same package, differing only in the
	// declared spelling.
	_, quiet := inferFixture(t, "signature-agrees.flow")
	if agreeing := errorsIn(quiet); len(agreeing) != 0 {
		t.Errorf("a flow whose body delivers exactly what it declares was reported: %v", messages(agreeing))
	}
}

// TestShippedRosterIsExactlyTheTwelveRegisteredAnalyzersInOrder pins the shipped
// set by NAME IN REGISTRATION ORDER.
//
// A COUNT IS NOT ENOUGH and that is why this pins names: a count is green for any
// arrangement summing to twelve and would bless a swapped or renamed analyzer.
// Six shipped prose sites across three modules state the number — lang/lint's
// doc.go and batch.go, this module's budget_test.go and driver.go, and two in
// lang/lsp's analyze.go — and before this test existed a thirteenth registered
// analyzer reddened nothing at all: every module suite stayed green and the
// budget test still passed, because one extra structural walk costs single-digit
// microseconds against a millisecond budget.
//
// IT ALSO ASSERTS THE INFERENCE ANALYZER IS ABSENT. TypeInferenceAnalyzer is a
// constructor precisely because it needs a caller-supplied *loader.Packages, and
// registering it would either force a Pass field change or leave it silent when
// no package set was supplied.
func TestShippedRosterIsExactlyTheTwelveRegisteredAnalyzersInOrder(t *testing.T) {
	want := []string{
		"symbols", "flowgraph", "resolve", "reachability", "cycles", "signature",
		"state", "switches", "errorrouting", "typeflow", "guidance", "checkpointanchor",
	}

	got := All()
	if len(got) != len(want) {
		t.Fatalf("All() returns %d analyzers, want %d — six shipped prose sites state the number: %v",
			len(got), len(want), namesOf(got))
	}

	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("registered analyzer %d is %q, want %q", i, got[i].Name, name)
		}
	}

	for _, a := range got {
		if a.Name == inferenceName {
			t.Errorf("%s is REGISTERED, but it is constructor-built because it needs a caller-supplied "+
				"package set; registering it forces a Pass field change or a silent lane", inferenceName)
		}
	}

	t.Logf("shipped roster pinned: %d analyzers in registration order", len(got))
}

// TestBuildInferredTypesRefusesANilPackageSet pins that the entry point refuses
// rather than handing back an empty table beside a nil error, which a caller
// cannot tell from a run that inferred nothing.
func TestBuildInferredTypesRefusesANilPackageSet(t *testing.T) {
	src := loadSource(t, filepath.Join(inferenceDir, "Screening.flow"))

	table, diags, err := BuildInferredTypes([]Source{src}, nil, "")
	if err == nil {
		t.Fatal("BuildInferredTypes accepted a nil package set instead of refusing it")
	}

	if table != nil || diags != nil {
		t.Errorf("BuildInferredTypes refused but still handed back a table %v and diagnostics %v", table, diags)
	}
}
