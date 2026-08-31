// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import "fmt"

// tokenNames give the structural token kinds a name a diagnostic can print.
// Keyword kinds are absent on purpose: their own text is the best name for
// them.
var tokenNames = map[tokenKind]string{
	tokEOF:        "end of file",
	tokNewline:    "end of line",
	tokIdent:      "a name",
	tokString:     "a string",
	tokNumber:     "a number",
	tokArrow:      "\"->\"",
	tokComma:      "\",\"",
	tokDot:        "\".\"",
	tokAssign:     "\"=\"",
	tokLBrace:     "\"{\"",
	tokRBrace:     "\"}\"",
	tokLParen:     "\"(\"",
	tokRParen:     "\")\"",
	tokNoteText:   "a note body",
	tokGoSpan:     "Go source",
	tokGoFuncSpan: "a func body",
	tokIllegal:    "an unrecognized character",
}

// describe names a token for a diagnostic message.
func describe(t token) string {
	if name, ok := tokenNames[t.kind]; ok {
		return name
	}
	return fmt.Sprintf("%q", t.text)
}

// entryKeywords open a file-level entry or a flow-body statement. Recovery
// resynchronizes on them.
var entryKeywords = map[tokenKind]bool{
	kwImport: true, kwConst: true, kwParam: true, kwFunc: true, kwFlow: true,
	kwState: true, kwVar: true, kwNote: true, kwOn: true,
	kwSource: true, kwTransform: true, kwBranch: true, kwSwitch: true,
	kwTee: true, kwSink: true, kwDrop: true, kwLoop: true, kwSend: true, kwUse: true,
}

// clauseStarters are the six keywords that may open a trailing clause, and so
// the six that let a statement continue onto the next line.
var clauseStarters = map[tokenKind]bool{
	kwReads: true, kwWrites: true, kwOver: true,
	kwCheckpoint: true, kwOn: true, kwNote: true,
}

// advance fetches the next token together with the position just past it.
//
// The end is taken from the lexer's own cursor rather than derived from the
// token's text, which keeps it exact for the region scans whose text omits its
// delimiters.
func (p *parser) advance() {
	p.lexDiagsBeforeTok = len(p.lex.diags)
	p.tok, p.end = p.lex.scan()
}

// at reports whether the lookahead is of the given kind.
func (p *parser) at(kind tokenKind) bool { return p.tok.kind == kind }

// accept consumes the lookahead when it is of the given kind.
func (p *parser) accept(kind tokenKind) bool {
	if !p.at(kind) {
		return false
	}
	p.advance()
	return true
}

// expect consumes the lookahead when it is of the given kind, and otherwise
// RECORDS A DIAGNOSTIC AND CONTINUES.
//
// Continuing is the whole point of this parser: a fail-fast expect would unwind
// the parse and throw away every statement after the first mistake, which is
// exactly the tree an editor most needs.
func (p *parser) expect(kind tokenKind, label string) bool {
	if p.accept(kind) {
		return true
	}
	p.diagHeref("expected %s, found %s", label, describe(p.tok))
	return false
}

// expectIdent consumes an identifier, reporting what was wanted when the
// lookahead is something else.
func (p *parser) expectIdent(label string) (Ident, bool) {
	if !p.at(tokIdent) {
		p.diagHeref("expected %s, found %s", label, describe(p.tok))
		return Ident{NamePos: p.tok.pos}, false
	}
	name := Ident{Name: p.tok.text, NamePos: p.tok.pos}
	p.advance()
	return name, true
}

// separatedIdents parses `name { sep name }` — the shape a comma-separated list
// and a dotted reference path both take.
func (p *parser) separatedIdents(sep tokenKind, label string) []Ident {
	first, ok := p.expectIdent(label)
	if !ok {
		return nil
	}
	names := []Ident{first}
	for p.accept(sep) {
		next, nextOK := p.expectIdent(label)
		if !nextOK {
			break
		}
		names = append(names, next)
	}
	return names
}

// identList parses a comma-separated list of names, the shape the from-list, the
// reads and writes clauses and the tee and use target lists all take.
func (p *parser) identList(label string) []Ident {
	return p.separatedIdents(tokComma, label)
}

// parseFlowRef parses a dotted reference path to an embedded flow.
func (p *parser) parseFlowRef() []Ident {
	return p.separatedIdents(tokDot, "a flow reference")
}

// parseFrom parses `from a, b, c`.
func (p *parser) parseFrom() []Ident {
	if !p.expect(kwFrom, "\"from\"") {
		return nil
	}
	return p.identList("an input name")
}

// skipToEndOfLine discards the rest of the line WITHOUT consuming its newline,
// so the caller's own line-terminator handling still runs and the mistake is
// reported once rather than twice.
func (p *parser) skipToEndOfLine() {
	from, last := p.tok.pos, p.tok.pos
	for !p.at(tokNewline) && !p.at(tokEOF) && !entryKeywords[p.tok.kind] {
		last = p.end
		p.advance()
	}
	p.noteSkip(from, last)
}

// cursor is a restorable parser position: the lexer's byte cursor, the token the
// parser is holding, and how many diagnostics the lexer had recorded.
type cursor struct {
	off   int
	line  int
	col   int
	tok   token
	end   Position
	diags int
}

// save captures the parser's position so a tentative read can be undone.
func (p *parser) save() cursor {
	return cursor{off: p.lex.off, line: p.lex.line, col: p.lex.col, tok: p.tok, end: p.end, diags: len(p.lex.diags)}
}

// restore rewinds to a saved position, discarding anything the tentative read
// recorded so a re-scan cannot report the same problem twice.
func (p *parser) restore(c cursor) {
	p.lex.off, p.lex.line, p.lex.col = c.off, c.line, c.col
	p.lex.diags = p.lex.diags[:c.diags]
	p.tok, p.end = c.tok, c.end
}

// rewindToLookahead puts the lexer's cursor back at the start of the token the
// parser is holding.
//
// This is what the lexer's re-entrancy is for. The parser holds one token of
// lookahead, so by the time it decides a Go fragment starts here the lexer has
// already scanned past that token; a raw-span scan without this rewind would
// begin one token late and silently drop the fragment's first lexeme.
func (p *parser) rewindToLookahead() {
	p.lex.off = p.tok.pos.Offset
	p.lex.line = p.tok.pos.Line
	p.lex.col = p.tok.pos.Col
	// A RETRACTED READ RETRACTS ITS DIAGNOSTICS. The lookahead that is being
	// rewound past was scanned under the ordinary token rules, and anything the
	// lexer concluded from that read — an unterminated string, say — is about a
	// reading the parser has just discarded.
	p.lex.diags = p.lex.diags[:p.lexDiagsBeforeTok]
}

// goSpan captures a verbatim Go fragment ending at one of the stop kinds.
func (p *parser) goSpan(stop ...tokenKind) GoSpan {
	p.rewindToLookahead()
	span := p.lex.scanGoSpan(stop...)
	stopPos := p.lex.pos()
	p.advance()
	return GoSpan{Text: span.text, Start: span.pos, Stop: stopPos}
}

// goFuncSpan captures a func declaration's parameters, results and body.
func (p *parser) goFuncSpan() GoSpan {
	p.rewindToLookahead()
	span := p.lex.scanGoFuncSpan()
	stopPos := p.lex.pos()
	p.advance()
	return GoSpan{Text: span.text, Start: span.pos, Stop: stopPos}
}

// errorTerminal is the second word of the `on error` form. It is a quoted
// TERMINAL in the grammar and deliberately NOT a keyword: adding it to the
// keyword table is the natural mistake the keyword census refuses.
const errorTerminal = "error"

// acceptErrorTerminal consumes the `error` word of an `on error` form, reporting
// its absence and continuing so the handler reference is still read.
func (p *parser) acceptErrorTerminal() {
	if p.at(tokIdent) && p.tok.text == errorTerminal {
		p.advance()
		return
	}
	p.diagHeref("expected %q after \"on\", found %s", errorTerminal, describe(p.tok))
}

// diagAtf records one positioned problem.
func (p *parser) diagAtf(start, end Position, format string, args ...any) {
	p.record(Diagnostic{Pos: start, End: end, Message: fmt.Sprintf(format, args...)})
}

// record is the ONE place a diagnostic enters the parser's list, and so the one
// place the cap is applied. The lexer's own findings are drained through it too,
// rather than appended raw, so there is a single mechanism to get wrong instead
// of two that mask each other.
func (p *parser) record(d Diagnostic) {
	if len(p.diags) >= maxDiagnostics {
		p.suppressed++
		return
	}
	p.diags = append(p.diags, d)
}

// noteSkip records the span recovery discarded, coalescing consecutive skips
// within one statement into a single range.
func (p *parser) noteSkip(from, to Position) {
	if to.Offset <= from.Offset {
		return
	}
	if !p.didSkip {
		p.skippedFrom, p.didSkip = from, true
	}
	p.skippedTo = to
}

// takeSkipped returns a BadStmt covering whatever recovery discarded since the
// last call, and reports whether there was any.
//
// EVERY BadStmt IS POSITIONED AND IN TREE ORDER, so a consumer walking the tree
// sees an unbroken sequence of spans covering the file. That is what makes a
// partial tree usable rather than merely non-nil.
func (p *parser) takeSkipped() (BadStmt, bool) {
	if !p.didSkip {
		return BadStmt{}, false
	}
	p.didSkip = false
	return BadStmt{Start: p.skippedFrom, Stop: p.skippedTo}, true
}

// closeBracedRegion consumes a braced region's closing brace, reporting ONE
// diagnostic positioned at the OPENING brace when the region ran to end of file.
//
// The position is the point: at end of file the missing brace is everywhere and
// nowhere, and the only place a reader can act on is the brace that opened the
// region and was never closed. Reports whether the region closed.
func (p *parser) closeBracedRegion(open Position, what string) bool {
	if p.accept(tokRBrace) {
		return true
	}
	if p.at(tokEOF) {
		p.diagAtf(open, open, "%s is never closed: no \"}\" before end of file", what)
		return false
	}
	p.diagHeref("expected \"}\" to close %s, found %s", what, describe(p.tok))
	return false
}

// diagHeref records a problem covering the lookahead token.
func (p *parser) diagHeref(format string, args ...any) {
	p.diagAtf(p.tok.pos, p.end, format, args...)
}

// skipToStatementBoundary discards the rest of a broken statement and returns
// the position it stopped at.
//
// THE BOUNDARY IS THE CONTINUATION RULE READ IN REVERSE: a newline ends the
// statement only when the line after it does NOT open with a clause keyword.
// Stopping at the first newline instead would strand a broken statement's own
// clause lines as garbage statements, each producing its own spurious
// diagnostic — one mistake reported five times.
//
// A closing brace also ends the skip, so a broken statement inside a switch body
// or a state block does not swallow the brace that closes the region.
func (p *parser) skipToStatementBoundary() Position {
	from, last := p.tok.pos, p.end
	consumed := false
	for !p.at(tokEOF) && !p.at(tokRBrace) {
		if p.at(tokNewline) {
			if p.atClause() {
				continue
			}
			last, consumed = p.end, true
			p.advance()
			break
		}
		last, consumed = p.end, true
		p.advance()
	}
	if !consumed && !p.at(tokEOF) {
		// The skip is also the parser's guarantee of forward progress. A stray
		// closing brace outside a braced region stops the loop immediately, and
		// a caller looping on a malformed entry would then spin on it forever.
		last = p.end
		p.advance()
	}
	p.noteSkip(from, last)
	return last
}

// endOfLine consumes the newline that terminates a declaration or statement,
// reporting anything else left on the line.
func (p *parser) endOfLine(what string) Position {
	if p.at(tokEOF) {
		p.diagHeref("%s is not terminated by a newline; every .flow file ends with one", what)
		return p.tok.pos
	}
	if p.at(tokNewline) {
		stop := p.tok.pos
		p.advance()
		return stop
	}
	p.diagHeref("unexpected %s after %s", describe(p.tok), what)
	return p.skipToStatementBoundary()
}
