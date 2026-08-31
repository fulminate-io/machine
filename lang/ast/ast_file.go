// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

// File is one parsed .flow source file.
//
// Several flows may live in one file and there is no relationship between a
// flow's name and the file's name.
type File struct {
	Decls []Decl
	Start Position
	Stop  Position
}

// Pos returns the file's start.
func (f File) Pos() Position { return f.Start }

// End returns the position just past the file's last declaration.
func (f File) End() Position { return f.Stop }

func (File) isNode() {}

// ImportDecl brings a Go package into scope for the qualified names statements
// reference.
//
// Path is the literal as written, quotes included: the parser captures it and
// resolves nothing.
type ImportDecl struct {
	Alias   *Ident
	Path    string
	PathPos Position
	Start   Position
	Stop    Position
}

// Pos returns the declaration's start.
func (d ImportDecl) Pos() Position { return d.Start }

// End returns the position just past the declaration.
func (d ImportDecl) End() Position { return d.Stop }

func (ImportDecl) isNode() {}
func (ImportDecl) isDecl() {}

// FuncDecl is a Go function declared at file level and referenced by bare name
// from a statement's Go reference position.
//
// Funcs are declare-anywhere and hoisted: a func is a leaf the graph calls
// rather than a node in it, so the backward-only declaration discipline that
// governs statements does not apply to one.
//
// The NAME is the only thing the parser reads out of a func. Body holds
// everything from the opening parenthesis through the matching close brace as
// one opaque span, and binding a bare-name reference to it is the analysis
// engine's work.
type FuncDecl struct {
	Name  Ident
	Body  GoSpan
	Start Position
	Stop  Position
}

// Pos returns the declaration's start.
func (d FuncDecl) Pos() Position { return d.Start }

// End returns the position just past the func body's closing brace.
func (d FuncDecl) End() Position { return d.Stop }

func (FuncDecl) isNode() {}
func (FuncDecl) isDecl() {}

// FlowDecl is one flow: its name, its optional signature, its flow-level
// declarations and its body of statements.
//
// A FLOW BODY IS BRACELESS. It runs from the flow line to the next `flow` or
// `func` declaration, or to end of file, and End reports where that body ends.
type FlowDecl struct {
	Name      Ident
	Signature *FlowSignature
	Note      *NoteBlock
	State     *StateDecl
	Vars      []VarDecl
	OnError   *OnErrorDecl
	Body      []Stmt
	Start     Position
	Stop      Position
}

// Pos returns the flow declaration's start.
func (d FlowDecl) Pos() Position { return d.Start }

// End returns the position just past the flow's braceless body.
func (d FlowDecl) End() Position { return d.Stop }

func (FlowDecl) isNode() {}
func (FlowDecl) isDecl() {}

// FlowSignature is a flow's declared input type and its named outputs.
//
// When a signature is present the body consumes an implicit `in`, and the
// analysis engine checks that every declared output is delivered.
type FlowSignature struct {
	Input   GoSpan
	Outputs []FlowOutput
	Start   Position
	Stop    Position
}

// Pos returns the signature's opening parenthesis.
func (s FlowSignature) Pos() Position { return s.Start }

// End returns the position just past the last output.
func (s FlowSignature) End() Position { return s.Stop }

func (FlowSignature) isNode() {}

// FlowOutput is one named output of a flow signature.
type FlowOutput struct {
	Name  Ident
	Type  GoSpan
	Start Position
	Stop  Position
}

// Pos returns the output name's start.
func (o FlowOutput) Pos() Position { return o.Start }

// End returns the position just past the output's type.
func (o FlowOutput) End() Position { return o.Stop }

func (FlowOutput) isNode() {}
