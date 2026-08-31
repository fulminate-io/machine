// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// nameFact is a throwaway fact carrying one string, used to prove propagation.
type nameFact struct{ Value string }

// AFact marks nameFact as a Fact.
func (*nameFact) AFact() {}

// recorder builds an analyzer that appends its own name to a shared slice.
func recorder(name string, seen *[]string, requires ...*Analyzer) *Analyzer {
	return &Analyzer{
		Name:     name,
		Doc:      "records that it ran",
		Requires: requires,
		Run: func(_ *Pass) (any, error) {
			*seen = append(*seen, name)
			return nil, nil
		},
		ResultType: reflect.TypeOf((*any)(nil)).Elem(),
	}
}

// TestDriverRunsRequiresFirst pins topological execution.
//
// The analyzers are handed to the driver in the order C, B, A — the exact
// reverse of their dependency order. A driver that ran the set in the order it
// was given would observe [C B A] and this assertion separates the two
// implementations. Handing them over already sorted would pass against both and
// prove nothing.
func TestDriverRunsRequiresFirst(t *testing.T) {
	var seen []string
	a := recorder("A", &seen)
	b := recorder("B", &seen, a)
	c := recorder("C", &seen, b)

	if _, err := Run(nil, []*Analyzer{c, b, a}); err != nil {
		t.Fatalf("the driver refused an acyclic set: %v", err)
	}

	if got := strings.Join(seen, " "); got != "A B C" {
		t.Errorf("analyzers ran in order [%s], want [A B C]", got)
	}
}

// TestDriverRunsEachRequirementOnce pins that a diamond does not run its shared
// prerequisite twice, which a walk without a done-marker would.
func TestDriverRunsEachRequirementOnce(t *testing.T) {
	var seen []string
	base := recorder("base", &seen)
	left := recorder("left", &seen, base)
	right := recorder("right", &seen, base)
	top := recorder("top", &seen, left, right)

	if _, err := Run(nil, []*Analyzer{top}); err != nil {
		t.Fatalf("the driver refused a diamond: %v", err)
	}

	var bases int
	for _, name := range seen {
		if name == "base" {
			bases++
		}
	}
	if bases != 1 {
		t.Errorf("the shared prerequisite ran %d times, want 1: %v", bases, seen)
	}
	if seen[0] != "base" {
		t.Errorf("the shared prerequisite ran %s first, want base: %v", seen[0], seen)
	}
}

// TestDriverRefusesARequiresCycle pins that a cycle is a loud error naming its
// members, not a dropped edge and not a partial run.
//
// Asserting only that SOME error came back would pass against a driver that
// errors for any reason at all, so both names are required to appear in the
// message and the diagnostics are required to be empty.
func TestDriverRefusesARequiresCycle(t *testing.T) {
	var seen []string
	p := recorder("cyclic-p", &seen)
	q := recorder("cyclic-q", &seen)
	p.Requires = []*Analyzer{q}
	q.Requires = []*Analyzer{p}

	diags, err := Run(nil, []*Analyzer{p})
	if err == nil {
		t.Fatal("the driver accepted a Requires cycle")
	}
	for _, name := range []string{"cyclic-p", "cyclic-q"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the cycle error does not name %s: %v", name, err)
		}
	}
	if len(diags) != 0 {
		t.Errorf("a refused run returned %d diagnostics, want none", len(diags))
	}
}

// TestFactCrossesFileBoundary pins the half of the framework nothing else fails
// without until the signature analyzer meets a cross-file `use`.
//
// P exports a fact while visiting file one; Q, which requires P, imports it while
// visiting file two. A stubbed fact store leaves Q with nothing.
func TestFactCrossesFileBoundary(t *testing.T) {
	one := parseSource(t, "one.flow", "flow one\nsource ingest Poll\nsink done audit.Store from ingest\n")
	two := parseSource(t, "two.flow", "flow two\nsource ingest Poll\nsink done audit.Store from ingest\n")

	exporter := &Analyzer{
		Name: "fact-exporter",
		Doc:  "exports a fact about the flow declared in one.flow",
		Run: func(p *Pass) (any, error) {
			for _, src := range p.Sources {
				if src.Path == "one.flow" {
					p.ExportFact("one", &nameFact{Value: "declared in one.flow"})
				}
			}
			return nil, nil
		},
	}

	var observed string
	importer := &Analyzer{
		Name:     "fact-importer",
		Doc:      "imports that fact while visiting two.flow",
		Requires: []*Analyzer{exporter},
		Run: func(p *Pass) (any, error) {
			for _, src := range p.Sources {
				if src.Path != "two.flow" {
					continue
				}
				var got nameFact
				if p.ImportFact("one", &got) {
					observed = got.Value
				}
			}
			return nil, nil
		},
	}

	if _, err := Run([]Source{one, two}, []*Analyzer{importer}); err != nil {
		t.Fatalf("the fact run failed: %v", err)
	}
	if observed != "declared in one.flow" {
		t.Errorf("the importer observed %q, want the fact the exporter recorded", observed)
	}
}

// TestImportFactMissesAnUnexportedObject pins the negative direction: a lookup
// for an object nothing exported reports false and leaves the target untouched.
//
// Without it, "the importer observed the fact" above is satisfied by an
// ImportFact that returns true unconditionally.
func TestImportFactMissesAnUnexportedObject(t *testing.T) {
	var found bool
	var value string
	probe := &Analyzer{
		Name: "fact-probe",
		Doc:  "looks for a fact nobody exported",
		Run: func(p *Pass) (any, error) {
			var got nameFact
			found = p.ImportFact("nobody", &got)
			value = got.Value
			return nil, nil
		},
	}

	if _, err := Run(nil, []*Analyzer{probe}); err != nil {
		t.Fatalf("the probe run failed: %v", err)
	}
	if found {
		t.Error("ImportFact reported a fact for an object nothing exported")
	}
	if value != "" {
		t.Errorf("ImportFact wrote %q into the target on a miss", value)
	}
}

// TestDriverStampsTheReportingAnalyzersName pins that Code is structural rather
// than a convention each analyzer has to remember.
func TestDriverStampsTheReportingAnalyzersName(t *testing.T) {
	reporter := &Analyzer{
		Name: "stamper",
		Doc:  "reports one diagnostic and sets no Code",
		Run: func(p *Pass) (any, error) {
			p.Report(p.Sources[0], Diagnostic{Message: "something", Severity: SeverityWarning})
			return nil, nil
		},
	}

	stamped := parseSource(t, "stamped.flow", "flow one\nsource ingest Poll\nsink done audit.Store from ingest\n")
	diags, err := Run([]Source{stamped}, []*Analyzer{reporter})
	if err != nil {
		t.Fatalf("the reporting run failed: %v", err)
	}
	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics, want 1: %v", len(diags), messages(diags))
	}
	if diags[0].Code != "stamper" {
		t.Errorf("the diagnostic carries Code %q, want the reporting analyzer's name", diags[0].Code)
	}
}

// TestDriverSurfacesAnAnalyzerError pins that a failing analyzer aborts the run
// under its own name rather than being absorbed.
func TestDriverSurfacesAnAnalyzerError(t *testing.T) {
	failing := &Analyzer{
		Name: "explodes",
		Doc:  "always fails",
		Run:  func(_ *Pass) (any, error) { return nil, errFailing },
	}

	diags, err := Run(nil, []*Analyzer{failing})
	if err == nil {
		t.Fatal("the driver absorbed an analyzer failure")
	}
	if !strings.Contains(err.Error(), "explodes") {
		t.Errorf("the failure does not name the analyzer: %v", err)
	}
	if len(diags) != 0 {
		t.Errorf("a failed run returned %d diagnostics, want none", len(diags))
	}
}

// TestDiagnosticsSortByPositionThenCode pins the deterministic ordering, using
// two analyzers reporting out of position order so a run that returned arrival
// order would be visibly different.
func TestDiagnosticsSortByPositionThenCode(t *testing.T) {
	late := reportAt("zebra", 40)
	early := reportAt("alpha", 10)
	tie := reportAt("beta", 10)

	ordering := parseSource(t, "ordering.flow", "flow one\nsource ingest Poll\nsink done audit.Store from ingest\n")
	diags, err := Run([]Source{ordering}, []*Analyzer{late, tie, early})
	if err != nil {
		t.Fatalf("the ordering run failed: %v", err)
	}

	var got []string
	for _, d := range diags {
		got = append(got, d.Code)
	}
	if want := "alpha beta zebra"; strings.Join(got, " ") != want {
		t.Errorf("diagnostics ordered [%s], want [%s]", strings.Join(got, " "), want)
	}
}

// reportAt builds an analyzer reporting one diagnostic at a fixed offset.
func reportAt(name string, offset int) *Analyzer {
	return &Analyzer{
		Name: name,
		Doc:  "reports one diagnostic at a fixed offset",
		Run: func(p *Pass) (any, error) {
			p.Report(p.Sources[0], Diagnostic{Pos: position(offset), Message: name, Severity: SeverityHint})
			return nil, nil
		},
	}
}

// TestDiagnosticsAreFileAttributedAndSortStable gates what the Path amendment
// actually buys, rather than that the field exists.
//
// ATTRIBUTION. Both findings sit at OFFSET 0, which is the deliberate choice:
// every parsed file's tree starts there, so position cannot tell them apart and
// the only thing that can is the file each is about. They also carry the same
// message and the same code, so nothing else in the sort key can stand in for
// Path.
//
// SORT STABILITY ACROSS SOURCES ORDER. Running the same two files with the
// Sources slice reversed returns identical diagnostics. That is the property a
// consumer needs — two runs over the same files produce the same output — and it
// is the leg that discriminates: with Path leading the key the order follows the
// content, while without it the two findings tie and the sort falls back to
// arrival order, so reversing the input reverses the output.
func TestDiagnosticsAreFileAttributedAndSortStable(t *testing.T) {
	one := loadSource(t, filepath.Join("testdata", "driver", "one.flow"))
	two := loadSource(t, filepath.Join("testdata", "driver", "two.flow"))

	atZero := &Analyzer{
		Name: "at-zero",
		Doc:  "reports one identical diagnostic at offset zero for every source",
		Run: func(p *Pass) (any, error) {
			for _, src := range p.Sources {
				p.Report(src, Diagnostic{
					Pos:      position(0),
					Message:  "a finding about this file",
					Severity: SeverityWarning,
				})
			}
			return nil, nil
		},
	}

	forward, err := Run([]Source{one, two}, []*Analyzer{atZero})
	if err != nil {
		t.Fatalf("the forward run failed: %v", err)
	}
	if len(forward) != 2 {
		t.Fatalf("got %d diagnostics, want one per source: %v", len(forward), messages(forward))
	}
	if forward[0].Path == forward[1].Path {
		t.Fatalf("two findings in two files both carry path %q, so neither can be attributed", forward[0].Path)
	}
	for i, want := range []string{one.Path, two.Path} {
		if forward[i].Path != want {
			t.Errorf("diagnostic %d carries path %q, want %q", i, forward[i].Path, want)
		}
	}

	reverse, rerr := Run([]Source{two, one}, []*Analyzer{atZero})
	if rerr != nil {
		t.Fatalf("the reversed run failed: %v", rerr)
	}
	if !reflect.DeepEqual(forward, reverse) {
		t.Errorf("reversing the Sources slice changed the output.\nforward: %v\nreverse: %v",
			messages(forward), messages(reverse))
	}
}

// TestDriverRefusesASourceWithNoTree pins that a caller-supplied source missing
// its parsed tree is refused at the entry point, by name.
//
// Every analyzer reads Source.File, so without this the failure is a nil
// dereference inside whichever analyzer happens to run first — a panic naming an
// internal walker rather than the input that caused it.
func TestDriverRefusesASourceWithNoTree(t *testing.T) {
	good := parseSource(t, "good.flow", "flow one\nsource ingest Poll\nsink done audit.Store from ingest\n")

	for _, tc := range []struct {
		name string
		src  Source
		want string
	}{
		{name: "named", src: Source{Path: "half-read.flow"}, want: "half-read.flow"},
		{name: "unnamed", src: Source{}, want: "(unnamed)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Run([]Source{good, tc.src}, []*Analyzer{SymbolsAnalyzer})
			if err == nil {
				t.Fatal("a source with no parsed tree was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name the offending source: %v", err)
			}
		})
	}
}
