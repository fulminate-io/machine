// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"strconv"
	"strings"
)

// RenderMermaid draws a source's flows as a Mermaid flowchart.
//
// MERMAID IS THE RULED FORMAT for the human-facing diagram, chosen because it is
// text, renders on GitHub, and is cheap to generate. THE DIRECTION IS ONE WAY
// ONLY: there is no diagram-to-text path and none should be designed, because
// visual round-tripping was ruled a net negative for a language whose primary
// author is a model.
//
// The output is deterministic — statements and edges are emitted in body order,
// never from a map — because it is a string a test compares and a human diffs.
// A renderer walking a map produces valid-looking Mermaid in a different order
// each run, which no single-render assertion detects.
func RenderMermaid(src Source) (string, error) {
	set, err := graphsFor([]Source{src})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.Grow(mermaidSize(set))
	write(&b, "flowchart TD\n")
	for f := range set.Files {
		for i := range set.Files[f].Graphs {
			renderGraph(&b, i, &set.Files[f].Graphs[i])
		}
	}
	return b.String(), nil
}

// graphsFor derives the graphs for a set of sources through the real driver.
func graphsFor(srcs []Source) (*GraphSet, error) {
	var set *GraphSet
	capture := &Analyzer{
		Name:     "mermaid-capture",
		Doc:      "captures the derived graph out of one driver run",
		Requires: []*Analyzer{FlowgraphAnalyzer},
		Run: func(p *Pass) (any, error) {
			derived, ok := p.ResultOf[FlowgraphAnalyzer].(*GraphSet)
			if !ok {
				return nil, errNoGraph
			}
			set = derived
			return nil, nil
		},
	}

	if _, err := Run(srcs, []*Analyzer{capture}); err != nil {
		return nil, err
	}
	if set == nil {
		return nil, errNoGraph
	}
	return set, nil
}

// mermaidSize estimates the output length so the builder allocates once.
func mermaidSize(set *GraphSet) int {
	const perLine = 48

	lines := 1
	for f := range set.Files {
		for i := range set.Files[f].Graphs {
			lines += len(set.Files[f].Graphs[i].Nodes) + len(set.Files[f].Graphs[i].Edges) + 2
		}
	}
	return lines * perLine
}

// renderGraph writes one flow as a labeled subgraph.
func renderGraph(b *strings.Builder, index int, graph *FlowGraph) {
	write(b, "  subgraph ", nodeID(index, -1), "[\"flow ", escapeMermaid(graph.Flow), "\"]\n")

	for i := range graph.Nodes {
		renderNode(b, index, &graph.Nodes[i])
	}
	for _, edge := range graph.Edges {
		renderEdge(b, index, edge)
	}
	write(b, "  end\n")
}

// renderNode writes one statement.
func renderNode(b *strings.Builder, flow int, n *GraphNode) {
	write(b, "    ", nodeID(flow, n.Stmt), "[\"", escapeMermaid(n.Kind))
	if n.Label != "" {
		write(b, " ", escapeMermaid(n.Label))
	}
	write(b, "\"]\n")
}

// renderEdge writes one edge, marking a send distinctly so a loop is visually
// identifiable — the same distinction the derived graph carries as Backward.
func renderEdge(b *strings.Builder, flow int, edge GraphEdge) {
	arrow := " -->|"
	if edge.Backward {
		arrow = " -.->|"
	}
	write(b, "    ", nodeID(flow, edge.From), arrow, escapeMermaid(edge.Name), "| ", nodeID(flow, edge.To), "\n")
}

// write appends every part to the builder.
//
// strings.Builder.WriteString is documented never to return a non-nil error, but
// the linter cannot know that, and discarding the result at each of a dozen call
// sites would be noisier than routing them all through here.
func write(b *strings.Builder, parts ...string) {
	for _, part := range parts {
		_, _ = b.WriteString(part)
	}
}

// nodeID builds a diagram-unique identifier. A statement index of -1 names the
// flow's own subgraph.
func nodeID(flow, stmt int) string {
	if stmt < 0 {
		return "f" + strconv.Itoa(flow)
	}
	return "f" + strconv.Itoa(flow) + "n" + strconv.Itoa(stmt)
}

// escapeMermaid neutralizes the characters Mermaid treats as syntax inside a
// quoted label.
//
// A node's label is an identifier today, so this rarely does anything — but a
// label is derived from source text, and the moment one carries a quote or a
// bracket an unescaped diagram stops parsing rather than looking wrong. A note's
// prose never reaches the diagram at all.
func escapeMermaid(text string) string {
	replacer := strings.NewReplacer(
		`"`, "&quot;",
		"\n", " ",
		"\r", " ",
		"[", "&#91;",
		"]", "&#93;",
		"{", "&#123;",
		"}", "&#125;",
		"|", "&#124;",
	)
	return replacer.Replace(text)
}
