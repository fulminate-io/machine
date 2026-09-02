// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"path/filepath"
	"testing"
)

// stateDir holds this module's own var-semantics fixtures.
var stateDir = filepath.Join("testdata", "state")

// TestStateRejectsBothContractFixturesAndSparesStrawmen covers the two
// shared-contract fixtures and the corpus sweep.
//
// THE STRAWMAN LEG IS WHAT CATCHES AN OVER-BROAD RULE, and it catches a specific
// one. Every written-never-read name across all three strawmen is a STATE FIELD
// — enrichment's enriched_total, payments' processed and by_type, toy's charged
// — and ZERO vars are written-never-read, so a written-never-read rule that
// failed to scope itself to vars would red all three canonical programs.
func TestStateRejectsBothContractFixturesAndSparesStrawmen(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		says    []string
	}{
		{fixture: "traversal-wide-var.flow", says: []string{"the var total", "never read", "belongs in the state block"}},
		{fixture: "wrapper-type-state.flow", says: []string{"retired wrapper spelling", "bare Go types"}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			src := loadSource(t, filepath.Join(sharedContractDir, tc.fixture))
			diags := withCode(analyze(t, StateAnalyzer, src), StateAnalyzer.Name)
			if len(diags) == 0 {
				t.Fatalf("%s produced no state diagnostic; its own note calls it an analysis reject", tc.fixture)
			}
			if len(errorsIn(diags)) == 0 {
				t.Errorf("%s produced no state ERROR: %v", tc.fixture, messages(diags))
			}
			var said bool
			for _, d := range diags {
				if containsAll(d.Message, tc.says...) {
					said = true
				}
			}
			if !said {
				t.Errorf("no diagnostic says %v; got %v", tc.says, messages(diags))
			}
			t.Logf("%s: %v", tc.fixture, messages(diags))
		})
	}

	for name, diags := range sweepCorpus(t, StateAnalyzer, strawmanDir) {
		if len(diags) != 0 {
			t.Errorf("strawman %s produced state diagnostics: %v", name, messages(diags))
		}
	}

	// Derived rather than assumed, and asserted so it cannot drift silently:
	// payments' `var span *ops.Span clone ops.CloneSpan` is neither read nor
	// written by any statement, and none of the four checks fires on it. A
	// declared-but-entirely-unused var is a rule this module does not ship, and
	// adding one would red payments.
	payments := loadSource(t, filepath.Join(strawmanDir, "payments.flow"))
	table, _ := symbolsOf(t, payments)
	flow, ok := table.Flow("payments")
	if !ok {
		t.Fatal("payments.flow tables no flow named payments")
	}
	if _, declared := flow.Vars["span"]; !declared {
		t.Fatal("payments.flow no longer declares var span; the unused-var observation below is stale")
	}
	if len(flow.Reads["span"])+len(flow.Writes["span"]) != 0 {
		t.Errorf("payments.flow now reads or writes span, so it is no longer the unused-var case")
	}
}

// TestStateZeroValueReadsLegalButUnwrittenReadsError gates the ruled var
// semantics in BOTH directions, because the distinction between the two
// fixtures is the whole content of the rule.
//
// Without the legal-read leg, the error leg passes against an over-broad
// ORDERING rule — and an ordering rule is exactly what the ticket's own prose
// suggests when it describes the analysis as "does anyone write key X before
// this node reads it". That is not what was ruled, and an over-broad rule here
// fires on correct programs, which is the more damaging direction.
func TestStateZeroValueReadsLegalButUnwrittenReadsError(t *testing.T) {
	legal := loadSource(t, filepath.Join(stateDir, "zero-value-read-before-write.flow"))
	if got := withCode(analyze(t, StateAnalyzer, legal), StateAnalyzer.Name); len(got) != 0 {
		t.Errorf("reading a var before the first write to it was reported, but vars carry zero-value semantics: %v",
			messages(got))
	}

	unwritten := loadSource(t, filepath.Join(stateDir, "read-with-no-write-anywhere.flow"))
	diags := errorsIn(withCode(analyze(t, StateAnalyzer, unwritten), StateAnalyzer.Name))
	if len(diags) != 1 {
		t.Fatalf("got %d state errors, want exactly the unwritten read: %v", len(diags), messages(diags))
	}
	if !containsAll(diags[0].Message, "attempt is read", "no statement anywhere writes it") {
		t.Errorf("the diagnostic does not describe the ruled rule: %s", diags[0].Message)
	}
	t.Logf("unwritten read: %v", messages(diags))
}

// TestStateReportsUndeclaredAccess pins the first of the four checks, which
// nothing else exercises.
func TestStateReportsUndeclaredAccess(t *testing.T) {
	src := parseSource(t, "undeclared.flow",
		"flow orders\nsource ingest Poll\n"+
			"transform charge billing.Charge from ingest\n  reads nowhere  writes elsewhere\n"+
			"sink done audit.Store from charge\n")
	diags := withCode(analyze(t, StateAnalyzer, src), StateAnalyzer.Name)

	want := map[string]bool{"nowhere": false, "elsewhere": false}
	for _, d := range diags {
		for name := range want {
			if containsAll(d.Message, "declares no var or state field named "+name) {
				want[name] = true
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("no diagnostic reports the undeclared name %q; got %v", name, messages(diags))
		}
	}
}
