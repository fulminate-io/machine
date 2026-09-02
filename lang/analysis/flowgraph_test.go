// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
)

// sendRef is one send statement, classified by what its target names.
type sendRef struct {
	stmt   int
	source string
	target string
	toLoop bool
}

// TestFlowgraphMarksBothSendTargetKinds asserts the derived graph is non-empty
// and that send edges are marked, across BOTH target kinds SendStmt documents:
// "its target may be a node declared earlier or a loop label".
//
// The kinds are DERIVED per strawman by classifying each send's target against
// that flow's loop labels, not assumed from a table in the plan. If a future
// corpus ever leaves one kind unrepresented this fails loudly and names which,
// rather than passing on the one kind that remains.
func TestFlowgraphMarksBothSendTargetKinds(t *testing.T) {
	var toLoop, toNode int

	for _, name := range strawmanFiles {
		src := loadSource(t, filepath.Join(strawmanDir, name))
		set, diags := graphsOf(t, src)
		if len(diags) != 0 {
			t.Errorf("%s produced diagnostics while deriving its graph: %v", name, messages(diags))
		}

		flow := firstFlow(t, src)
		graph, ok := set.Graph(flow.Name.Name)
		if !ok {
			t.Fatalf("%s derived no graph for flow %s", name, flow.Name.Name)
		}

		// THE VALUE-TRAP FLOOR. A pointer-form walk derives no edges at all, and
		// an empty edge set is otherwise indistinguishable from a graph with
		// nothing to connect.
		if len(graph.Edges) == 0 {
			t.Fatalf("%s derived ZERO edges; a walk that matched nothing looks exactly like this", name)
		}

		sends := sendsIn(flow)
		if len(sends) == 0 {
			t.Fatalf("%s declares no send, so it cannot exercise either target kind", name)
		}
		for _, send := range sends {
			if send.toLoop {
				toLoop++
			} else {
				toNode++
			}
			assertSendEdgesMarked(t, name, graph, send)
		}
		t.Logf("%s: %d nodes, %d edges, sends %v", name, len(graph.Nodes), len(graph.Edges), sends)

		assertOnlySendEdgesAreBackward(t, name, graph)
	}

	if toLoop == 0 {
		t.Error("no strawman sends to a LOOP LABEL, so that target kind is untested")
	}
	if toNode == 0 {
		t.Error("no strawman sends to a NODE, so that target kind is untested")
	}
	t.Logf("send target kinds across the corpus: %d to a loop label, %d to a node", toLoop, toNode)
}

// assertSendEdgesMarked checks that the edges leaving one send are marked
// backward, and that the send produced at least one edge.
func assertSendEdgesMarked(t *testing.T, fixture string, graph *FlowGraph, send sendRef) {
	t.Helper()

	var found int
	for _, edge := range graph.Edges {
		if edge.From != send.stmt {
			continue
		}
		found++
		if !edge.Backward {
			t.Errorf("%s: the edge %s carries from the send at statement %d is not marked backward",
				fixture, edge.Name, send.stmt)
		}
		if edge.Name != send.target {
			t.Errorf("%s: the send at statement %d produced an edge for %q, want its target %q",
				fixture, send.stmt, edge.Name, send.target)
		}
	}
	if found == 0 {
		t.Errorf("%s: the send at statement %d (%s -> %s) produced no edge at all",
			fixture, send.stmt, send.source, send.target)
	}
}

// assertOnlySendEdgesAreBackward is the other direction, without which "every
// send edge is backward" is satisfied by a graph that marks every edge backward.
func assertOnlySendEdgesAreBackward(t *testing.T, fixture string, graph *FlowGraph) {
	t.Helper()

	var forward int
	for _, edge := range graph.Edges {
		from, ok := graph.Node(edge.From)
		if !ok {
			t.Fatalf("%s: edge %s starts at statement %d, which is not a node", fixture, edge.Name, edge.From)
		}
		if from.Kind == kindSend {
			continue
		}
		forward++
		if edge.Backward {
			t.Errorf("%s: the edge %s from the %s at statement %d is marked backward",
				fixture, edge.Name, from.Kind, edge.From)
		}
	}
	if forward == 0 {
		t.Errorf("%s: every edge in the graph starts at a send, so the backward mark discriminates nothing", fixture)
	}
}

// sendsIn enumerates a flow's sends and classifies each target against the
// flow's own loop labels.
func sendsIn(flow ast.FlowDecl) []sendRef {
	loops := map[string]bool{}
	for _, stmt := range flow.Body {
		if loop, ok := stmt.(ast.LoopStmt); ok {
			loops[loop.Name.Name] = true
		}
	}

	var out []sendRef
	for i, stmt := range flow.Body {
		send, ok := stmt.(ast.SendStmt)
		if !ok {
			continue
		}
		out = append(out, sendRef{
			stmt:   i,
			source: send.Source.Name,
			target: send.Target.Name,
			toLoop: loops[send.Target.Name],
		})
	}
	return out
}

// String renders a send for a log line.
func (s sendRef) String() string {
	kind := "node"
	if s.toLoop {
		kind = "loop"
	}
	return fmt.Sprintf("%s->%s(%s)@%d", s.source, s.target, kind, s.stmt)
}

// TestFlowgraphEdgesAreStable pins the derived graph against a regenerated
// golden, so a change to the dataflow model is visible as a diff rather than as
// a downstream analyzer quietly changing its mind.
func TestFlowgraphEdgesAreStable(t *testing.T) {
	var lines []string
	for _, name := range strawmanFiles {
		src := loadSource(t, filepath.Join(strawmanDir, name))
		set, _ := graphsOf(t, src)
		for _, file := range set.Files {
			for i := range file.Graphs {
				lines = append(lines, graphLines(name, &file.Graphs[i])...)
			}
		}
	}
	checkGolden(t, "flowgraph.txt", lines)
}

// graphLines renders one graph in statement order.
func graphLines(fixture string, graph *FlowGraph) []string {
	out := []string{fmt.Sprintf("%s %s nodes %d edges %d entry %v",
		fixture, graph.Flow, len(graph.Nodes), len(graph.Edges), graph.Entry)}
	for _, n := range graph.Nodes {
		out = append(out, fmt.Sprintf("%s %s node %d %s %s in %v out %v",
			fixture, graph.Flow, n.Stmt, n.Kind, n.Label, n.Inputs, n.Outputs))
	}
	for _, e := range graph.Edges {
		mark := "forward"
		if e.Backward {
			mark = "backward"
		}
		out = append(out, fmt.Sprintf("%s %s edge %d->%d %s %s", fixture, graph.Flow, e.From, e.To, e.Name, mark))
	}
	return out
}

// TestFlowgraphRefusesAMissingSymbolsTable pins that a mistyped prerequisite is
// a stop rather than an empty graph, because an empty graph reports every
// program clean.
func TestFlowgraphRefusesAMissingSymbolsTable(t *testing.T) {
	pass := &Pass{Analyzer: FlowgraphAnalyzer, ResultOf: map[*Analyzer]any{SymbolsAnalyzer: "not a table"}}
	if _, err := FlowgraphAnalyzer.Run(pass); err == nil {
		t.Fatal("the flowgraph analyzer accepted a symbols result of the wrong type")
	}
}
