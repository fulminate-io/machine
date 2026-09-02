// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
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
