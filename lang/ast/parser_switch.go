// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

// parseSwitch parses the tenth statement shape:
// `switch <name> from <inputs> on <subject> [clauses] { arms [else] }`.
//
// Switch is the one shape where the input list precedes the Go fragment, and its
// body is one of exactly two braced regions in the language.
//
// WHAT THE PARSER OWNS IS STRUCTURE ONLY. It does not classify an arm's values
// into literals versus Go predicates — telling `"a", "b" ->` from `isValid(in)
// ->` would need an imported Go expression grammar with precedence
// backtracking, which is exactly what keeps this grammar out of the LL(1) band.
// Classification belongs to the analysis engine.
func (p *parser) parseSwitch() Stmt {
	stmt := SwitchStmt{Start: p.tok.pos}
	p.advance()

	stmt.Name, _ = p.expectIdent("a node name")
	stmt.From = p.parseFrom()
	p.expect(kwOn, "\"on\"")
	stmt.Subject = p.goSpan(tokLBrace)
	p.parseClauses(&stmt.Clauses)
	if !p.parseSwitchBody(&stmt) {
		stmt.Stop = p.tok.pos
		return stmt
	}
	stmt.Stop = p.endOfLine("a switch statement")
	return stmt
}

// parseSwitchBody parses the braced arm block.
//
// AT LEAST ONE ARM IS REQUIRED. The notation says so directly — the production
// is one-or-more rather than zero-or-more — so this rule needs no annotation and
// the conformance recognizer rejects an empty switch exactly as the parser does.
func (p *parser) parseSwitchBody(stmt *SwitchStmt) bool {
	open := p.tok.pos
	if !p.expect(tokLBrace, "\"{\"") {
		return false
	}
	p.expect(tokNewline, "a newline after the opening brace")
	p.parseSwitchArms(stmt)
	if len(stmt.Arms) == 0 {
		p.diagHeref("a switch needs at least one arm")
	}
	return p.closeBracedRegion(open, "the switch body")
}

// parseSwitchArms reads arms until the closing brace.
//
// EXHAUSTIVENESS IS NOT THE PARSER'S QUESTION: a switch with no else parses
// clean here, because requiring one would make every provably-exhaustive switch
// unparseable.
func (p *parser) parseSwitchArms(stmt *SwitchStmt) {
	// As in the state block, no blank-line guard is needed: the lexer collapses
	// a run of blank lines into one newline token and every arm consumes its own
	// line terminator.
	for !p.at(tokEOF) && !p.at(tokRBrace) {
		if p.at(kwElse) {
			p.parseSwitchElse(stmt)
			continue
		}
		arm, ok := p.parseSwitchArm()
		if !ok {
			return
		}
		if stmt.Else != nil {
			p.diagAtf(arm.Start, arm.Stop, "this arm follows the else arm and can never match; else must be last")
		}
		stmt.Arms = append(stmt.Arms, arm)
	}
}

// parseSwitchArm parses `<value> { , <value> } -> <target>`.
func (p *parser) parseSwitchArm() (SwitchArm, bool) {
	arm := SwitchArm{Start: p.tok.pos}
	first := p.goSpan(tokComma, tokArrow)
	if first.Text == "" {
		p.diagHeref("expected a switch arm value, found %s", describe(p.tok))
		p.skipToEndOfLine()
		arm.Stop = p.tok.pos
		return arm, false
	}
	arm.Values = []GoSpan{first}
	for p.accept(tokComma) {
		arm.Values = append(arm.Values, p.goSpan(tokComma, tokArrow))
	}
	if !p.expect(tokArrow, "\"->\"") {
		p.skipToEndOfLine()
		arm.Stop = p.tok.pos
		return arm, false
	}
	arm.Target, _ = p.expectIdent("the arm target")
	arm.Stop = arm.Target.End()
	p.endOfLine("a switch arm")
	return arm, true
}

// parseSwitchElse parses `else -> <target>`.
//
// An else is OPTIONAL and, when present, must be LAST: top-down first-match
// routing makes every arm after it dead.
//
// THAT ORDERING IS A PARSER-ONLY RULE, annotated as the fifth in the grammar
// header. Placing [ Else ] after the arm repetition looks like it expresses the
// ordering and does not: an Arm begins with the opaque goSpan terminal and
// `else` is not in that scanner's stop set, so `else -> target` matches Arm like
// any other arm value. The notation cannot constrain the content of an opaque
// terminal. The conformance recognizer is what proved it.
func (p *parser) parseSwitchElse(stmt *SwitchStmt) {
	at := p.tok.pos
	p.advance()

	p.expect(tokArrow, "\"->\"")
	target, _ := p.expectIdent("the else target")
	if stmt.Else != nil {
		p.diagAtf(at, target.End(), "a switch takes at most one else")
	} else {
		stmt.Else = &target
	}
	p.endOfLine("an else arm")
}
