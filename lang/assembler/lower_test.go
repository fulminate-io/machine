// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"slices"
	"strings"
	"testing"
)

// planOf builds and lowers a source, failing on any diagnostic.
func planOf(t *testing.T, src string, inputTypes map[string]string) *Plan {
	t.Helper()
	program := graphOf(t, src)
	program.InputTypes = inputTypes
	plan, diags := lower(program)
	if len(diags) != 0 {
		t.Fatalf("lowering refused:\n%s", strings.Join(messagesOf(diags), "\n"))
	}

	return plan
}

// opsFor lists the methods called for one flow node, in plan order.
func opsFor(plan *Plan, node string) []string {
	var out []string
	for _, op := range plan.Ops {
		if op.Node == node || strings.HasPrefix(op.Node, node+derivedSep) {
			out = append(out, op.Method)
		}
	}

	return out
}

// TestLowerCoversEveryStatementShape proves all ten shapes lower,
// that every op names a method from the runtime's closed vocabulary, and that a
// shape with no lowering REDS rather than being skipped.
func TestLowerCoversEveryStatementShape(t *testing.T) {
	plan := planOf(t, allShapesFixture, map[string]string{
		"in": "Order", "t": "Order", "b": "Order", "split": "Order",
		"route": "Order", "out": "Order", "arm": "Order",
	})

	// EVERY OP DRAWS FROM THE CLOSED SET. An op naming anything else is Go that
	// will not compile against the runtime.
	for _, op := range plan.Ops {
		if !slices.Contains(builderMethods, op.Method) {
			t.Errorf("the plan calls %q, which is not in the runtime's builder vocabulary %v",
				op.Method, builderMethods)
		}
		if op.Node == "" {
			t.Errorf("an op names no flow node: %+v", op)
		}
		if op.Start.Line == 0 {
			t.Errorf("op %s:%s carries no source position, so a later failure could not be positioned",
				op.Method, op.Node)
		}
	}

	// EACH SHAPE LOWERS TO THE METHOD THE TABLE NAMES.
	for node, want := range map[string][]string{
		"in":                          {MethodSource},
		"t":                           {MethodMap},
		"b":                           {MethodIf},
		"split":                       {MethodTee},
		"route":                       {MethodIf},
		"out":                         {MethodMap, MethodDrop},
		"right" + derivedSep + "drop": {MethodDrop},
		"no" + derivedSep + "send":    {MethodSend},
	} {
		if got := opsFor(plan, node); !slices.Equal(got, want) {
			t.Errorf("%s lowers to %v, want %v", node, got, want)
		}
	}

	// A LOOP DECLARES NOTHING. It is a label, and an op emitted for it would
	// declare a node the runtime never asked for.
	for _, op := range plan.Ops {
		if op.Node == "again" {
			t.Errorf("the loop label produced an op: %+v", op)
		}
	}
}

// allShapesFixture writes every statement shape the grammar admits inside a flow.
const allShapesFixture = "flow shapes\n" +
	"source in Poll\n" +
	"loop again\n" +
	"transform t Foo from in, again\n" +
	"branch b Pred from t -> yes, no\n" +
	"tee split from yes -> left, right\n" +
	"switch route from left on left.Kind {\n  \"a\" -> arm\n}\n" +
	"sink out Store from arm\n" +
	"drop right\n" +
	"send no -> again\n"

// TestAShapeWithNoLoweringRedsRatherThanBeingSkipped drives the dispatch's
// mandatory default arm.
//
// The statement set is closed today, so this reaches the arm directly rather than
// through source: a node carrying a kind past the enumeration is exactly what a
// new grammar shape would produce before anyone taught the lowering about it, and
// the requirement is that it REFUSES rather than emitting nothing.
func TestAShapeWithNoLoweringRedsRatherThanBeingSkipped(t *testing.T) {
	program := graphOf(t, "flow f\nsource in Poll\nsink out Store from in\n")
	program.Nodes = append(program.Nodes, Node{
		Name: "future", Kind: NodeKind(99),
		Start: program.Nodes[0].Start, Stop: program.Nodes[0].Stop,
	})

	_, diags := lower(program)
	if len(diags) == 0 {
		t.Fatal("a statement shape with no lowering was skipped in silence")
	}
	joined := strings.Join(messagesOf(diags), "\n")
	if !strings.Contains(joined, "no lowering") {
		t.Errorf("the diagnostics %q do not say the shape has no lowering", joined)
	}
	// The known positive: the same program without the unknown kind lowers clean.
	clean := graphOf(t, "flow f\nsource in Poll\nsink out Store from in\n")
	if _, diags := lower(clean); len(diags) != 0 {
		t.Fatalf("the control program refused: %v", messagesOf(diags))
	}
}

// TestSwitchAndTeeLowerToChains proves the two chaining lowerings and the
// derived-name rule they depend on.
//
// N ARMS BECOME N CHAINED Ifs and N targets become N-1 chained Tees, each link
// taking the previous link's FALSE branch so first-match-wins survives. The names
// carry a separator no flow identifier can contain, which is what makes a derived
// name uncollidable with one an author wrote.
func TestSwitchAndTeeLowerToChains(t *testing.T) {
	t.Run("an N-arm switch chains N Ifs", func(t *testing.T) {
		plan := planOf(t, "flow routing\n"+
			"source in Poll\n"+
			"switch route from in on in.Kind {\n"+
			"  \"a\" -> first\n  \"b\" -> second\n  \"c\" -> third\n"+
			"  else -> rest\n}\n"+
			"sink s1 Store from first\nsink s2 Store from second\n"+
			"sink s3 Store from third\nsink s4 Store from rest\n",
			map[string]string{})

		names := chainNames(plan, MethodIf)
		want := []string{"route", "route" + derivedSep + "1", "route" + derivedSep + "2"}
		if !slices.Equal(names, want) {
			t.Errorf("a 3-arm switch chained %v, want %v", names, want)
		}
	})

	t.Run("an N-target tee chains N-1 Tees", func(t *testing.T) {
		plan := planOf(t, "flow fan\n"+
			"source in Poll\n"+
			"tee split from in -> a, b, c, d\n"+
			"sink s1 Store from a\nsink s2 Store from b\n"+
			"sink s3 Store from c\nsink s4 Store from d\n",
			map[string]string{})

		names := chainNames(plan, MethodTee)
		want := []string{"split", "split" + derivedSep + "1", "split" + derivedSep + "2"}
		if !slices.Equal(names, want) {
			t.Errorf("a 4-target tee chained %v, want %v", names, want)
		}
	})

	t.Run("a derived name cannot collide with a source-written one", func(t *testing.T) {
		// THE SEPARATOR IS THE WHOLE GUARANTEE, and it is checked against the
		// PARSER rather than asserted: a flow identifier carrying the separator
		// must not parse, so no author can write the name the generator derives.
		const collidingName = "route" + derivedSep + "1"
		src := "flow f\nsource in Poll\ntransform " + collidingName + " Foo from in\nsink out Store from " +
			collidingName + "\n"
		if _, err := parseFlowSource(src); err == nil {
			t.Fatalf("%q parsed as a flow identifier, so a derived name CAN collide with a source-written one",
				collidingName)
		}

		// The known positive: the same name without the separator parses fine, so
		// the refusal above is the separator's doing and not something else.
		clean := "flow f\nsource in Poll\ntransform route1 Foo from in\nsink out Store from route1\n"
		if _, err := parseFlowSource(clean); err != nil {
			t.Fatalf("the control source does not parse, so the assertion above proves nothing: %v", err)
		}
	})
}

// chainNames lists the node names of every op calling one method, in plan order.
func chainNames(plan *Plan, method string) []string {
	var out []string
	for _, op := range plan.Ops {
		if op.Method == method {
			out = append(out, op.Node)
		}
	}

	return out
}

// TestASinkLowersToAMapAndADrainDrop pins the shape with no direct method.
func TestASinkLowersToAMapAndADrainDrop(t *testing.T) {
	plan := planOf(t, "flow f\nsource in Poll\nsink out Store from in\n", map[string]string{})

	var mapOp, dropOp *Op
	for i := range plan.Ops {
		switch {
		case plan.Ops[i].Method == MethodMap && plan.Ops[i].Node == "out":
			mapOp = &plan.Ops[i]
		case plan.Ops[i].Method == MethodDrop:
			dropOp = &plan.Ops[i]
		}
	}
	if mapOp == nil {
		t.Fatal("the sink emitted no Map; there is no Sink method to call instead")
	}
	if dropOp == nil {
		t.Fatal("the sink emitted no Drop, so its produced flow is never consumed")
	}
	if want := "out" + derivedSep + "drain"; dropOp.Node != want {
		t.Errorf("the drain is named %q, want %q", dropOp.Node, want)
	}
	if dropOp.Receiver != mapOp.Results[0] {
		t.Errorf("the drain drops %q, want the sink's own result %q", dropOp.Receiver, mapOp.Results[0])
	}
}
