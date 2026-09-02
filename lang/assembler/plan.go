// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import "github.com/whitaker-io/machine/lang/ast"

// The runtime builder methods, and the whole of what the emitter may call.
//
// THE SET IS CLOSED AT SEVEN and it is the runtime's, not this package's. A
// generator that emitted a method the runtime does not export would produce Go
// that does not compile; one that never learned about a method the runtime added
// would silently keep lowering a shape the worse way. Both directions are gated
// by deriving this set from machine/flow.go and machine/machine.go at test time
// rather than by trusting this list.
const (
	MethodSource = "Source"
	MethodMap    = "Map"
	MethodIf     = "If"
	MethodTee    = "Tee"
	MethodSend   = "Send"
	MethodDrop   = "Drop"
	MethodOutput = "Output"
)

// builderMethods is the vocabulary, declared once.
var builderMethods = []string{
	MethodSource,
	MethodMap,
	MethodIf,
	MethodTee,
	MethodSend,
	MethodDrop,
	MethodOutput,
}

// Op is one call in an emitted wiring function.
//
// IT IS A CALL, NOT A NODE. A flow node can lower to more than one op — a sink
// becomes a Map followed by a Drop, an N-arm switch becomes N-1 chained Ifs — so
// the plan is a call sequence and Node names which flow statement each call came
// from.
type Op struct {
	// Method is the runtime method to call, always a member of builderMethods.
	Method string
	// Receiver is the Go variable the call is made on: the machine variable for
	// Source, and the flow variable a previous op bound for everything else.
	Receiver string
	// Results are the Go variable names this call's results bind to, in return
	// order. Source binds two, If and Tee bind two, Map binds one, and Send and
	// Drop bind none.
	Results []string
	// Node is the flow node name this op declares, as the runtime will see it.
	// Derived names carry the unforgeable separator.
	Node string
	// Ref is the Go reference the statement named, pasted verbatim. It is opaque
	// text this package never interprets.
	Ref string
	// Options are the emitted NodeOption expressions, verbatim and in emission
	// order.
	Options []string
	// TypeArg is an explicit Go type argument the call needs, empty when the
	// compiler can infer one.
	//
	// Only Machine.Source needs one: its payload type appears in its RETURN and
	// in no argument, so nothing is available to infer from. Every other builder
	// call takes the node function, whose signature names the type.
	TypeArg string
	// After names the flow node that must be declared BEFORE this op, and is set
	// only on a Send.
	//
	// Send merges into the same downstream consumer its target already feeds, so
	// the node being RE-ENTERED must already have one. That is the runtime's rule
	// and its violation compiles cleanly and fails at Start, so the ordering pass
	// reads this field rather than guessing from the call's arguments.
	After string
	// Start and Stop span the .flow statement this op came from, so a failure
	// later in emission is still positioned against source the author wrote.
	Start ast.Position
	Stop  ast.Position
}

// Config carries what a caller decides rather than what a .flow source states.
type Config struct {
	// Package is the generated file's package clause.
	Package string
	// Qualifier prefixes every process-global state handle name, so two programs
	// in one process cannot collide in the runtime's single handle namespace.
	Qualifier string
}

// HandleKind distinguishes the two state shapes the runtime declares.
type HandleKind int

const (
	// HandleKey is a stack handle, declared from a flow's `var`.
	HandleKey HandleKind = iota
	// HandleCell is a heap handle, declared from a flow's `state` block.
	HandleCell
)

// Handle is one state handle a generated file declares.
//
// THE NAME IS PROCESS-GLOBAL. The runtime keeps keys and cells in ONE namespace
// shared across every machine in the process and panics on a duplicate, so the
// name carries the qualifier and the flow, and an inlined subflow's handles carry
// the instance as well — which is what gives two instances of one subflow
// independent state rather than a collision at declaration.
type Handle struct {
	// Var is the Go variable the generated file binds the handle to.
	Var string
	// Name is the process-global handle name.
	Name string
	// Kind selects NewKey or NewCell.
	Kind HandleKind
	// Type is the declared Go type spelling, pasted verbatim.
	Type string
}

// Boundary is the bindable-output fact lang/analysis exports for one flow.
//
// IT IS CONSUMED, NEVER RE-DERIVED. lang/analysis is the single owner of "which
// outputs does this flow expose"; this package resolves a use statement's
// identifiers against that fact and refuses loudly when its own graph disagrees.
// Two implementations of one ruled semantic drift apart invisibly, changing no
// build result and no runtime behavior, which is why there is only one.
type Boundary struct {
	// Outputs are the names a consumer may bind, as analysis exported them.
	Outputs []string
}

// Plan is the ordered call sequence for one flow.
//
// ORDER IS THE PRODUCT. The runtime requires a Send's target to already have a
// consumer, so the sequence here is not the source's statement order; it is the
// order computed to satisfy that constraint.
type Plan struct {
	// Flow is the flow's declared name.
	Flow string
	// Ops are the calls, in emission order.
	Ops []Op
	// Handles are the state handles this flow declares, including those an
	// inlined subflow contributes under its instance name.
	Handles []Handle
	// OnError is the flow-level error handler's Go expression, empty when the
	// flow declares none. It is EXPOSED for the host rather than installed by
	// Wire, because a machine's error handler is a machine.New option.
	OnError string
}
