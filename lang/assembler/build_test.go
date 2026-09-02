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

// graphOf parses a flow source and builds its graph, failing on any diagnostic.
func graphOf(t *testing.T, src string) *Program {
	t.Helper()
	program, diags := buildOf(t, src)
	if len(diags) != 0 {
		t.Fatalf("the fixture must build clean, got %d diagnostics: %v", len(diags), messagesOf(diags))
	}

	return program
}

// buildOf parses a flow source and builds its graph, returning both halves.
func buildOf(t *testing.T, src string) (*Program, []Diagnostic) {
	t.Helper()
	file, err := ast.Parse([]byte(src))
	if err != nil {
		t.Fatalf("the fixture must parse clean: %v", err)
	}
	flow, ok := file.Decls[0].(ast.FlowDecl)
	if !ok {
		t.Fatalf("declaration 0 is %T, want ast.FlowDecl", file.Decls[0])
	}

	return buildProgram(flow)
}

// messagesOf projects diagnostics to their messages.
func messagesOf(diags []Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Pos.String()+": "+d.Message)
	}

	return out
}

// edgesInto lists the producers feeding a node, in edge order.
func edgesInto(program *Program, node string) []string {
	var out []string
	for _, e := range program.Edges {
		if e.To == node {
			out = append(out, e.From)
		}
	}

	return out
}

// nodeNamed returns the node with a given name.
func nodeNamed(t *testing.T, program *Program, name string) Node {
	t.Helper()
	for _, n := range program.Nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("the graph carries no node named %q; it has %v", name, nodeNames(program))

	return Node{}
}

// nodeNames lists every node name in graph order.
func nodeNames(program *Program) []string {
	out := make([]string, 0, len(program.Nodes))
	for _, n := range program.Nodes {
		out = append(out, n.Name)
	}

	return out
}

// TestGraphResolvesFromSendLoopAndFanIn covers the four resolution rules on one
// graph, because they interact: a fan-in whose later members are a loop label and
// a send target is the shape a rule tested in isolation gets wrong.
func TestGraphResolvesFromSendLoopAndFanIn(t *testing.T) {
	const src = "flow payments\n" +
		"source ingest Poll\n" +
		"loop retry\n" +
		"transform enrich Lookup from ingest, retry\n" +
		"branch settle Succeeded from enrich -> done, flaky\n" +
		"transform backoff Wait from flaky\n" +
		"send backoff -> retry\n" +
		"sink store Insert from done\n"

	program := graphOf(t, src)

	t.Run("plain from-reference", func(t *testing.T) {
		if got := edgesInto(program, "settle"); len(got) != 1 || got[0] != "enrich" {
			t.Errorf("settle is fed by %v, want [enrich]", got)
		}
	})

	t.Run("multi-from fan-in keeps source order", func(t *testing.T) {
		// THE ORDER IS THE RULE, not an artifact. The FIRST name is the
		// declaring input the node is constructed from; every later one is a
		// merge routed in by a Send. A resolver that sorted or set-ified these
		// would silently swap which one constructs the node.
		enrich := nodeNamed(t, program, "enrich")
		if len(enrich.Inputs) != 2 || enrich.Inputs[0] != "ingest" || enrich.Inputs[1] != "retry" {
			t.Fatalf("enrich consumes %v, want [ingest retry] in that order", enrich.Inputs)
		}
		if got := edgesInto(program, "enrich"); len(got) != 2 || got[0] != "ingest" || got[1] != "retry" {
			t.Errorf("enrich's inbound edges are %v, want [ingest retry] in that order", got)
		}
	})

	t.Run("loop label resolves without a producing statement", func(t *testing.T) {
		// `retry` is published by no statement: it is a label. It must still
		// resolve, and it must NOT be reported as an unknown name.
		if got := edgesInto(program, "enrich"); len(got) < 2 || got[1] != "retry" {
			t.Errorf("the loop label did not resolve into an edge; enrich is fed by %v", got)
		}
	})

	t.Run("send contributes a node consuming its source", func(t *testing.T) {
		send := nodeNamed(t, program, "backoff"+derivedSep+"send")
		if send.Kind != KindSend {
			t.Errorf("the send node has kind %s, want send", send.Kind)
		}
		if len(send.Inputs) != 1 || send.Inputs[0] != "backoff" {
			t.Errorf("the send consumes %v, want [backoff]", send.Inputs)
		}
		if len(send.Outputs) != 0 {
			t.Errorf("the send publishes %v, want nothing", send.Outputs)
		}
	})

	t.Run("a branch publishes both targets and a sink publishes nothing", func(t *testing.T) {
		settle := nodeNamed(t, program, "settle")
		if len(settle.Outputs) != 2 || settle.Outputs[0] != "done" || settle.Outputs[1] != "flaky" {
			t.Errorf("settle publishes %v, want [done flaky]", settle.Outputs)
		}
		if store := nodeNamed(t, program, "store"); len(store.Outputs) != 0 {
			t.Errorf("the sink publishes %v; a sink is terminal and publishes nothing", store.Outputs)
		}
	})
}

// TestGraphPublishesSwitchArmTargets pins the rule the corpus establishes: a
// switch publishes its ARM TARGETS, not its own name.
//
// The fixture is the shape of testdata/valid/switch-with-else.flow, whose
// following statements consume billable, refundable and other and consume the
// switch's own name nowhere. A switch publishing `route` would leave all three
// from-references unresolvable, so this test is what catches that reading.
func TestGraphPublishesSwitchArmTargets(t *testing.T) {
	const src = "flow routing\n" +
		"source ingest Poll\n" +
		"switch route from ingest on ingest.Kind {\n" +
		"  \"card\", \"wallet\" -> billable\n" +
		"  isRefund(ingest) -> refundable\n" +
		"  else -> other\n" +
		"}\n" +
		"transform charge Charge from billable\n" +
		"transform refund Refund from refundable\n" +
		"sink hold Store from other, charge, refund\n"

	program := graphOf(t, src)

	route := nodeNamed(t, program, "route")
	want := []string{"billable", "refundable", "other"}
	if len(route.Outputs) != len(want) {
		t.Fatalf("the switch publishes %v, want %v", route.Outputs, want)
	}
	for i, name := range want {
		if route.Outputs[i] != name {
			t.Errorf("switch output %d is %q, want %q", i, route.Outputs[i], name)
		}
	}

	// The consuming half: each arm target feeds the statement that names it.
	for consumer, producer := range map[string]string{"charge": "route", "refund": "route"} {
		if got := edgesInto(program, consumer); len(got) != 1 || got[0] != producer {
			t.Errorf("%s is fed by %v, want [%s]", consumer, got, producer)
		}
	}
	// And the else target reaches the sink alongside the two transforms.
	if got := edgesInto(program, "hold"); len(got) != 3 {
		t.Fatalf("the sink is fed by %v, want three inbound edges", got)
	}
}

// TestGraphRefusesEveryUnresolvableShape proves the builder reports rather than
// drops, over each way a name can fail to resolve.
func TestGraphRefusesEveryUnresolvableShape(t *testing.T) {
	for name, tc := range map[string]struct{ src, want string }{
		"unknown declaring input": {
			src:  "flow f\nsource in Poll\ntransform t Foo from nowhere\n",
			want: "which no statement in this flow produces",
		},
		"unknown merge input": {
			src:  "flow f\nsource in Poll\ntransform t Foo from in, nowhere\n",
			want: "merges \"nowhere\"",
		},
		"duplicate node name": {
			src:  "flow f\nsource in Poll\ntransform t Foo from in\ntransform t Bar from in\n",
			want: "node names are unique within a flow",
		},
		"loop label nothing sends to": {
			src:  "flow f\nsource in Poll\nloop retry\ntransform t Foo from in, retry\n",
			want: "is never sent to",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, diags := buildOf(t, tc.src)
			if len(diags) == 0 {
				t.Fatalf("the builder accepted %q", tc.src)
			}
			joined := strings.Join(messagesOf(diags), "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("the diagnostics %q do not report %q", joined, tc.want)
			}
			for _, d := range diags {
				if d.Pos.Line == 0 {
					t.Errorf("a diagnostic is unpositioned: %q", d.Message)
				}
			}
		})
	}

	// THE KNOWN POSITIVE. Every case above asserts a NON-empty diagnostic set, so
	// a builder that reported on everything would pass all four. This is the same
	// shapes built clean.
	graphOf(t, "flow f\nsource in Poll\nloop retry\ntransform t Foo from in, retry\nsend t -> retry\n")
}

// TestGraphCarriesFlowLevelDeclarations proves the Program lifts the flow's own
// declarations rather than only its statements.
func TestGraphCarriesFlowLevelDeclarations(t *testing.T) {
	const src = "flow declared\n" +
		"note \"\"\"what this flow is for\"\"\"\n" +
		"state {\n  seen map[string]bool\n}\n" +
		"var attempt int\n" +
		"on error Handle\n" +
		"source in Poll\n" +
		"sink out Store from in\n"

	program := graphOf(t, src)

	if program.Name != "declared" {
		t.Errorf("the program is named %q", program.Name)
	}
	if program.Note == nil || !strings.Contains(program.Note.Text, "what this flow is for") {
		t.Errorf("the flow note did not reach the program: %v", program.Note)
	}
	if len(program.State) != 1 || program.State[0].Name.Name != "seen" {
		t.Errorf("the state fields are %v, want one named seen", program.State)
	}
	if len(program.Vars) != 1 || program.Vars[0].Name.Name != "attempt" {
		t.Errorf("the vars are %v, want one named attempt", program.Vars)
	}
	if program.OnError == nil {
		t.Error("the flow-level on-error did not reach the program")
	}
	if program.Signature != nil {
		t.Errorf("a flow with no header carries a signature: %v", program.Signature)
	}

	// A flow with no state block leaves the field empty rather than panicking,
	// which is the arm the fixture above cannot reach.
	bare := graphOf(t, "flow bare\nsource in Poll\nsink out Store from in\n")
	if len(bare.State) != 0 {
		t.Errorf("a flow with no state block carries %v", bare.State)
	}
}

// TestGraphWalksEveryStatementShape proves each shape reaches the graph with the
// kind it should, so a missing case arm is caught as a set rather than one at a
// time.
func TestGraphWalksEveryStatementShape(t *testing.T) {
	const src = "flow shapes\n" +
		"source in Poll\n" +
		"loop again\n" +
		"transform t Foo from in, again\n" +
		"branch b Pred from t -> yes, no\n" +
		"tee split from yes -> left, right\n" +
		"switch route from left on left.Kind {\n  \"a\" -> arm\n}\n" +
		"sink out Store from arm\n" +
		"drop right\n" +
		"send no -> again\n"

	program := graphOf(t, src)

	want := map[string]NodeKind{
		"in":                          KindSource,
		"t":                           KindTransform,
		"b":                           KindBranch,
		"split":                       KindTee,
		"route":                       KindSwitch,
		"out":                         KindSink,
		"right" + derivedSep + "drop": KindDrop,
		"no" + derivedSep + "send":    KindSend,
	}
	for name, kind := range want {
		if got := nodeNamed(t, program, name); got.Kind != kind {
			t.Errorf("%s has kind %s, want %s", name, got.Kind, kind)
		}
	}
	// A loop is a LABEL, not a node. Its absence from the node set is the rule.
	for _, n := range program.Nodes {
		if n.Name == "again" {
			t.Errorf("the loop label became a node: %+v", n)
		}
	}
	if len(program.Nodes) != len(want) {
		t.Errorf("the graph holds %d nodes (%v), want %d", len(program.Nodes), nodeNames(program), len(want))
	}
}

// parseFlowSource parses a flow source and returns the parse error, if any. It
// is the primitive the derived-name collision test uses to ask the PARSER
// whether an identifier is writable rather than asserting it.
func parseFlowSource(src string) (*ast.File, error) {
	return ast.Parse([]byte(src))
}

// lower is the single-flow lowering entry point.
//
// IT LIVES IN A TEST FILE ON PURPOSE. The production path is lowerFile, which
// supplies the dependency set and the exported boundaries that a single flow
// cannot carry; this one-flow convenience is reached only from tests, and the
// module's linter reads no _test.go, so a production helper nothing production
// calls is reported unused. The rule is that such a helper belongs here.
func lower(p *Program) (*Plan, []Diagnostic) {
	return lowerProgram(p, map[string]*Program{}, map[string]Boundary{}, Config{})
}
