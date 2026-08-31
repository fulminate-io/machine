// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

// StateDecl is a flow's state block, one of exactly two braced regions in the
// language — the switch body is the other, and a func body is brace delimited
// but opaque.
//
// It holds ONLY cross-datum shared entries, as bare name-and-type fields.
type StateDecl struct {
	Fields []StateField
	Start  Position
	Stop   Position
}

// Pos returns the state block's start.
func (d StateDecl) Pos() Position { return d.Start }

// End returns the position just past the state block's closing brace.
func (d StateDecl) End() Position { return d.Stop }

func (StateDecl) isNode() {}
func (StateDecl) isDecl() {}

// StateField is one entry of a state block.
//
// A state entry is shared across datums and is never cloned, which is why it
// carries no clone override where a var does.
type StateField struct {
	Name  Ident
	Type  GoSpan
	Start Position
	Stop  Position
}

// Pos returns the field name's start.
func (f StateField) Pos() Position { return f.Start }

// End returns the position just past the field's type.
func (f StateField) End() Position { return f.Stop }

func (StateField) isNode() {}

// VarDecl declares a flow-body variable with an optional clone override.
//
// The override is the ONE place `clone` appears in the language. It is a
// var-declaration clause rather than a statement clause because a var is what is
// fresh per datum and copied per tee branch.
//
// `var h func(int) error` parses: the type span begins with the `func` keyword,
// and that works because `func` is deliberately not in the Go span's stop set.
type VarDecl struct {
	Name  Ident
	Type  GoSpan
	Clone *GoSpan
	Start Position
	Stop  Position
}

// Pos returns the declaration's start.
func (d VarDecl) Pos() Position { return d.Start }

// End returns the position just past the declaration.
func (d VarDecl) End() Position { return d.Stop }

func (VarDecl) isNode() {}
func (VarDecl) isDecl() {}

// ConstDecl declares a compile-time constant with an optional explicit type.
type ConstDecl struct {
	Name  Ident
	Type  *GoSpan
	Value GoSpan
	Start Position
	Stop  Position
}

// Pos returns the declaration's start.
func (d ConstDecl) Pos() Position { return d.Start }

// End returns the position just past the declaration.
func (d ConstDecl) End() Position { return d.Stop }

func (ConstDecl) isNode() {}
func (ConstDecl) isDecl() {}

// ParamDecl declares a caller-supplied parameter with a required type and a
// default value.
type ParamDecl struct {
	Name    Ident
	Type    GoSpan
	Default GoSpan
	Start   Position
	Stop    Position
}

// Pos returns the declaration's start.
func (d ParamDecl) Pos() Position { return d.Start }

// End returns the position just past the declaration.
func (d ParamDecl) End() Position { return d.Stop }

func (ParamDecl) isNode() {}
func (ParamDecl) isDecl() {}

// OnErrorDecl is a flow-level error handler.
//
// It must appear BEFORE the first statement of the flow body: after any
// statement, a line beginning with `on` is that statement's clause instead.
//
// Handler is never empty and never begins with an arrow. Error handling is a
// declaration, not an edge.
type OnErrorDecl struct {
	Handler GoSpan
	Start   Position
	Stop    Position
}

// Pos returns the declaration's start.
func (d OnErrorDecl) Pos() Position { return d.Start }

// End returns the position just past the handler reference.
func (d OnErrorDecl) End() Position { return d.Stop }

func (OnErrorDecl) isNode() {}
func (OnErrorDecl) isDecl() {}

// NoteBlock is a raw prose block attached to a flow or to a statement.
//
// Text is verbatim between the delimiters: indentation and internal newlines are
// preserved, and there are no escapes, so a note cannot contain the delimiter.
type NoteBlock struct {
	Text  string
	Start Position
	Stop  Position
}

// Pos returns the note's opening delimiter.
func (n NoteBlock) Pos() Position { return n.Start }

// End returns the position just past the note's closing delimiter.
func (n NoteBlock) End() Position { return n.Stop }

func (NoteBlock) isNode() {}
func (NoteBlock) isDecl() {}
