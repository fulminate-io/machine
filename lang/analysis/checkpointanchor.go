// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"reflect"

	"github.com/whitaker-io/machine/lang/ast"
)

// CheckpointAnchorAnalyzer refuses a checkpoint clause whose anchor has no meaning
// on the statement shape carrying it.
var CheckpointAnchorAnalyzer = &Analyzer{
	Name: "checkpointanchor",
	Doc: "checkpointanchor refuses a `checkpoint` clause on a TERMINAL statement that is not marked " +
		"`idempotent`. The two anchors are not interchangeable: an unmarked node is journaled on " +
		"COMPLETION, whose meaning is that resume re-injects the record into the node's SUCCESSORS " +
		"without re-running it — and a terminal has no successors. Every candidate completion " +
		"semantic for an unmarked terminal is therefore either undefined or already covered, since " +
		"retirement fires for every datum at completion regardless. A MARKED terminal stays legal " +
		"and anchors on ARRIVAL, which is fully meaningful: the input is journaled before the sink " +
		"runs and recovery re-runs the sink. This is an ANALYSIS diagnostic rather than a parse " +
		"error because the clause and the shape are each well formed and wrong only in " +
		"combination, and because Clauses is ONE grammar production shared by every clause-bearing " +
		"shape — refusing it in the parser would special-case shape against the grammar's own " +
		"structure. It is reported at ERROR rather than HINT because the ruling makes the shape " +
		"illegal, which is the opposite of errorrouting's unhandled-failure case, where the ruled " +
		"default makes silence legal.",
	Requires:   []*Analyzer{SymbolsAnalyzer},
	Run:        runCheckpointAnchor,
	ResultType: reflect.TypeOf((*CheckpointAnchors)(nil)),
}

// Anchor names which side of a node's function its checkpoint is recorded at.
const (
	anchorArrival    = "arrival"
	anchorCompletion = "completion"
)

// CheckpointAnchor is one checkpointed statement's resolved anchor.
type CheckpointAnchor struct {
	Flow     string
	Stmt     int
	Label    string
	Anchor   string
	Terminal bool
	Pos      ast.Position
}

// CheckpointAnchors is the analyzer's result: one entry per statement carrying a
// checkpoint clause.
type CheckpointAnchors struct {
	Anchors []CheckpointAnchor
}

// runCheckpointAnchor resolves and checks every flow in every source.
func runCheckpointAnchor(p *Pass) (any, error) {
	table, ok := p.ResultOf[SymbolsAnalyzer].(*SymbolTable)
	if !ok {
		return nil, errNoSymbols
	}

	out := &CheckpointAnchors{}
	for f := range table.Files {
		for i := range table.Files[f].Flows {
			anchorFlow(p, table.Files[f].Src, &table.Files[f].Flows[i], out)
		}
	}

	return out, nil
}

// anchorFlow resolves every checkpointed statement in one flow.
func anchorFlow(p *Pass, src Source, flow *FlowSymbols, out *CheckpointAnchors) {
	for i, stmt := range flow.Body {
		clauses, name, ok := namedClauses(stmt)
		if !ok || clauses.Checkpoint == nil {
			continue
		}

		anchor := CheckpointAnchor{
			Flow: flow.Name, Stmt: i, Label: name.Name,
			Anchor: anchorCompletion, Terminal: isTerminalShape(stmt), Pos: name.NamePos,
		}
		if clauses.Idempotent != nil {
			anchor.Anchor = anchorArrival
		}
		out.Anchors = append(out.Anchors, anchor)

		if anchor.Terminal && anchor.Anchor == anchorCompletion {
			reportTerminalAnchor(p, src, flow, anchor)
		}
	}
}

// reportTerminalAnchor names the node, the clause and BOTH ways out.
//
// Naming both exits is the point rather than a courtesy: an author told only that
// the clause is wrong cannot act on it, and the two exits mean different things —
// marking the node keeps the checkpoint and moves its anchor, removing the clause
// gives it up.
func reportTerminalAnchor(p *Pass, src Source, flow *FlowSymbols, anchor CheckpointAnchor) {
	p.Report(src, Diagnostic{
		Pos: anchor.Pos,
		End: endOfName(anchor.Pos, anchor.Label),
		Message: "the checkpoint clause on " + anchor.Label + " in flow " + flow.Name +
			" has no completion semantic: " + anchor.Label + " is terminal and has no successors " +
			"for a resume to re-inject into. Either mark " + anchor.Label + " idempotent, which " +
			"anchors the checkpoint at arrival and re-runs the node on recovery, or remove the " +
			"checkpoint clause",
		Severity: SeverityError,
	})
}

// isTerminalShape reports whether a statement shape produces nothing downstream.
//
// THE SET HAS EXACTLY ONE MEMBER TODAY and that is a fact about the grammar rather
// than a simplification. A sink and a drop are the two terminals, and a drop does
// not embed Clauses at all — `drop ... checkpoint` is unrepresentable — so the
// terminal AND clause-bearing set is SinkStmt alone. A set of size one is exactly
// the kind that grows silently, which is why its size is pinned by a test rather
// than left to be rediscovered.
func isTerminalShape(stmt ast.Stmt) bool {
	_, terminal := stmt.(ast.SinkStmt)

	return terminal
}
