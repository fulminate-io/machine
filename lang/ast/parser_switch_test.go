// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import "testing"

// switchSource wraps a switch body in the smallest flow that can hold one.
func switchSource(body string) string {
	return "flow s\nsource in Poll\nswitch route from in on in.Kind {\n" + body + "}\n"
}

// TestParseSwitchArmsAndElse covers ALL FOUR states of the switch body.
//
// Both halves are load bearing. A test covering only the clean cases cannot tell
// a parser that enforces the two structural rules from one that ignores them,
// and a test covering only the diagnostic cases would pass against a parser that
// rejects every switch it sees.
func TestParseSwitchArmsAndElse(t *testing.T) {
	t.Run("arms with no else", func(t *testing.T) {
		file := mustParse(t, switchSource("\t\"a\", \"b\" -> first\n\tisValid(in) -> second\n"))
		stmt := switchIn(t, file)

		if len(stmt.Arms) != 2 {
			t.Fatalf("parsed %d arms, want 2", len(stmt.Arms))
		}
		if stmt.Else != nil {
			t.Errorf("a switch with no else parsed one: %v", stmt.Else)
		}
		if stmt.Subject.Text != "in.Kind" {
			t.Errorf("subject is %q, want in.Kind", stmt.Subject.Text)
		}

		// The parser does NOT classify an arm's values: a multi-value literal
		// arm and a Go predicate arm are both just positioned spans.
		if len(stmt.Arms[0].Values) != 2 {
			t.Errorf("the multi-value arm has %d values, want 2", len(stmt.Arms[0].Values))
		}
		if got := stmt.Arms[1].Values[0].Text; got != "isValid(in)" {
			t.Errorf("the predicate arm's value is %q, want isValid(in)", got)
		}
		if stmt.Arms[0].Target.Name != "first" || stmt.Arms[1].Target.Name != "second" {
			t.Errorf("arm targets are %q and %q", stmt.Arms[0].Target.Name, stmt.Arms[1].Target.Name)
		}
	})

	t.Run("arms with a trailing else", func(t *testing.T) {
		file := mustParse(t, switchSource("\t\"a\" -> first\n\telse -> other\n"))
		stmt := switchIn(t, file)

		if len(stmt.Arms) != 1 {
			t.Fatalf("parsed %d arms, want 1", len(stmt.Arms))
		}
		if stmt.Else == nil || stmt.Else.Name != "other" {
			t.Fatalf("else target is %v, want other", stmt.Else)
		}
	})

	t.Run("zero arms is a diagnostic", func(t *testing.T) {
		requireDiagnostic(t, diagnosticsFor(t, switchSource("")), "at least one arm")
	})

	t.Run("a non-last else is a diagnostic", func(t *testing.T) {
		diags := diagnosticsFor(t, switchSource("\telse -> other\n\t\"a\" -> first\n"))
		requireDiagnostic(t, diags, "else must be last")
	})
}

// TestParseSwitchCarriesClauses covers the clause position, which sits between
// the subject and the opening brace.
//
// THE CLAUSE ORDER HERE IS LOAD-BEARING, and it is a property of the span reader
// rather than of the switch. A clause carrying a Go-expression operand may not be
// the LAST clause on a switch: `{` is a BRACKET to the span reader, not a stop
// token, so a span still open when the body's brace arrives opens a depth instead
// of ending, swallows the arms, and the statement dies at end of file. Measured on
// `over` before `checkpoint` ever took an operand — `... reads seen over
// ratelimit.New(5) {` already failed with `expected "{", found end of line`, while
// the same clause written before `reads` parsed clean. So the constraint is the
// language's, pre-dates this clause growing a codec, and is exercised here by
// putting checkpoint first and letting `reads` end its span.
func TestParseSwitchCarriesClauses(t *testing.T) {
	const codec = "machine.GobCodec[Order]{}"

	file := mustParse(t, "flow s\nsource in Poll\n"+
		"switch route from in on in.Kind checkpoint "+codec+" reads seen {\n\t\"a\" -> first\n}\n")
	stmt := switchIn(t, file)

	if stmt.Subject.Text != "in.Kind" {
		t.Errorf("subject is %q; a clause keyword must end the subject span", stmt.Subject.Text)
	}
	if len(stmt.Reads) != 1 || stmt.Reads[0].Name != "seen" {
		t.Errorf("reads clause is %v", stmt.Reads)
	}
	if stmt.Checkpoint == nil {
		t.Fatalf("checkpoint clause was not recorded")
	}
	// The operand ends at the FOLLOWING CLAUSE KEYWORD, not at the brace. An
	// operand that read `machine.GobCodec[Order]{} reads seen` would mean the span
	// ran past its clause, which is the failure this ordering exists to avoid.
	if stmt.Checkpoint.Text != codec {
		t.Errorf("the checkpoint operand is %q, want %q; the span did not end at the next clause", stmt.Checkpoint.Text, codec)
	}
	if len(stmt.From) != 1 || stmt.From[0].Name != "in" {
		t.Errorf("from list is %v", stmt.From)
	}
}

// switchIn returns the single switch statement of a one-flow fixture.
func switchIn(t *testing.T, file *File) SwitchStmt {
	t.Helper()
	for _, stmt := range flowAt(t, file, 0).Body {
		if sw, ok := stmt.(SwitchStmt); ok {
			return sw
		}
	}
	t.Fatalf("the flow carries no switch statement")
	return SwitchStmt{}
}
