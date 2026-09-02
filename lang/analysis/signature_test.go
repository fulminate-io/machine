// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"path/filepath"
	"reflect"
	"testing"
)

// signatureDir holds this module's own signature fixtures.
var signatureDir = filepath.Join("testdata", "signature")

// TestSignatureDeliveryAndUseArity covers both checks plus the accepting
// direction.
//
// THE NAME IS DELIBERATELY UNCHANGED. Bindings are no longer checked by arity,
// but landed criterion 6c2411eb31a4a5c3321743d9c83a50f9 runs this test BY NAME,
// so renaming it would break a gate that has nothing to do with this change.
// What the test asserts moved to by-name binding; what it is called did not.
//
// subflow-and-use.flow is the reference shape from lang/ast's VALID corpus: it
// declares two outputs, delivers both by naming them as branch targets, and
// binds both by name at the use site.
func TestSignatureDeliveryAndUseArity(t *testing.T) {
	valid := loadSource(t, filepath.Join(astTestdata, "valid", "subflow-and-use.flow"))
	if got := withCode(analyze(t, SignatureAnalyzer, valid), SignatureAnalyzer.Name); len(got) != 0 {
		t.Errorf("subflow-and-use.flow is a VALID fixture but produced signature diagnostics: %v", messages(got))
	}

	for _, tc := range []struct {
		fixture string
		names   []string
	}{
		{fixture: "undelivered-output.flow", names: []string{"declares the output bad", "no statement delivers it"}},
		{
			fixture: "use-binds-unknown-name.flow",
			names:   []string{"binds clean", "which is not an output of that flow", "its outputs are", "ok, bad"},
		},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			src := loadSource(t, filepath.Join(signatureDir, tc.fixture))
			diags := withCode(analyze(t, SignatureAnalyzer, src), SignatureAnalyzer.Name)
			if len(diags) != 1 {
				t.Fatalf("got %d signature diagnostics, want 1: %v", len(diags), messages(diags))
			}
			if !containsAll(diags[0].Message, tc.names...) {
				t.Errorf("the diagnostic does not say what is wrong: %s", diags[0].Message)
			}
			if diags[0].Severity != SeverityError {
				t.Errorf("the diagnostic carries severity %s, want error", diags[0].Severity)
			}
			t.Logf("%s: %v", tc.fixture, messages(diags))
		})
	}
}

// TestSignatureResolvesAcrossFilesViaFacts is asserted separately rather than
// folded into the delivery and binding legs, because a same-file-only
// implementation passes every single-file assertion and fails only when the
// referenced flow moves — which is precisely the shape that ships broken.
//
// THE DECLARING FILE IS SECOND IN THE RUN, so an implementation that only ever
// looked at names it had already walked past would find nothing and stay silent.
func TestSignatureResolvesAcrossFilesViaFacts(t *testing.T) {
	caller := parseSource(t, "caller.flow",
		"flow main\nsource ingest Poll\nuse screen screening from ingest -> clean\n"+
			"sink store warehouse.Insert from clean\n")
	declarer := parseSource(t, "declarer.flow",
		"flow screening (Order) -> ok OkResult, bad ErrResult\n"+
			"branch check fraud.Clean from in -> ok, bad\n")

	diags, err := Run([]Source{caller, declarer}, []*Analyzer{SignatureAnalyzer})
	if err != nil {
		t.Fatalf("the cross-file run failed: %v", err)
	}
	got := withCode(diags, SignatureAnalyzer.Name)
	if len(got) != 1 {
		t.Fatalf("the cross-file unknown binding produced %d diagnostics, want 1: %v", len(got), messages(got))
	}
	if !containsAll(got[0].Message, "binds clean", "which is not an output of that flow", "ok, bad") {
		t.Errorf("the cross-file diagnostic does not name the binding or the real outputs: %s", got[0].Message)
	}
	t.Logf("cross-file: %v", messages(got))

	// THE ACCEPTING DIRECTION, in the same run and across the same boundary, and
	// it is also THE DISCRIMINATOR for by-name binding: the bindings arrive OUT
	// OF DECLARATION ORDER. Under the retired positional rule this bound `bad` to
	// `ok` and `ok` to `bad` and said nothing, so an implementation that still
	// matches by position passes every count assertion and fails right here.
	matching := parseSource(t, "matching.flow",
		"flow main\nsource ingest Poll\nuse screen screening from ingest -> bad, ok\n"+
			"sink store warehouse.Insert from ok\nsink hold fraud.Quarantine from bad\n")
	quiet, qerr := Run([]Source{matching, declarer}, []*Analyzer{SignatureAnalyzer})
	if qerr != nil {
		t.Fatalf("the matching cross-file run failed: %v", qerr)
	}
	if ok := withCode(quiet, SignatureAnalyzer.Name); len(ok) != 0 {
		t.Errorf("a cross-file use binding both outputs out of declaration order was reported: %v", messages(ok))
	}

	// A SUBSET IS LEGAL. A caller wanting one of two outputs says so by naming
	// it, and there is no count to violate.
	subset := parseSource(t, "subset.flow",
		"flow main\nsource ingest Poll\nuse screen screening from ingest -> bad\n"+
			"sink hold fraud.Quarantine from bad\n")
	partial, perr := Run([]Source{subset, declarer}, []*Analyzer{SignatureAnalyzer})
	if perr != nil {
		t.Fatalf("the subset cross-file run failed: %v", perr)
	}
	if ok := withCode(partial, SignatureAnalyzer.Name); len(ok) != 0 {
		t.Errorf("a use binding a legal SUBSET of the outputs was reported: %v", messages(ok))
	}

	// A DUPLICATE IS REFUSED, reported at the second occurrence.
	repeated := parseSource(t, "repeated.flow",
		"flow main\nsource ingest Poll\nuse screen screening from ingest -> ok, ok\n"+
			"sink store warehouse.Insert from ok\n")
	dupes, derr := Run([]Source{repeated, declarer}, []*Analyzer{SignatureAnalyzer})
	if derr != nil {
		t.Fatalf("the duplicate cross-file run failed: %v", derr)
	}
	dup := withCode(dupes, SignatureAnalyzer.Name)
	if len(dup) != 1 {
		t.Fatalf("the duplicate binding produced %d diagnostics, want 1: %v", len(dup), messages(dup))
	}
	if !containsAll(dup[0].Message, "binds ok more than once") {
		t.Errorf("the duplicate diagnostic does not say the name was bound twice: %s", dup[0].Message)
	}
	t.Logf("duplicate: %v", messages(dup))
}

// TestSignatureIsSilentOnAnUnresolvableFlowReference pins the documented
// boundary: a reference naming a flow that is not in the run is not a
// diagnostic, because deciding it needs a loader.
func TestSignatureIsSilentOnAnUnresolvableFlowReference(t *testing.T) {
	src := parseSource(t, "elsewhere.flow",
		"flow main\nsource ingest Poll\nuse screen other.screening from ingest -> clean\n"+
			"sink store warehouse.Insert from clean\n")
	if got := withCode(analyze(t, SignatureAnalyzer, src), SignatureAnalyzer.Name); len(got) != 0 {
		t.Errorf("a use naming a flow outside the run was reported: %v", messages(got))
	}
}

// TestSignatureIsSilentOnTheCanonicalCorpus is the standing strawman sweep. No
// strawman declares a signature or uses another flow, so this records that the
// analyzer has nothing to say about them rather than leaving it assumed.
func TestSignatureIsSilentOnTheCanonicalCorpus(t *testing.T) {
	for name, diags := range sweepCorpus(t, SignatureAnalyzer, strawmanDir) {
		if len(diags) != 0 {
			t.Errorf("strawman %s produced signature diagnostics: %v", name, messages(diags))
		}
	}
	for name, diags := range sweepCorpus(t, SignatureAnalyzer, filepath.Join(astTestdata, "valid")) {
		if len(diags) != 0 {
			t.Errorf("valid fixture %s produced signature diagnostics: %v", name, messages(diags))
		}
	}
}

// boundaryProbe reads the signature analyzer's exported boundary through the
// driver's OWN fact channel, which is the only way any caller sees a fact: the
// driver holds them in a map local to one run, so a test that reached around it
// would be asserting against a second copy rather than against what a real
// consumer receives.
func boundaryProbe(into map[string]flowOutputsFact) *Analyzer {
	return &Analyzer{
		Name:     "boundaryprobe",
		Doc:      "boundaryprobe is a test-only analyzer that imports every flow's outputs fact",
		Requires: []*Analyzer{SignatureAnalyzer, SymbolsAnalyzer},
		Run: func(p *Pass) (any, error) {
			table, ok := p.ResultOf[SymbolsAnalyzer].(*SymbolTable)
			if !ok {
				return nil, errNoSymbols
			}
			for f := range table.Files {
				for i := range table.Files[f].Flows {
					var fact flowOutputsFact
					if name := table.Files[f].Flows[i].Name; p.ImportFact(name, &fact) {
						into[name] = fact
					}
				}
			}
			return nil, nil
		},
		ResultType: reflect.TypeOf((*struct{})(nil)),
		FactTypes:  []Fact{(*flowOutputsFact)(nil)},
	}
}

// producedNamesOf is one fact's produced boundary as plain names, in the order
// the fact carries them.
func producedNamesOf(fact flowOutputsFact) []string {
	names := make([]string, 0, len(fact.Produced))
	for _, produced := range fact.Produced {
		names = append(names, produced.Name)
	}
	return names
}

// TestASignatureLessFlowStillExportsItsBoundary pins the erasure fix: a flow with
// no header exports the names its body produces and the statement each connects
// from, rather than exporting nothing at all.
//
// THE KNOWN POSITIVE IS IN THE SAME RUN. A headered flow sits beside it and must
// still carry its DECLARED outputs, so a fact channel that had simply stopped
// working reads differently from one that now carries both shapes.
func TestASignatureLessFlowStillExportsItsBoundary(t *testing.T) {
	headerless := parseSource(t, "headerless.flow",
		"flow enrich\nsource ingest Poll\ntransform tidy pkg.Tidy from ingest\n"+
			"branch check pkg.Clean from tidy -> ok, bad\nsink out pkg.Write from ok\n"+
			"sink err pkg.Err from bad\n")
	headered := parseSource(t, "headered.flow",
		"flow screening (Order) -> ok OkResult, bad ErrResult\n"+
			"branch check fraud.Clean from in -> ok, bad\n")

	facts := map[string]flowOutputsFact{}
	if _, err := Run([]Source{headerless, headered}, []*Analyzer{boundaryProbe(facts)}); err != nil {
		t.Fatalf("the boundary run failed: %v", err)
	}

	enrich, exported := facts["enrich"]
	if !exported {
		t.Fatal("the signature-less flow enrich exported NO fact at all, which is the erasure this fixes")
	}
	if len(enrich.Outputs) != 0 {
		t.Errorf("enrich declares no signature but its fact carries declared outputs %v", enrich.Outputs)
	}
	got := producedNamesOf(enrich)
	want := []string{"ingest", "tidy", "ok", "bad"}
	if len(got) != len(want) {
		t.Fatalf("enrich's boundary is %v, want the four names its body produces %v", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("enrich's boundary at %d is %q, want %q — the boundary reads in body order", i, got[i], name)
		}
	}
	for _, produced := range enrich.Produced {
		if produced.Stmt < 0 {
			t.Errorf("the boundary name %q carries statement %d, so it connects from nothing", produced.Name, produced.Stmt)
		}
	}

	// THE KNOWN POSITIVE, same run, same channel.
	screening, ok := facts["screening"]
	if !ok {
		t.Fatal("CONTROL FAILED: the headered flow exported no fact either, so the fact channel proves nothing")
	}
	if len(screening.Outputs) != 2 {
		t.Errorf("the headered flow carries %d declared outputs, want 2: %v", len(screening.Outputs), screening.Outputs)
	}
	t.Logf("signature-less boundary: %v", got)
}

// TestAHeaderedFlowStillDeclaresItsOwnOutputs pins that forwarding the produced
// boundary did not overwrite what a header states.
//
// Outputs is the DECLARED set and Produced is what the body makes; a header
// SELECTS the public subset from the second, so the two are distinct fields and
// collapsing them would silently make every internal name bindable.
func TestAHeaderedFlowStillDeclaresItsOwnOutputs(t *testing.T) {
	headered := parseSource(t, "headered.flow",
		"flow screening (Order) -> ok OkResult, bad ErrResult\n"+
			"branch check fraud.Clean from in -> ok, bad\ntransform tidy pkg.Tidy from ok\n")

	facts := map[string]flowOutputsFact{}
	if _, err := Run([]Source{headered}, []*Analyzer{boundaryProbe(facts)}); err != nil {
		t.Fatalf("the headered run failed: %v", err)
	}

	fact, exported := facts["screening"]
	if !exported {
		t.Fatal("the headered flow exported no outputs fact")
	}
	if len(fact.Outputs) != 2 || fact.Outputs[0] != "ok" || fact.Outputs[1] != "bad" {
		t.Errorf("the declared outputs are %v, want [ok bad] in signature order", fact.Outputs)
	}
	// The body produces `tidy` as well, which is NOT declared. It rides in
	// Produced and must not reach Outputs, or the header stops selecting.
	produced := producedNamesOf(fact)
	if len(produced) < 3 {
		t.Fatalf("the headered flow's produced boundary is %v, want it to carry the internal name too", produced)
	}
	for _, name := range fact.Outputs {
		if name == "tidy" {
			t.Error("an internal name reached the DECLARED outputs, so the header no longer selects")
		}
	}
	t.Logf("headered declared %v over produced %v", fact.Outputs, produced)
}

// TestAUseOfASignatureLessFlowIsCheckedByName is what the forwarded boundary
// BUYS. Before it, a use of a headerless flow imported no fact and was silent;
// now the names that flow produces are what a consumer's bindings are checked
// against, so such a dependency goes from unchecked to checked.
//
// IT CARRIES ITS OWN KNOWN POSITIVE IN THE SAME RUN: the accepting direction
// alone is satisfied by a check that stopped running, so a binding naming an
// output the headerless flow does NOT produce must be reported and must list the
// real names.
func TestAUseOfASignatureLessFlowIsCheckedByName(t *testing.T) {
	dependency := parseSource(t, "dependency.flow",
		"flow enrich\nsource ingest Poll\ntransform tidy pkg.Tidy from ingest\n"+
			"branch check pkg.Clean from tidy -> ok, bad\nsink out pkg.Write from ok\n"+
			"sink err pkg.Err from bad\n")

	rejected := parseSource(t, "rejected.flow",
		"flow main\nsource feed Poll\nuse sub enrich from feed -> clean\n"+
			"sink store pkg.Write from clean\n")
	diags, err := Run([]Source{rejected, dependency}, []*Analyzer{SignatureAnalyzer})
	if err != nil {
		t.Fatalf("the headerless-dependency run failed: %v", err)
	}
	got := withCode(diags, SignatureAnalyzer.Name)
	if len(got) != 1 {
		t.Fatalf("a binding naming nothing the headerless flow produces gave %d diagnostics, want 1: %v",
			len(got), messages(got))
	}
	if !containsAll(got[0].Message, "binds clean", "which is not an output of that flow", "ok", "bad") {
		t.Errorf("the diagnostic does not name the binding or the headerless flow's real outputs: %s", got[0].Message)
	}
	t.Logf("headerless dependency checked by name: %s", got[0].Message)

	// THE ACCEPTING DIRECTION, across the same boundary: names the flow really
	// does produce are bound without complaint, out of body order.
	accepted := parseSource(t, "accepted.flow",
		"flow main\nsource feed Poll\nuse sub enrich from feed -> bad, ok\n"+
			"sink store pkg.Write from ok\nsink hold pkg.Err from bad\n")
	quiet, qerr := Run([]Source{accepted, dependency}, []*Analyzer{SignatureAnalyzer})
	if qerr != nil {
		t.Fatalf("the accepting headerless run failed: %v", qerr)
	}
	if ok := withCode(quiet, SignatureAnalyzer.Name); len(ok) != 0 {
		t.Errorf("a use binding names the headerless flow really produces was reported: %v", messages(ok))
	}
}

// TestFlowOutputsFactIsAFact pins that the signature analyzer's fact satisfies
// the framework's marker, which is what lets it cross a file boundary at all.
func TestFlowOutputsFactIsAFact(t *testing.T) {
	var fact Fact = &flowOutputsFact{Outputs: []string{"ok", "bad"}}
	fact.AFact()

	typed, ok := fact.(*flowOutputsFact)
	if !ok {
		t.Fatalf("the fact round-tripped as %T", fact)
	}
	if len(typed.Outputs) != 2 {
		t.Errorf("the fact carries %d outputs, want 2", len(typed.Outputs))
	}
}
