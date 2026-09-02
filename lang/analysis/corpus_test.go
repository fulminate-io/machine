// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// sharedContractDir is lang/ast's analysis-rejects corpus, read ACROSS THE
// MODULE BOUNDARY.
//
// Those four files are a fixed two-sided contract: lang/ast owns them and this
// module reads them, so a change to either side that breaks the other surfaces
// here rather than at integration time. THE CORPUS IS CLOSED — lang/ast's own
// suite asserts set equality against a pinned count of four, so a fifth file
// added there reds that suite on two axes. This module's own fixtures live under
// lang/analysis/testdata.
const sharedContractDir = astTestdata + "/analysis-rejects"

// strawmanDir holds the three canonical programs every rule is swept over.
const strawmanDir = astTestdata + "/strawman"

// rejectedBy maps each shared-contract fixture to the analyzer that must reject
// it.
//
// The mapping is the point. A fixture rejected by the WRONG analyzer is a defect
// a bare "some diagnostic appeared" assertion cannot see, and the whole value of
// the contract is that each file exercises the rule its own note describes.
var rejectedBy = map[string]string{
	"declare-after-use-loop.flow": "resolve",
	"destructuring-arm.flow":      "switches",
	"traversal-wide-var.flow":     "state",
	"wrapper-type-state.flow":     "state",
}

// TestSharedContractFixturesAreRejected is this plan's anchor capability gate,
// reject-what-is-wrong half.
func TestSharedContractFixturesAreRejected(t *testing.T) {
	fixtures := corpusFiles(t, sharedContractDir)
	if len(fixtures) != len(rejectedBy) {
		t.Fatalf("%s holds %d fixtures but this test maps %d; a fixture added to lang/ast without a "+
			"mapping here would otherwise be skipped: %v", sharedContractDir, len(fixtures), len(rejectedBy), fixtures)
	}

	for _, path := range fixtures {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			want, mapped := rejectedBy[name]
			if !mapped {
				t.Fatalf("%s is in the shared corpus but no analyzer is named for it", name)
			}

			src := loadSource(t, path)
			diags, err := Run([]Source{src}, All())
			if err != nil {
				t.Fatalf("analyzing %s failed: %v", name, err)
			}
			if got := withCode(diags, want); len(got) == 0 {
				t.Errorf("%s produced no %s diagnostic; the run reported %v", name, want, messages(diags))
			}
			t.Logf("%s rejected by %s: %v", name, want, messages(withCode(diags, want)))
		})
	}
}

// TestStrawmenProduceNoErrors is the anchor gate's other half: stay silent on
// what is right.
//
// WARNINGS AND HINTS ARE PERMITTED and deliberately not asserted absent —
// toy.flow's unconsumed `archive` output is a ruled hint, and asserting the
// canonical programs produce NOTHING would make that ruling unimplementable.
func TestStrawmenProduceNoErrors(t *testing.T) {
	for _, path := range corpusFiles(t, strawmanDir) {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			src := loadSource(t, path)
			diags, err := Run([]Source{src}, All())
			if err != nil {
				t.Fatalf("analyzing %s failed: %v", name, err)
			}
			if errs := errorsIn(diags); len(errs) != 0 {
				t.Errorf("%s produced %d errors: %v", name, len(errs), messages(errs))
			}
			t.Logf("%s: %d diagnostics, none of them errors: %v", name, len(diags), messages(diags))
		})
	}
}

// TestAnalyzerDocsCarryTheirDisclosures gates the six truthfulness statements
// the plan mandates.
//
// Each exists to stop a consumer over-reading a check's SILENCE as a proof:
// typeflow's silence is not a type-safety proof, state's bare-type check is a
// two-name denylist rather than validation, resolve ships no unimported-
// qualifier check, switches cannot prove coverage in v1, errorrouting's
// well-formedness leg is not the enforcement, and flowgraph's send modeling is
// one of two defensible readings. Those sentences change nothing observable when
// dropped, so nothing but a gate catches their omission.
//
// THE HOME IS Analyzer.Doc rather than a source comment, because Doc is what a
// downstream consumer can read AT RUNTIME — a Go comment is invisible to the LSP
// and the linter — and because it is assertable over All() regardless of whether
// the analyzer variables are exported.
func TestAnalyzerDocsCarryTheirDisclosures(t *testing.T) {
	required := map[string][]string{
		"flowgraph":    {"produces its Target name"},
		"typeflow":     {"not type checking"},
		"state":        {"denylist"},
		"resolve":      {"unimported-qualifier", "v82"},
		"switches":     {"prove coverage"},
		"errorrouting": {"not the enforcement"},
		"typeinference": {
			"IT IS NOT REGISTERED",
			"retyped consumer",
			"opt-in stability contract",
		},
	}

	registered := map[string]*Analyzer{}
	for _, a := range All() {
		registered[a.Name] = a
	}

	// THE CONSTRUCTED ANALYZER IS SEEDED EXPLICITLY. typeinference is built by a
	// constructor rather than registered, so a registry-only walk would skip its
	// disclosures entirely while every other gate stayed green — which is exactly
	// the silent gap this one exists to close. nil is safe: only Doc is read here,
	// and it is a constant.
	inference := TypeInferenceAnalyzer(nil)
	registered[inference.Name] = inference

	// THE CONTROL. A registry-driven gate is exactly the shape that passes
	// vacuously, so an empty registry is a loud failure rather than a loop that
	// runs zero times.
	if len(registered) == 0 {
		t.Fatal("CONTROL FAILED: the registry is empty, so this gate would pass vacuously")
	}

	for _, name := range sortedKeys(required) {
		a, found := registered[name]
		if !found {
			t.Errorf("analyzer %s is not registered, so its required disclosures cannot be checked", name)
			continue
		}
		for _, phrase := range required[name] {
			if !strings.Contains(strings.ToLower(a.Doc), strings.ToLower(phrase)) {
				t.Errorf("analyzer %s Doc omits its required disclosure %q", name, phrase)
			}
		}
	}
	// THE CENSUS LINE DISCLOSES THAT ITS POPULATION IS NOT All(). Without that
	// clause a reader comparing this count against the registry is off by one and
	// concludes the registry grew, which is the opposite of what this plan did.
	t.Logf("checked %d disclosures across %d analyzers, one of them constructed rather than registered",
		len(required), len(registered))
}

// corpusFiles lists a corpus directory's .flow sources, sorted.
//
// It carries the same empty-glob CONTROL lang/ast's own corpusFiles uses: a
// moved or renamed directory fails loudly instead of iterating an empty set and
// reporting success.
func corpusFiles(t *testing.T, dir string) []string {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(dir, "*.flow"))
	if err != nil {
		t.Fatalf("globbing %s failed: %v", dir, err)
	}
	if len(paths) == 0 {
		t.Fatalf("CONTROL FAILED: %s holds no .flow sources", dir)
	}
	sort.Strings(paths)
	return paths
}
