// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"reflect"
	"sort"
	"strings"
)

// TypeflowAnalyzer propagates the flow language's OWN declared type spellings
// along the derived edges.
var TypeflowAnalyzer = &Analyzer{
	Name: "typeflow",
	Doc: "typeflow gives each name the structural type IDENTITY the language itself states — a " +
		"flow signature's declared input and output types — and reports a fan-in whose inputs " +
		"carry different identities, which is the shape the grammar permits syntactically and " +
		"where a static check earns its keep. THIS IS STRUCTURAL AGREEMENT BETWEEN DECLARED " +
		"SPELLINGS AND IS NOT TYPE CHECKING. It resolves nothing through go/types, loads no " +
		"packages, and compares text: two spellings that differ textually while denoting the same " +
		"Go type are NOT equated in v1, and two that match textually are not thereby proven to " +
		"name the same type. Where no declared type is available the edge is UNTYPED and this " +
		"analyzer says nothing — that silence is not agreement, and no consumer should read a " +
		"clean typeflow result as a proof of type safety. The go/types deepening lands with the " +
		"loader.",
	Requires:   []*Analyzer{FlowgraphAnalyzer, SymbolsAnalyzer},
	Run:        runTypeflow,
	ResultType: reflect.TypeOf((*struct{})(nil)),
}

// runTypeflow checks every flow's fan-ins.
func runTypeflow(p *Pass) (any, error) {
	set, ok := p.ResultOf[FlowgraphAnalyzer].(*GraphSet)
	if !ok {
		return nil, errNoGraph
	}
	table, ok := p.ResultOf[SymbolsAnalyzer].(*SymbolTable)
	if !ok {
		return nil, errNoSymbols
	}

	for f := range set.Files {
		for i := range set.Files[f].Graphs {
			graph := &set.Files[f].Graphs[i]
			flow, found := table.Flow(graph.Flow)
			if !found {
				continue
			}
			checkFanIns(p, set.Files[f].Src, graph, declaredTypes(flow))
		}
	}
	return nil, nil
}

// declaredTypes maps each name the language gives a declared type to that type's
// verbatim spelling.
//
// The set is small on purpose, because it is exactly what the language states
// without resolving anything: a flow signature's input type, which the implicit
// input carries, and each declared output's type. A node's output name has no
// declared type anywhere, which is why most edges are untyped.
func declaredTypes(flow *FlowSymbols) map[string]string {
	out := map[string]string{}
	if !flow.HasSignature {
		return out
	}
	// THE DECLARED INPUT IS ONE OF THE TWO STATEMENTS A SIGNATURE MAKES, and the
	// implicit `in` is the name that carries it. Without this entry a fan-in
	// joining the input with a declared output sees ONE identity rather than two
	// and reads as agreement — silence, which is the direction a consumer
	// over-reads. lang/ast declares FlowSignature.Input beside Outputs for the
	// same reason.
	if spelling := strings.TrimSpace(flow.Input.Text); spelling != "" {
		out[implicitInput] = spelling
	}
	for _, output := range flow.Outputs {
		if spelling := strings.TrimSpace(output.Type.Text); spelling != "" {
			out[output.Name.Name] = spelling
		}
	}
	return out
}

// checkFanIns reports a statement joining inputs whose declared identities
// disagree.
func checkFanIns(p *Pass, src Source, graph *FlowGraph, types map[string]string) {
	for i := range graph.Nodes {
		node := &graph.Nodes[i]
		if len(node.Inputs) < 2 {
			continue
		}
		spellings := distinctTypes(node.Inputs, types)
		if len(spellings) < 2 {
			continue
		}
		p.Report(src, Diagnostic{
			Pos: node.Pos,
			End: endOfName(node.Pos, node.Label),
			Message: "the " + node.Kind + " " + node.Label + " in flow " + graph.Flow +
				" joins inputs whose declared types disagree: " + strings.Join(spellings, " and "),
			Severity: SeverityError,
		})
	}
}

// distinctTypes is the set of declared spellings a fan-in's inputs carry, sorted.
//
// An input with no declared type contributes NOTHING rather than a distinct
// empty identity: an untyped edge is silence, and counting it as its own
// identity would turn every ordinary fan-in into a disagreement.
func distinctTypes(inputs []string, types map[string]string) []string {
	seen := map[string]bool{}
	for _, name := range inputs {
		if spelling, ok := types[name]; ok {
			seen[spelling] = true
		}
	}
	out := make([]string, 0, len(seen))
	for spelling := range seen {
		out = append(out, spelling)
	}
	sort.Strings(out)
	return out
}
