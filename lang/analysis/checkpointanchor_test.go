// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
)

// THE THREE BEHAVIOUR ARMS MUST DISAGREE WITH EACH OTHER, and that is what makes
// them a set rather than three spellings of one assertion. An analyzer that
// refuses every checkpoint on a terminal passes the refusal arm and fails the
// marked-terminal accept; one that refuses every checkpoint anywhere passes both
// refusals and fails the non-terminal accept; one that refuses nothing fails only
// the first. No single wrong analyzer satisfies any two.

func TestCheckpointOnAnUnmarkedTerminalIsRefusedNamingBothExits(t *testing.T) {
	src := parseSource(t, "unmarked-terminal.flow", `flow t
source ingest Poll
sink done Store from ingest
  checkpoint
`)

	diags := withCode(analyze(t, CheckpointAnchorAnalyzer, src), CheckpointAnchorAnalyzer.Name)
	if len(diags) != 1 {
		t.Fatalf("an unmarked terminal carrying a checkpoint produced %d diagnostics, want 1: %v",
			len(diags), messages(diags))
	}
	if diags[0].Severity != SeverityError {
		t.Errorf("the refusal is severity %s, want error: the ruling makes this shape illegal, "+
			"which is the opposite of the unhandled-failure case where the ruled default makes silence legal",
			diags[0].Severity)
	}

	// IT NAMES THE NODE, THE CLAUSE AND BOTH EXITS. An author told only that the
	// clause is wrong cannot act on it, and the two exits mean different things:
	// marking keeps the checkpoint and moves its anchor, removing gives it up.
	message := diags[0].Message
	for _, want := range []string{"done", "checkpoint", "idempotent", "remove"} {
		if !strings.Contains(message, want) {
			t.Fatalf("the refusal %q does not name %q", message, want)
		}
	}
	t.Logf("the refusal names the node, the clause and both exits: %s", message)
}

func TestCheckpointOnAnIdempotentTerminalIsAccepted(t *testing.T) {
	// A MARKED terminal is fully meaningful: the input is journaled before the sink
	// runs and recovery re-runs the sink. This arm is what an analyzer refusing
	// every checkpoint on a terminal fails.
	src := parseSource(t, "marked-terminal.flow", `flow t
source ingest Poll
sink done Store from ingest
  checkpoint  idempotent
`)

	diags := withCode(analyze(t, CheckpointAnchorAnalyzer, src), CheckpointAnchorAnalyzer.Name)
	if len(diags) != 0 {
		t.Fatalf("an idempotent-marked terminal was refused: %v", messages(diags))
	}

	// CONTROL: the analyzer actually SAW the statement and resolved it, so the
	// silence above is an accept rather than a walk that skipped it.
	result, _ := resultOf(t, CheckpointAnchorAnalyzer, src)
	anchors, ok := result.(*CheckpointAnchors)
	if !ok || len(anchors.Anchors) != 1 {
		t.Fatalf("the analyzer resolved %v, want exactly one anchored statement", result)
	}
	if anchors.Anchors[0].Anchor != anchorArrival {
		t.Fatalf("the marked terminal resolved to the %s anchor, want arrival", anchors.Anchors[0].Anchor)
	}
	if !anchors.Anchors[0].Terminal {
		t.Fatal("the sink did not resolve as terminal, so this arm is not exercising the terminal path")
	}
	t.Logf("the idempotent-marked terminal was accepted with arrival semantics: %+v", anchors.Anchors[0])
}

func TestCheckpointOnAnUnmarkedNonTerminalIsAccepted(t *testing.T) {
	// Completion is the DEFAULT and is meaningful wherever there are successors.
	// This arm is what an analyzer refusing every checkpoint anywhere fails.
	src := parseSource(t, "unmarked-nonterminal.flow", `flow t
source ingest Poll
transform step Work from ingest
  checkpoint
sink done Store from step
`)

	diags := withCode(analyze(t, CheckpointAnchorAnalyzer, src), CheckpointAnchorAnalyzer.Name)
	if len(diags) != 0 {
		t.Fatalf("an unmarked NON-terminal was refused: %v", messages(diags))
	}

	result, _ := resultOf(t, CheckpointAnchorAnalyzer, src)
	anchors, ok := result.(*CheckpointAnchors)
	if !ok || len(anchors.Anchors) != 1 {
		t.Fatalf("the analyzer resolved %v, want exactly one anchored statement", result)
	}
	if anchors.Anchors[0].Anchor != anchorCompletion {
		t.Fatalf("the unmarked transform resolved to the %s anchor, want completion", anchors.Anchors[0].Anchor)
	}
	if anchors.Anchors[0].Terminal {
		t.Fatal("the transform resolved as terminal, so the terminal test is matching the wrong shapes")
	}
	t.Logf("the unmarked non-terminal was accepted with completion semantics: %+v", anchors.Anchors[0])
}

func TestTheTerminalShapeSetIsClosed(t *testing.T) {
	// TWO NUMBERS ARE PINNED AND THE SECOND IS THE ONE THAT MOVES. Pinning only the
	// terminal subset watches a quantity a new shape does NOT change: add a
	// clause-bearing statement to the grammar and the terminal count stays at 1
	// while the population this refusal must consider grows, so the gate would keep
	// printing its number while going blind. The clause-bearing total is what a new
	// shape actually moves.
	clauseBearing := []ast.Stmt{
		ast.SourceStmt{}, ast.TransformStmt{}, ast.BranchStmt{}, ast.TeeStmt{},
		ast.SinkStmt{}, ast.SwitchStmt{}, ast.UseStmt{},
	}

	// The list is derived from the SAME enumeration the error router walks, rather
	// than hand-maintained beside it: every member must be one namedClauses admits.
	total := 0
	terminal := 0
	terminalNames := make([]string, 0, 1)
	for _, stmt := range clauseBearing {
		if _, _, ok := namedClauses(stmt); !ok {
			t.Fatalf("%T is in this test's list but namedClauses does not admit it; the two enumerations have diverged", stmt)
		}
		total++
		if isTerminalShape(stmt) {
			terminal++
			name := strings.TrimPrefix(strings.TrimPrefix(typeName(stmt), "analysis."), "ast.")
			terminalNames = append(terminalNames, name)
		}
	}

	// CONTROL: a shape namedClauses does NOT admit is genuinely excluded, so the
	// total above is a measurement rather than the length of a list.
	if _, _, ok := namedClauses(ast.DropStmt{}); ok {
		t.Fatal("CONTROL FAILED: namedClauses admits DropStmt, which carries no Clauses at all")
	}

	if total != 7 {
		t.Fatalf("the clause-bearing shape total is %d, want 7: a shape was added to the grammar and "+
			"this refusal's population grew with it", total)
	}
	if terminal != 1 || terminalNames[0] != "SinkStmt" {
		t.Fatalf("the terminal subset is %d %v, want exactly 1 (SinkStmt): a new terminal shape carrying "+
			"clauses would escape the refusal entirely", terminal, terminalNames)
	}
	t.Logf("clause-bearing statement shapes: %d; of those, terminal: %d (%s)",
		total, terminal, strings.Join(terminalNames, ", "))
}

func TestTheValidCorpusCarriesNoRefusedShape(t *testing.T) {
	// A FIXTURE UNDER valid/ THAT THE LANGUAGE REFUSES IS A CORPUS THAT LIES, so
	// this sweeps the directory THROUGH THE ANALYZER rather than grepping it.
	// Clauses continue onto the next line, so `sink ... checkpoint` is two lines in
	// the source and a single-line grep for it matches nothing — an earlier draft of
	// this check carried exactly that leg and passed against the very fixture that
	// carried the refused shape.
	swept := sweepCorpus(t, CheckpointAnchorAnalyzer, filepath.Join(astTestdata, "valid"))

	names := make([]string, 0, len(swept))
	for name, diags := range swept {
		names = append(names, name)
		if len(diags) != 0 {
			t.Errorf("valid fixture %s carries the refused shape: %v", name, messages(diags))
		}
	}
	t.Logf("swept the valid corpus through the analyzer: %d fixtures, %v", len(swept), names)
}
