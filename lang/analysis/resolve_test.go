// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
)

// TestResolveRejectsDeclareAfterUseAndAcceptsHoistedFuncs pins the rule in both
// directions.
//
// Asserting only the rejection would be satisfied by an analyzer that flags
// EVERY forward reference — including the legal hoisted-func ones the parser
// explicitly accepts, which is why the two func-ordering fixtures ride the same
// test rather than a separate one that could be dropped independently.
func TestResolveRejectsDeclareAfterUseAndAcceptsHoistedFuncs(t *testing.T) {
	reject := loadSource(t, filepath.Join(sharedContractDir, "declare-after-use-loop.flow"))
	diags := withCode(analyze(t, ResolveAnalyzer, reject), ResolveAnalyzer.Name)
	if len(diags) == 0 {
		t.Fatal("declare-after-use-loop.flow produced no resolve diagnostic; its own note calls it a declare-before-use reject")
	}

	// The fixture's note names the reference: "`loop retry` is consumed by
	// `transform try` before it is declared". The diagnostic has to sit on that
	// reference rather than merely somewhere in the file.
	retry := retryReference(t, reject)
	var onRetry int
	for _, d := range diags {
		if d.Pos == retry {
			onRetry++
		}
		if d.Severity != SeverityError {
			t.Errorf("a resolve diagnostic carries severity %s, want error: %s", d.Severity, d.Message)
		}
	}
	if onRetry == 0 {
		t.Errorf("no resolve diagnostic sits on the retry reference at %s; got %v", retry, messages(diags))
	}
	t.Logf("declare-after-use-loop.flow resolve diagnostics: %v", messages(diags))

	// THE OVER-BROAD DIRECTION. A func is declare-anywhere and hoisted, and
	// lang/ast ships both orderings as VALID fixtures.
	for _, name := range []string{"func-before-use.flow", "func-after-use.flow"} {
		src := loadSource(t, filepath.Join(astTestdata, "valid", name))
		if got := withCode(analyze(t, ResolveAnalyzer, src), ResolveAnalyzer.Name); len(got) != 0 {
			t.Errorf("%s is a VALID fixture but produced resolve diagnostics: %v", name, messages(got))
		}
	}
}

// retryReference finds the position of the `retry` name where transform try
// consumes it.
func retryReference(t *testing.T, src Source) ast.Position {
	t.Helper()

	table, _ := symbolsOf(t, src)
	flow, ok := table.Flow("payments")
	if !ok {
		t.Fatal("declare-after-use-loop.flow tables no flow named payments")
	}
	refs := flow.Consumers["retry"]
	if len(refs) == 0 {
		t.Fatal("declare-after-use-loop.flow tables no reference to retry")
	}
	// The first reference in statement order is the one transform try makes,
	// which is the one the fixture's note is about; the later one is the send's
	// target, which is exempt from ordering.
	return refs[0].Pos
}

// TestResolveIsSilentOnTheCanonicalCorpus is the standing strawman sweep for
// this analyzer, recorded rather than assumed.
//
// Zero resolve diagnostics on all three holds ONLY because no
// unimported-qualifier check ships: toy.flow references http. and pubsub. while
// importing neither.
func TestResolveIsSilentOnTheCanonicalCorpus(t *testing.T) {
	for name, diags := range sweepCorpus(t, ResolveAnalyzer, strawmanDir) {
		if len(diags) != 0 {
			t.Errorf("strawman %s produced resolve diagnostics: %v", name, messages(diags))
		}
	}
	for name, diags := range sweepCorpus(t, ResolveAnalyzer, filepath.Join(astTestdata, "valid")) {
		if len(diags) != 0 {
			t.Errorf("valid fixture %s produced resolve diagnostics: %v", name, messages(diags))
		}
	}
}

// TestResolveReportsAnUndefinedName pins the other diagnostic kind, and the
// undefined half of the send exemption: a send's target is exempt from ORDERING
// but must still resolve.
func TestResolveReportsAnUndefinedName(t *testing.T) {
	src := parseSource(t, "undefined.flow",
		"flow orders\nsource ingest Poll\nsink done audit.Store from nowhere\nsend done -> missing\n")
	diags := withCode(analyze(t, ResolveAnalyzer, src), ResolveAnalyzer.Name)

	want := map[string]bool{"nowhere": false, "missing": false}
	for _, d := range diags {
		for name := range want {
			if containsAll(d.Message, "the name "+name+" is referenced but no statement") {
				want[name] = true
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("no undefined-reference diagnostic names %q; got %v", name, messages(diags))
		}
	}
}

// TestBadSpanSuppressionDiscriminates exercises the recovered-region predicate
// directly, and records what the corpus says about whether it can be reached.
//
// THE PREDICATE IS TESTED HERE RATHER THAN THROUGH AN ANALYZER because against
// today's parser no analyzer can reach it. A BadStmt holds the span of tokens
// the parser SKIPPED, and skipped tokens never become identifiers, so no tabled
// reference — and therefore no diagnostic position — can land inside one. A test
// asserting "no resolve diagnostic appeared inside a bad span" would pass on
// every corpus file whether the suppression existed or not, which is a green
// that proves nothing.
//
// The corpus scan below is a recorded OBSERVATION, not the assertion: it fails
// only if the scan itself finds nothing to look at, so a future parser change
// that starts tabling references inside a recovered region shows up as a
// changed count rather than as silence.
func TestBadSpanSuppressionDiscriminates(t *testing.T) {
	flow := &FlowSymbols{
		Name: "orders",
		Bad:  []ast.BadStmt{{Start: ast.Position{Offset: 100}, Stop: ast.Position{Offset: 200}}},
	}

	for _, tc := range []struct {
		name   string
		offset int
		want   bool
	}{
		{name: "before the region", offset: 99, want: false},
		{name: "at its first byte", offset: 100, want: true},
		{name: "inside it", offset: 150, want: true},
		{name: "at its last byte", offset: 199, want: true},
		{name: "just past its end", offset: 200, want: false},
	} {
		if got := insideBadSpan(flow, ast.Position{Offset: tc.offset}); got != tc.want {
			t.Errorf("a position %s (offset %d) reported %t, want %t", tc.name, tc.offset, got, tc.want)
		}
	}

	// A flow with no recovered region suppresses nothing, which is the case
	// every clean file takes.
	if insideBadSpan(&FlowSymbols{Name: "orders"}, ast.Position{Offset: 150}) {
		t.Error("a flow with no recovered region suppressed a position anyway")
	}

	files, withBad, inside := scanRecoveredRegions(t)
	if withBad == 0 {
		t.Fatalf("CONTROL FAILED: none of the %d parsed corpus files carries a BadStmt, so the scan checked nothing", files)
	}
	t.Logf("corpus observation: %d parsed files, %d carry a recovered region, %d references sit inside one",
		files, withBad, inside)
}

// scanRecoveredRegions counts corpus files with a recovered region, and the
// references tabled inside one.
func scanRecoveredRegions(t *testing.T) (files, withBad, inside int) {
	t.Helper()

	for _, dir := range []string{"broken", "invalid", "valid", "strawman", "analysis-rejects"} {
		paths, err := filepath.Glob(filepath.Join(astTestdata, dir, "*.flow"))
		if err != nil {
			t.Fatalf("globbing %s failed: %v", dir, err)
		}
		for _, path := range paths {
			file, ok := partialTree(t, path)
			if !ok {
				continue
			}
			files++
			table, _ := symbolsOf(t, Source{Path: path, File: file})
			for i := range table.Files {
				for j := range table.Files[i].Flows {
					flow := &table.Files[i].Flows[j]
					if len(flow.Bad) == 0 {
						continue
					}
					withBad++
					for _, refs := range flow.Consumers {
						for _, ref := range refs {
							if insideBadSpan(flow, ref.Pos) {
								inside++
							}
						}
					}
				}
			}
		}
	}
	return files, withBad, inside
}

// partialTree parses a file and returns whatever tree the parser produced, which
// for a broken source arrives on the error rather than as a return value.
func partialTree(t *testing.T, path string) (*ast.File, bool) {
	t.Helper()

	body, err := readFixture(t, path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	file, perr := ast.Parse(body)
	if file != nil {
		return file, true
	}
	var aerr *ast.Error
	if errors.As(perr, &aerr) && aerr.File != nil {
		return aerr.File, true
	}
	return nil, false
}
