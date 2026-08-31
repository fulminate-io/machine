// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

// nodeHead is the prefix source, transform and sink share: a name, a Go
// reference, an optional from-list and the trailing clauses.
//
// The three shapes differ only in whether they take inputs, so they are written
// through this one helper rather than as three near-identical blocks.
type nodeHead struct {
	start   Position
	name    Ident
	ref     GoSpan
	clauses Clauses
}

// parseNodeHead reads the shared prefix. THE GO REFERENCE COMES BEFORE `from` on
// every shape that invokes Go code.
//
// The reference is either a bare name or a qualified one and the parser treats
// both the same: it captures the span and BINDS NOTHING. Whether a bare name
// resolves to a local func declaration and a qualified one to an import is the
// analysis engine's work, so an unresolvable name is not a syntax error here.
func (p *parser) parseNodeHead(withInputs bool) nodeHead {
	head := nodeHead{start: p.tok.pos}
	p.advance()

	head.name, _ = p.expectIdent("a node name")
	head.ref = p.goSpan(kwFrom)
	if withInputs {
		head.clauses.From = p.parseFrom()
	}
	p.parseClauses(&head.clauses)
	return head
}

// parseSource parses `source <name> <goRef> [clauses]`.
//
// A source has no inputs, so its Clauses.From stays empty.
func (p *parser) parseSource() Stmt {
	head := p.parseNodeHead(false)
	stop := p.endOfLine("a source statement")
	return SourceStmt{Clauses: head.clauses, Name: head.name, Ref: head.ref, Start: head.start, Stop: stop}
}

// parseTransform parses `transform <name> <goRef> from <inputs> [clauses]`.
func (p *parser) parseTransform() Stmt {
	head := p.parseNodeHead(true)
	stop := p.endOfLine("a transform statement")
	return TransformStmt{Clauses: head.clauses, Name: head.name, Ref: head.ref, Start: head.start, Stop: stop}
}

// parseSink parses `sink <name> <goRef> from <inputs> [clauses]`.
func (p *parser) parseSink() Stmt {
	head := p.parseNodeHead(true)
	stop := p.endOfLine("a sink statement")
	return SinkStmt{Clauses: head.clauses, Name: head.name, Ref: head.ref, Start: head.start, Stop: stop}
}

// parseBranch parses `branch <name> <goRef> from <inputs> -> <true>, <false>`.
func (p *parser) parseBranch() Stmt {
	stmt := BranchStmt{Start: p.tok.pos}
	p.advance()

	stmt.Name, _ = p.expectIdent("a node name")
	stmt.Ref = p.goSpan(kwFrom)
	stmt.From = p.parseFrom()
	p.expect(tokArrow, "\"->\"")
	stmt.TrueTarget, _ = p.expectIdent("the true target")
	p.expect(tokComma, "\",\"")
	stmt.FalseTarget, _ = p.expectIdent("the false target")
	p.parseClauses(&stmt.Clauses)
	stmt.Stop = p.endOfLine("a branch statement")
	return stmt
}

// parseTee parses `tee <name> from <inputs> -> <targets...>`.
//
// A tee names no Go reference: it is route-only and applies no function.
func (p *parser) parseTee() Stmt {
	stmt := TeeStmt{Start: p.tok.pos}
	p.advance()

	stmt.Name, _ = p.expectIdent("a node name")
	stmt.From = p.parseFrom()
	p.expect(tokArrow, "\"->\"")
	stmt.Targets = p.identList("a tee target")
	p.parseClauses(&stmt.Clauses)
	stmt.Stop = p.endOfLine("a tee statement")
	return stmt
}

// parseDrop parses `drop <input>` — one bare name, and terminal, so there is no
// output name to carry.
func (p *parser) parseDrop() Stmt {
	stmt := DropStmt{Start: p.tok.pos}
	p.advance()

	stmt.Input, _ = p.expectIdent("the name to drop")
	stmt.Stop = p.endOfLine("a drop statement")
	return stmt
}

// parseLoop parses `loop <name>` — one bare name declaring a re-entry point.
func (p *parser) parseLoop() Stmt {
	stmt := LoopStmt{Start: p.tok.pos}
	p.advance()

	stmt.Name, _ = p.expectIdent("a loop name")
	stmt.Stop = p.endOfLine("a loop statement")
	return stmt
}

// parseSend parses `send <source> -> <target>`, the only backward arrow in the
// language. The target may be a node name or a loop name; the parser does not
// distinguish them and resolves neither.
func (p *parser) parseSend() Stmt {
	stmt := SendStmt{Start: p.tok.pos}
	p.advance()

	stmt.Source, _ = p.expectIdent("the name to send")
	p.expect(tokArrow, "\"->\"")
	stmt.Target, _ = p.expectIdent("the send target")
	stmt.Stop = p.endOfLine("a send statement")
	return stmt
}

// parseUse parses `use <instance> <flowRef> from <inputs> -> <bindings...>`.
//
// The bindings are POSITIONAL and caller-chosen: they name the embedded flow's
// outputs in signature order, and matching them up is the analysis engine's job.
func (p *parser) parseUse() Stmt {
	stmt := UseStmt{Start: p.tok.pos}
	p.advance()

	stmt.Instance, _ = p.expectIdent("an instance name")
	stmt.Flow = p.parseFlowRef()
	stmt.From = p.parseFrom()
	p.expect(tokArrow, "\"->\"")
	stmt.Bindings = p.identList("an output binding")
	p.parseClauses(&stmt.Clauses)
	stmt.Stop = p.endOfLine("a use statement")
	return stmt
}
