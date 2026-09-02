// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ast

// SwitchStmt routes each datum to the target of the first arm that matches,
// top-down.
//
// Switch is the one shape where the input list precedes the Go fragment, and its
// body is one of exactly two braced regions in the language.
//
// Arms always holds at least one arm; a switch with none is a diagnostic. Else
// is optional at parse level and, when present, is always last, because
// first-match routing makes every arm after an else dead. Whether the arms cover
// the subject exhaustively is the analysis engine's question, not the parser's:
// a switch with no else parses clean.
type SwitchStmt struct {
	Clauses
	Name    Ident
	Subject GoSpan
	Arms    []SwitchArm
	Else    *Ident
	Start   Position
	Stop    Position
}

// Pos returns the statement's start.
func (s SwitchStmt) Pos() Position { return s.Start }

// End returns the position just past the switch body's closing brace.
func (s SwitchStmt) End() Position { return s.Stop }

func (SwitchStmt) isNode() {}
func (SwitchStmt) isStmt() {}

// SwitchArm is one arm of a switch: its values and the target they route to.
//
// The parser does NOT classify an arm's values into literals versus Go
// predicates — they are positioned verbatim spans, and telling one from the
// other would need an imported Go expression grammar with precedence
// backtracking. Classification belongs to the analysis engine.
type SwitchArm struct {
	Values []GoSpan
	Target Ident
	Start  Position
	Stop   Position
}

// Pos returns the arm's start.
func (a SwitchArm) Pos() Position { return a.Start }

// End returns the position just past the arm's target.
func (a SwitchArm) End() Position { return a.Stop }

func (SwitchArm) isNode() {}
