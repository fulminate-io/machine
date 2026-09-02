// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

// The preamble helper names, reserved in every generated file.
//
// A .flow source declaring a verbatim func under either name is refused at
// generation time rather than producing Go that fails to compile against a
// redeclaration the author never wrote.
const (
	readsHelperName  = readsHelper
	writesHelperName = writesHelper
	// filterReadsHelper and filterWritesHelper are the branch-shaped overloads.
	// A branch's predicate is a machine.Filter rather than a
	// machine.Transformation, so it carries its type through a different
	// signature and needs its own helper under a distinct name.
	filterReadsHelper  = "flowReadsOfFilter"
	filterWritesHelper = "flowWritesOfFilter"
)

// preamble is the helper block written into every generated file.
//
// WHY THESE HELPERS EXIST AT ALL, and it is the discovery that shapes the whole
// emitter. machine.WithReads and machine.WithWrites take only KeyRefs, which
// carry no payload type, and Go does not infer a generic function's type argument
// from the parameter it is being passed into. The compiler says so directly:
//
//	in call to machine.WithReads, cannot infer T
//
// and WithReads' own doc comment states the same in words — the type parameter
// cannot be inferred from a KeyRef list, so a call site writes it.
//
// A generator taking that at face value would have to SPELL every node's Go input
// type at every reads or writes clause, which makes the commonest clause in the
// language depend on full type resolution. The avoidance is to pass the NODE
// FUNCTION as an ignored first parameter: the function's own signature names the
// type, so Go infers it and the generator spells nothing.
//
// A node's error handler needs no helper, because machine.WithErrorHandler infers
// its type from the handler's own signature.
const preamble = `// flowReadsOf declares a node's read capabilities, carrying the node's input
// type through the node function's own signature.
//
// The function is IGNORED. It is a parameter only so Go can infer U: WithReads
// takes KeyRefs, which name no type, and a generic call's type argument is not
// inferred from the parameter it is passed into.
func ` + readsHelperName + `[U, V any](_ machine.Transformation[U, V], refs ...machine.KeyRef) machine.NodeOption[U] {
	return machine.WithReads[U](refs...)
}

// flowWritesOf declares a node's write capabilities, on the same terms.
//
// A write capability does NOT imply a read capability: a node that reads,
// modifies and writes one handle declares it in both.
func ` + writesHelperName + `[U, V any](_ machine.Transformation[U, V], refs ...machine.KeyRef) machine.NodeOption[U] {
	return machine.WithWrites[U](refs...)
}

// flowReadsOfFilter is flowReadsOf for a branch, whose predicate is a Filter
// rather than a Transformation and so carries its type through a different
// signature.
func ` + filterReadsHelper + `[U any](_ machine.Filter[U], refs ...machine.KeyRef) machine.NodeOption[U] {
	return machine.WithReads[U](refs...)
}

// flowWritesOfFilter is flowWritesOf for a branch.
func ` + filterWritesHelper + `[U any](_ machine.Filter[U], refs ...machine.KeyRef) machine.NodeOption[U] {
	return machine.WithWrites[U](refs...)
}
`
