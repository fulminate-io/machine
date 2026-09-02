// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/analysis"
	"github.com/whitaker-io/machine/lang/loader"
)

// upstreamPkg is the import path of the cross-module fixture's upstream module.
const upstreamPkg = "example.com/upstream"

// loadUpstream loads the cross-module fixture's upstream module.
func loadUpstream(t *testing.T) *loader.Packages {
	t.Helper()
	pkgs, err := loader.Load(crossmoduleDir+"/upstream", []string{"./..."})
	if err != nil {
		t.Fatalf("the upstream fixture module did not load: %v", err)
	}

	return pkgs
}

// TestRegistrationFactsDeduplicateBySpellingAndSortByIt pins the property that
// makes a generated file a function of the ANSWER rather than of how many
// declarations reached it.
//
// TWO RUNS OVER THE SAME SOURCES MUST EMIT BYTE-IDENTICAL OUTPUT, so one type
// declared at four sites is one registration and the order is the spelling's
// rather than the derivation's discovery order.
func TestRegistrationFactsDeduplicateBySpellingAndSortByIt(t *testing.T) {
	required := &analysis.Registrations{Required: []analysis.Registration{
		{Spelling: "Zulu", Flow: "A", Name: "one"},
		{Spelling: "Alpha", Flow: "B", Name: "two"},
		{Spelling: "Zulu", Flow: "C", Name: "three"},
		{Spelling: "", Flow: "D", Name: "four"},
		{Spelling: "Mike", Flow: "E", Name: "five"},
	}}

	got := registrationFacts(required)
	if len(got) != 3 {
		t.Fatalf("registrationFacts produced %d registrations, want 3: %+v", len(got), got)
	}
	for i, want := range []string{"Alpha", "Mike", "Zulu"} {
		if got[i].Spelling != want {
			t.Errorf("registration %d is %q, want %q — sorted by spelling", i, got[i].Spelling, want)
		}
	}
	// THE DECLARATION THAT MADE IT NECESSARY IS KEPT, so a wrong emission can be
	// traced back to what asked for it. The first site under a spelling wins.
	if got[2].Flow != "A" || got[2].Name != "one" {
		t.Errorf("the deduplicated Zulu carries (%s, %s), want the first site that required it",
			got[2].Flow, got[2].Name)
	}
}

// TestRegistrationFactsAnswerNothingForAnAbsentTable covers the nil arm a caller
// reaches when the derivation produced no table at all.
func TestRegistrationFactsAnswerNothingForAnAbsentTable(t *testing.T) {
	if got := registrationFacts(nil); got != nil {
		t.Errorf("an absent registration table produced %+v, want nil", got)
	}
}

// TestBoundaryFactsLeaveAnAbsentFlowOutRatherThanEnteringItEmpty pins the
// distinction the lowering refuses on.
func TestBoundaryFactsLeaveAnAbsentFlowOutRatherThanEnteringItEmpty(t *testing.T) {
	// An empty Boundaries lists no flows, so the re-key produces an empty map
	// rather than a map of empty facts.
	got := boundaryFacts(&analysis.Boundaries{})
	if len(got) != 0 {
		t.Errorf("an empty boundary set re-keyed to %+v, want no entries at all", got)
	}
}

// TestMergeImportedRunsTheDependencyThroughTheSameGateAndReKeysItsAnswer is the
// bridge's whole contract.
//
// THE DERIVATION STAYS lang/analysis'S. The dependency's own source is run
// through the same Gate the run's sources went through, and only the KEY moves —
// onto the reference the consumer wrote, which is what everything downstream
// already looks up by.
func TestMergeImportedRunsTheDependencyThroughTheSameGateAndReKeysItsAnswer(t *testing.T) {
	dep := crossSource(t, upstreamFlow)
	programs, diags := buildFile(dep.File)
	if len(diags) != 0 {
		t.Fatalf("the upstream fixture must build clean:\n%s", strings.Join(messagesOf(diags), "\n"))
	}
	if len(programs) != 1 {
		t.Fatalf("the upstream fixture declares %d flows, want 1", len(programs))
	}

	facts := Facts{}
	imported := []Imported{{Ref: "upstream.Screen", Source: dep, Program: programs[0]}}

	refused := mergeImported(&facts, imported, loadUpstream(t), upstreamPkg)

	if facts.Imported["upstream.Screen"] != programs[0] {
		t.Error("the imported graph was not recorded under the reference the author wrote")
	}
	// THE BOUNDARY IS RE-KEYED TO THE REFERENCE. The declaration in this fixture
	// still carries its own name, so this exercises the fallback that finds the
	// fact under the reference's trailing part.
	names, known := facts.Boundary["upstream.Screen"]
	if !known {
		t.Fatalf("no boundary was re-keyed to upstream.Screen; the gate refused:\n%s",
			strings.Join(messagesOf(refused), "\n"))
	}
	if len(names.Outputs) != 2 {
		t.Errorf("the re-keyed boundary is %v, want the flow's two declared outputs", names.Outputs)
	}
}

// TestMergeImportedIsSilentWithNothingToMerge covers the early return, so a run
// carrying no cross-module reference pays nothing for the feature.
func TestMergeImportedIsSilentWithNothingToMerge(t *testing.T) {
	facts := Facts{}
	if refused := mergeImported(&facts, nil, nil, upstreamPkg); refused != nil {
		t.Errorf("merging nothing reported %+v", refused)
	}
	if facts.Imported != nil {
		t.Errorf("merging nothing built an imported map: %+v", facts.Imported)
	}
}

// TestMergeImportedCarriesADependencysGateFailureRatherThanDroppingIt pins that
// a dependency the analyzers cannot process is reported rather than silently
// leaving an absent boundary behind.
func TestMergeImportedCarriesADependencysGateFailureRatherThanDroppingIt(t *testing.T) {
	dep := crossSource(t, upstreamFlow)
	programs, _ := buildFile(dep.File)

	facts := Facts{}
	// A NIL PACKAGE SET IS WHAT THE GATE REFUSES ON, which is the reachable shape
	// of "this dependency could not be analyzed".
	refused := mergeImported(&facts, []Imported{{Ref: "upstream.Screen", Source: dep, Program: programs[0]}},
		nil, upstreamPkg)

	if len(refused) == 0 {
		t.Fatal("a dependency the gate could not analyze reported nothing")
	}
	joined := strings.Join(messagesOf(refused), "\n")
	if !strings.Contains(joined, "could not be analyzed") {
		t.Errorf("the refusal does not name the failure.\ngot:\n%s", joined)
	}
	// AND THE GRAPH IS STILL RECORDED, so the lowering refuses on the absent
	// boundary rather than on a missing dependency — two different messages for
	// two different problems.
	if facts.Imported["upstream.Screen"] == nil {
		t.Error("a dependency whose analysis failed was dropped from the imported set entirely")
	}
}

// TestRenderPrefersADiagnosticsOwnPathOverTheCallersFile pins the attribution a
// crossed-in diagnostic depends on.
//
// A REFUSAL THIS PACKAGE RAISED CARRIES NO PATH and the caller's name is right; a
// refusal that crossed in from the gate names the file that caused it, which may
// be any file in the run or a dependency in another module.
func TestRenderPrefersADiagnosticsOwnPathOverTheCallersFile(t *testing.T) {
	own := Diagnostic{Message: "a local refusal"}
	if got := Render("caller.flow", own); !strings.HasPrefix(got, "caller.flow:") {
		t.Errorf("a pathless diagnostic rendered as %q, want the caller's file", got)
	}

	crossed := Diagnostic{Message: "a crossed-in refusal", Path: "upstream/screen.flow"}
	if got := Render("caller.flow", crossed); !strings.HasPrefix(got, "upstream/screen.flow:") {
		t.Errorf("a diagnostic carrying its own path rendered as %q, want that path", got)
	}
}
