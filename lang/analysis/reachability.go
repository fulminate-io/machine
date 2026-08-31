// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import "reflect"

// ReachabilityAnalyzer reports statements no data can reach, and outputs nothing
// consumes.
var ReachabilityAnalyzer = &Analyzer{
	Name: "reachability",
	Doc: "reachability reports a statement no data can reach, at error severity, and an output " +
		"no statement consumes, at hint severity. THE ROOT SET IS RULED: roots are every source, " +
		"PLUS any loop label targeted by a REACHABLE send. A loop carries no from-clause, so a " +
		"send naming it is the only syntactic route in, which is why an orphaned chain is " +
		"expressible at all and why treating every loop label as a root would silently disable " +
		"orphan detection for exactly the shape that produced the defect. The rule is a FIXPOINT " +
		"rather than a single pass, because a send confers root-hood on its target only once the " +
		"send is itself reachable, and that send ordinarily sits downstream of the label it " +
		"targets. A sink and a drop are terminal and produce nothing, so neither is ever an " +
		"unconsumed output.",
	Requires:   []*Analyzer{FlowgraphAnalyzer},
	Run:        runReachability,
	ResultType: reflect.TypeOf((*struct{})(nil)),
}

// runReachability checks every flow in every source.
func runReachability(p *Pass) (any, error) {
	set, ok := p.ResultOf[FlowgraphAnalyzer].(*GraphSet)
	if !ok {
		return nil, errNoGraph
	}
	for f := range set.Files {
		for i := range set.Files[f].Graphs {
			graph := &set.Files[f].Graphs[i]
			reportDeadNodes(p, graph)
			reportUnconsumedOutputs(p, graph)
		}
	}
	return nil, nil
}

// reachableNodes runs the ruled root rule to a fixpoint and reports, per node,
// whether data can reach it.
//
// THE LOOP CLAUSE IS WHAT MAKES THIS A FIXPOINT. A loop label becomes available
// only when a REACHABLE send produces it, and that send may sit downstream of
// the label — the ordinary shape. A single pass over the body in declaration
// order gets that case wrong every time: it reaches the label before the send
// that feeds it exists in the available set, marks the label dead, and never
// looks again.
//
// lang/ast/strawman_reachability_test.go implements the identical reading in
// orphanedStatements, and carries the pre-amendment toy text as a known positive
// yielding exactly three orphans.
func reachableNodes(graph *FlowGraph) []bool {
	available := make(map[string]bool, len(graph.Nodes))
	for _, name := range graph.Entry {
		available[name] = true
	}

	reached := make([]bool, len(graph.Nodes))
	for changed := true; changed; {
		changed = false
		for i := range graph.Nodes {
			if reached[i] || !nodeReachable(&graph.Nodes[i], available) {
				continue
			}
			reached[i] = true
			changed = true
			for _, name := range graph.Nodes[i].Outputs {
				available[name] = true
			}
		}
	}
	return reached
}

// nodeReachable applies the root rule to one node.
//
// A LOOP is reachable only when its own label is already available, which
// happens exactly when a reachable send produces it. Every other input-free
// statement is a root: a source introduces data, and a BadStmt is a region the
// parser already reported on and is not compounded here.
func nodeReachable(n *GraphNode, available map[string]bool) bool {
	if n.Kind == kindLoop {
		return available[n.Label]
	}
	if len(n.Inputs) == 0 {
		return true
	}
	for _, name := range n.Inputs {
		if available[name] {
			return true
		}
	}
	return false
}

// reportDeadNodes reports every statement the fixpoint could not reach.
func reportDeadNodes(p *Pass, graph *FlowGraph) {
	reached := reachableNodes(graph)
	for i := range graph.Nodes {
		if reached[i] || graph.Nodes[i].Kind == kindBad {
			continue
		}
		p.Report(Diagnostic{
			Pos: graph.Nodes[i].Pos,
			End: endOfName(graph.Nodes[i].Pos, graph.Nodes[i].Label),
			Message: "no data can reach the " + graph.Nodes[i].Kind + " " + graph.Nodes[i].Label +
				" in flow " + graph.Flow,
			Severity: SeverityError,
		})
	}
}

// reportUnconsumedOutputs reports each produced name nothing reads.
//
// The report is PER NAME at its earliest producer rather than per node, because
// a loop label and the send that targets it both produce the same name and a
// per-node report would say the same thing twice about one dead end.
//
// A FLOW SIGNATURE'S DECLARED OUTPUTS COUNT AS CONSUMED. They are consumed by
// the CALLER rather than by a statement in this flow, which is the whole purpose
// of declaring them, so an analysis looking only inside the flow sees a dead end
// that is not one. Whether a declared output is actually delivered is a separate
// question and the signature analyzer owns it.
func reportUnconsumedOutputs(p *Pass, graph *FlowGraph) {
	consumed := make(map[string]bool, len(graph.Nodes))
	for _, name := range graph.Declared {
		consumed[name] = true
	}
	for i := range graph.Nodes {
		for _, name := range graph.Nodes[i].Inputs {
			consumed[name] = true
		}
	}

	seen := make(map[string]bool, len(graph.Nodes))
	for i := range graph.Nodes {
		for _, name := range graph.Nodes[i].Outputs {
			if consumed[name] || seen[name] {
				continue
			}
			seen[name] = true
			p.Report(unconsumedOutput(graph, &graph.Nodes[i], name))
		}
	}
}

// unconsumedOutput builds the hint for a produced name nothing reads.
//
// The severity is RULED at hint rather than warning or error: an unconsumed
// output is a shape an author may reasonably intend, and the canonical corpus
// contains one deliberately.
func unconsumedOutput(graph *FlowGraph, n *GraphNode, name string) Diagnostic {
	return Diagnostic{
		Pos: n.Pos,
		End: endOfName(n.Pos, n.Label),
		Message: "the " + n.Kind + " " + n.Label + " produces " + name +
			", which no statement in flow " + graph.Flow + " consumes",
		Severity: SeverityHint,
	}
}
