// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
)

// updateGolden rewrites the regenerated expectation files instead of comparing
// against them.
var updateGolden = flag.Bool("update", false, "regenerate the golden files from the fixtures")

// errFailing is the failure a deliberately-broken test analyzer returns.
var errFailing = errors.New("deliberate analyzer failure")

// goldenDir holds this module's regenerated expectation files.
const goldenDir = "testdata/golden"

// position builds a one-line position at a byte offset, for tests that care
// about ordering rather than about real source.
func position(offset int) ast.Position {
	return ast.Position{Offset: offset, Line: 1, Col: offset + 1}
}

// astTestdata is lang/ast's corpus, read across the module boundary.
//
// The four analysis-rejects fixtures there are a FIXED CROSS-MODULE CONTRACT:
// lang/ast owns them and this module reads them, so a change to either side that
// breaks the other surfaces here rather than at integration time.
const astTestdata = "../ast/testdata"

// strawmanDir holds the three canonical programs every rule is swept over.
var strawmanDir = filepath.Join(astTestdata, "strawman")

// strawmanFiles are the canonical programs, named rather than globbed so a
// missing one is a failure instead of a shorter loop.
var strawmanFiles = []string{"enrichment.flow", "payments.flow", "toy.flow"}

// parseSource parses src under path, failing the test if it does not parse.
func parseSource(t *testing.T, path, src string) Source {
	t.Helper()

	file, err := ast.Parse([]byte(src))
	if err != nil {
		t.Fatalf("%s does not parse: %v", path, err)
	}
	return Source{Path: path, Src: []byte(src), File: file}
}

// readFixture reads a corpus file's bytes without parsing them, for the tests
// that need the parse to fail.
func readFixture(t *testing.T, path string) ([]byte, error) {
	t.Helper()

	return os.ReadFile(path) //nolint:gosec // a test reading its own corpus
}

// loadSource reads and parses a corpus file.
func loadSource(t *testing.T, path string) Source {
	t.Helper()

	src, err := os.ReadFile(path) //nolint:gosec // a test reading its own corpus
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	return parseSource(t, path, string(src))
}

// strawmen loads the three canonical programs.
func strawmen(t *testing.T) []Source {
	t.Helper()

	out := make([]Source, 0, len(strawmanFiles))
	for _, name := range strawmanFiles {
		out = append(out, loadSource(t, filepath.Join(strawmanDir, name)))
	}
	return out
}

// analyze runs one analyzer, and everything it requires, over one source.
func analyze(t *testing.T, a *Analyzer, src Source) []Diagnostic {
	t.Helper()

	diags, err := Run([]Source{src}, []*Analyzer{a})
	if err != nil {
		t.Fatalf("analyzer %s failed on %s: %v", a.Name, src.Path, err)
	}
	return diags
}

// resultOf runs an analyzer THROUGH THE DRIVER and returns the value it
// produced, alongside every diagnostic the run reported.
//
// The capture rides a throwaway analyzer that Requires the one under test and
// reads it out of Pass.ResultOf, rather than calling Analyzer.Run directly with
// a hand-built Pass. Calling Run directly would not exercise the dependency
// ordering, and an analyzer whose Requires list is wrong would still pass.
func resultOf(t *testing.T, a *Analyzer, srcs ...Source) (any, []Diagnostic) {
	t.Helper()

	var captured any
	probe := &Analyzer{
		Name:     "probe-" + a.Name,
		Doc:      "captures the result of " + a.Name,
		Requires: []*Analyzer{a},
		Run: func(p *Pass) (any, error) {
			captured = p.ResultOf[a]
			return nil, nil
		},
	}

	diags, err := Run(srcs, []*Analyzer{probe})
	if err != nil {
		t.Fatalf("running %s failed: %v", a.Name, err)
	}
	return captured, diags
}

// symbolsOf runs the symbols analyzer and returns its table.
func symbolsOf(t *testing.T, srcs ...Source) (*SymbolTable, []Diagnostic) {
	t.Helper()

	got, diags := resultOf(t, SymbolsAnalyzer, srcs...)
	table, ok := got.(*SymbolTable)
	if !ok {
		t.Fatalf("the symbols analyzer produced %T, want *SymbolTable", got)
	}
	return table, diags
}

// graphsOf runs the flowgraph analyzer and returns its graphs.
func graphsOf(t *testing.T, srcs ...Source) (*GraphSet, []Diagnostic) {
	t.Helper()

	got, diags := resultOf(t, FlowgraphAnalyzer, srcs...)
	set, ok := got.(*GraphSet)
	if !ok {
		t.Fatalf("the flowgraph analyzer produced %T, want *GraphSet", got)
	}
	return set, diags
}

// checkGolden compares rendered lines against a golden file, rewriting it when
// -update is set.
//
// The expectation is REGENERATED FROM THE FIXTURES rather than typed from prose,
// because a count typed into a test is a tree-derived number masquerading as a
// locked one and goes stale the moment the corpus moves.
func checkGolden(t *testing.T, name string, lines []string) {
	t.Helper()

	path := filepath.Join(goldenDir, name)
	body := strings.Join(lines, "\n") + "\n"
	if *updateGolden {
		if err := os.MkdirAll(goldenDir, 0o750); err != nil {
			t.Fatalf("cannot create %s: %v", goldenDir, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("cannot write %s: %v", path, err)
		}
		t.Logf("rewrote %s (%d lines)", path, len(lines))
		return
	}

	want, err := os.ReadFile(path) //nolint:gosec // a test reading its own golden
	if err != nil {
		t.Fatalf("cannot read %s (regenerate with -update): %v", path, err)
	}
	if string(want) != body {
		t.Errorf("%s is stale.\n--- want ---\n%s\n--- got ---\n%s", path, want, body)
	}
}

// withCode keeps only the diagnostics one analyzer reported.
//
// A run includes every analyzer the named one requires, and those report too, so
// a test asserting "this analyzer said nothing" has to say whose silence it
// means.
func withCode(diags []Diagnostic, code string) []Diagnostic {
	out := make([]Diagnostic, 0, len(diags))
	for _, d := range diags {
		if d.Code == code {
			out = append(out, d)
		}
	}
	return out
}

// containsAll reports whether every fragment appears in s.
func containsAll(s string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(s, fragment) {
			return false
		}
	}
	return true
}

// sortedKeys is a name table's keys in a stable order, for failure messages.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// messages renders diagnostics for a failure message.
func messages(diags []Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Pos.String()+" ["+d.Code+"/"+d.Severity.String()+"] "+d.Message)
	}
	return out
}

// firstFlow returns the first flow declaration in a parsed file.
func firstFlow(t *testing.T, src Source) ast.FlowDecl {
	t.Helper()

	for _, decl := range src.File.Decls {
		if flow, ok := decl.(ast.FlowDecl); ok {
			return flow
		}
	}
	t.Fatalf("%s declares no flow", src.Path)
	return ast.FlowDecl{}
}
