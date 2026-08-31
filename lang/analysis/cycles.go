// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"reflect"
	"strconv"
	"strings"
)

// CyclesAnalyzer finds loops in the derived graph and reports the ones the
// language has no way to express deliberately.
var CyclesAnalyzer = &Analyzer{
	Name: "cycles",
	Doc: "cycles finds strongly connected components in the derived graph and reports any cycle " +
		"containing NO backward send edge. The distinction is the whole content of the analyzer: " +
		"a cycle formed through loop and send is the language's only sanctioned way to express " +
		"iteration, while a cycle with no send edge cannot have been written deliberately, since " +
		"forward references are the only other edge kind and cannot close a loop by " +
		"construction. Such a cycle means either a declare-before-use violation, which resolve " +
		"reports separately, or that the graph derivation is wrong — so this reports as much a " +
		"self-check on flowgraph as a user diagnostic. A send is NECESSARY BUT NOT SUFFICIENT " +
		"for a cycle: it closes one only if its target also reaches back to it, which is why " +
		"this is computed rather than read off the presence of the keyword.",
	Requires:   []*Analyzer{FlowgraphAnalyzer},
	Run:        runCycles,
	ResultType: reflect.TypeOf((*CycleSet)(nil)),
}

// Cycle is one strongly connected component that forms a loop.
//
// HasSend records whether any edge INSIDE the component is a backward send edge,
// which is what tells a sanctioned loop from an accidental one.
type Cycle struct {
	Flow    string
	Stmts   []int
	HasSend bool
}

// FileCycles is one source file's cycles.
type FileCycles struct {
	Path   string
	Cycles []Cycle
}

// CycleSet is the cycles analyzer's result over every source in the run.
type CycleSet struct {
	Files []FileCycles
}

// runCycles finds and judges every flow's cycles.
func runCycles(p *Pass) (any, error) {
	set, ok := p.ResultOf[FlowgraphAnalyzer].(*GraphSet)
	if !ok {
		return nil, errNoGraph
	}

	out := &CycleSet{Files: make([]FileCycles, 0, len(set.Files))}
	for f := range set.Files {
		found := FileCycles{Path: set.Files[f].Path}
		for i := range set.Files[f].Graphs {
			graph := &set.Files[f].Graphs[i]
			for _, cycle := range cyclesIn(graph) {
				found.Cycles = append(found.Cycles, cycle)
				if !cycle.HasSend {
					p.Report(sendFreeCycle(graph, cycle))
				}
			}
		}
		out.Files = append(out.Files, found)
	}
	return out, nil
}

// sendFreeCycle builds the diagnostic for a cycle the language cannot express.
func sendFreeCycle(graph *FlowGraph, cycle Cycle) Diagnostic {
	labels := make([]string, 0, len(cycle.Stmts))
	for _, stmt := range cycle.Stmts {
		if n, ok := graph.Node(stmt); ok {
			labels = append(labels, n.Kind+" "+n.Label)
		} else {
			labels = append(labels, "statement "+strconv.Itoa(stmt))
		}
	}

	pos := graph.Pos
	if n, ok := graph.Node(cycle.Stmts[0]); ok {
		pos = n.Pos
	}
	return Diagnostic{
		Pos: pos,
		End: pos,
		Message: "flow " + graph.Flow + " contains a cycle with no send: " + strings.Join(labels, " -> ") +
			". A loop is expressed with loop and send; a cycle without one cannot be written deliberately",
		Severity: SeverityError,
	}
}

// cyclesIn returns every strongly connected component of a flow's graph that
// forms a loop, in statement order.
//
// A component of one node is a cycle only when it carries a self-edge; every
// larger component is a cycle by definition.
func cyclesIn(graph *FlowGraph) []Cycle {
	t := newTarjan(graph)
	t.run()

	var out []Cycle
	for _, comp := range t.comps {
		if len(comp) == 1 && !t.hasSelfEdge(comp[0]) {
			continue
		}
		sortInts(comp)
		out = append(out, Cycle{Flow: graph.Flow, Stmts: t.stmtsOf(comp), HasSend: t.componentHasSend(comp)})
	}
	return out
}

// tarjan holds the state of one iterative strongly-connected-components walk.
//
// The walk is ITERATIVE rather than recursive. A flow is tens of statements, so
// recursion would work today; an explicit stack costs nothing extra and takes
// the depth question off the table for a graph derived from source a generator
// might produce.
type tarjan struct {
	adj     [][]int
	send    [][]bool
	index   []int
	low     []int
	onStack []bool
	stack   []int
	comps   [][]int
	next    int
	graph   *FlowGraph
}

// frame is one node's position in the iterative walk.
type frame struct {
	node int
	edge int
}

// newTarjan builds the adjacency and the parallel send-edge marking.
func newTarjan(graph *FlowGraph) *tarjan {
	n := len(graph.Nodes)
	t := &tarjan{
		adj:     make([][]int, n),
		send:    make([][]bool, n),
		index:   make([]int, n),
		low:     make([]int, n),
		onStack: make([]bool, n),
		graph:   graph,
	}
	for i := range t.index {
		t.index[i] = -1
	}

	at := make(map[int]int, n)
	for i := range graph.Nodes {
		at[graph.Nodes[i].Stmt] = i
	}
	for _, edge := range graph.Edges {
		from, okFrom := at[edge.From]
		to, okTo := at[edge.To]
		if !okFrom || !okTo {
			continue
		}
		t.adj[from] = append(t.adj[from], to)
		t.send[from] = append(t.send[from], edge.Backward)
	}
	return t
}

// run walks every node not yet visited.
func (t *tarjan) run() {
	for v := range t.adj {
		if t.index[v] == -1 {
			t.walk(v)
		}
	}
}

// walk explores one root's component, keeping its own stack.
func (t *tarjan) walk(root int) {
	t.open(root)
	work := []frame{{node: root}}
	for len(work) > 0 {
		top := &work[len(work)-1]
		if next, ok := t.advance(top); ok {
			t.open(next)
			work = append(work, frame{node: next})
			continue
		}
		if t.low[top.node] == t.index[top.node] {
			t.closeComponent(top.node)
		}
		done := top.node
		work = work[:len(work)-1]
		if len(work) > 0 {
			parent := work[len(work)-1].node
			t.low[parent] = min(t.low[parent], t.low[done])
		}
	}
}

// advance takes the frame's next unexplored edge, folding already-visited
// neighbors into the low-link as it goes.
//
// It reports a neighbor to descend into, or false when the frame's edges are
// exhausted.
func (t *tarjan) advance(f *frame) (int, bool) {
	for f.edge < len(t.adj[f.node]) {
		w := t.adj[f.node][f.edge]
		f.edge++
		if t.index[w] == -1 {
			return w, true
		}
		if t.onStack[w] {
			t.low[f.node] = min(t.low[f.node], t.index[w])
		}
	}
	return 0, false
}

// open assigns a node its index and pushes it.
func (t *tarjan) open(v int) {
	t.index[v] = t.next
	t.low[v] = t.next
	t.next++
	t.stack = append(t.stack, v)
	t.onStack[v] = true
}

// closeComponent pops one completed component off the stack.
func (t *tarjan) closeComponent(root int) {
	var comp []int
	for {
		w := t.stack[len(t.stack)-1]
		t.stack = t.stack[:len(t.stack)-1]
		t.onStack[w] = false
		comp = append(comp, w)
		if w == root {
			break
		}
	}
	t.comps = append(t.comps, comp)
}

// hasSelfEdge reports whether a node is its own successor.
func (t *tarjan) hasSelfEdge(v int) bool {
	for _, w := range t.adj[v] {
		if w == v {
			return true
		}
	}
	return false
}

// componentHasSend reports whether any edge WITHIN a component is a backward
// send edge.
//
// Edges leaving the component do not count: a send that routes out of a loop
// does not make that loop the sanctioned kind.
func (t *tarjan) componentHasSend(comp []int) bool {
	inside := make(map[int]bool, len(comp))
	for _, v := range comp {
		inside[v] = true
	}
	for _, v := range comp {
		for i, w := range t.adj[v] {
			if inside[w] && t.send[v][i] {
				return true
			}
		}
	}
	return false
}

// stmtsOf projects component members back to statement indices.
func (t *tarjan) stmtsOf(comp []int) []int {
	out := make([]int, 0, len(comp))
	for _, v := range comp {
		out = append(out, t.graph.Nodes[v].Stmt)
	}
	return out
}
