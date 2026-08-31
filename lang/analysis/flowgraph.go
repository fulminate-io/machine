// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"reflect"

	"github.com/whitaker-io/machine/lang/ast"
)

// The statement kinds a graph node carries. They name the shape a node was
// derived from, which is what the Mermaid renderer draws and what the cycles
// analyzer reads to tell a send edge from every other edge.
const (
	kindSource    = "source"
	kindTransform = "transform"
	kindBranch    = "branch"
	kindTee       = "tee"
	kindSink      = "sink"
	kindSwitch    = "switch"
	kindUse       = "use"
	kindDrop      = "drop"
	kindLoop      = "loop"
	kindSend      = "send"
	kindBad       = "bad"
)

// FlowgraphAnalyzer derives the dataflow graph every structural analysis reads.
var FlowgraphAnalyzer = &Analyzer{
	Name: "flowgraph",
	Doc: "flowgraph derives a directed dataflow graph per flow: a node per statement, and an " +
		"edge from the statement producing a name to each statement consuming it. THE SEND " +
		"MODEL IS A CHOICE AND THIS IS ITS DISCLOSURE: a send is modeled as consuming its " +
		"Source name and producing its Target name, matching the reference implementation in " +
		"lang/ast. The alternative reading is that a send re-executes the target NODE; it " +
		"changes which statements sit inside a derived cycle but not whether the cycle carries " +
		"a send. Send edges are marked Backward because a send is the language's only backward " +
		"arrow, and the cycles analyzer reads that mark to tell a sanctioned loop from an " +
		"accidental one.",
	Requires:   []*Analyzer{SymbolsAnalyzer},
	Run:        runFlowgraph,
	ResultType: reflect.TypeOf((*GraphSet)(nil)),
}

// GraphNode is one statement as the dataflow model sees it.
//
// Inputs and Outputs are the DATAFLOW names, which are not the symbols table's
// producers and consumers: a send's Target appears in Outputs here and in the
// symbols table's Consumers, because a send references a name rather than
// declaring one but routes data into it.
type GraphNode struct {
	Stmt    int
	Kind    string
	Label   string
	Pos     ast.Position
	Inputs  []string
	Outputs []string
}

// GraphEdge carries one name from the statement that produces it to a statement
// that consumes it.
//
// Backward marks an edge ORIGINATING AT A SEND. It is not a claim about
// declaration order — a send may feed a loop label declared immediately above it
// — but about which arrow the language considers backward, and SendStmt's own
// documentation names it "the only backward arrow in the language".
type GraphEdge struct {
	From     int
	To       int
	Name     string
	Backward bool
}

// FlowGraph is one flow's derived graph.
//
// Entry holds the names available before any statement runs. It is empty for an
// ordinary flow and holds the implicit input for a flow with a signature, whose
// body consumes a name no statement declares.
type FlowGraph struct {
	Flow  string
	Pos   ast.Position
	Entry []string
	Nodes []GraphNode
	Edges []GraphEdge
}

// FileGraphs is one source file's derived graphs.
type FileGraphs struct {
	Path   string
	Graphs []FlowGraph
}

// GraphSet is the flowgraph analyzer's result over every source in the run.
type GraphSet struct {
	Files []FileGraphs
}

// Graph finds a flow's graph anywhere in the run.
func (g *GraphSet) Graph(flow string) (*FlowGraph, bool) {
	for f := range g.Files {
		for i := range g.Files[f].Graphs {
			if g.Files[f].Graphs[i].Flow == flow {
				return &g.Files[f].Graphs[i], true
			}
		}
	}
	return nil, false
}

// Node finds a graph node by its statement index.
func (g *FlowGraph) Node(stmt int) (*GraphNode, bool) {
	for i := range g.Nodes {
		if g.Nodes[i].Stmt == stmt {
			return &g.Nodes[i], true
		}
	}
	return nil, false
}

// runFlowgraph derives a graph for every flow the symbols analyzer tabled.
func runFlowgraph(p *Pass) (any, error) {
	table, ok := p.ResultOf[SymbolsAnalyzer].(*SymbolTable)
	if !ok {
		return nil, errNoSymbols
	}

	set := &GraphSet{Files: make([]FileGraphs, 0, len(table.Files))}
	for _, file := range table.Files {
		graphs := FileGraphs{Path: file.Path, Graphs: make([]FlowGraph, 0, len(file.Flows))}
		for i := range file.Flows {
			graphs.Graphs = append(graphs.Graphs, buildGraph(&file.Flows[i]))
		}
		set.Files = append(set.Files, graphs)
	}
	return set, nil
}

// buildGraph derives one flow's nodes and then its edges.
func buildGraph(flow *FlowSymbols) FlowGraph {
	graph := FlowGraph{
		Flow:  flow.Name,
		Pos:   flow.Pos,
		Nodes: make([]GraphNode, 0, len(flow.Body)),
	}
	if flow.HasSignature {
		graph.Entry = []string{implicitInput}
	}
	for i, stmt := range flow.Body {
		graph.Nodes = append(graph.Nodes, dataflowOf(i, stmt))
	}
	graph.Edges = deriveEdges(graph.Nodes)
	return graph
}

// deriveEdges connects each producing statement to every statement consuming the
// name it produces.
//
// Iteration is over the node slice rather than over a map, at both levels, so
// the edge order is a function of statement order alone — the Mermaid renderer
// depends on that, and a map walk here would make its output differ run to run
// while staying valid.
func deriveEdges(nodes []GraphNode) []GraphEdge {
	producers := map[string][]int{}
	for i := range nodes {
		for _, name := range nodes[i].Outputs {
			producers[name] = append(producers[name], i)
		}
	}

	var edges []GraphEdge
	for i := range nodes {
		for _, name := range nodes[i].Inputs {
			for _, from := range producers[name] {
				edges = append(edges, GraphEdge{
					From:     nodes[from].Stmt,
					To:       nodes[i].Stmt,
					Name:     name,
					Backward: nodes[from].Kind == kindSend,
				})
			}
		}
	}
	return edges
}

// dataflowOf reads one statement's dataflow contribution.
//
// THE DISPATCH IS SPLIT ON THE CLAUSES LINE and the per-shape iteration is
// extracted, per the locked dispatch rule — the same shape the symbols analyzer
// establishes. Every case is the VALUE form: a pointer-form case compiles clean
// and never matches.
func dataflowOf(i int, stmt ast.Stmt) GraphNode {
	if node, ok := clauseBearingFlow(i, stmt); ok {
		return node
	}
	return plainFlow(i, stmt)
}

// clauseBearingFlow reads the seven shapes that embed Clauses.
func clauseBearingFlow(i int, stmt ast.Stmt) (GraphNode, bool) {
	switch s := stmt.(type) {
	case ast.SourceStmt:
		return node(i, kindSource, s.Name, nil, []string{s.Name.Name}), true
	case ast.TransformStmt:
		return node(i, kindTransform, s.Name, identNames(s.From), []string{s.Name.Name}), true
	case ast.BranchStmt:
		return node(i, kindBranch, s.Name, identNames(s.From), branchTargets(s)), true
	case ast.TeeStmt:
		return node(i, kindTee, s.Name, identNames(s.From), identNames(s.Targets)), true
	case ast.SinkStmt:
		return node(i, kindSink, s.Name, identNames(s.From), nil), true
	case ast.SwitchStmt:
		return node(i, kindSwitch, s.Name, identNames(s.From), switchTargets(s)), true
	case ast.UseStmt:
		return node(i, kindUse, s.Instance, identNames(s.From), identNames(s.Bindings)), true
	default:
		return GraphNode{}, false
	}
}

// plainFlow reads the four shapes that carry no clauses.
//
// A SEND PRODUCES ITS TARGET NAME and consumes its source, which is the modeled
// reading this analyzer's Doc discloses. A LOOP is the one shape with no inputs
// at all — it carries no from-clause — so a send naming it is the only route in,
// which is the whole reason an orphaned chain is expressible.
func plainFlow(i int, stmt ast.Stmt) GraphNode {
	switch s := stmt.(type) {
	case ast.DropStmt:
		return node(i, kindDrop, s.Input, []string{s.Input.Name}, nil)
	case ast.LoopStmt:
		return node(i, kindLoop, s.Name, nil, []string{s.Name.Name})
	case ast.SendStmt:
		return node(i, kindSend, s.Source, []string{s.Source.Name}, []string{s.Target.Name})
	case ast.BadStmt:
		return GraphNode{Stmt: i, Kind: kindBad, Pos: s.Start}
	default:
		return GraphNode{Stmt: i, Kind: kindBad, Pos: stmt.Pos()}
	}
}

// node assembles one graph node.
func node(i int, kind string, name ast.Ident, inputs, outputs []string) GraphNode {
	return GraphNode{
		Stmt:    i,
		Kind:    kind,
		Label:   name.Name,
		Pos:     name.NamePos,
		Inputs:  inputs,
		Outputs: outputs,
	}
}

// branchTargets is a branch's two outputs, in declaration order.
func branchTargets(s ast.BranchStmt) []string {
	return []string{s.TrueTarget.Name, s.FalseTarget.Name}
}

// switchTargets is one target per arm plus the else target when present.
func switchTargets(s ast.SwitchStmt) []string {
	out := make([]string, 0, len(s.Arms)+1)
	for _, arm := range s.Arms {
		out = append(out, arm.Target.Name)
	}
	if s.Else != nil {
		out = append(out, s.Else.Name)
	}
	return out
}

// identNames projects identifiers down to their text.
func identNames(ids []ast.Ident) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.Name)
	}
	return out
}
