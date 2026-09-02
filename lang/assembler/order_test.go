// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
)

// TestEmissionOrderSatisfiesTheSendConstraint proves the pass produces an order
// the runtime accepts, and refuses when none exists.
//
// THE CONSTRAINT IS THE RUNTIME'S AND ITS VIOLATION IS INVISIBLE TO go build.
// Flow.Send's own doc states it: the target must already have a consumer, so the
// node being re-entered is declared BEFORE the Send that closes the loop, and a
// target with no consumer yet is a declaration error reported from Start. A
// generator emitting Sends in SOURCE order produces a program that compiles and
// then fails at Start, which no compile-time gate can catch.
func TestEmissionOrderSatisfiesTheSendConstraint(t *testing.T) {
	t.Run("a send is emitted after the node it re-enters", func(t *testing.T) {
		// THE SOURCE ORDER IS DELIBERATELY WRONG. `send` is written before the
		// statement that consumes the loop label, so a pass that preserved source
		// order would emit the Send first and the program would fail at Start.
		plan := planOf(t, "flow looped\n"+
			"source ingest Poll\n"+
			"loop retry\n"+
			"transform backoff Wait from ingest\n"+
			"send backoff -> retry\n"+
			"transform enrich Lookup from ingest, retry\n"+
			"sink done Store from enrich\n",
			map[string]string{"ingest": "Order", "backoff": "Order", "enrich": "Order", "done": "Order"})

		sendAt, reenteredAt := -1, -1
		for i, op := range plan.Ops {
			switch {
			case op.Method == MethodSend:
				sendAt = i
				if op.After != "enrich" {
					t.Errorf("the send re-enters %q, want enrich", op.After)
				}
			case op.Node == "enrich":
				reenteredAt = i
			}
		}
		if sendAt < 0 {
			t.Fatalf("the plan emits no Send: %v", planNodes(plan))
		}
		if reenteredAt < 0 {
			t.Fatalf("the plan never declares the re-entered node: %v", planNodes(plan))
		}
		if sendAt < reenteredAt {
			t.Errorf("the Send is emitted at %d, before the node it re-enters at %d; "+
				"this compiles and fails at Start", sendAt, reenteredAt)
		}
	})

	t.Run("everything else keeps its relative order", func(t *testing.T) {
		// The pass moves SENDS, and nothing else. A pass that reordered node
		// declarations would break the receiver chain each call depends on.
		plan := planOf(t, allShapesFixture, allShapesTypes)
		var nodes []string
		for _, op := range plan.Ops {
			if op.Method != MethodSend {
				nodes = append(nodes, op.Node)
			}
		}
		want := []string{"in", "t", "b", "split", "route", "out", "out" + derivedSep + "drain",
			"right" + derivedSep + "drop"}
		if strings.Join(nodes, ",") != strings.Join(want, ",") {
			t.Errorf("the non-send order is %v, want %v", nodes, want)
		}
	})

	t.Run("a graph admitting no order is refused, naming the participants", func(t *testing.T) {
		// A Send whose target is never declared admits no order at all. It must
		// be refused rather than emitted into a plan that would fail Start.
		program := graphOf(t, "flow f\nsource in Poll\nsink out Store from in\n")
		program.InputTypes = map[string]string{}
		program.Nodes = append(program.Nodes, Node{
			Name: "orphan" + derivedSep + "send", Kind: KindSend,
			Stmt:  ast.SendStmt{Source: ast.Ident{Name: "in"}, Target: ast.Ident{Name: "in"}},
			Start: program.Nodes[0].Start, Stop: program.Nodes[0].Stop,
		})

		l := newLowering(program, Config{})
		l.plan.Ops = []Op{
			{Method: MethodSource, Node: "in", Results: []string{"in"}},
			{Method: MethodSend, Node: "orphan" + derivedSep + "send", After: "never-declared",
				Start: program.Nodes[0].Start, Stop: program.Nodes[0].Stop},
		}
		ordered := l.ordered()

		if len(l.diags) == 0 {
			t.Fatal("a send whose target is never declared was emitted without a word")
		}
		message := l.diags[0].Message
		for _, want := range []string{"no emission order", "never-declared", "orphan"} {
			if !strings.Contains(message, want) {
				t.Errorf("the refusal %q does not name %q; an author cannot see which nodes are involved", message, want)
			}
		}
		if l.diags[0].Pos.Line == 0 {
			t.Errorf("the ordering refusal is unpositioned: %q", message)
		}
		// The unsatisfiable send is still carried, so a caller inspecting the
		// partial plan sees what could not be placed rather than a silent gap.
		if len(ordered) != 2 {
			t.Errorf("the refused plan holds %d ops, want both carried through", len(ordered))
		}
	})

	// THE KNOWN POSITIVE for the refusal above: the same pass over a satisfiable
	// graph reports nothing. Without it, a pass that refused every send would pass
	// the refusal subtest.
	t.Run("a satisfiable graph is not refused", func(t *testing.T) {
		program := graphOf(t, "flow ok\n"+
			"source ingest Poll\nloop retry\n"+
			"transform enrich Lookup from ingest, retry\n"+
			"transform backoff Wait from enrich\n"+
			"send backoff -> retry\n"+
			"sink done Store from enrich\n")
		program.InputTypes = map[string]string{
			"ingest": "Order", "enrich": "Order", "backoff": "Order", "done": "Order",
		}
		if _, diags := lower(program); len(diags) != 0 {
			t.Fatalf("a satisfiable graph was refused:\n%s", strings.Join(messagesOf(diags), "\n"))
		}
	})
}

// TestTheSendArgumentIsTheFlowThatPrecedesTheReenteredNode pins the runtime
// semantic the ordering pass exists to serve.
//
// Send merges into the SAME downstream consumer its target already feeds, so
// closing a cycle means passing the flow that PRECEDES the node to re-enter, not
// the flow that node produces. Passing the wrong one routes the loop a node too
// far along, and it compiles either way.
func TestTheSendArgumentIsTheFlowThatPrecedesTheReenteredNode(t *testing.T) {
	plan := planOf(t, "flow looped\n"+
		"source ingest Poll\nloop retry\n"+
		"transform enrich Lookup from ingest, retry\n"+
		"transform backoff Wait from enrich\n"+
		"send backoff -> retry\n"+
		"sink done Store from enrich\n",
		map[string]string{"ingest": "Order", "enrich": "Order", "backoff": "Order", "done": "Order"})

	for _, op := range plan.Ops {
		if op.Method != MethodSend {
			continue
		}
		// enrich's declaring input is `ingest`, so the flow preceding enrich is
		// the source's. `enrich` itself would be the flow enrich PRODUCES.
		if op.Ref != varOf("ingest") {
			t.Errorf("the send passes %q, want the flow preceding the re-entered node, %q",
				op.Ref, varOf("ingest"))
		}
		if op.Ref == varOf("enrich") {
			t.Error("the send passes the flow the re-entered node PRODUCES, which routes the loop one node too far")
		}

		return
	}
	t.Fatal("the plan emits no Send")
}
