// Package subject's REGISTRATION fixtures: one named wrapper per shape class
// that a state entry can take.
//
// THE WRAPPER IS A CARRIER, NOT THE SUBJECT. A bare state entry reaches the codec
// as its UNDERLYING shape — `by_type map[string]int` is a map in the interface
// slot, not a named type — so the test resolves the wrapper and walks its
// Underlying. The wrapper exists only because a fixture module has to give the
// shape a name to resolve it by. The one exception is the named row, which walks
// the named type itself because that is the arm that already worked and is kept
// as a regression control.
package subject

// ByType is the corpus's most common state shape: `by_type map[string]int`
// appears in payments.flow, declarations.flow and declare-after-use-loop.flow.
type ByType map[string]int

// Seen is the corpus's other map shape, from clauses-any-order.flow.
type Seen map[string]bool

// IntArray is the row most likely to be reasoned wrong from the slice
// behavior: gob carries []int through an interface slot unregistered but
// REFUSES [3]int, an array of the identical element.
type IntArray [3]int

// NestedSlice is a slice whose element is not basic, which gob refuses even
// though a slice of a basic element is carried.
type NestedSlice [][]int

// PlainSlice is a slice of a named element.
type PlainSlice []Plain

// IntSlice is BOOTSTRAPPED: gob carries a slice of a basic element through an
// interface slot with no registration at all.
type IntSlice []int

// Counter is BOOTSTRAPPED: a bare basic type needs nothing.
type Counter int

// Signal is ALREADY REFUSED for a different and stronger reason — the walk
// reports it as a silent drop, so the registration arm must add nothing. A
// second reason to refuse the same type is noise for whoever composes the
// diagnostic.
type Signal chan int
