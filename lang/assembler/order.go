// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import "strings"

// ordered returns the plan's ops in an order the runtime accepts.
//
// THE CONSTRAINT IS THE RUNTIME'S, NOT AN INVENTION. Flow.Send's own doc states
// it: the target must already have a consumer, so the node being re-entered is
// declared BEFORE the Send that closes the loop, and a target with no consumer
// yet is a declaration error reported from Start. A generator emitting Sends in
// SOURCE order therefore produces a program that COMPILES and then fails at
// Start — a defect `go build` cannot see, which is why the ordering is a pass
// rather than a convention.
//
// Everything that is not a Send keeps its relative order; Sends are moved to the
// earliest point at which their target has been declared. A Send whose target is
// never declared is refused with the participating names, rather than emitted
// into a plan that would fail Start.
func (l *lowering) ordered() []Op {
	declared := map[string]bool{}
	var (
		out     []Op
		pending []Op
	)
	for _, op := range l.plan.Ops {
		if op.Method == MethodSend {
			pending = append(pending, op)

			continue
		}
		declared[op.Node] = true
		out = append(out, op)
		out, pending = drainSatisfied(out, pending, declared)
	}
	out, pending = drainSatisfied(out, pending, declared)
	l.refuseUnsatisfiedSends(pending)

	return append(out, pending...)
}

// drainSatisfied appends every pending Send whose target is now declared.
//
// It loops because appending one Send can never satisfy another, but draining in
// one pass would leave a Send behind whose target was declared earlier in the
// same sweep.
func drainSatisfied(out, pending []Op, declared map[string]bool) (placed, waiting []Op) {
	for {
		moved := false
		remaining := pending[:0:0]
		for _, send := range pending {
			if declared[sendTargetNode(send)] {
				out = append(out, send)
				moved = true

				continue
			}
			remaining = append(remaining, send)
		}
		pending = remaining
		if !moved {
			return out, pending
		}
	}
}

// refuseUnsatisfiedSends reports the sends whose targets were never declared.
//
// The diagnostic NAMES THE PARTICIPANTS. A cycle of sends with no declaring input
// admits no order at all, and an author told only that ordering failed cannot see
// which nodes are involved.
func (l *lowering) refuseUnsatisfiedSends(pending []Op) {
	if len(pending) == 0 {
		return
	}
	participants := make([]string, 0, len(pending))
	for _, send := range pending {
		participants = append(participants, send.Node+" -> "+sendTargetNode(send))
	}
	l.diagf(pending[0].Start, pending[0].Stop,
		"no emission order satisfies the runtime's send rule: the target of a send must already have a consumer, "+
			"and these sends target nodes that are never declared: %s", strings.Join(participants, "; "))
}

// sendTargetNode reads the flow node a send op must follow.
//
// It is the node being RE-ENTERED, recorded on the op at lowering time rather
// than recovered from the call's arguments — the emitted call takes the flow that
// PRECEDES that node, so the argument alone does not name it.
func sendTargetNode(op Op) string {
	return op.After
}
