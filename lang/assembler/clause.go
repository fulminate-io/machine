// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"strings"

	"github.com/whitaker-io/machine/lang/ast"
)

// options lowers one statement's clause bundle into the option expressions the
// emitter writes.
//
// THE DISPATCH IS TOTAL. It walks the clause partition rather than the clauses
// present, so every member of ast.Clauses gets a disposition on every statement:
// lowered into an option, held with a positioned diagnostic, or refused by the
// mandatory default arm. The statement type switch has the same discipline; this
// is the separate surface whose lack of it let a clause go missing.
func (l *lowering) options(n Node) []string {
	var out []string
	for _, name := range clauseOrder {
		disposition, classified := clausePartition[name]
		switch {
		case !classified:
			l.diagf(n.Start, n.Stop,
				"the %q clause is in the AST but this generator classifies it neither lowered nor held", name)
		case l.held[name]:
			l.reportHeld(n, name)
		case disposition == clauseHeld:
			l.reportHeld(n, name)
		default:
			out = append(out, l.lowerClause(n, name)...)
		}
	}

	// Options this node received from ANOTHER node's clause. They are appended
	// last so a node's own clauses read first in the emitted call.
	return append(out, l.carried[n.Name]...)
}

// clauseOrder fixes the emission order of the option list.
//
// It is stable rather than map order so generated output is deterministic; the
// drift test over the golden fixtures depends on that.
var clauseOrder = []string{
	clauseFrom, clauseReads, clauseWrites, clauseOver,
	clauseOnError, clauseNote, clauseCheckpoint, clauseIdempotent,
}

// reportHeld emits the positioned not-yet-lowered diagnostic for a held clause.
//
// IT IS A DIAGNOSTIC AND NOT SILENCE. A held clause is one the grammar accepts
// and the generator cannot yet express; saying so is the whole difference
// between a feature that is not ready and a feature that was dropped.
func (l *lowering) reportHeld(n Node, clause string) {
	if !clausePresent(n.Clauses, clause) {
		return
	}
	l.diagf(n.Start, n.Stop, "the %q clause on %q is not yet lowered by this generator",
		strings.ToLower(clause), n.Name)
}

// lowerClause renders one lowered clause's option expressions.
func (l *lowering) lowerClause(n Node, clause string) []string {
	switch clause {
	case clauseFrom, clauseNote:
		// FROM IS AN EDGE, NOT AN OPTION: it was consumed building the graph and
		// is what chose this node's receiver. NOTE carries documentation and
		// emits nothing. Both are lowered — neither is dropped — and neither
		// produces an option expression.
		return nil
	case clauseReads:
		return capabilityOption(capabilityHelper(n, readsHelperName, filterReadsHelper), n, n.Clauses.Reads)
	case clauseWrites:
		return capabilityOption(capabilityHelper(n, writesHelperName, filterWritesHelper), n, n.Clauses.Writes)
	case clauseOver:
		return spanOption("machine.WithEdge", n.Clauses.Over)
	case clauseOnError:
		return spanOption("machine.WithErrorHandler", n.Clauses.OnError)
	case clauseCheckpoint:
		return spanOption("machine.WithCheckpoint", n.Clauses.Checkpoint)
	case clauseIdempotent:
		return l.idempotentOption(n)
	default:
		return nil
	}
}

// capabilityHelper picks the preamble helper whose signature matches the node's
// own function shape.
//
// A BRANCH'S PREDICATE IS A Filter, NOT A Transformation, and a switch lowers to
// a chain of the same. Passing a Filter to the Transformation-shaped helper does
// not compile, so the choice is a property of the node's kind rather than a
// stylistic one.
func capabilityHelper(n Node, transformation, filter string) string {
	switch n.Kind {
	case KindBranch, KindSwitch:
		return filter
	default:
		return transformation
	}
}

// capabilityOption renders a reads or writes clause through the emitted preamble
// helper.
//
// THE HELPER EXISTS FOR TYPE INFERENCE, and that is measured rather than
// assumed: machine.WithReads takes only KeyRefs, which carry no payload type, and
// Go does not infer a generic function's type argument from the parameter it is
// passed into. A direct call would not compile. The helper takes the node's own
// function as well, so the payload type is inferred from it.
func capabilityOption(helper string, n Node, refs []ast.Ident) []string {
	if len(refs) == 0 {
		return nil
	}
	args := make([]string, 0, len(refs)+1)
	args = append(args, refOf(n))
	for _, ref := range refs {
		args = append(args, ref.Name)
	}

	return []string{helper + "(" + strings.Join(args, ", ") + ")"}
}

// spanOption renders an option whose argument is a Go span pasted verbatim.
func spanOption(option string, span *ast.GoSpan) []string {
	if span == nil {
		return nil
	}

	return []string{option + "(" + span.Text + ")"}
}

// idempotentOption renders the arrival-anchor marker.
//
// IT NEEDS AN EXPLICIT TYPE ARGUMENT and that is measured, not stylistic:
// machine.WithIdempotent takes no arguments at all, so there is nothing for Go to
// infer from, and a bare call does not compile — observed as `cannot infer T`
// against the real runtime. The spelling comes from the driver-supplied input
// types; without one the generator says so rather than emitting a call that will
// not build.
func (l *lowering) idempotentOption(n Node) []string {
	if n.Clauses.Idempotent == nil {
		return nil
	}
	spelling, ok := l.program.InputTypes[n.Name]
	if !ok || strings.TrimSpace(spelling) == "" {
		l.diagf(n.Start, n.Stop,
			"the idempotent clause on %q needs that node's input type, which no type information supplies", n.Name)

		return nil
	}

	return []string{"machine.WithIdempotent[" + spelling + "]()"}
}

// successorCodecOptions renders the codec-only option carried onto the SUCCESSOR
// of a completion-anchored checkpoint node.
//
// WHY A CROSS-NODE EMISSION EXISTS AT ALL. A completion-anchored checkpoint
// journals what the node PRODUCED, and it reaches the codec for that through its
// outbound emitter, which carries the CONSUMER's codec. So the codec that
// marshals the record belongs to the successor. Without this the runtime refuses
// at Start, telling the author to checkpoint the successor or mark the node
// idempotent — both of which make the author pay for an implementation seam.
//
// A BRANCHING SUCCESSOR CARRIES IT ON BOTH OUTLETS, because a branching node's
// two emitters are bound separately and either can be the one that journals. A
// lowering covering only one branch fails at Start on the other.
func (l *lowering) successorCodecOptions(n Node) map[string][]string {
	if n.Clauses.Checkpoint == nil || n.Clauses.Idempotent != nil {
		// No checkpoint, or an ARRIVAL anchor — which marshals the node's own
		// input with the node's own codec and needs no successor codec at all.
		return nil
	}
	family, why := codecFamilyOf(n.Clauses.Checkpoint.Text)
	if why != "" {
		l.diagf(n.Clauses.Checkpoint.Start, n.Clauses.Checkpoint.Stop,
			"%s: %q cannot be re-instantiated at the successor's type; write %s",
			why, strings.TrimSpace(n.Clauses.Checkpoint.Text), admissibleCodecForms)

		return nil
	}

	return l.codecForEachSuccessor(n, family)
}

// codecForEachSuccessor instantiates the family at every successor's input type.
func (l *lowering) codecForEachSuccessor(n Node, family codecFamily) map[string][]string {
	out := map[string][]string{}
	for _, successor := range l.successorsOf(n) {
		spelling, ok := l.program.InputTypes[successor]
		if !ok || strings.TrimSpace(spelling) == "" {
			l.diagf(n.Start, n.Stop,
				"the checkpoint on %q needs the input type of its successor %q, which no type information supplies",
				n.Name, successor)

			continue
		}
		out[successor] = append(out[successor], "machine.WithCodec("+family.instantiate(spelling)+")")
	}

	return out
}

// successorsOf lists the nodes consuming anything this node produces.
func (l *lowering) successorsOf(n Node) []string {
	var out []string
	seen := map[string]bool{}
	for _, out2 := range n.Outputs {
		for _, e := range l.program.Edges {
			if e.Output == out2 && !seen[e.To] {
				seen[e.To] = true
				out = append(out, e.To)
			}
		}
	}

	return out
}

// clausePresent reports whether a clause is written on a statement.
//
// It is a dispatch over the partition's own member names rather than a
// reflective field read, so a clause added to the AST reaches the mandatory
// default arm below instead of being silently reported absent.
func clausePresent(c ast.Clauses, clause string) bool {
	switch clause {
	case clauseFrom:
		return len(c.From) > 0
	case clauseReads:
		return len(c.Reads) > 0
	case clauseWrites:
		return len(c.Writes) > 0
	case clauseOver:
		return c.Over != nil
	case clauseOnError:
		return c.OnError != nil
	case clauseNote:
		return c.Note != nil
	case clauseCheckpoint:
		return c.Checkpoint != nil
	case clauseIdempotent:
		return c.Idempotent != nil
	default:
		return false
	}
}
