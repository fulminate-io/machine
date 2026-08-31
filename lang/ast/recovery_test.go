// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import (
	"strconv"
	"strings"
	"testing"
)

// TestRecoverySkipsToStatementBoundary covers the three things recovery has to
// get right: where a broken statement ends, that a half-typed func body does not
// cost the declarations above it, and that the diagnostic list is bounded.
func TestRecoverySkipsToStatementBoundary(t *testing.T) {
	t.Run("a broken statement takes its own clause lines with it", func(t *testing.T) {
		src := `flow orders
source ingest Poll
node broken Foo from ingest
	reads seen
	writes tally
sink out Write from ingest
`
		file, err := Parse([]byte(src))
		parseErr, ok := err.(*Error)
		if !ok {
			t.Fatalf("Parse returned %T, want *Error", err)
		}

		// THE DISCRIMINATING ASSERTION. The boundary is the continuation rule
		// read in reverse, so the two clause lines belong to the broken
		// statement. A naive skip to the first newline strands them as garbage
		// statements and reports the one mistake three times.
		if len(parseErr.Diagnostics) != 1 {
			t.Fatalf("got %d diagnostics, want 1; the skip stopped at the first newline: %v",
				len(parseErr.Diagnostics), parseErr.Diagnostics)
		}

		body := flowAt(t, file, 0).Body
		if len(body) != 3 {
			t.Fatalf("got %d statements, want source, BadStmt, sink: %v", len(body), body)
		}
		bad, isBad := body[1].(BadStmt)
		if !isBad {
			t.Fatalf("statement 1 is %T, want BadStmt", body[1])
		}
		if _, isSink := body[2].(SinkStmt); !isSink {
			t.Errorf("the statement after the break is %T, want SinkStmt", body[2])
		}

		// The BadStmt carries the exact span of what was skipped, so a consumer
		// walking the tree sees an unbroken sequence covering the file.
		if bad.Pos().Line != 3 {
			t.Errorf("BadStmt starts on line %d, want 3", bad.Pos().Line)
		}
		if bad.End().Line < 5 {
			t.Errorf("BadStmt ends on line %d; it must cover the clause lines through line 5", bad.End().Line)
		}
		skipped := src[bad.Pos().Offset:bad.End().Offset]
		if !strings.Contains(skipped, "writes tally") {
			t.Errorf("the BadStmt span is %q and does not cover the clause continuation lines", skipped)
		}
		assertTreeOrder(t, body)
	})

	t.Run("a half-typed func body keeps the declarations above it", func(t *testing.T) {
		src := "import x \"example.com/x\"\nflow one\nsource ingest Poll\n\nfunc Helper() error {\n\tif x {\n"
		file, err := Parse([]byte(src))
		if file == nil {
			t.Fatalf("Parse returned a nil File for a half-typed func body")
		}
		parseErr, ok := err.(*Error)
		if !ok {
			t.Fatalf("Parse returned %T, want *Error", err)
		}

		// The lexer reports what only it can see, and the parser attaches the
		// partial node rather than discarding it.
		d := requireDiagnostic(t, parseErr.Diagnostics, "unterminated func body")
		openBrace := strings.Index(src, "{")
		if d.Pos.Offset != openBrace {
			t.Errorf("the diagnostic is at offset %d, want the opening brace at %d", d.Pos.Offset, openBrace)
		}

		var sawImport, sawFlow, sawFunc bool
		for _, decl := range file.Decls {
			switch decl.(type) {
			case ImportDecl:
				sawImport = true
			case FlowDecl:
				sawFlow = true
			case FuncDecl:
				sawFunc = true
			}
		}
		if !sawImport || !sawFlow || !sawFunc {
			t.Fatalf("declarations above the half-typed func did not survive: import=%v flow=%v func=%v",
				sawImport, sawFlow, sawFunc)
		}
		if got := len(flowAt(t, file, 0).Body); got != 1 {
			t.Errorf("the flow above the half-typed func has %d statements, want 1", got)
		}
	})

	t.Run("the diagnostic list is capped and says so", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("flow orders\n")
		const broken = 150
		for i := range broken {
			b.WriteString("node bad" + strconv.Itoa(i) + "\n")
		}

		_, err := Parse([]byte(b.String()))
		parseErr, ok := err.(*Error)
		if !ok {
			t.Fatalf("Parse returned %T, want *Error", err)
		}

		// CONTROL: the input really does drive the count past the cap, so a cap
		// that never engaged cannot pass as one that did.
		if broken <= maxDiagnostics {
			t.Fatalf("the fixture produces at most %d problems, which cannot reach the cap of %d",
				broken, maxDiagnostics)
		}
		if len(parseErr.Diagnostics) != maxDiagnostics+1 {
			t.Fatalf("got %d diagnostics, want %d recorded plus one cap notice",
				len(parseErr.Diagnostics), maxDiagnostics)
		}

		last := parseErr.Diagnostics[len(parseErr.Diagnostics)-1]
		wantSuppressed := strconv.Itoa(broken - maxDiagnostics)
		if !strings.Contains(last.Message, wantSuppressed) {
			t.Fatalf("the final diagnostic is %q and does not name the %s suppressed problems",
				last.Message, wantSuppressed)
		}
		if !strings.Contains(last.Message, "not reported") {
			t.Errorf("the final diagnostic is %q and does not say the rest went unreported", last.Message)
		}
	})
}

// TestRecoveryClosesUnclosedBracedRegions covers the two braced regions the
// parser reads. There is deliberately no flow-body case: a flow body is
// braceless and ends at the next `flow` or `func` line or at end of file, so the
// failure cannot occur.
func TestRecoveryClosesUnclosedBracedRegions(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want string
	}{
		{
			name: "state block",
			src:  "flow one\nstate {\n\tseen map[string]bool\n",
			want: "the state block is never closed",
		},
		{
			name: "switch body",
			src:  "flow one\nsource in Poll\nswitch route from in on in.Kind {\n\t\"a\" -> first\n",
			want: "the switch body is never closed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := Parse([]byte(tc.src))
			if file == nil {
				t.Fatalf("Parse returned a nil File")
			}
			parseErr, ok := err.(*Error)
			if !ok {
				t.Fatalf("Parse returned %T, want *Error", err)
			}

			d := requireDiagnostic(t, parseErr.Diagnostics, tc.want)
			openBrace := strings.Index(tc.src, "{")
			if d.Pos.Offset != openBrace {
				t.Errorf("the diagnostic is at offset %d, want the OPENING brace at %d", d.Pos.Offset, openBrace)
			}

			// Everything parsed before the region ran out is retained.
			if len(file.Decls) == 0 {
				t.Fatalf("an unclosed region cost the whole file")
			}
		})
	}
}

// assertTreeOrder checks that statement spans are positioned and non-decreasing,
// which is what lets a consumer walk the tree as an unbroken sequence.
func assertTreeOrder(t *testing.T, body []Stmt) {
	t.Helper()
	previous := 0
	for i, stmt := range body {
		start, end := stmt.Pos(), stmt.End()
		if start.Line < 1 || start.Col < 1 {
			t.Errorf("statement %d (%T) is not positioned: %+v", i, stmt, start)
		}
		if end.Offset < start.Offset {
			t.Errorf("statement %d (%T) ends before it starts: %v..%v", i, stmt, start, end)
		}
		if start.Offset < previous {
			t.Errorf("statement %d (%T) starts at %d, before the previous statement ended at %d",
				i, stmt, start.Offset, previous)
		}
		previous = end.Offset
	}
}
