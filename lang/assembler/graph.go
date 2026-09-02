// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import "github.com/whitaker-io/machine/lang/ast"

// NodeKind is the closed set of statement shapes a flow body can hold, one
// member per production the grammar admits inside a flow.
//
// IT IS CLOSED ON PURPOSE. Every consumer that switches on a Kind carries a
// default arm that REFUSES, so a shape added to the grammar without a lowering
// is a loud positioned diagnostic rather than a statement that quietly emits
// nothing. A generator that silently skips what it does not understand produces
// a program the author believes contains something it does not.
type NodeKind int

// The kinds, one per statement shape in the grammar's flow body.
const (
	// KindInvalid is the zero value and names no shape. It exists so a Node
	// built by an incomplete walk is distinguishable from a legitimate Source,
	// which would otherwise share the zero value and pass unnoticed.
	KindInvalid NodeKind = iota
	KindSource
	KindTransform
	KindBranch
	KindSwitch
	KindTee
	KindSink
	KindDrop
	KindLoop
	KindSend
	KindUse
)

// String renders the kind for a diagnostic. An unnamed kind renders its number
// rather than an empty string, because a diagnostic naming nothing is worse than
// one naming a number a reader can look up.
func (k NodeKind) String() string {
	if name, ok := kindNames[k]; ok {
		return name
	}

	return "NodeKind(" + itoa(int(k)) + ")"
}

// kindNames spells every member once. A table rather than a switch because the
// set is closed and a lookup cannot grow a branch per member.
var kindNames = map[NodeKind]string{
	KindInvalid:   "invalid",
	KindSource:    "source",
	KindTransform: "transform",
	KindBranch:    "branch",
	KindSwitch:    "switch",
	KindTee:       "tee",
	KindSink:      "sink",
	KindDrop:      "drop",
	KindLoop:      "loop",
	KindSend:      "send",
	KindUse:       "use",
}

// itoa renders a small non-negative int without pulling in strconv for one call
// on a path that only runs when a kind is out of range.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return sign + string(digits)
}

// Node is one statement of a flow, resolved into the graph.
//
// THE NAMES ARE THE EDGES. A flow declares no edges of its own: a statement
// names the outputs it consumes, and the graph is derived by matching those
// names against the outputs earlier statements produce. Inputs and Outputs
// therefore hold NAMES rather than pointers, and the Edge set is derived from
// them rather than stored twice.
//
// Stmt is the ast statement this node came from, kept BY VALUE because lang/ast
// stores statements by value and the pointer forms do not exist. It is retained
// so lowering can reach the shape-specific fields (a switch's arms, a branch's
// two targets) without the graph having to mirror every one of them.
type Node struct {
	// Name is the statement's declared node name, unique within the flow.
	Name string
	Kind NodeKind
	// Stmt is the ast statement, by value.
	Stmt ast.Stmt
	// Inputs are the output names this node consumes, in the order the source
	// wrote them. ORDER MATTERS for exactly one reason: the FIRST name is the
	// declaring input, the one whose type the node's own boundary takes, and the
	// rest are merge inputs.
	Inputs []string
	// Outputs are the names this node produces. Most shapes produce one; a
	// branch produces two, a switch and a tee produce one per target.
	Outputs []string
	// Clauses is the statement's trailing clause bundle, carried whole so the
	// lowering's clause dispatch reads one surface rather than per-shape copies.
	Clauses ast.Clauses
	// Start and Stop span the statement in the .flow source, so a refusal raised
	// against this node is positioned without re-walking the AST.
	Start ast.Position
	Stop  ast.Position
}

// Edge is one derived connection: an output name a producer declared, and the
// node that consumes it.
//
// It is DERIVED from the nodes' name lists rather than authored, which is why it
// carries the name as well as both ends. A diagnostic about an edge has to name
// the name the author actually wrote, not the pair of nodes the tooling matched.
type Edge struct {
	// Output is the produced name that links the two, as written in the source.
	Output string
	// From is the name of the node that produces Output.
	From string
	// To is the name of the node that consumes it.
	To string
}

// Program is one flow, resolved.
//
// It is the whole of what Phase 1 produces and the whole of what lowering
// consumes. It carries NO Go type information at all: every edge here was
// derived from names the AST already held, which is what lets the graph be built
// and validated before any package is loaded.
type Program struct {
	// Name is the flow's declared name.
	Name string
	// Note is the flow-level note block, or nil.
	Note *ast.NoteBlock
	// Signature is the flow's declared header, or nil when it has none. A flow
	// with no header declares no output names, and its bindable boundary is
	// derived above this module.
	Signature *ast.FlowSignature
	// State are the declared state fields, in source order.
	State []ast.StateField
	// Vars are the declared vars, in source order.
	Vars []ast.VarDecl
	// OnError is the flow-level error handler, or nil.
	OnError *ast.OnErrorDecl
	// Nodes are the flow's statements in SOURCE order. Emission order is
	// computed separately and is not this order, because the runtime requires a
	// Send's target to already have a consumer.
	Nodes []Node
	// Edges are the derived connections between them.
	Edges []Edge
	// InputTypes maps a node name to the Go type spelling of its input, supplied
	// by the DRIVER rather than derived here.
	//
	// The generator needs it in exactly two places and derives it in neither: the
	// explicit type argument on an idempotent marker, and the successor type a
	// completion-anchored checkpoint's codec family is re-instantiated at. Both
	// come from cross-module type inference that lives above this package, so
	// this map is how that fact arrives. An absent entry is reported, never
	// guessed.
	InputTypes map[string]string
	// Start and Stop span the flow declaration.
	Start ast.Position
	Stop  ast.Position
}

// Generated is one emitted Go file: the name to write it under and its bytes.
//
// It is declared here rather than beside the emitter because Error carries a
// PARTIAL slice of it — a run that refuses partway still hands back what it
// built, and the error type cannot reference a type declared later.
type Generated struct {
	// Name is the file name, `<flow-file basename>.flow.go`.
	Name string
	// Source is the emitted Go.
	Source []byte
}
