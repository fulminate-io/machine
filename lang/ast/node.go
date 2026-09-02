// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

// Node is any element of the syntax tree.
//
// The interface is SEALED by its unexported marker: no package outside this one
// can add a node type, so a consumer switching over the tree knows the set it is
// switching over is closed.
//
// Both ends of a node's span are exposed rather than only its start, because the
// editor contract is ranges and a start-only position cannot express one.
type Node interface {
	Pos() Position
	End() Position
	isNode()
}

// Decl is a file-level declaration: an import, a const, a param, a func or a
// flow.
type Decl interface {
	Node
	isDecl()
}

// Stmt is a flow-body statement: one of the ten shapes the language declares,
// or the BadStmt a recovered parse leaves in their place.
type Stmt interface {
	Node
	isStmt()
}

// Ident is a positioned name.
type Ident struct {
	Name    string
	NamePos Position
}

// Pos returns the identifier's start.
func (i Ident) Pos() Position { return i.NamePos }

// End returns the position just past the identifier.
func (i Ident) End() Position {
	return Position{
		Offset: i.NamePos.Offset + len(i.Name),
		Line:   i.NamePos.Line,
		Col:    i.NamePos.Col + len(i.Name),
	}
}

func (Ident) isNode() {}

// GoSpan is a verbatim fragment of Go source with its exact span.
//
// The parser captures the text and never interprets it. Whether a bare name in
// one resolves to a local func declaration and a qualified one to an import is
// the analysis engine's work, not the parser's.
type GoSpan struct {
	Text  string
	Start Position
	Stop  Position
}

// Pos returns the fragment's start.
func (g GoSpan) Pos() Position { return g.Start }

// End returns the position just past the fragment.
func (g GoSpan) End() Position { return g.Stop }

func (GoSpan) isNode() {}

// Clauses is the trailing clause bundle every node-bearing statement shape
// carries. The parser accepts the clauses in ANY ORDER and each of them at most
// once.
//
// From is empty on a source, which has no inputs to name.
//
// Checkpoint carries the codec the clause names, as an opaque Go span — the same
// shape Over carries its transport factory in, and for the same reason: the
// operand is Go text this package does not interpret. The span carries its own
// Start and Stop, so holding the operand costs nothing an editor or the analysis
// engine needs. Nil means absent, and a NON-NIL Checkpoint always has NON-EMPTY
// Text, because the parser refuses an empty operand.
//
// It was a bare position until the clause grew that operand. The operand is
// REQUIRED rather than optional because a checkpoint the runtime can journal
// needs a codec, and no default is picked on the author's behalf.
//
// Idempotent is still a position, for the reason Checkpoint no longer has: it
// takes no operand at all, so its presence is the whole payload and a bool would
// throw away the one thing a consumer needs, which is where the clause sits.
//
// What the clauses MEAN, which the parser does not act on: Idempotent SELECTS THE
// ANCHOR. The recovery ledger records an UNMARKED node's COMPLETION with the
// datum's envelope, after its function returns, and resume re-injects that record
// into the successors WITHOUT re-running the node. A MARKED node is recorded at its
// ARRIVAL instead, before its function runs, and resume re-runs the node — which is
// safe precisely because the marker declares it so. Implicit checkpoints at flow
// ingress and remote-edge boundaries exist regardless.
//
// There is deliberately NO Clone field here. `clone` is a VAR-DECLARATION
// clause, not a statement clause: cloning is a per-traversal concern, and a var
// is what is fresh per datum and copied per tee branch.
type Clauses struct {
	From       []Ident
	Reads      []Ident
	Writes     []Ident
	Over       *GoSpan
	Checkpoint *GoSpan
	Idempotent *Position
	OnError    *GoSpan
	Note       *NoteBlock
}
