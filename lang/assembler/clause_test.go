// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	goast "go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// clausesSourcePath is the AST file the partition is checked against. It is READ,
// not imported, so this adds no module dependency — the same source-derivation
// idiom the builder-vocabulary gate uses.
var clausesSourcePath = filepath.Join("..", "ast", "node.go")

// TestEveryClauseIsLoweredOrHeldAndNoneIsDropped is the gate against a clause
// that parses, reaches the graph, and is dropped without a word.
//
// FOR ONE CLAUSE THAT IS NOT A MISSING FEATURE BUT A SEMANTIC INVERSION. An
// unmarked node anchors its checkpoint on COMPLETION and a node marked idempotent
// anchors on ARRIVAL; completion is the default. Dropping the marker therefore
// reverses what the author wrote, and the failure surfaces only during recovery,
// as a non-idempotent node's side effects happening twice.
//
// FIVE LEGS, each covering a way the others can go vacuous.
func TestEveryClauseIsLoweredOrHeldAndNoneIsDropped(t *testing.T) {
	surface := deriveClauseFields(t)

	// LEG 1 — THE DERIVATION CONTROL. A parse that read nothing, or a renamed
	// type, would leave the parity checks comparing the partition against an
	// empty set and passing in silence.
	if len(surface) < 8 {
		t.Fatalf("CONTROL FAILED: ast.Clauses derived only %d fields (%v); the derivation is not reading the real surface",
			len(surface), surface)
	}

	// LEG 2 — PARITY, BOTH DIRECTIONS.
	for _, field := range surface {
		if _, classified := clausePartition[field]; !classified {
			t.Errorf("ast.Clauses carries %q and the partition classifies it neither lowered nor held; "+
				"it would be dropped in silence", field)
		}
	}
	for field := range clausePartition {
		if !slices.Contains(surface, field) {
			t.Errorf("the partition classifies %q, which ast.Clauses no longer carries", field)
		}
	}

	// LEG 3 — THE EMPTY-HELD PIN. Both members that were once held are lowered
	// now. A clause parked in HELD without a ruling recording why reds here.
	for field, disposition := range clausePartition {
		if disposition == clauseHeld {
			t.Errorf("%q is parked in HELD; the held set is empty and a new member needs a ruling recorded with it",
				field)
		}
	}

	// LEG 4 — THE HELD PATH, KEPT UNDER TEST WITH A SYNTHETIC MEMBER. With no
	// live held clause the drop-detection path has nothing to exercise and would
	// stop proving anything; an untested path is exactly how a held clause would
	// later be dropped in silence.
	t.Run("the held path reports rather than drops", func(t *testing.T) {
		program := graphOf(t, checkpointFixture)
		program.InputTypes = map[string]string{"ingest": "Order", "charge": "Order", "done": "Receipt"}

		l := &lowering{
			program: program,
			plan:    &Plan{Flow: program.Name},
			flowVar: map[string]string{},
			carried: map[string][]string{},
			// Checkpoint is LOWERED in the partition; holding it for this one
			// dispatch drives the held arm without editing the partition.
			held: map[string]bool{"Checkpoint": true},
		}
		charge := nodeNamed(t, program, "charge")
		l.options(charge)

		if len(l.diags) == 0 {
			t.Fatal("a held clause produced no diagnostic; it was dropped in silence")
		}
		d := l.diags[0]
		if !strings.Contains(d.Message, "not yet lowered") {
			t.Errorf("the held diagnostic reads %q, want it to say the clause is not yet lowered", d.Message)
		}
		if !strings.Contains(d.Message, "checkpoint") {
			t.Errorf("the held diagnostic %q does not name the clause", d.Message)
		}
		if d.Pos.Line == 0 {
			t.Errorf("the held diagnostic is unpositioned: %q", d.Message)
		}
	})

	// LEG 5 — THE SAME-RUN KNOWN POSITIVE. The same clause, through the same
	// dispatch, with its real LOWERED disposition, produces no diagnostic and does
	// produce an option. Without this, leg 4 would pass over a dispatch that
	// diagnosed everything.
	t.Run("a lowered clause produces an option and no diagnostic", func(t *testing.T) {
		program := graphOf(t, checkpointFixture)
		program.InputTypes = map[string]string{"ingest": "Order", "charge": "Order", "done": "Receipt"}

		l := &lowering{
			program: program,
			plan:    &Plan{Flow: program.Name},
			flowVar: map[string]string{},
			carried: map[string][]string{},
			held:    heldClauses(),
		}
		options := l.options(nodeNamed(t, program, "charge"))

		if len(l.diags) != 0 {
			t.Fatalf("a lowered clause produced diagnostics: %v", messagesOf(l.diags))
		}
		if !slices.ContainsFunc(options, func(o string) bool { return strings.Contains(o, "WithCheckpoint") }) {
			t.Errorf("the lowered checkpoint emitted no option: %v", options)
		}
	})
}

// checkpointFixture is one flow carrying a checkpoint clause, used by both the
// held-path probe and its known positive so the two differ only in disposition.
const checkpointFixture = "flow orders\n" +
	"source ingest Poll\n" +
	"transform charge Bill from ingest\n" +
	"  checkpoint machine.GobCodec[Order]{}\n" +
	"sink done Store from charge\n"

// deriveClauseFields reads ast.Clauses' field names out of the AST's source.
func deriveClauseFields(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), clausesSourcePath, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", clausesSourcePath, err)
	}

	var fields []string
	goast.Inspect(file, func(n goast.Node) bool {
		spec, ok := n.(*goast.TypeSpec)
		if !ok || spec.Name.Name != "Clauses" {
			return true
		}
		structType, ok := spec.Type.(*goast.StructType)
		if !ok {
			return false
		}
		for _, field := range structType.Fields.List {
			for _, name := range field.Names {
				fields = append(fields, name.Name)
			}
		}

		return false
	})

	return fields
}

// TestClauseOrderCoversThePartition pins the emission order against the
// partition, so a clause classified but never walked is caught.
//
// The dispatch iterates clauseOrder rather than the partition map, both for
// deterministic output and because map iteration order would make the emitted
// option list unstable. A member missing from clauseOrder would therefore never
// be dispatched at all, which is a silent drop wearing a classification.
func TestClauseOrderCoversThePartition(t *testing.T) {
	for field := range clausePartition {
		if !slices.Contains(clauseOrder, field) {
			t.Errorf("%q is classified but absent from clauseOrder, so the dispatch never reaches it", field)
		}
	}
	for _, field := range clauseOrder {
		if _, ok := clausePartition[field]; !ok {
			t.Errorf("clauseOrder walks %q, which the partition does not classify", field)
		}
	}
	if len(clauseOrder) != len(clausePartition) {
		t.Errorf("clauseOrder walks %d clauses against %d classified", len(clauseOrder), len(clausePartition))
	}
}

// TestClausePresentReadsEveryMember is the known-positive for the presence
// dispatch: every clause the partition names must be detectable as present, or a
// held member would silently report absent and never raise its diagnostic.
func TestClausePresentReadsEveryMember(t *testing.T) {
	program := graphOf(t, "flow every\n"+
		"state {\n  seen map[string]bool\n}\n"+
		"var attempt int\n"+
		"source ingest Poll\n"+
		"transform charge Bill from ingest\n"+
		"  reads attempt  writes seen  over ratelimit.New(5)\n"+
		"  checkpoint machine.GobCodec[Order]{}  idempotent\n"+
		"  on error Handle\n"+
		"  note \"\"\"every clause at once\"\"\"\n"+
		"sink done Store from charge\n")

	charge := nodeNamed(t, program, "charge")
	for _, clause := range clauseOrder {
		if !clausePresent(charge.Clauses, clause) {
			t.Errorf("clausePresent reports %q absent on a statement that writes every clause", clause)
		}
	}
}
