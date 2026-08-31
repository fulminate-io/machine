// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"path/filepath"
	"testing"
)

// signatureDir holds this module's own signature fixtures.
var signatureDir = filepath.Join("testdata", "signature")

// TestSignatureDeliveryAndUseArity covers both checks plus the accepting
// direction.
//
// subflow-and-use.flow is the reference shape from lang/ast's VALID corpus: it
// declares two outputs, delivers both by naming them as branch targets, and
// binds both at the use site.
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
		{fixture: "use-arity-mismatch.flow", names: []string{"binds 1 names", "declares 2 outputs", "ok, bad"}},
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
// folded into the delivery and arity legs, because a same-file-only
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
		t.Fatalf("the cross-file arity mismatch produced %d diagnostics, want 1: %v", len(got), messages(got))
	}
	if !containsAll(got[0].Message, "binds 1 names", "declares 2 outputs") {
		t.Errorf("the cross-file diagnostic does not report the arity: %s", got[0].Message)
	}
	t.Logf("cross-file: %v", messages(got))

	// THE ACCEPTING DIRECTION, in the same run and across the same boundary.
	// Without it the assertion above is satisfied by an analyzer that reports
	// every cross-file use.
	matching := parseSource(t, "matching.flow",
		"flow main\nsource ingest Poll\nuse screen screening from ingest -> clean, suspect\n"+
			"sink store warehouse.Insert from clean\nsink hold fraud.Quarantine from suspect\n")
	quiet, qerr := Run([]Source{matching, declarer}, []*Analyzer{SignatureAnalyzer})
	if qerr != nil {
		t.Fatalf("the matching cross-file run failed: %v", qerr)
	}
	if ok := withCode(quiet, SignatureAnalyzer.Name); len(ok) != 0 {
		t.Errorf("a cross-file use binding the right number of names was reported: %v", messages(ok))
	}
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
