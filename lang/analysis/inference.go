// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"fmt"
	"go/token"
	"go/types"
	"reflect"
	"sort"
	"strings"

	"github.com/whitaker-io/machine/lang/ast"
	"github.com/whitaker-io/machine/lang/loader"
)

// environment is one FLOW FILE's import set expressed as a Go scope a spelling
// can be evaluated in.
//
// THE PROBLEM IT SOLVES: a flow file's Go reference must be evaluated under THAT
// FILE'S imports, and no Go package with that import set exists anywhere on disk.
// Resolving `stripe.IsPayment` against the package that DECLARES stripe refuses
// with `undefined: stripe`, because inside its own package a package name is not
// in scope. The route that works is to build a synthetic package holding one
// types.PkgName per import the flow file declared, and evaluate the verbatim
// spelling there.
//
// One environment is built per FILE rather than per node: construction costs
// ~692ns against a ~1.7µs simple evaluation, and the memo makes repeated
// spellings free — the corpus repeats them heavily.
type environment struct {
	pkg  *types.Package
	fset *token.FileSet
	memo map[string]types.Type
}

// newEnvironment builds one file's evaluation scope from its import declarations.
//
// An import naming a package the loader never loaded, or one whose scope declares
// nothing, binds NOTHING rather than binding a guess: the reference that needed it
// then refuses by name downstream, which is a reported refusal instead of a wrong
// type presented as an answer.
func newEnvironment(pkgs *loader.Packages, file *FileSymbols) *environment {
	env := &environment{
		pkg:  types.NewPackage(syntheticPath, syntheticName),
		fset: token.NewFileSet(),
		memo: map[string]types.Type{},
	}

	for _, imp := range file.Imports {
		pkg, ok := packageOf(pkgs, strings.Trim(imp.Path, `"`))
		if !ok {
			continue
		}

		name := imp.Alias
		if name == "" {
			name = pkg.Name()
		}

		env.pkg.Scope().Insert(types.NewPkgName(token.NoPos, env.pkg, name, pkg))
	}
	env.pkg.MarkComplete()

	return env
}

// syntheticPath and syntheticName identify the package a flow file's references
// are evaluated in. It holds nothing but the file's imports.
const (
	syntheticPath = "flowanalysis"
	syntheticName = "flowanalysis"
)

// packageOf recovers a loaded package through the loader's exported Scope alone,
// which is what keeps package LOADING inside lang/loader.
//
// A PACKAGE'S DECLARED NAME IS NOT DERIVABLE FROM ITS PATH: the corpus's own
// github.com/stripe/stripe-go/v82 is named stripe, and a last-segment guess yields
// "v82". Any object the scope declares knows the package it belongs to, so the
// first one recovers the real *types.Package — name included — without standing up
// a second loading surface beside the loader's.
func packageOf(pkgs *loader.Packages, path string) (*types.Package, bool) {
	scope, loaded := pkgs.Scope(path)
	if !loaded {
		return nil, false
	}

	names := scope.Names()
	if len(names) == 0 {
		return nil, false
	}

	obj := scope.Lookup(names[0])
	if obj == nil || obj.Pkg() == nil {
		return nil, false
	}

	return obj.Pkg(), true
}

// eval resolves one verbatim spelling in this file's environment, memoized.
//
// BOTH REFUSALS NAME THE SPELLING and neither hands back a type beside a nil
// error. types.Eval reports an invalid type for a spelling it cannot make sense
// of, and a caller cannot tell that shape from a real answer — which is what
// makes returning it a defect rather than a convenience.
func (e *environment) eval(spelling string) (types.Type, error) {
	if hit, ok := e.memo[spelling]; ok {
		return hit, nil
	}

	evaluated, err := types.Eval(e.fset, e.pkg, token.NoPos, spelling)
	if err != nil {
		return nil, fmt.Errorf("the reference %s does not resolve: %w", spelling, err)
	}

	if evaluated.Type == nil || evaluated.Type == types.Typ[types.Invalid] {
		return nil, fmt.Errorf("the reference %s resolves to no type", spelling)
	}

	e.memo[spelling] = evaluated.Type

	return evaluated.Type, nil
}

// refOf reads the Go reference off the statement at stmt, when it carries one.
//
// EVERY CASE IS THE VALUE FORM, matching what lang/ast stores in a Body slice. A
// pointer-form case compiles clean and is silently always false.
//
// The default arm returns not-found SILENTLY, which is correct rather than a
// swallowed error: a tee, drop, loop, send, switch or use legitimately names no
// reference, and an unknown statement shape is already reported loudly upstream.
func refOf(body []ast.Stmt, stmt int) (ast.GoSpan, bool) {
	if stmt < 0 || stmt >= len(body) {
		return ast.GoSpan{}, false
	}

	switch s := body[stmt].(type) {
	case ast.SourceStmt:
		return s.Ref, true
	case ast.TransformStmt:
		return s.Ref, true
	case ast.BranchStmt:
		return s.Ref, true
	case ast.SinkStmt:
		return s.Ref, true
	default:
		return ast.GoSpan{}, false
	}
}

// spellingOf is the Go text a reference evaluates as.
//
// A BARE reference naming a func the flow file itself declares resolves to that
// func's literal: lang/ast captures FuncDecl.Body as one opaque span running from
// the opening parenthesis through the matching close brace, so the span IS the
// literal minus its `func` keyword. Every other reference is handed through
// verbatim — one door, no second mechanism.
func spellingOf(file *FileSymbols, ref ast.GoSpan) string {
	text := strings.TrimSpace(ref.Text)
	if decl, declared := file.Funcs[text]; declared {
		return "func" + decl.Body.Text
	}

	return text
}

// carried is the payload a resolved reference hands downstream.
//
// A signature contributes its FIRST NON-ERROR RESULT, because that is the datum
// the next node receives. Anything else IS its own type, which is the shape a
// generic instantiation call such as `Listen[Order](":8080")` takes: it has
// already been applied, so its type is the value it yields.
func carried(resolved types.Type) types.Type {
	sig, ok := resolved.(*types.Signature)
	if !ok {
		return resolved
	}

	results := sig.Results()
	for i := range results.Len() {
		if result := results.At(i).Type(); !isError(result) {
			return result
		}
	}

	return nil
}

// isError reports whether a type IS the universe's error interface.
//
// It is compared by identity against types.Universe rather than by matching the
// name "error": a name match would be a text comparison of the kind this whole
// analyzer exists to replace, and the literal is a goconst violation against the
// pre-existing diagnostic.go besides.
func isError(typ types.Type) bool {
	return types.Identical(typ, types.Universe.Lookup("error").Type())
}

// inference is one flow's propagation state.
//
// It is a struct rather than a parameter list because the walk needs the
// environment, the file, the flow, its graph, the types settled so far, the nodes
// already refused and the reporting channel — and the pinned linter's argument
// limit is five.
type inference struct {
	env     *environment
	file    *FileSymbols
	flow    *FlowSymbols
	graph   *FlowGraph
	typed   map[string]types.Type
	refused map[int]bool
	report  func(node *GraphNode, reason error)
}

// run propagates types along the flowgraph to a fixed point.
//
// THE WALK IS A FIXED POINT AND ONE FORWARD PASS IS NOT ENOUGH. A send is the
// language's only backward arrow, so a loop label takes its type from a send
// declared BELOW it, and a single statement-order pass leaves every loop-fed name
// untyped. The bound is one round per node, which is the longest chain any graph
// can hold, and it breaks as soon as a round moves nothing. Every round after the
// first is map lookups, because reference evaluation is memoized on the file's
// environment.
func (in *inference) run() {
	for range in.graph.Nodes {
		if !in.round() {
			return
		}
	}
}

// round assigns every node it can and reports whether anything moved.
func (in *inference) round() bool {
	moved := false
	for i := range in.graph.Nodes {
		if in.assign(&in.graph.Nodes[i]) {
			moved = true
		}
	}

	return moved
}

// assign gives a node's output names the type that node carries.
func (in *inference) assign(node *GraphNode) bool {
	carriedType, known := in.typeFor(node)
	if !known {
		return false
	}

	moved := false
	for _, name := range node.Outputs {
		if _, settled := in.typed[name]; settled {
			continue
		}

		in.typed[name] = carriedType
		moved = true
	}

	return moved
}

// typeFor is the KIND-AWARE half of propagation, and the half a plausible-looking
// implementation gets wrong.
//
// A source and a transform APPLY their reference, so they hand downstream what
// that reference yields. EVERY OTHER SHAPE IS PASS-THROUGH and carries the type of
// its first typed input: a branch routes on a predicate and a switch on a subject,
// and neither CONVERTS the datum, so a branch target carries the branch's INPUT
// type and never the predicate's bool. Taking each node's own reference result is
// the naive reading; it compiles, builds clean, and types every branch target
// bool.
func (in *inference) typeFor(node *GraphNode) (types.Type, bool) {
	switch node.Kind {
	case kindSource, kindTransform:
		return in.applied(node)
	default:
		return in.passedThrough(node)
	}
}

// applied resolves a node's own reference and takes what it yields.
func (in *inference) applied(node *GraphNode) (types.Type, bool) {
	ref, carriesRef := refOf(in.flow.Body, node.Stmt)
	if !carriesRef {
		return nil, false
	}

	resolved, err := in.env.eval(spellingOf(in.file, ref))
	if err != nil {
		in.refuse(node, err)

		return nil, false
	}

	yielded := carried(resolved)

	return yielded, yielded != nil
}

// passedThrough takes the type of a node's first typed input.
func (in *inference) passedThrough(node *GraphNode) (types.Type, bool) {
	for _, name := range node.Inputs {
		if settled, known := in.typed[name]; known {
			return settled, true
		}
	}

	return nil, false
}

// refuse reports an unresolvable reference ONCE per node and leaves its names
// untyped, rather than handing back a guess beside a nil error.
func (in *inference) refuse(node *GraphNode, reason error) {
	if in.refused[node.Stmt] {
		return
	}

	in.refused[node.Stmt] = true
	in.report(node, reason)
}

// checkTypedFanIns reports a statement joining inputs whose INFERRED types
// disagree.
//
// THIS IS THE DEEPENING THE STRUCTURAL typeflow ANALYZER COULD NOT DO, and that
// analyzer is left exactly as it is: it stays registered, stays cheap, stays
// structural, and keeps its Doc's disclosure that it is not type checking. Two
// analyzers asking the same question of different inputs is the deepening;
// retrofitting the structural one would falsify a shipped disclosure.
func (in *inference) checkTypedFanIns(report func(Diagnostic)) {
	for i := range in.graph.Nodes {
		node := &in.graph.Nodes[i]
		if len(node.Inputs) < 2 {
			continue
		}

		rendered := in.distinctInferred(node.Inputs)
		if len(rendered) < 2 {
			continue
		}

		report(Diagnostic{
			Pos: node.Pos,
			End: endOfName(node.Pos, node.Label),
			Message: "the " + node.Kind + " " + node.Label + " in flow " + in.graph.Flow +
				" joins inputs whose inferred types disagree: " + strings.Join(rendered, " and "),
			Severity: SeverityError,
		})
	}
}

// distinctInferred renders the set of inferred identities a fan-in's inputs
// carry, sorted.
//
// DISTINCTNESS IS types.Identical, not string comparison: that is the whole point
// of inferring real types rather than comparing spellings, since two spellings
// may denote one type and one spelling may denote two.
//
// An input with no inferred type contributes NOTHING rather than a distinct empty
// identity, matching the structural analyzer's distinctTypes: counting absence as
// its own identity turns every ordinary fan-in into a disagreement.
func (in *inference) distinctInferred(inputs []string) []string {
	seen := make([]types.Type, 0, len(inputs))

	for _, name := range inputs {
		settled, known := in.typed[name]
		if !known || identicalToAny(seen, settled) {
			continue
		}

		seen = append(seen, settled)
	}

	rendered := make([]string, 0, len(seen))
	for _, typ := range seen {
		rendered = append(rendered, types.TypeString(typ, nil))
	}
	sort.Strings(rendered)

	return rendered
}

// identicalToAny reports whether typ is already present in seen by type identity.
func identicalToAny(seen []types.Type, typ types.Type) bool {
	for _, already := range seen {
		if types.Identical(already, typ) {
			return true
		}
	}

	return false
}

// inferenceName is the analyzer's name and the Code every diagnostic it reports
// carries.
const inferenceName = "typeinference"

// inferenceDoc is a constant so a gate can assert the text without constructing
// an analyzer, which this one cannot be without a caller-supplied package set.
const inferenceDoc = "typeinference resolves every node's Go reference to a real types.Type through the " +
	"loader's loaded packages and propagates those types along the derived flowgraph, reporting a " +
	"fan-in whose inputs disagree over real type IDENTITY rather than over spellings. IT IS NOT " +
	"REGISTERED AND THAT IS THE DESIGN: it is built by a constructor because it needs a " +
	"*loader.Packages the caller owns, and Pass has no channel for a caller-supplied value. " +
	"Registering it would either force a Pass field change every consumer must answer for, or leave " +
	"it silent whenever no package set was supplied, which is a silently degraded lane. THE EXPORTED " +
	"SURFACE IS AN OPT-IN STABILITY CONTRACT: a caller that constructs this analyzer accepts that " +
	"loading real packages costs seconds and is done once per run, not once per keystroke — the " +
	"registered structural analyzers are what an editor runs. A reference that does not resolve is " +
	"REPORTED and its name is left untyped; nothing here hands back a guess beside a nil error."

// TypeInferenceAnalyzer builds the inference pass over a caller-supplied package
// set.
//
// IT IS A CONSTRUCTOR RATHER THAN A REGISTERED VAR because the *loader.Packages it
// needs is owned by the caller above both modules, and loader.Load is seconds of
// work called ONCE per generation run. All() therefore still returns twelve, and
// the shipped-roster test pins that.
func TypeInferenceAnalyzer(pkgs *loader.Packages) *Analyzer {
	return &Analyzer{
		Name:       inferenceName,
		Doc:        inferenceDoc,
		Requires:   []*Analyzer{SymbolsAnalyzer, FlowgraphAnalyzer},
		Run:        func(p *Pass) (any, error) { return runInference(p, pkgs) },
		ResultType: reflect.TypeOf((*InferredTypes)(nil)),
	}
}

// InferredTypes is every flow's inferred boundary, keyed by flow-level NAME.
//
// PER NAME IS THE RULED SHAPE. A flow's boundary is its named outputs and what
// they connect from, and a consumer binds them BY NAME, so there is no positional
// order to invent and none is offered.
type InferredTypes struct {
	flows map[string]map[string]types.Type
	order []string
}

// Flow returns every inferred name in one flow, and whether that flow was seen.
func (t *InferredTypes) Flow(flow string) (map[string]types.Type, bool) {
	names, ok := t.flows[flow]

	return names, ok
}

// Name returns one flow-level name's inferred type, and whether it has one.
//
// A name with no inferred type is reported as ABSENT rather than as a nil type
// beside a true: a caller cannot tell that shape from a real answer.
func (t *InferredTypes) Name(flow, name string) (types.Type, bool) {
	names, ok := t.flows[flow]
	if !ok {
		return nil, false
	}

	settled, known := names[name]

	return settled, known
}

// Flows lists every flow the run inferred, in the order the run walked them.
func (t *InferredTypes) Flows() []string { return t.order }

// BuildInferredTypes runs the inference once and hands back the built table with
// the diagnostics it reported.
//
// This extends the seam guidance.go's BuildGuidance established: an anonymous
// capture analyzer names what it wants in Requires, runs through the REAL driver,
// and lifts the built table out of Pass.ResultOf, so the prerequisite ordering
// stays the driver's rather than becoming a second copy of it.
//
// A NIL PACKAGE SET IS REFUSED rather than yielding an empty table beside a nil
// error, which a caller cannot tell from a run that inferred nothing.
func BuildInferredTypes(srcs []Source, pkgs *loader.Packages) (*InferredTypes, []Diagnostic, error) {
	if pkgs == nil {
		return nil, nil, errNoPackages
	}

	var table *InferredTypes

	inference := TypeInferenceAnalyzer(pkgs)
	capture := &Analyzer{
		Name:     "typeinference-capture",
		Doc:      "captures the inferred type table out of one driver run",
		Requires: []*Analyzer{inference},
		Run: func(p *Pass) (any, error) {
			built, ok := p.ResultOf[inference].(*InferredTypes)
			if !ok {
				return nil, errNoInferredTypes
			}
			table = built

			return nil, nil
		},
	}

	diags, err := Run(srcs, []*Analyzer{capture})
	if err != nil {
		return nil, nil, err
	}

	if table == nil {
		return nil, nil, errNoInferredTypes
	}

	return table, diags, nil
}

// runInference infers every flow in the run, one environment per FILE.
func runInference(p *Pass, pkgs *loader.Packages) (any, error) {
	symbols, ok := p.ResultOf[SymbolsAnalyzer].(*SymbolTable)
	if !ok {
		return nil, errNoSymbols
	}

	graphs, ok := p.ResultOf[FlowgraphAnalyzer].(*GraphSet)
	if !ok {
		return nil, errNoGraphs
	}

	if len(symbols.Files) != len(graphs.Files) {
		return nil, errFileMismatch
	}

	table := &InferredTypes{flows: map[string]map[string]types.Type{}}
	for f := range symbols.Files {
		inferFile(p, pkgs, fileRun{symbols: &symbols.Files[f], graphs: &graphs.Files[f]}, table)
	}

	return table, nil
}

// fileRun pairs one file's symbol table with its derived graphs.
//
// They are matched BY INDEX because both are built by walking p.Sources in order,
// and runInference refuses outright when the two lengths disagree rather than
// pairing a flow with another file's graph.
type fileRun struct {
	symbols *FileSymbols
	graphs  *FileGraphs
}

// inferFile builds one environment for the file and infers each of its flows.
func inferFile(p *Pass, pkgs *loader.Packages, file fileRun, table *InferredTypes) {
	env := newEnvironment(pkgs, file.symbols)

	for i := range file.symbols.Flows {
		flow := &file.symbols.Flows[i]

		graph, found := graphOf(file.graphs, flow.Name)
		if !found {
			continue
		}

		in := &inference{
			env: env, file: file.symbols, flow: flow, graph: graph,
			typed: map[string]types.Type{}, refused: map[int]bool{},
			report: func(node *GraphNode, reason error) {
				p.Report(file.symbols.Src, Diagnostic{
					Pos: node.Pos, End: endOfName(node.Pos, node.Label),
					Message: "the " + node.Kind + " " + node.Label + " in flow " + flow.Name +
						" has no inferred type: " + reason.Error(),
					Severity: SeverityWarning,
				})
			},
		}
		in.run()
		in.checkTypedFanIns(func(d Diagnostic) { p.Report(file.symbols.Src, d) })

		table.flows[flow.Name] = in.typed
		table.order = append(table.order, flow.Name)
	}
}

// graphOf finds one flow's derived graph within the file that declared it.
//
// It is scoped to the FILE rather than reaching through GraphSet.Graph, which
// searches the whole run: two files may declare flows of the same name, and a
// flow must be typed against the imports of the file it was written in.
func graphOf(file *FileGraphs, flow string) (*FlowGraph, bool) {
	for i := range file.Graphs {
		if file.Graphs[i].Flow == flow {
			return &file.Graphs[i], true
		}
	}

	return nil, false
}
