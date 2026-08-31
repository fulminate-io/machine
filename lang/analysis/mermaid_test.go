// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderMermaidMatchesGraphAndIsDeterministic pins that the diagram is a
// faithful and stable rendering of the derived graph.
//
// THE DETERMINISM LEG IS A REAL RISK RATHER THAN CEREMONY. A renderer iterating
// a map emits valid-looking Mermaid in a different order on every run, which no
// single-render assertion detects and which turns the output into a diff nobody
// can read.
func TestRenderMermaidMatchesGraphAndIsDeterministic(t *testing.T) {
	var lines []string

	for _, name := range strawmanFiles {
		src := loadSource(t, filepath.Join(strawmanDir, name))
		set, _ := graphsOf(t, src)

		var wantNodes, wantEdges, wantBackward int
		for _, file := range set.Files {
			for i := range file.Graphs {
				wantNodes += len(file.Graphs[i].Nodes)
				wantEdges += len(file.Graphs[i].Edges)
				for _, edge := range file.Graphs[i].Edges {
					if edge.Backward {
						wantBackward++
					}
				}
			}
		}
		if wantNodes == 0 || wantEdges == 0 {
			t.Fatalf("%s derived %d nodes and %d edges; the counts below would compare nothing",
				name, wantNodes, wantEdges)
		}

		out, err := RenderMermaid(src)
		if err != nil {
			t.Fatalf("rendering %s failed: %v", name, err)
		}
		gotNodes, gotEdges, gotBackward := countMermaid(out)

		if gotNodes != wantNodes {
			t.Errorf("%s rendered %d nodes, want the graph's %d", name, gotNodes, wantNodes)
		}
		if gotEdges != wantEdges {
			t.Errorf("%s rendered %d edges, want the graph's %d", name, gotEdges, wantEdges)
		}
		if gotBackward != wantBackward {
			t.Errorf("%s rendered %d send edges distinctly, want %d", name, gotBackward, wantBackward)
		}

		// DETERMINISM: the same source rendered twice, byte for byte.
		again, aerr := RenderMermaid(src)
		if aerr != nil {
			t.Fatalf("re-rendering %s failed: %v", name, aerr)
		}
		if again != out {
			t.Errorf("%s rendered differently on a second call", name)
		}

		t.Logf("%s: %d nodes, %d edges, %d of them sends", name, gotNodes, gotEdges, gotBackward)
		lines = append(lines, name)
		lines = append(lines, strings.Split(strings.TrimRight(out, "\n"), "\n")...)
	}

	checkGolden(t, "mermaid.txt", lines)
}

// countMermaid counts node declarations, edges, and the send edges drawn with
// the dotted arrow.
func countMermaid(out string) (nodes, edges, backward int) {
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.Contains(trimmed, "-.->|"):
			edges++
			backward++
		case strings.Contains(trimmed, "-->|"):
			edges++
		case strings.HasPrefix(trimmed, "f") && strings.Contains(trimmed, `["`):
			nodes++
		}
	}
	return nodes, edges, backward
}

// TestRenderMermaidEscapesLabelSyntax pins that a label carrying Mermaid syntax
// is neutralized rather than emitted raw.
//
// A node label is an identifier today, so this path is not otherwise exercised
// — and an unescaped diagram does not look wrong, it stops parsing.
func TestRenderMermaidEscapesLabelSyntax(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{in: `say "hi"`, want: "say &quot;hi&quot;"},
		{in: "a[b]c", want: "a&#91;b&#93;c"},
		{in: "x|y", want: "x&#124;y"},
		{in: "line\nbreak", want: "line break"},
	} {
		if got := escapeMermaid(tc.in); got != tc.want {
			t.Errorf("escapeMermaid(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRenderMermaidRefusesAnUnparsedSource pins that a source with no tree is an
// error rather than an empty diagram, which a caller could not tell from a file
// with no flows.
func TestRenderMermaidRefusesAnUnparsedSource(t *testing.T) {
	if _, err := RenderMermaid(Source{Path: "nothing.flow"}); err == nil {
		t.Fatal("rendering a source with no parsed tree returned no error")
	}
}
