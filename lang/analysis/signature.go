// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"reflect"
	"strconv"
	"strings"

	"github.com/whitaker-io/machine/lang/ast"
)

// SignatureAnalyzer checks a flow against its declared signature, and a `use`
// against the signature of the flow it embeds.
var SignatureAnalyzer = &Analyzer{
	Name: "signature",
	Doc: "signature checks that a flow with a declared signature delivers every output it " +
		"declares, and that a use statement binds exactly as many names as the flow it embeds " +
		"declares outputs — bindings are positional, in signature order. The embedded flow's " +
		"outputs travel as a FACT rather than by scanning other sources, because the flow may be " +
		"declared in a different file and facts are the framework's cross-file channel. A use " +
		"whose reference resolves to no flow in the run is NOT reported: a dotted reference may " +
		"name a flow outside the run entirely, and deciding that needs a loader, which the " +
		"structural-first ruling puts out of scope. Output TYPES are not checked here; that is " +
		"typeflow's question.",
	Requires:   []*Analyzer{SymbolsAnalyzer},
	Run:        runSignature,
	ResultType: reflect.TypeOf((*struct{})(nil)),
	FactTypes:  []Fact{(*flowOutputsFact)(nil)},
}

// flowOutputsFact is what one file's flow declaration tells another file's use
// statement: the names this flow hands back, in signature order.
type flowOutputsFact struct {
	Outputs []string
}

// AFact marks flowOutputsFact as a Fact.
func (*flowOutputsFact) AFact() {}

// runSignature exports each flow's declared outputs, then checks deliveries and
// use arities against them.
//
// The two passes are separate because a use statement may precede the flow it
// embeds — in the same file or in a file later in the run — so every fact has to
// exist before any use is judged.
func runSignature(p *Pass) (any, error) {
	table, ok := p.ResultOf[SymbolsAnalyzer].(*SymbolTable)
	if !ok {
		return nil, errNoSymbols
	}

	for f := range table.Files {
		for i := range table.Files[f].Flows {
			exportOutputs(p, &table.Files[f].Flows[i])
		}
	}
	for f := range table.Files {
		for i := range table.Files[f].Flows {
			flow := &table.Files[f].Flows[i]
			reportUndeliveredOutputs(p, table.Files[f].Src, flow)
			reportUseArities(p, table.Files[f].Src, flow)
		}
	}
	return nil, nil
}

// exportOutputs records one flow's declared outputs for any file's use statement
// to import.
func exportOutputs(p *Pass, flow *FlowSymbols) {
	if !flow.HasSignature {
		return
	}
	names := make([]string, 0, len(flow.Outputs))
	for _, out := range flow.Outputs {
		names = append(names, out.Name.Name)
	}
	p.ExportFact(flow.Name, &flowOutputsFact{Outputs: names})
}

// reportUndeliveredOutputs reports each declared output no statement produces.
//
// This is the parser handing work over explicitly: FlowSignature's own
// documentation says "the analysis engine checks that every declared output is
// delivered".
func reportUndeliveredOutputs(p *Pass, src Source, flow *FlowSymbols) {
	for _, out := range flow.Outputs {
		if _, produced := flow.Producers[out.Name.Name]; produced {
			continue
		}
		p.Report(src, Diagnostic{
			Pos:      out.Name.NamePos,
			End:      out.Name.End(),
			Message:  "flow " + flow.Name + " declares the output " + out.Name.Name + " but no statement delivers it",
			Severity: SeverityError,
		})
	}
}

// reportUseArities checks each use statement's bindings against the embedded
// flow's declared outputs.
func reportUseArities(p *Pass, src Source, flow *FlowSymbols) {
	for _, stmt := range flow.Body {
		use, ok := stmt.(ast.UseStmt)
		if !ok {
			continue
		}
		checkUseArity(p, src, flow, use)
	}
}

// checkUseArity imports the embedded flow's outputs and compares counts.
//
// A missing fact is SILENCE rather than a diagnostic: the reference may name a
// flow that is simply not part of this run.
func checkUseArity(p *Pass, src Source, flow *FlowSymbols, use ast.UseStmt) {
	var outputs flowOutputsFact
	if !p.ImportFact(useReference(use), &outputs) {
		return
	}
	if len(use.Bindings) == len(outputs.Outputs) {
		return
	}
	p.Report(src, Diagnostic{
		Pos: use.Instance.NamePos,
		End: use.End(),
		Message: "the use of " + useReference(use) + " in flow " + flow.Name + " binds " +
			strconv.Itoa(len(use.Bindings)) + " names but that flow declares " +
			strconv.Itoa(len(outputs.Outputs)) + " outputs (" + strings.Join(outputs.Outputs, ", ") +
			"), and bindings are positional in signature order",
		Severity: SeverityError,
	})
}

// useReference is the dotted path a use statement names, which is the object key
// a flow's outputs fact is stored under.
func useReference(use ast.UseStmt) string {
	parts := make([]string, 0, len(use.Flow))
	for _, part := range use.Flow {
		parts = append(parts, part.Name)
	}
	return strings.Join(parts, ".")
}
