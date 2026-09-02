// Package orders is the inference fixtures' real subject package.
//
// ITS PACKAGE CLAUSE IS DELIBERATELY NOT ITS LAST PATH SEGMENT. The import path
// ends in v2, and the package is named orders — the same shape the real corpus
// carries in github.com/stripe/stripe-go/v82, which is named stripe. A consumer
// that binds an import qualifier by guessing the last path segment resolves
// nothing here, so the environment's name recovery through the loader's scope is
// actually exercised rather than accidentally agreeing with a guess.
package orders

// Order is what a flow ingests.
type Order struct {
	ID string
}

// Scored is what scoring an Order yields.
type Scored struct {
	Order Order
	Risk  float64
}

// Receipt is what storing a Scored yields.
type Receipt struct {
	ID string
}

// Listen is the generic instantiation shape: a flow names it as
// Listen[Order](":8080"), which is a CALL that has already been applied, so the
// reference's type is the value it yields rather than a signature.
func Listen[T any](addr string) T {
	var zero T

	_ = addr

	return zero
}

// Score is the ordinary two-result shape, whose second result is an error and is
// therefore not what the next node receives.
func Score(o Order) (Scored, error) { return Scored{Order: o}, nil }

// Clean is a PREDICATE. A branch routes on it without converting the datum, so a
// branch target carries the branch's input type and never this bool.
func Clean(s Scored) bool { return s.Risk < 1 }

// Store is a sink reference.
func Store(s Scored) Receipt { return Receipt{ID: s.Order.ID} }

// Quarantine is the other side of a branch.
func Quarantine(s Scored) Receipt { return Receipt{ID: s.Order.ID} }

// Tag exists so a DISAGREEING fan-in is constructible from real types: it turns a
// Receipt back into an Order, so two edges reaching one node genuinely carry
// different type identities rather than differing only in spelling.
func Tag(r Receipt) Order { return Order{ID: r.ID} }
