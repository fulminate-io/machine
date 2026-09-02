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

// requireClauseOperand refuses a clause whose Go-expression operand is empty,
// and is the ONE guarded arm both `over` and `checkpoint` call.
//
// IT IS EXPLICIT WORK BECAUSE THE SPAN READER DOES NOT SUPPLY IT. A goSpan ends
// at a depth-zero newline or at a clause keyword, so BOTH a clause written bare
// at end of line and one written `checkpoint idempotent` produce a perfectly
// well-formed EMPTY span and no diagnostic at all. Adopting the goSpan
// production alone therefore refuses nothing.
//
// IT IS ONE HELPER RATHER THAN A COPY IN EACH CLAUSE on purpose. Two copies are
// how one of them later drifts or is dropped, and the failure is silent: the
// clause that kept its guard passes every test written for it while the other
// goes on accepting an operandless clause.
//
// The diagnostic is positioned on the SPAN, not at the current token. After the
// span is read the parser sits on whatever follows it — for a bare clause at end
// of line, the newline — so reporting here would point at the wrong line. An
// empty span's Start and Stop collapse to the point the operand should have
// occupied, which is where the author has to type.
func (p *parser) requireClauseOperand(span GoSpan, keyword, what string) {
	if span.Text == "" {
		p.diagAtf(span.Start, span.Stop, "%q needs an operand: %s", keyword, what)
	}
}

// clauseOver parses `over <factory>`.
func (p *parser) clauseOver(cl *Clauses) {
	p.advance()
	factory := p.goSpan()
	p.requireClauseOperand(factory, textOver, "a transport factory expression")
	cl.Over = &factory
}

// clauseCheckpoint parses `checkpoint <codec>`.
//
// ITS OPERAND IS A GO EXPRESSION, read by the same span reader `over` uses and
// left uninterpreted here: this package does not know what a codec is. The
// canonical form is `checkpoint machine.GobCodec[Order]{}`.
//
// THE OPERAND IS REQUIRED, and its absence is refused through the shared arm
// above rather than by this production. The clause was bare until the codec
// became part of it; the old zero-arity rule read the operand as the start of
// the next clause, which is why the parser had to diagnose a trailing token
// itself. That is now the span reader's job, and the only thing left to check is
// that the span is not empty.
func (p *parser) clauseCheckpoint(cl *Clauses) {
	p.advance()
	codec := p.goSpan()
	p.requireClauseOperand(codec, textCheckpoint, "a codec expression")
	cl.Checkpoint = &codec
}

// clauseIdempotent parses the bare `idempotent` clause, which marks the node SAFE
// TO RUN AGAIN on the same datum and thereby selects the checkpoint anchor.
//
// ITS ZERO ARITY IS A PARSE RULE, and it is now the ONLY clause that keeps one.
// `checkpoint` stated the same rule until it grew a codec operand; this clause
// takes none, so the parser records only its position and diagnoses a trailing
// token itself — a clause loop that simply continued after a bare keyword would
// read that token as the start of the next clause and report nothing at all.
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
