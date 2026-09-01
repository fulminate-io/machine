// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import (
	"slices"
	"strings"
)

// The three region scans below each have their own termination rule, and they
// are genuinely different: a note body tracks nothing, a Go span tracks bracket
// depth, and a func body tracks Go's own literal and comment forms. Conflating
// any two of them produces wrong spans.
const (
	// noteDelim opens and closes a raw note body. There are no escapes inside
	// one: the body runs verbatim to the first following delimiter, which is
	// what makes its end trivially decidable.
	noteDelim = `"""`

	lineComment       = "//"
	blockCommentOpen  = "/*"
	blockCommentClose = "*/"

	spanTrimCutset = " \t\r"
)

// spanStopKeywords are the SEVEN reserved words that end a Go span at bracket
// depth zero. It is a closed set, NOT "any keyword", and that difference is load
// bearing: `func` is a keyword too, and `var h func(int) error` and
// `clone func(x T) T` begin their TYPE spellings with it. Adding func here is a
// one-line change that returns an empty span and parses the declaration as
// garbage.
//
// SIX ARE THE CLAUSE KEYWORDS, which end a span because a clause follows it.
// `else` is the seventh and earns its place differently: a switch arm begins
// with a Go span, so without this an else arm would scan as an ordinary arm
// value and the grammar's trailing `[ Else ]` would order nothing. Stopping here
// makes an else arm unmatchable as an Arm, which is what makes else-last a rule
// the notation expresses rather than one only the parser knows. Nothing valid is
// lost: a Go EXPRESSION never begins with `else`, and `else` inside a func body
// is untouched because that body is scanned by the brace-balanced func span,
// which does not consult this table at all.
var spanStopKeywords = map[string]bool{
	textReads:      true,
	textWrites:     true,
	textOver:       true,
	textCheckpoint: true,
	textIdempotent: true,
	textOn:         true,
	textNote:       true,
	textElse:       true,
}

// scanNoteText scans a raw note body from its opening delimiter to the first
// following one. The body is verbatim: indentation, internal newlines and any
// braces it holds are preserved, and nothing inside it is tracked.
//
// An unterminated note runs to end of file, emits the partial text and records a
// diagnostic at the OPENING delimiter.
func (l *lexer) scanNoteText() token {
	open := l.pos()
	l.advanceN(len(noteDelim))
	start := l.off
	for l.off < len(l.src) {
		if l.hasPrefix(noteDelim) {
			text := string(l.src[start:l.off])
			l.advanceN(len(noteDelim))
			return token{kind: tokNoteText, text: text, pos: open}
		}
		l.advance()
	}
	l.diag(open, l.pos(), "unterminated note: no closing "+noteDelim+" before end of file")
	return token{kind: tokNoteText, text: string(l.src[start:l.off]), pos: open}
}

// scanGoSpan scans an opaque run of Go text, tracking depth across (), [] and {}
// and stopping only at a DEPTH-ZERO member of stop, at a clause keyword, or at a
// newline.
//
// Depth tracking is what lets a span hold commas: `func(a, b int) error` and
// `Foo[K, V]` both contain one that must not terminate the span, and
// `http.Listen[Order](":8080")` nests brackets inside parens.
//
// The stop set is a parameter because the EBNF recognizer calls this same helper
// with sets derived from the grammar's FOLLOW computation.
func (l *lexer) scanGoSpan(stop ...tokenKind) token {
	l.skipHorizontal()
	p := l.pos()
	start := l.off
	depth := 0
	for l.off < len(l.src) {
		if l.goSpanStep(&depth, stop) {
			break
		}
	}
	return token{kind: tokGoSpan, text: l.trimSpanTail(start), pos: p}
}

// trimSpanTail rewinds the cursor over the whitespace trailing a span and
// returns the span's text. The rewind is exact because a span stops at a
// newline, so its tail can only hold horizontal whitespace.
func (l *lexer) trimSpanTail(start int) string {
	text := strings.TrimRight(string(l.src[start:l.off]), spanTrimCutset)
	back := (l.off - start) - len(text)
	l.off -= back
	l.col -= back
	return text
}

// goSpanStep consumes one lexeme of a Go span, reporting whether the span ends
// here.
func (l *lexer) goSpanStep(depth *int, stop []tokenKind) bool {
	c := l.src[l.off]
	if isIdentStart(c) {
		return l.goSpanIdentStep(*depth, stop)
	}
	if *depth == 0 && (c == '\n' || l.stopSetMatches(stop)) {
		return true
	}
	delta := bracketDelta(c)
	if *depth+delta < 0 {
		return true
	}
	*depth += delta
	l.advance()
	return false
}

// goSpanIdentStep consumes a whole identifier, reporting whether it ends the
// span. Consuming it whole is what keeps a reserved word recognizable only at a
// word boundary: `myreads` must not end a span at its `reads`, and `elsewhere`
// must not end one at its `else`.
func (l *lexer) goSpanIdentStep(depth int, stop []tokenKind) bool {
	word := l.identAt(l.off)
	if depth == 0 {
		if spanStopKeywords[word] {
			return true
		}
		if kind, ok := keywords[word]; ok && slices.Contains(stop, kind) {
			return true
		}
	}
	l.advanceN(len(word))
	return false
}

// stopSetMatches reports whether the cursor sits on a member of the stop set.
func (l *lexer) stopSetMatches(stop []tokenKind) bool {
	if len(stop) == 0 {
		return false
	}
	return slices.Contains(stop, l.peekKind())
}

// peekKind reports the punctuation or arrow kind under the cursor without
// advancing. Anything else reads as tokIllegal, which no caller puts in a stop
// set.
func (l *lexer) peekKind() tokenKind {
	if l.hasPrefix(arrowOp) {
		return tokArrow
	}
	if kind, ok := singleByteTokens[l.src[l.off]]; ok {
		return kind
	}
	return tokIllegal
}

// bracketDelta returns the depth change c makes.
func bracketDelta(c byte) int {
	switch c {
	case '(', '[', '{':
		return 1
	case ')', ']', '}':
		return -1
	default:
		return 0
	}
}

// funcSpanState carries the three nesting counters a func declaration span needs
// plus the position of the body's opening brace.
type funcSpanState struct {
	paren    int
	bracket  int
	brace    int
	sawBody  bool
	bodyOpen Position
}

// scanGoFuncSpan scans a func declaration's parameter list, results and body as
// one opaque span, from the parenthesis after the name to the brace that closes
// the body.
//
// This is the only GO-AWARE scan in the lexer. It takes no stop set — the span
// SELF-TERMINATES at brace balance — and it skips Go's five literal and comment
// forms whole, because a brace inside any of them is text rather than structure.
//
// An unterminated body runs to end of file, emits the partial span and records a
// diagnostic at the OPENING brace.
func (l *lexer) scanGoFuncSpan() token {
	l.skipHorizontal()
	open := l.pos()
	start := l.off
	state := funcSpanState{bodyOpen: open}
	for l.off < len(l.src) {
		if l.funcSpanStep(&state) {
			return token{kind: tokGoFuncSpan, text: string(l.src[start:l.off]), pos: open}
		}
	}
	l.diag(state.bodyOpen, l.pos(), "unterminated func body: no closing brace before end of file")
	return token{kind: tokGoFuncSpan, text: string(l.src[start:l.off]), pos: open}
}

// funcSpanStep consumes one lexeme of a func span, reporting whether the span is
// complete.
func (l *lexer) funcSpanStep(state *funcSpanState) bool {
	if l.skipGoLiteralOrComment() {
		return false
	}
	switch l.src[l.off] {
	case '(':
		state.paren++
	case ')':
		state.paren--
	case '[':
		state.bracket++
	case ']':
		state.bracket--
	case '{':
		return l.funcSpanOpenBrace(state)
	case '}':
		state.brace--
		if state.sawBody && state.brace == 0 {
			l.advance()
			return true
		}
	}
	l.advance()
	return false
}

// funcSpanOpenBrace records the body's opening brace and counts it.
//
// The body is the first brace outside the parameter list and any type
// parameters that does not open an anonymous struct or interface type.
func (l *lexer) funcSpanOpenBrace(state *funcSpanState) bool {
	if !state.sawBody && state.paren == 0 && state.bracket == 0 && !l.atTypeLiteralBrace() {
		state.sawBody = true
		state.bodyOpen = l.pos()
	}
	state.brace++
	l.advance()
	return false
}

// atTypeLiteralBrace reports whether the brace under the cursor opens an
// anonymous struct or interface type rather than the func body. Both forms are
// introduced by their keyword immediately before the brace, so a func returning
// `struct{ A int }` still finds its real body.
func (l *lexer) atTypeLiteralBrace() bool {
	j := l.off
	for j > 0 && strings.IndexByte(" \t\r\n", l.src[j-1]) >= 0 {
		j--
	}
	end := j
	for j > 0 && isIdentByte(l.src[j-1]) {
		j--
	}
	word := string(l.src[j:end])
	return word == "struct" || word == "interface"
}

// skipGoLiteralOrComment consumes a Go interpreted string, rune literal, raw
// string, line comment or block comment whole when the cursor sits at one,
// reporting whether it did.
//
// All five matter: a naive brace counter and a Go-aware one were measured
// against these forms and diverged on every one, each truncating the span
// mid-literal on the brace the form was holding as text.
func (l *lexer) skipGoLiteralOrComment() bool {
	switch {
	case l.hasPrefix(lineComment):
		l.skipUntilByte('\n')
	case l.hasPrefix(blockCommentOpen):
		l.advanceN(len(blockCommentOpen))
		l.skipPastDelim(blockCommentClose)
	case l.src[l.off] == '`':
		l.advance()
		l.skipPastDelim("`")
	case l.src[l.off] == '"':
		l.skipQuoted('"')
	case l.src[l.off] == '\'':
		l.skipQuoted('\'')
	default:
		return false
	}
	return true
}

// skipQuoted consumes a quoted literal opened by q, honoring backslash escapes
// and stopping at a line break, which neither of Go's quoted forms may cross.
func (l *lexer) skipQuoted(q byte) {
	l.advance()
	for l.off < len(l.src) {
		c := l.src[l.off]
		if c == '\\' {
			l.advanceN(2)
			continue
		}
		l.advance()
		if c == q || c == '\n' {
			return
		}
	}
}

// skipPastDelim consumes bytes through the next occurrence of d.
func (l *lexer) skipPastDelim(d string) {
	for l.off < len(l.src) {
		if l.hasPrefix(d) {
			l.advanceN(len(d))
			return
		}
		l.advance()
	}
}

// skipUntilByte consumes bytes up to but not including the next b.
func (l *lexer) skipUntilByte(b byte) {
	for l.off < len(l.src) && l.src[l.off] != b {
		l.advance()
	}
}
