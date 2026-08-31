// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"path/filepath"
	"testing"
)

// TestCyclesSparesSendLoopsAndReportsSendFreeCycles shows the analyzer able both
// to report and to stay silent.
//
// The silence leg is only worth anything because the strawmen genuinely contain
// cycles: a Tarjan run over the parsed trees finds exactly one in each, all of
// size four, all carrying a send. Asserting that here is what separates "the
// sanctioned-loop path was exercised and stayed quiet" from "no cycle was found,
// so nothing could have been reported".
func TestCyclesSparesSendLoopsAndReportsSendFreeCycles(t *testing.T) {
	// THE REPORTING LEG.
	src := loadSource(t, filepath.Join("testdata", "cycles", "send-free-cycle.flow"))
	diags := withCode(analyze(t, CyclesAnalyzer, src), CyclesAnalyzer.Name)
	if len(diags) != 1 {
		t.Fatalf("the send-free fixture produced %d cycles diagnostics, want 1: %v", len(diags), messages(diags))
	}
	if !containsAll(diags[0].Message, "contains a cycle with no send", "score", "adjust") {
		t.Errorf("the diagnostic does not name the cycle's members: %s", diags[0].Message)
	}
	t.Logf("send-free cycle: %v", messages(diags))

	// THE SILENCE LEG, with its non-vacuity check in the same run.
	for _, name := range strawmanFiles {
		strawman := loadSource(t, filepath.Join(strawmanDir, name))
		set, found := cyclesOf(t, strawman)
		if got := withCode(found, CyclesAnalyzer.Name); len(got) != 0 {
			t.Errorf("strawman %s produced cycles diagnostics: %v", name, messages(got))
		}

		cycles := allCycles(set)
		if len(cycles) != 1 {
			t.Errorf("%s carries %d cycles, want exactly 1: %v", name, len(cycles), cycles)
			continue
		}
		if len(cycles[0].Stmts) != 4 {
			t.Errorf("%s carries a cycle of %d statements, want 4: %v", name, len(cycles[0].Stmts), cycles[0].Stmts)
		}
		if !cycles[0].HasSend {
			t.Errorf("%s carries a cycle with no send, which should have been reported: %v", name, cycles[0].Stmts)
		}
		t.Logf("%s cycle: statements %v, carries a send: %t", name, cycles[0].Stmts, cycles[0].HasSend)
	}
}

// TestCyclesIgnoresASendThatClosesNothing pins that the send mark is read from
// edges INSIDE a component, not from the file containing a send anywhere.
//
// A send is necessary but not sufficient: it closes a loop only if its target
// also reaches back to it.
func TestCyclesIgnoresASendThatClosesNothing(t *testing.T) {
	src := parseSource(t, "sideways.flow",
		"flow orders\nsource ingest Poll\n"+
			"transform score billing.Score from adjust\n"+
			"transform adjust billing.Adjust from score\n"+
			"transform tail billing.Tail from ingest\n"+
			"send tail -> done\n"+
			"sink done audit.Store from tail\n")

	diags := withCode(analyze(t, CyclesAnalyzer, src), CyclesAnalyzer.Name)
	if len(diags) != 1 {
		t.Fatalf("got %d cycles diagnostics, want the send-free cycle to still be reported: %v",
			len(diags), messages(diags))
	}
	if !containsAll(diags[0].Message, "score", "adjust") {
		t.Errorf("the reported cycle is not the send-free one: %s", diags[0].Message)
	}
}

// TestCyclesRefusesAMissingGraph pins that a mistyped prerequisite stops rather
// than reporting a program cycle-free.
func TestCyclesRefusesAMissingGraph(t *testing.T) {
	pass := &Pass{Analyzer: CyclesAnalyzer, ResultOf: map[*Analyzer]any{FlowgraphAnalyzer: 42}}
	if _, err := CyclesAnalyzer.Run(pass); err == nil {
		t.Fatal("the cycles analyzer accepted a flowgraph result of the wrong type")
	}
}
