// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ast

import "testing"

// clausesOfFirstTransform returns the clause bundle of the first transform
// statement in the first flow, which is where both clause tests put the clause
// under test.
func clausesOfFirstTransform(t *testing.T, file *File) Clauses {
	t.Helper()
	for _, stmt := range flowAt(t, file, 0).Body {
		if transform, ok := stmt.(TransformStmt); ok {
			return transform.Clauses
		}
	}
	t.Fatalf("the fixture carries no transform statement to read clauses from")
	return Clauses{}
}

// requireClauseOperandRefusal asserts a source is refused for an EMPTY clause
// operand, and that the refusal is POSITIONED on the line the clause sits on
// rather than at the start of the file or past the end of the statement.
//
// The position leg is not decoration. A refusal raised through diagHeref would
// report at the token AFTER the empty span, which for a bare clause at end of
// line is the newline — a diagnostic pointing at the wrong line. Asserting the
// line pins the span-positioned form.
func requireClauseOperandRefusal(t *testing.T, src, keyword string, wantLine int) {
	t.Helper()
	d := requireDiagnostic(t, diagnosticsFor(t, src), keyword+"\" needs an operand")
	if d.Pos.Line != wantLine {
		t.Errorf("the refusal is at line %d, want the clause's own line %d (%s)", d.Pos.Line, wantLine, d.Pos)
	}
}

// TestCheckpointClauseRequiresACodecOperand proves the ruled production in both
// directions: `checkpoint <goSpan>` parses with the operand RECORDED, and the
// bare form is a positioned error.
//
// The two negative cases are deliberately different shapes. A bare clause at end
// of line reaches the empty-span refusal through a newline stop; `checkpoint
// idempotent` reaches it because the span reader stops at a clause keyword, so
// the operand is empty even though a token follows on the same line. The second
// is the one a production change alone does not catch — measured before this
// change, the analogous `over idempotent` parsed clean with an empty span.
func TestCheckpointClauseRequiresACodecOperand(t *testing.T) {
	const operand = "machine.GobCodec[Order]{}"

	file := mustParse(t, "flow c\nsource in Poll\ntransform t Foo from in checkpoint "+operand+"\n")
	clauses := clausesOfFirstTransform(t, file)
	if clauses.Checkpoint == nil {
		t.Fatalf("the checkpoint clause was not recorded at all")
	}
	if clauses.Checkpoint.Text != operand {
		t.Errorf("the checkpoint operand is %q, want %q", clauses.Checkpoint.Text, operand)
	}

	for _, tc := range []struct {
		name string
		src  string
	}{
		{"bare at end of line", "flow c\nsource in Poll\ntransform t Foo from in checkpoint\n"},
		{"clause keyword follows", "flow c\nsource in Poll\ntransform t Foo from in checkpoint idempotent\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireClauseOperandRefusal(t, tc.src, "checkpoint", 3)
		})
	}
}

// TestOverClauseRequiresATransportOperand proves the SHARED refusal fires on its
// other caller.
//
// This is the leg a copied-and-drifted implementation fails. The empty-span
// guard is one helper with two callers; a guard written into clauseCheckpoint
// alone passes the checkpoint test and the whole corpus while leaving `over`
// exactly as it was — measured before this change, a bare `over` parsed CLEAN
// with an empty span.
func TestOverClauseRequiresATransportOperand(t *testing.T) {
	const operand = "ratelimit.New(5)"

	file := mustParse(t, "flow c\nsource in Poll\ntransform t Foo from in over "+operand+"\n")
	clauses := clausesOfFirstTransform(t, file)
	if clauses.Over == nil {
		t.Fatalf("the over clause was not recorded at all")
	}
	if clauses.Over.Text != operand {
		t.Errorf("the over operand is %q, want %q", clauses.Over.Text, operand)
	}

	for _, tc := range []struct {
		name string
		src  string
	}{
		{"bare at end of line", "flow c\nsource in Poll\ntransform t Foo from in over\n"},
		{"clause keyword follows", "flow c\nsource in Poll\ntransform t Foo from in over idempotent\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireClauseOperandRefusal(t, tc.src, "over", 3)
		})
	}
}
