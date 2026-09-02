// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import "testing"

// switchWithClause wraps a switch whose header carries a clause operand LAST,
// directly before the body brace. That position is the one the span reader used
// to swallow, so it is the position both legs of the first test drive.
func switchWithClause(header, body string) string {
	return "flow s\nsource in Poll\nswitch route from in on in.Kind " + header + " {\n" + body + "}\n"
}

// TestClauseOperandMayBeLastBeforeASwitchBody proves a clause carrying a
// Go-expression operand may sit LAST before a switch body.
//
// THE DEFECT IT PINS, measured on the tree before this change: the span reader
// counted the body's `{` through bracketDelta, so at depth zero it INCREMENTED
// rather than stopping. The span swallowed the brace, the arms and the closing
// brace, and the statement died with `expected "{", found end of line` — a
// message naming neither the clause nor the cause.
//
// WHY THE COMPOSITE-LITERAL ARM IS NOT OPTIONAL. The obvious fix is to make every
// depth-zero `{` a stop token, and it passes the switch cases while truncating
// `machine.GobCodec[Order]{}` in half at the `]`. The literal arms below are what
// separate the ruled adjacency rule from that wrong one: a brace ADJACENT to the
// expression is part of it, a brace SEPARATED by horizontal whitespace ends the
// span. gofmt writes a composite literal's brace adjacent and a block's spaced,
// so the rule follows the formatter rather than inventing a convention.
func TestClauseOperandMayBeLastBeforeASwitchBody(t *testing.T) {
	t.Run("over sits last before the body", func(t *testing.T) {
		file := mustParse(t, switchWithClause("reads a over ratelimit.New(5)", "\t\"a\" -> first\n"))
		stmt := switchIn(t, file)

		if len(stmt.Arms) != 1 {
			t.Fatalf("parsed %d arms, want 1 — the span swallowed the body", len(stmt.Arms))
		}
		if stmt.Clauses.Over == nil {
			t.Fatalf("the over clause was not recorded at all")
		}
		if got := stmt.Clauses.Over.Text; got != "ratelimit.New(5)" {
			t.Errorf("the over operand is %q, want ratelimit.New(5)", got)
		}
	})

	t.Run("checkpoint sits last before the body, its composite literal intact", func(t *testing.T) {
		const operand = "machine.GobCodec[Order]{}"

		file := mustParse(t, switchWithClause("reads a checkpoint "+operand, "\t\"a\" -> first\n"))
		stmt := switchIn(t, file)

		if len(stmt.Arms) != 1 {
			t.Fatalf("parsed %d arms, want 1 — the span swallowed the body", len(stmt.Arms))
		}
		if stmt.Clauses.Checkpoint == nil {
			t.Fatalf("the checkpoint clause was not recorded at all")
		}
		// THE ANTI-OVERREACH LEG. A rule stopping at EVERY depth-zero brace
		// truncates this operand to "machine.GobCodec[Order]" and passes the
		// arm count above, so the count alone cannot tell the two rules apart.
		if got := stmt.Clauses.Checkpoint.Text; got != operand {
			t.Errorf("the checkpoint operand is %q, want %q — an adjacent brace is part of the expression", got, operand)
		}
	})

	t.Run("a composite literal operand survives outside a switch", func(t *testing.T) {
		const operand = "machine.GobCodec[Order]{}"

		file := mustParse(t, "flow c\nsource in Poll\ntransform t Foo from in checkpoint "+operand+"\n")
		clauses := clausesOfFirstTransform(t, file)
		if clauses.Checkpoint == nil {
			t.Fatalf("the checkpoint clause was not recorded at all")
		}
		if got := clauses.Checkpoint.Text; got != operand {
			t.Errorf("the checkpoint operand is %q, want %q", got, operand)
		}
	})

	t.Run("a parenthesized func literal parses whole and may sit last", func(t *testing.T) {
		const operand = "(func() machine.Codec[Order] { return nil })"

		file := mustParse(t, switchWithClause("checkpoint "+operand, "\t\"a\" -> first\n"))
		stmt := switchIn(t, file)

		if len(stmt.Arms) != 1 {
			t.Fatalf("parsed %d arms, want 1", len(stmt.Arms))
		}
		if stmt.Clauses.Checkpoint == nil {
			t.Fatalf("the checkpoint clause was not recorded at all")
		}
		// THE ESCAPE. Inside parentheses the body brace is at depth one, so it is
		// never a top-level brace and the only spaced top-level `{` left on the
		// line is the switch body's.
		if got := stmt.Clauses.Checkpoint.Text; got != operand {
			t.Errorf("the parenthesized func-literal operand is %q, want %q", got, operand)
		}
	})

	t.Run("the called parenthesized form parses whole too", func(t *testing.T) {
		const operand = "(func() machine.Codec[Order] { return nil })()"

		file := mustParse(t, switchWithClause("checkpoint "+operand, "\t\"a\" -> first\n"))
		stmt := switchIn(t, file)

		if stmt.Clauses.Checkpoint == nil {
			t.Fatalf("the checkpoint clause was not recorded at all")
		}
		if got := stmt.Clauses.Checkpoint.Text; got != operand {
			t.Errorf("the called form is %q, want %q", got, operand)
		}
	})

	t.Run("a bare func literal is refused with a message naming the escape", func(t *testing.T) {
		src := "flow c\nsource in Poll\ntransform t Foo from in over func() T { return nil }\n"
		// THE REFUSAL MUST NAME THE ROUTE OUT. A diagnostic reading only
		// `unexpected "{"` leaves the author with no next move; naming the
		// parenthesized form turns a dead end into a one-character fix.
		requireDiagnostic(t, diagnosticsFor(t, src), "parenthes")
	})
}

// TestGoSpanIsOpaqueToQuotedLiterals proves a brace inside a string or rune
// literal is TEXT rather than a bracket.
//
// THE DEFECT IT PINS, and it is a SEPARATE one from the swallowed arms: skipQuoted
// already existed but was called only from the func-span scanner, never from
// goSpanStep. So a brace inside a quoted literal opened a depth nothing closed,
// the span ran past the newline, and the statement died with a message about
// newline termination. An unbalanced brace in a string is legal Go and legal
// transport configuration.
//
// A FIX FOR THE SWALLOWED ARMS ALONE LEAVES THIS STANDING, which is why it is its
// own test rather than another subtest above.
func TestGoSpanIsOpaqueToQuotedLiterals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		operand string
	}{
		{"unbalanced open brace in a string", `pubsub.Topic("a{b")`},
		{"unbalanced close brace in a string", `pubsub.Topic("a}b")`},
		{"open brace in a rune", `f('{')`},
		{"close brace in a rune", `f('}')`},
		{"an escaped quote beside a brace", `pubsub.Topic("a\"{b")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := mustParse(t, "flow c\nsource in Poll\ntransform t Foo from in over "+tc.operand+"\n")
			clauses := clausesOfFirstTransform(t, file)
			if clauses.Over == nil {
				t.Fatalf("the over clause was not recorded at all")
			}
			if clauses.Over.Text != tc.operand {
				t.Errorf("the over operand is %q, want %q", clauses.Over.Text, tc.operand)
			}
		})
	}

	t.Run("a quoted brace does not hide a following switch body", func(t *testing.T) {
		file := mustParse(t, switchWithClause(`over pubsub.Topic("a{b")`, "\t\"a\" -> first\n"))
		stmt := switchIn(t, file)

		if len(stmt.Arms) != 1 {
			t.Fatalf("parsed %d arms, want 1", len(stmt.Arms))
		}
		if got := stmt.Clauses.Over.Text; got != `pubsub.Topic("a{b")` {
			t.Errorf("the over operand is %q", got)
		}
	})
}
