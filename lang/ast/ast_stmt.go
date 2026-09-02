// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ast

// SourceStmt introduces data into a flow.
//
// A source has no inputs, so its embedded Clauses.From is always empty; it
// carries the same trailing clauses as every other node-bearing shape.
type SourceStmt struct {
	Clauses
	Name  Ident
	Ref   GoSpan
	Start Position
	Stop  Position
}

// Pos returns the statement's start.
func (s SourceStmt) Pos() Position { return s.Start }

// End returns the position just past the statement and its clauses.
func (s SourceStmt) End() Position { return s.Stop }

func (SourceStmt) isNode() {}
func (SourceStmt) isStmt() {}

// TransformStmt applies a Go reference to its inputs and produces one output
// under its own name.
//
// The Go reference is either a bare name or a qualified one and the parser
// treats both the same, capturing the span and binding nothing.
type TransformStmt struct {
	Clauses
	Name  Ident
	Ref   GoSpan
	Start Position
	Stop  Position
}

// Pos returns the statement's start.
func (s TransformStmt) Pos() Position { return s.Start }

// End returns the position just past the statement and its clauses.
func (s TransformStmt) End() Position { return s.Stop }

func (TransformStmt) isNode() {}
func (TransformStmt) isStmt() {}

// BranchStmt routes each datum to one of two targets on a predicate.
type BranchStmt struct {
	Clauses
	Name        Ident
	Ref         GoSpan
	TrueTarget  Ident
	FalseTarget Ident
	Start       Position
	Stop        Position
}

// Pos returns the statement's start.
func (s BranchStmt) Pos() Position { return s.Start }

// End returns the position just past the statement and its clauses.
func (s BranchStmt) End() Position { return s.Stop }

func (BranchStmt) isNode() {}
func (BranchStmt) isStmt() {}

// TeeStmt copies each datum to every one of its targets.
//
// A tee is route-only: it names no Go reference because it applies no function.
type TeeStmt struct {
	Clauses
	Name    Ident
	Targets []Ident
	Start   Position
	Stop    Position
}

// Pos returns the statement's start.
func (s TeeStmt) Pos() Position { return s.Start }

// End returns the position just past the statement and its clauses.
func (s TeeStmt) End() Position { return s.Stop }

func (TeeStmt) isNode() {}
func (TeeStmt) isStmt() {}

// SinkStmt consumes its inputs and is terminal, so it produces no output for a
// later statement to name.
type SinkStmt struct {
	Clauses
	Name  Ident
	Ref   GoSpan
	Start Position
	Stop  Position
}

// Pos returns the statement's start.
func (s SinkStmt) Pos() Position { return s.Start }

// End returns the position just past the statement and its clauses.
func (s SinkStmt) End() Position { return s.Stop }

func (SinkStmt) isNode() {}
func (SinkStmt) isStmt() {}
