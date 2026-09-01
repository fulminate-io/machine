// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

// clauseBits give each clause a bit for the at-most-once check, which keeps the
// clause loop allocation free — a map per statement would be one allocation on
// the parse path per node in the file.
var clauseBits = map[tokenKind]uint8{
	kwReads:      1 << 0,
	kwWrites:     1 << 1,
	kwOver:       1 << 2,
	kwCheckpoint: 1 << 3,
	kwIdempotent: 1 << 6,
	kwOn:         1 << 4,
	kwNote:       1 << 5,
}

// clauseParsers dispatch a clause on its keyword.
var clauseParsers = map[tokenKind]func(*parser, *Clauses){
	kwReads:      (*parser).clauseReads,
	kwWrites:     (*parser).clauseWrites,
	kwOver:       (*parser).clauseOver,
	kwCheckpoint: (*parser).clauseCheckpoint,
	kwIdempotent: (*parser).clauseIdempotent,
	kwOn:         (*parser).clauseOnError,
	kwNote:       (*parser).clauseNote,
}

// parseClauses consumes a statement's trailing clauses in ANY ORDER.
//
// EACH CLAUSE MAY APPEAR AT MOST ONCE. A repeat is a diagnostic rather than a
// silent overwrite: the notation cannot say so, because the clauses are
// order-free and expressing "at most one of each" would impose an order the
// parser does not.
func (p *parser) parseClauses(cl *Clauses) {
	var seen uint8
	for p.atClause() {
		kind := p.tok.kind
		if bit := clauseBits[kind]; seen&bit != 0 {
			p.diagHeref("the %q clause is already given; each clause may appear at most once", p.tok.text)
		} else {
			seen |= bit
		}
		clauseParsers[kind](p, cl)
	}
}

// atClause reports whether a clause follows, looking past a newline when it has
// to.
//
// THE CONTINUATION RULE lives here: after a newline a statement continues only
// if the next line opens with one of the seven clause keywords, and otherwise the
// newline terminates it. That is one token of lookahead past the newline, which
// the lexer's re-entrancy supplies without giving the parser a second permanent
// lookahead slot. Indentation stays cosmetic and the grammar stays LL(1).
//
// This greed is also what routes `on` and `note` correctly. Once a clause
// bearing statement has been parsed, a line opening with either is ALWAYS that
// statement's clause rather than a sibling flow-level declaration — a parser
// that gets this wrong builds a wrong tree from source that parses without
// complaint.
func (p *parser) atClause() bool {
	if clauseStarters[p.tok.kind] {
		return true
	}
	if !p.at(tokNewline) {
		return false
	}
	saved := p.save()
	p.advance()
	if clauseStarters[p.tok.kind] {
		return true
	}
	p.restore(saved)
	return false
}

// clauseReads parses `reads a, b` — comma-separated state or var names.
func (p *parser) clauseReads(cl *Clauses) {
	p.advance()
	cl.Reads = p.identList("a state or var name")
}

// clauseWrites parses `writes a, b` — comma-separated state or var names.
func (p *parser) clauseWrites(cl *Clauses) {
	p.advance()
	cl.Writes = p.identList("a state or var name")
}

// clauseOver parses `over <factory>`.
func (p *parser) clauseOver(cl *Clauses) {
	p.advance()
	factory := p.goSpan()
	cl.Over = &factory
}

// clauseCheckpoint parses the bare `checkpoint` clause.
//
// ITS ZERO ARITY IS A PARSE RULE. After the keyword the parser records only its
// position, and anything other than a line end, another clause or a statement
// boundary following it is a diagnostic — a clause loop that simply continues
// after a bare keyword reads the operand as the start of the next clause and
// reports nothing at all.
func (p *parser) clauseCheckpoint(cl *Clauses) {
	at := p.tok.pos
	p.advance()
	cl.Checkpoint = &at

	// What may legally follow a bare checkpoint is FIRST(Clauses) — another
	// clause — or FOLLOW(Clauses), which is a newline from the six statement
	// productions and an opening brace from the switch production. Leaving the
	// brace out rejects `switch ... checkpoint {`, which is a correct switch.
	if p.at(tokNewline) || p.at(tokEOF) || p.at(tokLBrace) || clauseStarters[p.tok.kind] {
		return
	}
	p.diagHeref("\"checkpoint\" takes no operand, but %s follows it", describe(p.tok))
	p.skipToEndOfLine()
}

// clauseIdempotent parses the bare `idempotent` clause, which marks the node SAFE
// TO RUN AGAIN on the same datum and thereby selects the checkpoint anchor.
//
// ITS ZERO ARITY IS THE SAME PARSE RULE clauseCheckpoint states, for the same
// reason: the parser records only its position, and a clause loop that simply
// continued after a bare keyword would read an operand as the start of the next
// clause and report nothing at all.
func (p *parser) clauseIdempotent(cl *Clauses) {
	at := p.tok.pos
	p.advance()
	cl.Idempotent = &at

	// What may legally follow is FIRST(Clauses) — another clause — or
	// FOLLOW(Clauses), a newline from the statement productions and an opening
	// brace from the switch production.
	if p.at(tokNewline) || p.at(tokEOF) || p.at(tokLBrace) || clauseStarters[p.tok.kind] {
		return
	}
	p.diagHeref("\"idempotent\" takes no operand, but %s follows it", describe(p.tok))
	p.skipToEndOfLine()
}

// clauseOnError parses `on error <handler>`.
func (p *parser) clauseOnError(cl *Clauses) {
	p.advance()
	p.acceptErrorTerminal()
	handler := p.errorHandlerSpan()
	cl.OnError = &handler
}

// clauseNote parses `note """..."""`.
//
// Unlike the flow-level form this one leaves the terminating newline for the
// statement to consume, because a note clause may be followed by another clause
// on the same line.
func (p *parser) clauseNote(cl *Clauses) {
	at := p.tok.pos
	p.advance()

	if !p.at(tokNoteText) {
		p.diagHeref("expected a note body opened by %s, found %s", noteDelim, describe(p.tok))
		return
	}
	cl.Note = &NoteBlock{Text: p.tok.text, Start: at, Stop: p.end}
	p.advance()
}
