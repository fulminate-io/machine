// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import (
	"cmp"
	"fmt"
	"slices"
)

// maxDiagnostics caps what one parse records.
//
// The parse is bounded by the file, but a caller rendering diagnostics is not,
// and adversarial input can drive the count arbitrarily high. A SILENT
// truncation would tell an editor's user the file has exactly this many
// problems, which is false — so the cap is recorded as the final diagnostic and
// it names how many it suppressed.
const maxDiagnostics = 100

// parser holds exactly ONE token of lookahead.
//
// The grammar is LL(1) by construction — every statement shape opens with a
// distinct keyword — so one token is all it needs, and the streaming shape is
// what lets the parser switch the lexer into a raw-span scan at the exact
// grammar positions where a Go fragment is expected.
type parser struct {
	lex   *lexer
	src   []byte
	tok   token
	end   Position
	diags []Diagnostic

	// lexDiagsBeforeTok is how many diagnostics the lexer had recorded before
	// the current lookahead token was scanned, so a rewind can retract the ones
	// that read belongs to.
	lexDiagsBeforeTok int

	// suppressed counts the diagnostics the cap refused to record.
	suppressed int

	// skippedFrom, skippedTo and didSkip carry the span recovery discarded
	// while parsing the current statement, so the tree can carry a BadStmt
	// covering exactly that text.
	skippedFrom Position
	skippedTo   Position
	didSkip     bool
}

// Parse parses one .flow source file into a syntax tree.
//
// IT ALWAYS RETURNS A NON-NIL *File, and returns a non-nil *Error exactly when
// at least one problem was found. Handing back a usable value alongside a
// non-nil error is unusual Go and is deliberate here: a tolerant parser's whole
// product is the partial tree, so an editor asking for structure over a file its
// author is midway through typing still gets the shape of the whole file. Do not
// "fix" this to return a nil tree on error.
func Parse(src []byte) (*File, error) {
	p := &parser{src: src}
	p.lex = newLexer(p.src)
	p.advance()

	file := p.parseFile()
	p.drainLexerDiagnostics()
	if len(p.diags) == 0 {
		return file, nil
	}
	return file, &Error{Diagnostics: p.diags, File: file}
}

// drainLexerDiagnostics folds the lexer's own findings into the parser's and
// puts the merged list back into source order.
//
// The lexer reports what only it can see — an unterminated note, an unterminated
// func body — each positioned at its opening delimiter, and those must reach the
// caller through the same channel as syntax errors. Without this, a file whose
// only problem is an unterminated note returns a nil error.
func (p *parser) drainLexerDiagnostics() {
	for _, d := range p.lex.diags {
		p.record(d)
	}
	slices.SortStableFunc(p.diags, func(a, b Diagnostic) int {
		return cmp.Compare(a.Pos.Offset, b.Pos.Offset)
	})
	if p.suppressed == 0 {
		return
	}
	at := p.diags[len(p.diags)-1].End
	p.diags = append(p.diags, Diagnostic{
		Pos: at, End: at,
		Message: fmt.Sprintf("%d further problems in this file were not reported", p.suppressed),
	})
}

// declParsers dispatches a file-level entry on its opening keyword.
//
// A map of method expressions rather than a switch: a switch this wide over a
// token-kind enum measures past the cyclomatic limit, and a switch over a
// declared enum would additionally need a default to satisfy the exhaustive
// check.
var declParsers = map[tokenKind]func(*parser) Decl{
	kwImport: (*parser).parseImport,
	kwConst:  (*parser).parseConst,
	kwParam:  (*parser).parseParam,
	kwFunc:   (*parser).parseFunc,
	kwFlow:   (*parser).parseFlow,
	kwState:  (*parser).parseState,
	kwVar:    (*parser).parseVar,
	kwNote:   (*parser).parseNote,
	kwOn:     (*parser).parseOnError,
}

// stmtParsers dispatches a flow-body statement on its opening keyword. Ten
// shapes, one entry each.
var stmtParsers = map[tokenKind]func(*parser) Stmt{
	kwSource:    (*parser).parseSource,
	kwTransform: (*parser).parseTransform,
	kwBranch:    (*parser).parseBranch,
	kwSwitch:    (*parser).parseSwitch,
	kwTee:       (*parser).parseTee,
	kwSink:      (*parser).parseSink,
	kwDrop:      (*parser).parseDrop,
	kwLoop:      (*parser).parseLoop,
	kwSend:      (*parser).parseSend,
	kwUse:       (*parser).parseUse,
}

// parseFile walks the top-level entries.
func (p *parser) parseFile() *File {
	file := &File{Start: Position{Offset: 0, Line: 1, Col: 1}}
	sawFlow := false
	for p.tok.kind != tokEOF {
		if p.tok.kind == tokNewline {
			p.advance()
			continue
		}
		if decl := p.parseEntry(&sawFlow); decl != nil {
			file.Decls = append(file.Decls, decl)
		}
	}
	file.Stop = p.tok.pos
	return file
}

// parseEntry parses one file-level entry.
//
// The loop carries exactly one piece of context, and this is where it is used: a
// STATEMENT reaching file level has no flow to belong to. The grammar admits it
// — File derives Entry derives FlowEntry derives Statement with no FlowDecl
// anywhere in the chain — which is exactly why it is a parser-only rule the
// header annotates. It fails rather than degrades because a statement names
// inputs and produces an output inside a traversal: collecting one outside a
// flow would build a structure no consumer can interpret while reporting
// success.
func (p *parser) parseEntry(sawFlow *bool) Decl {
	if parse, ok := declParsers[p.tok.kind]; ok {
		if p.tok.kind == kwFlow {
			*sawFlow = true
		}
		return parse(p)
	}
	if _, isStatement := stmtParsers[p.tok.kind]; isStatement {
		p.diagStatementOutsideFlow(*sawFlow)
		p.skipToStatementBoundary()
		return nil
	}
	p.diagHeref("expected a declaration, found %s", describe(p.tok))
	p.skipToStatementBoundary()
	return nil
}

// diagStatementOutsideFlow reports a statement that reached file level, naming
// which of the two ways it got there.
func (p *parser) diagStatementOutsideFlow(sawFlow bool) {
	if sawFlow {
		p.diagHeref("%q has no flow to belong to: a func declaration ended the preceding flow body", p.tok.text)
		return
	}
	p.diagHeref("%q must follow a flow declaration; there is no flow to belong to", p.tok.text)
}

// parseStatement dispatches one flow-body statement, returning nil when the line
// opened with something that is not a statement keyword at all.
//
// That nil is not a dropped statement: the skip it performs is recorded, and the
// caller turns it into a positioned BadStmt. This is the path that catches every
// rejected shape spelling with a position on it.
func (p *parser) parseStatement() Stmt {
	p.didSkip = false
	parse, ok := stmtParsers[p.tok.kind]
	if !ok {
		p.diagHeref("expected a statement, found %s", describe(p.tok))
		p.skipToStatementBoundary()
		return nil
	}
	return parse(p)
}
