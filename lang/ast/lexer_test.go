// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import (
	"strings"
	"testing"
)

// lexAll drains a lexer, returning every token up to and including EOF.
func lexAll(src string) ([]token, []Diagnostic) {
	l := newLexer([]byte(src))
	var out []token
	for {
		t := l.next()
		out = append(out, t)
		if t.kind == tokEOF {
			return out, l.diags
		}
	}
}

// kindsOf projects a token slice down to its kinds.
func kindsOf(toks []token) []tokenKind {
	out := make([]tokenKind, 0, len(toks))
	for _, t := range toks {
		out = append(out, t.kind)
	}
	return out
}

// TestLexNewlineTerminatesStatements pins the one decision the lexer makes about
// vertical whitespace: it always emits a newline token, and a run of blank lines
// collapses to a single one. Whether that newline ends a statement is the
// parser's call, not the lexer's.
func TestLexNewlineTerminatesStatements(t *testing.T) {
	toks, diags := lexAll("flow a\n\n\n   \t\nflow b\n")
	if len(diags) != 0 {
		t.Fatalf("clean source produced diagnostics: %v", diags)
	}

	want := []tokenKind{kwFlow, tokIdent, tokNewline, kwFlow, tokIdent, tokNewline, tokEOF}
	got := kindsOf(toks)
	if len(got) != len(want) {
		t.Fatalf("got %d tokens %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("token %d is kind %d, want %d (all: %v)", i, got[i], want[i], got)
		}
	}

	// Indentation is cosmetic: a leading run of spaces and tabs produces no
	// token of its own and does not change what follows it.
	indented, _ := lexAll("\t   flow a\n")
	if indented[0].kind != kwFlow {
		t.Fatalf("leading whitespace produced kind %d, want the flow keyword", indented[0].kind)
	}
	if indented[0].pos.Col != 5 {
		t.Fatalf("indented keyword reported col %d, want 5", indented[0].pos.Col)
	}
}

// TestLexNoteBlockIsRawAndUnterminatedIsDiagnosed covers the note region, which
// tracks NOTHING. Note bodies routinely hold braces, and an escape mechanism is
// exactly the constraint the region exists to avoid, so its only terminator is
// the next delimiter.
func TestLexNoteBlockIsRawAndUnterminatedIsDiagnosed(t *testing.T) {
	body := "\n  returns EnrichResult{Ok, Retryable}\n  a \"quoted\" word and a { brace\n"
	toks, diags := lexAll("note \"\"\"" + body + "\"\"\"\n")
	if len(diags) != 0 {
		t.Fatalf("a terminated note produced diagnostics: %v", diags)
	}
	if toks[1].kind != tokNoteText {
		t.Fatalf("second token is kind %d, want tokNoteText", toks[1].kind)
	}
	if toks[1].text != body {
		t.Fatalf("note body was not raw:\n got %q\nwant %q", toks[1].text, body)
	}
	if toks[2].kind != tokNewline || toks[3].kind != tokEOF {
		t.Fatalf("note did not close cleanly: %v", kindsOf(toks))
	}

	// Unterminated: the partial text is still emitted, and the diagnostic lands
	// on the OPENING delimiter rather than at end of file.
	src := "flow a\nnote \"\"\"dangling {body\nstill going\n"
	unterminated, udiags := lexAll(src)
	if len(udiags) != 1 {
		t.Fatalf("got %d diagnostics for an unterminated note, want 1: %v", len(udiags), udiags)
	}
	openOffset := len("flow a\nnote ")
	if udiags[0].Pos.Offset != openOffset {
		t.Fatalf("diagnostic at offset %d, want the opening delimiter at %d", udiags[0].Pos.Offset, openOffset)
	}
	if udiags[0].Pos.Line != 2 {
		t.Fatalf("diagnostic on line %d, want line 2", udiags[0].Pos.Line)
	}
	text := unterminated[len(unterminated)-2].text
	if text != "dangling {body\nstill going\n" {
		t.Fatalf("unterminated note emitted %q", text)
	}
}

// TestLexGoSpanTracksBracketDepth covers the second region scan. Depth tracking
// is what lets a span hold a comma, and the stop condition is the SIX CLAUSE
// keywords rather than every keyword.
func TestLexGoSpanTracksBracketDepth(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		stop []tokenKind
		want string
	}{
		{"func type with a comma", "func(a, b int) error\n", nil, "func(a, b int) error"},
		{"map type", "map[string]int\n", nil, "map[string]int"},
		{"brackets nested in parens", "http.Listen[Order](\":8080\")\n", nil, "http.Listen[Order](\":8080\")"},
		{"generic type", "Foo[K, V]\n", nil, "Foo[K, V]"},
		{"stops at a clause keyword", "Gen reads a, b\n", nil, "Gen"},
		{"stops at a stop-set keyword", "Enrich from orders\n", []tokenKind{kwFrom}, "Enrich"},
		{"stops at a stop-set token", "Order) -> out\n", []tokenKind{tokRParen}, "Order"},
		{"a clause keyword inside brackets does not stop", "Wrap(reads, writes)\n", nil, "Wrap(reads, writes)"},
		{"a keyword prefix is not a keyword", "readsCounter\n", nil, "readsCounter"},
		{"clone ends a var type only when asked", "func(int) error clone Dup\n", []tokenKind{kwClone}, "func(int) error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLexer([]byte(tc.src))
			got := l.scanGoSpan(tc.stop...)
			if got.kind != tokGoSpan {
				t.Fatalf("kind %d, want tokGoSpan", got.kind)
			}
			if got.text != tc.want {
				t.Fatalf("span is %q, want %q", got.text, tc.want)
			}
		})
	}

	// THE TRAP CASE. `func` is a keyword, and adding it to the stop set is a
	// one-line change with a remote symptom: the type spelling of
	// `var h func(int) error` BEGINS with it, so the span comes back empty and
	// the declaration parses as garbage.
	l := newLexer([]byte("var h func(int) error\n"))
	if got := l.next(); got.kind != kwVar {
		t.Fatalf("first token kind %d, want kwVar", got.kind)
	}
	if got := l.next(); got.kind != tokIdent || got.text != "h" {
		t.Fatalf("second token is %d/%q, want an ident named h", got.kind, got.text)
	}
	span := l.scanGoSpan()
	if span.text != "func(int) error" {
		t.Fatalf("var type span is %q, want %q — has func joined the stop set?", span.text, "func(int) error")
	}
}

// goAwareCases are the five Go forms in which a brace is text rather than
// structure. A naive brace counter and a Go-aware one were measured against
// these and diverged on every one, each truncating the span mid-literal.
var goAwareCases = []struct {
	name string
	body string
}{
	{"double-quoted string", `() { s := "}" ; _ = s }`},
	{"rune literal", `() { c := '}'; _ = c }`},
	{"line comment", "() { // }\n\treturn\n}"},
	{"block comment", `() { /* } */ return }`},
	{"raw string", "() { s := `}`; _ = s }"},
}

// TestLexGoFuncSpanIsGoAware covers the third region scan on BOTH of its axes.
// Awareness is the five literal and comment forms; depth is a nested closure.
// The two are independent — the nested-closure body holds no brace inside a skip
// region, so a naive scanner handles it correctly — and a test covering one
// proves half.
func TestLexGoFuncSpanIsGoAware(t *testing.T) {
	for _, tc := range goAwareCases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLexer([]byte("func F" + tc.body + "\n"))
			l.next()
			l.next()
			span := l.scanGoFuncSpan()
			if span.kind != tokGoFuncSpan {
				t.Fatalf("kind %d, want tokGoFuncSpan", span.kind)
			}
			if span.text != tc.body {
				t.Fatalf("span truncated:\n got %q\nwant %q", span.text, tc.body)
			}
			if len(l.diags) != 0 {
				t.Fatalf("a balanced body produced diagnostics: %v", l.diags)
			}
			if next := l.next(); next.kind != tokNewline {
				t.Fatalf("the token after the span is kind %d, want tokNewline", next.kind)
			}
		})
	}

	t.Run("nested closures", func(t *testing.T) {
		body := "() error {\n\tg := func() error { return nil }\n\treturn g()\n}"
		l := newLexer([]byte("func F" + body + "\ndrop x\n"))
		l.next()
		l.next()
		span := l.scanGoFuncSpan()
		if span.text != body {
			t.Fatalf("nested body span:\n got %q\nwant %q", span.text, body)
		}
		if next := l.next(); next.kind != tokNewline {
			t.Fatalf("the token after a nested body is kind %d, want tokNewline", next.kind)
		}
		if after := l.next(); after.kind != kwDrop {
			t.Fatalf("the sentinel after the span is kind %d, want kwDrop", after.kind)
		}
	})

	t.Run("anonymous struct result", func(t *testing.T) {
		body := "() struct{ A int } { return struct{ A int }{A: 1} }"
		l := newLexer([]byte("func F" + body + "\n"))
		l.next()
		l.next()
		if span := l.scanGoFuncSpan(); span.text != body {
			t.Fatalf("struct-result span:\n got %q\nwant %q", span.text, body)
		}
	})

	t.Run("unterminated", func(t *testing.T) {
		src := "func F() {\n\treturn nil\n"
		l := newLexer([]byte(src))
		l.next()
		l.next()
		span := l.scanGoFuncSpan()
		if span.text != "() {\n\treturn nil\n" {
			t.Fatalf("unterminated span is %q", span.text)
		}
		if len(l.diags) != 1 {
			t.Fatalf("got %d diagnostics for an unterminated body, want 1: %v", len(l.diags), l.diags)
		}
		braceOffset := len("func F() ")
		if l.diags[0].Pos.Offset != braceOffset {
			t.Fatalf("diagnostic at offset %d, want the opening brace at %d", l.diags[0].Pos.Offset, braceOffset)
		}
	})
}

// TestLexPositionsAreByteAccurate pins Offset, Line and Col on every token,
// including across a multi-byte identifier: Col counts BYTES so an editor's own
// offsets map without a re-scan.
func TestLexPositionsAreByteAccurate(t *testing.T) {
	src := "flow café\n  transform t Foo\n"
	toks, diags := lexAll(src)
	if len(diags) != 0 {
		t.Fatalf("clean source produced diagnostics: %v", diags)
	}

	want := []struct {
		text string
		pos  Position
	}{
		{"flow", Position{Offset: 0, Line: 1, Col: 1}},
		{"café", Position{Offset: 5, Line: 1, Col: 6}},
		{"\n", Position{Offset: 10, Line: 1, Col: 11}},
		{"transform", Position{Offset: 13, Line: 2, Col: 3}},
		{"t", Position{Offset: 23, Line: 2, Col: 13}},
		{"Foo", Position{Offset: 25, Line: 2, Col: 15}},
		{"\n", Position{Offset: 28, Line: 2, Col: 18}},
	}
	if len(toks) != len(want)+1 {
		t.Fatalf("got %d tokens, want %d plus EOF: %v", len(toks), len(want), kindsOf(toks))
	}
	for i, w := range want {
		if toks[i].text != w.text {
			t.Fatalf("token %d text %q, want %q", i, toks[i].text, w.text)
		}
		if toks[i].pos != w.pos {
			t.Fatalf("token %d (%q) at %+v, want %+v", i, w.text, toks[i].pos, w.pos)
		}
	}

	// The multi-byte identifier is the control on the byte-counting claim: a
	// rune-counting column would report 10 for the newline, not 11.
	if got := len("café"); got != 5 {
		t.Fatalf("the fixture identifier is %d bytes, so this test no longer probes byte columns", got)
	}
	if toks[len(toks)-1].kind != tokEOF {
		t.Fatalf("last token is kind %d, want tokEOF", toks[len(toks)-1].kind)
	}
	if toks[len(toks)-1].pos.Offset != len(src) {
		t.Fatalf("EOF at offset %d, want %d", toks[len(toks)-1].pos.Offset, len(src))
	}
}

// TestLexNumbersAndEscapes covers the two token forms no corpus file reaches
// through the parser.
//
// Numbers are a case worth stating plainly: the lexer produces them, but every
// position where a numeric literal can appear in this language sits inside an
// opaque Go span, so nothing but a direct lex ever sees one. The scanner still
// has to be right, because that stops being true the moment the grammar grows a
// production that takes one.
func TestLexNumbersAndEscapes(t *testing.T) {
	toks, diags := lexAll("12 3.14 5. 0.5\n")
	if len(diags) != 0 {
		t.Fatalf("numbers produced diagnostics: %v", diags)
	}
	want := []string{"12", "3.14", "5", ".", "0.5", "\n"}
	for i, w := range want {
		if toks[i].text != w {
			t.Errorf("token %d is %q, want %q", i, toks[i].text, w)
		}
	}

	// A backslash escape must not end a string early, and a quoted literal must
	// not run past its line.
	escaped, escapedDiags := lexAll(`import "a\"b"` + "\n")
	if len(escapedDiags) != 0 {
		t.Fatalf("an escaped quote produced diagnostics: %v", escapedDiags)
	}
	if escaped[1].text != `"a\"b"` {
		t.Errorf("escaped string is %q", escaped[1].text)
	}

	// A source that ends mid-escape must not read past its own last byte. This
	// is the one call site that drives the cursor's bounds guard.
	truncated, truncatedDiags := lexAll(`import "abc\`)
	if len(truncatedDiags) != 1 {
		t.Fatalf("a string ending mid-escape produced %d diagnostics, want 1", len(truncatedDiags))
	}
	if truncated[len(truncated)-1].kind != tokEOF {
		t.Errorf("lexing did not reach EOF after a trailing backslash")
	}

	// The same two forms inside a Go func body, where the func scan owns the
	// skipping rather than the string scanner.
	l := newLexer([]byte("func F() string { s := \"a\\\"}\\\"b\"; c := '\\''; return s }\n"))
	l.next()
	l.next()
	span := l.scanGoFuncSpan()
	if !strings.HasSuffix(span.text, "return s }") {
		t.Errorf("escapes inside a func body truncated the span: %q", span.text)
	}
	if len(l.diags) != 0 {
		t.Fatalf("a balanced body with escapes produced diagnostics: %v", l.diags)
	}
}
