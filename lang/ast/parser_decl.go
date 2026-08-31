// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import "strings"

// parseImport parses `import [alias] "go/module/path"`.
//
// The path is captured verbatim; resolving it belongs to the loader, not here.
func (p *parser) parseImport() Decl {
	start := p.tok.pos
	p.advance()

	decl := ImportDecl{Start: start}
	if p.at(tokIdent) {
		alias := Ident{Name: p.tok.text, NamePos: p.tok.pos}
		decl.Alias = &alias
		p.advance()
	}
	if p.at(tokString) {
		decl.Path, decl.PathPos = p.tok.text, p.tok.pos
		p.advance()
	} else {
		p.diagHeref("expected a quoted import path, found %s", describe(p.tok))
	}
	decl.Stop = p.endOfLine("an import declaration")
	return decl
}

// parseConst parses `const name [Type] = <value>`.
func (p *parser) parseConst() Decl {
	start := p.tok.pos
	p.advance()

	decl := ConstDecl{Start: start}
	decl.Name, _ = p.expectIdent("a const name")
	if !p.at(tokAssign) {
		declared := p.goSpan(tokAssign)
		decl.Type = &declared
	}
	p.expect(tokAssign, "\"=\"")
	decl.Value = p.goSpan()
	decl.Stop = p.endOfLine("a const declaration")
	return decl
}

// parseParam parses `param name Type = <default>`.
func (p *parser) parseParam() Decl {
	start := p.tok.pos
	p.advance()

	decl := ParamDecl{Start: start}
	decl.Name, _ = p.expectIdent("a param name")
	decl.Type = p.goSpan(tokAssign)
	p.expect(tokAssign, "\"=\"")
	decl.Default = p.goSpan()
	decl.Stop = p.endOfLine("a param declaration")
	return decl
}

// parseFunc parses a top-level func declaration.
//
// THE PARSER READS EXACTLY ONE THING: the name, from the `func Name(` head.
// Everything from the opening parenthesis through the matching close brace is
// one opaque span. The parser never inspects the body and never resolves a
// bare-name reference against it; that binding happens at analysis time.
//
// A func with no name is the only way this declaration fails syntactically,
// because the name is the only thing it requires the parser to find.
func (p *parser) parseFunc() Decl {
	start, keywordEnd := p.tok.pos, p.end
	p.advance()

	decl := FuncDecl{Start: start}
	if !p.at(tokIdent) {
		p.diagAtf(start, keywordEnd, "a func declaration needs a name")
		decl.Stop = p.skipToStatementBoundary()
		return decl
	}
	decl.Name = Ident{Name: p.tok.text, NamePos: p.tok.pos}
	p.advance()
	decl.Body = p.goFuncSpan()
	decl.Stop = p.endOfLine("a func declaration")
	return decl
}

// parseFlow parses `flow name [signature]` and the braceless body that follows.
func (p *parser) parseFlow() Decl {
	decl := FlowDecl{Start: p.tok.pos}
	p.advance()

	decl.Name, _ = p.expectIdent("a flow name")
	if p.at(tokLParen) {
		decl.Signature = p.parseSignature()
	}
	decl.Stop = p.endOfLine("a flow declaration")
	p.parseFlowBody(&decl)
	return decl
}

// parseSignature parses `( InputType ) -> name Type { , name Type }`.
func (p *parser) parseSignature() *FlowSignature {
	sig := &FlowSignature{Start: p.tok.pos}
	p.advance()

	sig.Input = p.goSpan(tokRParen)
	p.expect(tokRParen, "\")\"")
	p.expect(tokArrow, "\"->\"")
	sig.Outputs = []FlowOutput{p.parseOutput()}
	for p.accept(tokComma) {
		sig.Outputs = append(sig.Outputs, p.parseOutput())
	}
	sig.Stop = sig.Outputs[len(sig.Outputs)-1].Stop
	return sig
}

// parseOutput parses one `name Type` pair of a flow signature.
//
// The type span stops at a comma so a MULTI-OUTPUT header parses: a stop set
// without one swallows every later output into the first one's type.
func (p *parser) parseOutput() FlowOutput {
	out := FlowOutput{Start: p.tok.pos}
	out.Name, _ = p.expectIdent("an output name")
	out.Type = p.goSpan(tokComma)
	out.Stop = out.Type.Stop
	return out
}

// parseFlowBody consumes the flow's entries.
//
// THE BODY IS BRACELESS: it runs to the next `flow` or `func` declaration, or to
// end of file. `func` being in that terminator set is not optional — a
// file-level func declared after a flow sits textually inside the flow's body
// region, and a terminator set of `flow` and EOF alone would swallow it as a
// statement and report an unexpected keyword rather than the truth.
func (p *parser) parseFlowBody(decl *FlowDecl) {
	for !p.at(tokEOF) && !p.at(kwFlow) && !p.at(kwFunc) {
		if p.at(tokNewline) {
			p.advance()
			continue
		}
		p.parseFlowEntry(decl)
	}
	decl.Stop = p.tok.pos
}

// parseFlowEntry parses one member of a flow body.
//
// `on` and `note` reach this function only when they are NOT a preceding
// statement's clause: the clause parser consumes a continuation line greedily,
// so once a clause-bearing statement has been parsed an `on` or `note` line is
// already gone by the time control returns here. That greed IS the enforcement
// of the rule that a flow-level `on error` or `note` precedes the first
// statement.
func (p *parser) parseFlowEntry(decl *FlowDecl) {
	switch p.tok.kind {
	case kwNote:
		p.flowNote(decl)
	case kwState:
		p.flowState(decl)
	case kwVar:
		p.flowVar(decl)
	case kwOn:
		p.flowOnError(decl)
	default:
		decl.Body = append(decl.Body, p.parseStatement())
	}
}

// flowNote attaches a flow-level note, keeping the first when a flow declares
// more than one so neither is silently discarded.
func (p *parser) flowNote(decl *FlowDecl) {
	note := p.parseNoteBlock()
	if decl.Note != nil {
		p.diagAtf(note.Start, note.Stop, "this flow already carries a note at %s", decl.Note.Start)
		return
	}
	decl.Note = note
}

// flowState attaches a flow's state block, keeping the first of several.
func (p *parser) flowState(decl *FlowDecl) {
	state, _ := p.parseState().(StateDecl)
	if decl.State != nil {
		p.diagAtf(state.Start, state.Stop, "this flow already declares state at %s", decl.State.Start)
		return
	}
	decl.State = &state
}

// flowVar appends a flow-body variable declaration.
func (p *parser) flowVar(decl *FlowDecl) {
	if declared, ok := p.parseVar().(VarDecl); ok {
		decl.Vars = append(decl.Vars, declared)
	}
}

// flowOnError attaches a flow-level error handler, keeping the first of several.
func (p *parser) flowOnError(decl *FlowDecl) {
	handler, _ := p.parseOnError().(OnErrorDecl)
	if decl.OnError != nil {
		p.diagAtf(handler.Start, handler.Stop, "this flow already declares an error handler at %s", decl.OnError.Start)
		return
	}
	decl.OnError = &handler
}

// parseState parses the `state { name Type ... }` block, one of exactly two
// braced regions the parser reads.
func (p *parser) parseState() Decl {
	decl := StateDecl{Start: p.tok.pos}
	p.advance()

	p.expect(tokLBrace, "\"{\"")
	p.expect(tokNewline, "a newline after the opening brace")
	for !p.at(tokEOF) && !p.at(tokRBrace) {
		if p.accept(tokNewline) {
			continue
		}
		field, ok := p.parseStateField()
		if !ok {
			break
		}
		decl.Fields = append(decl.Fields, field)
	}
	p.expect(tokRBrace, "\"}\"")
	decl.Stop = p.endOfLine("a state block")
	return decl
}

// parseStateField parses one `name Type` entry of a state block.
func (p *parser) parseStateField() (StateField, bool) {
	name, ok := p.expectIdent("a state field name")
	if !ok {
		p.skipToStatementBoundary()
		return StateField{}, false
	}
	field := StateField{Name: name, Start: name.NamePos}
	field.Type = p.goSpan()
	field.Stop = field.Type.Stop
	p.endOfLine("a state field")
	return field, true
}

// parseVar parses `var name Type [clone <factory>]`.
//
// NOTE THAT `var h func(int) error` PARSES. The type span begins with the `func`
// keyword, and it works only because `func` is deliberately absent from the Go
// span's stop set — the stop condition is the six clause keywords, not every
// keyword.
func (p *parser) parseVar() Decl {
	decl := VarDecl{Start: p.tok.pos}
	p.advance()

	decl.Name, _ = p.expectIdent("a var name")
	decl.Type = p.goSpan(kwClone)
	if p.accept(kwClone) {
		override := p.goSpan()
		decl.Clone = &override
	}
	decl.Stop = p.endOfLine("a var declaration")
	return decl
}

// parseOnError parses a flow-level `on error <handler>` declaration.
func (p *parser) parseOnError() Decl {
	decl := OnErrorDecl{Start: p.tok.pos}
	p.advance()

	p.acceptErrorTerminal()
	decl.Handler = p.errorHandlerSpan()
	decl.Stop = p.endOfLine("an on error declaration")
	return decl
}

// errorHandlerSpan captures the reference after `on error` and enforces the rule
// that it must be NON-EMPTY and must NOT BEGIN WITH an arrow. Error handling is
// a declaration, never an edge.
//
// The notation cannot express this, which is why it is annotated in the grammar
// header rather than encoded there: an arrow appears in neither FIRST nor FOLLOW
// of the clause set, so this span's stop set holds none and `-> handler` scans
// as a perfectly good non-empty span terminated by the newline.
func (p *parser) errorHandlerSpan() GoSpan {
	span := p.goSpan()
	switch {
	case span.Text == "":
		p.diagAtf(span.Start, span.Stop, "\"on error\" needs a handler reference")
	case strings.HasPrefix(span.Text, arrowOp):
		p.diagAtf(span.Start, span.Stop, "\"on error\" takes a handler reference, not an edge; remove the %q", arrowOp)
	}
	return span
}

// parseNote parses a flow-level note as a declaration.
func (p *parser) parseNote() Decl { return *p.parseNoteBlock() }

// parseNoteBlock parses `note """..."""`.
func (p *parser) parseNoteBlock() *NoteBlock {
	note := &NoteBlock{Start: p.tok.pos}
	p.advance()

	if !p.at(tokNoteText) {
		p.diagHeref("expected a note body opened by %s, found %s", noteDelim, describe(p.tok))
		note.Stop = p.skipToStatementBoundary()
		return note
	}
	note.Text, note.Stop = p.tok.text, p.end
	p.advance()
	p.endOfLine("a note")
	return note
}
