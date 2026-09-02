// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"reflect"
	"sort"
	"strings"

	"github.com/whitaker-io/machine/lang/ast"
)

// SignatureAnalyzer checks a flow against its declared signature, and a `use`
// against the boundary of the flow it embeds.
var SignatureAnalyzer = &Analyzer{
	Name: "signature",
	Doc: "signature checks that a flow with a declared signature delivers every output it " +
		"declares, and that a use statement binds its dependency's outputs BY NAME. Each binding " +
		"identifier names one of the embedded flow's outputs: the bindings are a SET, so order is " +
		"irrelevant and binding a subset is legal, an identifier naming no output is reported " +
		"against the flow's real output names, and a duplicate is refused. There is no count " +
		"check, because a count is meaningful only under an ordering the source never states. The " +
		"embedded flow's boundary travels as a FACT rather than by scanning other sources, because " +
		"the flow may be declared in a different file and facts are the framework's cross-file " +
		"channel. EVERY flow exports one, header or not: a declared signature SELECTS the public " +
		"subset, and a flow without one still has a boundary — every name its body produces is " +
		"bindable, which is what makes a signature-less flow a checkable dependency rather than a " +
		"hole. A use whose reference resolves to no flow in the run is NOT reported: a dotted " +
		"reference may name a flow outside the run entirely, and deciding that needs a loader, " +
		"which the structural-first ruling puts out of scope. Output TYPES are not checked here; " +
		"that is typeflow's question.",
	Requires:   []*Analyzer{SymbolsAnalyzer},
	Run:        runSignature,
	ResultType: reflect.TypeOf((*struct{})(nil)),
	FactTypes:  []Fact{(*flowOutputsFact)(nil)},
}

// flowOutputsFact is what one file's flow declaration tells another file's use
// statement: the boundary this flow hands back.
//
// Outputs is the header's DECLARED names and stays empty for a flow with no
// signature, because only a header states a declared set. Produced is every name
// the body produces together with the statement it connects from, and it is
// exported for EVERY flow. A signature-less flow has a boundary too — the names
// its body produces and what they connect from — and erasing it is what left a
// headerless dependency unbindable and therefore uncheckable.
type flowOutputsFact struct {
	Outputs  []string
	Produced []producedName
}

// producedName is one name a flow's body produces and the statement producing it.
type producedName struct {
	Name string
	Stmt int
}

// AFact marks flowOutputsFact as a Fact.
func (*flowOutputsFact) AFact() {}

// runSignature exports each flow's boundary, then checks deliveries and use
// bindings against it.
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
			reportUseBindings(p, table.Files[f].Src, flow)
		}
	}
	return nil, nil
}

// exportOutputs records one flow's boundary for any file's use statement to
// import.
//
// THERE IS NO EARLY RETURN FOR A SIGNATURE-LESS FLOW. Returning here was erasure:
// the body already declares named outputs and what they connect from, so a flow
// without a header still has a concrete boundary, and dropping it is what made a
// use of such a flow unbindable and therefore silently unchecked.
func exportOutputs(p *Pass, flow *FlowSymbols) {
	fact := &flowOutputsFact{Produced: producedBoundary(flow)}
	if flow.HasSignature {
		fact.Outputs = make([]string, 0, len(flow.Outputs))
		for _, out := range flow.Outputs {
			fact.Outputs = append(fact.Outputs, out.Name.Name)
		}
	}
	p.ExportFact(flow.Name, fact)
}

// producedBoundary is every name the flow's body produces, with the statement it
// connects from, ordered by that statement so the boundary reads in body order.
//
// NOTHING IS FILTERED OUT. The obvious derivation — produced names that no
// statement consumes — was measured against this tree and rejected: a declared
// output is consumed by the CALLER rather than by a statement in the flow, so
// two valid fixtures that declare `ok` and `bad` and also join them at a sink
// report an EMPTY boundary under it. The body carries no marker distinguishing a
// name handed back from one finished with internally, so every produced name is
// forwarded and the header, where there is one, does the selecting.
//
// The implicit signature input is not a produced name: the symbol table records
// it against signatureStmt rather than a statement, which is what excludes it.
func producedBoundary(flow *FlowSymbols) []producedName {
	type located struct {
		producedName
		offset int
	}
	found := make([]located, 0, len(flow.Producers))
	for name, refs := range flow.Producers {
		for _, ref := range refs {
			if ref.Stmt == signatureStmt {
				continue
			}
			found = append(found, located{producedName{Name: name, Stmt: ref.Stmt}, ref.Pos.Offset})
			break
		}
	}
	// SOURCE OFFSET, not name, is the tiebreak within one statement. A branch
	// declaring `-> ok, bad` produces both names at the same statement index, and
	// ordering those alphabetically would report a boundary in an order the author
	// never wrote. Ranging a map is unordered, so some total order is required;
	// this is the one that reads back as the source.
	sort.Slice(found, func(i, j int) bool {
		if found[i].Stmt != found[j].Stmt {
			return found[i].Stmt < found[j].Stmt
		}
		return found[i].offset < found[j].offset
	})
	out := make([]producedName, 0, len(found))
	for _, f := range found {
		out = append(out, f.producedName)
	}
	return out
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

// reportUseBindings checks each use statement's bindings against the embedded
// flow's boundary.
func reportUseBindings(p *Pass, src Source, flow *FlowSymbols) {
	for _, stmt := range flow.Body {
		use, ok := stmt.(ast.UseStmt)
		if !ok {
			continue
		}
		checkUseBindings(p, src, flow, use)
	}
}

// useContext is the per-use state a binding check reads.
//
// It is a struct rather than more parameters because the obvious shape takes
// seven arguments — pass, source, flow, use, the bindable set, the real names
// and the names already seen — and the pinned linter's argument limit is five.
type useContext struct {
	src      Source
	flow     *FlowSymbols
	use      ast.UseStmt
	bindable map[string]bool
	names    []string
	seen     map[string]bool
}

// checkUseBindings imports the embedded flow's boundary and checks every binding
// identifier NAMES one of its outputs.
//
// BINDINGS ARE A SET, not a sequence. Order carries no meaning, binding a subset
// is legal — a caller wanting one of two outputs says so by naming it — and there
// is no count check, because a count is only meaningful under an ordering the
// source never states.
//
// A missing fact is SILENCE rather than a diagnostic: the reference may name a
// flow that is simply not part of this run. An empty bindable set is silence for
// the same reason — there is nothing to check a name against.
func checkUseBindings(p *Pass, src Source, flow *FlowSymbols, use ast.UseStmt) {
	var outputs flowOutputsFact
	if !p.ImportFact(useReference(use), &outputs) {
		return
	}
	ctx := &useContext{src: src, flow: flow, use: use, names: bindableNames(outputs), seen: map[string]bool{}}
	if len(ctx.names) == 0 {
		return
	}
	ctx.bindable = make(map[string]bool, len(ctx.names))
	for _, name := range ctx.names {
		ctx.bindable[name] = true
	}
	for _, binding := range use.Bindings {
		reportBinding(p, ctx, binding)
	}
}

// bindableNames is what a use may bind: the DECLARED outputs when the flow has a
// header, and everything its body produces when it does not.
//
// A header SELECTS the public subset, which is the whole reason to declare one.
// Without a header there is no declared set to select with, so the boundary the
// body itself states is what a consumer binds against.
func bindableNames(outputs flowOutputsFact) []string {
	if len(outputs.Outputs) > 0 {
		return outputs.Outputs
	}
	names := make([]string, 0, len(outputs.Produced))
	for _, produced := range outputs.Produced {
		names = append(names, produced.Name)
	}
	return names
}

// reportBinding reports one binding identifier that names no output of the
// embedded flow, or that repeats one already bound.
func reportBinding(p *Pass, ctx *useContext, binding ast.Ident) {
	message := ""
	switch {
	case ctx.seen[binding.Name]:
		message = " binds " + binding.Name + " more than once; each output is bound at most once"
	case !ctx.bindable[binding.Name]:
		message = " binds " + binding.Name + ", which is not an output of that flow; its outputs are " +
			strings.Join(ctx.names, ", ")
	default:
		ctx.seen[binding.Name] = true
		return
	}
	ctx.seen[binding.Name] = true
	p.Report(ctx.src, Diagnostic{
		Pos:      binding.NamePos,
		End:      binding.End(),
		Message:  "the use of " + useReference(ctx.use) + " in flow " + ctx.flow.Name + message,
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
