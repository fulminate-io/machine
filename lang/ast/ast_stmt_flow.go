// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ast

// DropStmt discards its input.
//
// Input is the name the drop CONSUMES, not an output name: a drop is terminal
// and produces nothing for a later statement to reference.
type DropStmt struct {
	Input Ident
	Start Position
	Stop  Position
}

// Pos returns the statement's start.
func (s DropStmt) Pos() Position { return s.Start }

// End returns the position just past the statement.
func (s DropStmt) End() Position { return s.Stop }

func (DropStmt) isNode() {}
func (DropStmt) isStmt() {}

// LoopStmt declares a re-entry point a send can target.
//
// A loop is a label rather than a node: it applies nothing and carries no
// clauses.
type LoopStmt struct {
	Name  Ident
	Start Position
	Stop  Position
}

// Pos returns the statement's start.
func (s LoopStmt) Pos() Position { return s.Start }

// End returns the position just past the statement.
func (s LoopStmt) End() Position { return s.Stop }

func (LoopStmt) isNode() {}
func (LoopStmt) isStmt() {}

// SendStmt routes a datum onward, and is the only backward arrow in the
// language: its target may be a node declared earlier or a loop label.
type SendStmt struct {
	Source Ident
	Target Ident
	Start  Position
	Stop   Position
}

// Pos returns the statement's start.
func (s SendStmt) Pos() Position { return s.Start }

// End returns the position just past the statement.
func (s SendStmt) End() Position { return s.Stop }

func (SendStmt) isNode() {}
func (SendStmt) isStmt() {}

// UseStmt embeds another flow under a local instance name.
//
// Flow is the dotted reference path to the embedded flow. Bindings NAME the
// embedded flow's outputs: they are a SET, order carries no meaning, and binding
// a subset is legal. The parser keeps them in source order because that is what
// it read, and matching them against the embedded flow's boundary belongs to
// lang/analysis. See parseUse for why the positional rule was the defect.
type UseStmt struct {
	Clauses
	Instance Ident
	Flow     []Ident
	Bindings []Ident
	Start    Position
	Stop     Position
}

// Pos returns the statement's start.
func (s UseStmt) Pos() Position { return s.Start }

// End returns the position just past the statement and its clauses.
func (s UseStmt) End() Position { return s.Stop }

func (UseStmt) isNode() {}
func (UseStmt) isStmt() {}

// BadStmt stands in for source the parser could not read, holding the span of
// the tokens it skipped to reach the next statement boundary.
//
// It exists so a recovered parse still yields a COMPLETE, positioned tree: an
// editor asking for structure over a file with a mistake in it gets the shape of
// the whole file back, with the damage localized rather than truncating
// everything after it.
type BadStmt struct {
	Start Position
	Stop  Position
}

// Pos returns the start of the skipped span.
func (s BadStmt) Pos() Position { return s.Start }

// End returns the position just past the skipped span.
func (s BadStmt) End() Position { return s.Stop }

func (BadStmt) isNode() {}
func (BadStmt) isStmt() {}
