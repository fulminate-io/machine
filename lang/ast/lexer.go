// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

// arrowOp is the edge operator. It is an operator rather than a keyword, so it
// carries its own token kind.
const arrowOp = "->"

// singleByteTokens maps the punctuation bytes onto their kinds. A table rather
// than a switch keeps the operator scan inside the cyclomatic budget.
var singleByteTokens = map[byte]tokenKind{
	',': tokComma,
	'.': tokDot,
	'=': tokAssign,
	'{': tokLBrace,
	'}': tokRBrace,
	'(': tokLParen,
	')': tokRParen,
}

// lexer scans one source file into tokens.
//
// It is RE-ENTRANT AT AN OFFSET: next advances from wherever the cursor sits and
// caches nothing across calls, so a caller holding a byte offset can reposition
// and re-scan. The EBNF recognizer in the test suite depends on that to
// backtrack without materializing a token slice.
//
// The lexer makes no statement-termination decision. It always emits a newline
// token and leaves it to the parser to decide whether that newline ends a
// statement or precedes a continued clause line, which is what keeps the scanner
// context free.
type lexer struct {
	src   []byte
	off   int
	line  int
	col   int
	diags []Diagnostic
}

// newLexer returns a lexer positioned at the start of src.
func newLexer(src []byte) *lexer {
	return &lexer{src: src, line: 1, col: 1}
}

// pos returns the cursor's current position.
func (l *lexer) pos() Position {
	return Position{Offset: l.off, Line: l.line, Col: l.col}
}

// advance consumes one byte, maintaining the line and byte-column counters.
func (l *lexer) advance() {
	if l.off >= len(l.src) {
		return
	}
	if l.src[l.off] == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	l.off++
}

// advanceN consumes n bytes.
func (l *lexer) advanceN(n int) {
	for range n {
		l.advance()
	}
}

// diag records one positioned problem.
func (l *lexer) diag(start, end Position, message string) {
	l.diags = append(l.diags, Diagnostic{Pos: start, End: end, Message: message})
}

// hasPrefix reports whether the source at the cursor begins with s.
func (l *lexer) hasPrefix(s string) bool {
	return l.off+len(s) <= len(l.src) && string(l.src[l.off:l.off+len(s)]) == s
}

// skipHorizontal consumes spaces, tabs and carriage returns. Indentation is
// cosmetic in this language, so horizontal whitespace never produces a token.
func (l *lexer) skipHorizontal() {
	for l.off < len(l.src) {
		if c := l.src[l.off]; c != ' ' && c != '\t' && c != '\r' {
			return
		}
		l.advance()
	}
}

// next returns the token at the cursor and advances past it.
func (l *lexer) next() token {
	l.skipHorizontal()
	if l.off >= len(l.src) {
		return token{kind: tokEOF, pos: l.pos()}
	}
	c := l.src[l.off]
	switch {
	case c == '\n':
		return l.scanNewline()
	case l.hasPrefix(noteDelim):
		return l.scanNoteText()
	case c == '"':
		return l.scanString()
	case isIdentStart(c):
		return l.scanIdent()
	case isDigit(c):
		return l.scanNumber()
	}
	return l.scanOperator()
}

// scan returns the next token together with the position just past it.
//
// The end is the cursor's own position rather than a length added to the start,
// which keeps it exact for the region scans whose text omits its delimiters.
func (l *lexer) scan() (token, Position) {
	t := l.next()
	return t, l.pos()
}

// scanNewline emits one newline token for a line break and any blank lines
// following it, so a stretch of vertical whitespace reads as one terminator.
func (l *lexer) scanNewline() token {
	p := l.pos()
	l.advance()
	l.collapseBlankLines()
	return token{kind: tokNewline, text: "\n", pos: p}
}

// collapseBlankLines consumes every following line that holds only whitespace.
func (l *lexer) collapseBlankLines() {
	for {
		j := l.off
		for j < len(l.src) && (l.src[j] == ' ' || l.src[j] == '\t' || l.src[j] == '\r') {
			j++
		}
		if j >= len(l.src) || l.src[j] != '\n' {
			return
		}
		for l.off <= j {
			l.advance()
		}
	}
}

// scanIdent scans an identifier and resolves it against the keyword table.
func (l *lexer) scanIdent() token {
	p := l.pos()
	text := l.identAt(l.off)
	l.advanceN(len(text))
	if kind, ok := keywords[text]; ok {
		return token{kind: kind, text: text, pos: p}
	}
	return token{kind: tokIdent, text: text, pos: p}
}

// identAt returns the identifier beginning at off without moving the cursor.
func (l *lexer) identAt(off int) string {
	j := off
	for j < len(l.src) && isIdentByte(l.src[j]) {
		j++
	}
	return string(l.src[off:j])
}

// scanNumber scans a decimal literal with an optional fractional part.
func (l *lexer) scanNumber() token {
	p := l.pos()
	start := l.off
	l.consumeDigits()
	if l.off+1 < len(l.src) && l.src[l.off] == '.' && isDigit(l.src[l.off+1]) {
		l.advance()
		l.consumeDigits()
	}
	return token{kind: tokNumber, text: string(l.src[start:l.off]), pos: p}
}

// consumeDigits consumes a run of decimal digits.
func (l *lexer) consumeDigits() {
	for l.off < len(l.src) && isDigit(l.src[l.off]) {
		l.advance()
	}
}

// scanString scans a double-quoted literal, honoring backslash escapes. The
// token text keeps the surrounding quotes.
func (l *lexer) scanString() token {
	p := l.pos()
	start := l.off
	l.advance()
	for l.off < len(l.src) {
		c := l.src[l.off]
		if c == '\\' {
			l.advanceN(2)
			continue
		}
		if c == '\n' {
			break
		}
		l.advance()
		if c == '"' {
			return token{kind: tokString, text: string(l.src[start:l.off]), pos: p}
		}
	}
	l.diag(p, l.pos(), "unterminated string literal")
	return token{kind: tokString, text: string(l.src[start:l.off]), pos: p}
}

// scanOperator scans the arrow and the single-byte punctuation forms.
func (l *lexer) scanOperator() token {
	p := l.pos()
	if l.hasPrefix(arrowOp) {
		l.advanceN(len(arrowOp))
		return token{kind: tokArrow, text: arrowOp, pos: p}
	}
	c := l.src[l.off]
	l.advance()
	if kind, ok := singleByteTokens[c]; ok {
		return token{kind: kind, text: string(c), pos: p}
	}
	// An unrecognized byte is reported by the PARSER, not here. The lexer records
	// only what only IT can see — an unterminated note, an unterminated func
	// body. A byte that is no flow-language token is routinely a perfectly good
	// first byte of an opaque Go span, and the parser is the only layer that
	// knows which of the two it is looking at.
	return token{kind: tokIllegal, text: string(c), pos: p}
}

// nonASCII is the first byte value outside 7-bit ASCII. Identifiers admit those
// bytes so a name may be written in any script, as Go's own may be, without the
// lexer carrying a Unicode category table.
const nonASCII = 0x80

// isIdentStart reports whether c can begin an identifier.
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= nonASCII
}

// isIdentByte reports whether c can continue an identifier.
func isIdentByte(c byte) bool {
	return isIdentStart(c) || isDigit(c)
}

// isDigit reports whether c is a decimal digit.
func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}
