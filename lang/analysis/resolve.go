// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"reflect"
	"sort"

	"github.com/whitaker-io/machine/lang/ast"
)

// ResolveAnalyzer binds every referenced flow name to the statement that
// declares it.
var ResolveAnalyzer = &Analyzer{
	Name: "resolve",
	Doc: "resolve reports a flow name referenced with no declaration anywhere in its flow, and a " +
		"flow name referenced before the statement that declares it. A send's Target is exempt " +
		"from the ORDERING rule because a send is the only backward arrow in the language, but " +
		"it must still resolve. A func is exempt entirely: funcs are declare-anywhere and " +
		"hoisted, and lang/ast ships func-before-use and func-after-use as VALID fixtures. " +
		"THIS ANALYZER RESOLVES FLOW NAMES ONLY AND SHIPS NO UNIMPORTED-QUALIFIER CHECK, a " +
		"deliberate v1 non-goal with two independent reasons. First, the canonical corpus would " +
		"red: toy.flow writes http.Listen and pubsub.Topic while importing only billing and " +
		"audit. Second, it is undecidable without go/types, which the structural-first ruling " +
		"forbids — a package's NAME is not derivable from its import PATH, and payments.flow " +
		"imports github.com/stripe/stripe-go/v82 while referencing stripe., so a " +
		"last-path-segment heuristic yields v82 and false-flags a correct program.",
	Requires:   []*Analyzer{SymbolsAnalyzer},
	Run:        runResolve,
	ResultType: reflect.TypeOf((*struct{})(nil)),
}

// runResolve checks every flow in every source.
func runResolve(p *Pass) (any, error) {
	table, ok := p.ResultOf[SymbolsAnalyzer].(*SymbolTable)
	if !ok {
		return nil, errNoSymbols
	}
	for f := range table.Files {
		for i := range table.Files[f].Flows {
			resolveFlow(p, table.Files[f].Src, &table.Files[f].Flows[i])
		}
	}
	return nil, nil
}

// resolveFlow reports every unresolved and every out-of-order reference in one
// flow.
//
// Iteration is over sortedKeys rather than the map directly so the diagnostics a
// flow produces do not depend on map ordering. The driver sorts the run's output
// at the end, but a run that emitted a different SET on different iterations
// would be a different problem, and reproducing one is far easier when the
// emission order is fixed too.
func resolveFlow(p *Pass, src Source, flow *FlowSymbols) {
	for _, name := range sortedNames(flow.Consumers) {
		for _, ref := range flow.Consumers[name] {
			if insideBadSpan(flow, ref.Pos) {
				continue
			}
			checkReference(p, src, flow, name, ref)
		}
	}
}

// checkReference reports one reference that does not resolve, or resolves only
// to a later declaration.
func checkReference(p *Pass, src Source, flow *FlowSymbols, name string, ref NameRef) {
	declared, ok := flow.Producers[name]
	if !ok {
		p.Report(src, undefinedName(flow, name, ref))
		return
	}
	if isSendTarget(flow, ref) || declaredBefore(declared, ref) {
		return
	}
	p.Report(src, declaredLater(flow, name, ref, declared[0]))
}

// undefinedName builds the diagnostic for a name nothing declares.
func undefinedName(flow *FlowSymbols, name string, ref NameRef) Diagnostic {
	return Diagnostic{
		Pos:      ref.Pos,
		End:      endOfName(ref.Pos, name),
		Message:  "the name " + name + " is referenced but no statement in flow " + flow.Name + " declares it",
		Severity: SeverityError,
	}
}

// declaredLater builds the diagnostic for a name declared after its use.
func declaredLater(flow *FlowSymbols, name string, ref, first NameRef) Diagnostic {
	return Diagnostic{
		Pos: ref.Pos,
		End: endOfName(ref.Pos, name),
		Message: "the name " + name + " is referenced here but flow " + flow.Name +
			" declares it later, at " + first.Pos.String(),
		Severity: SeverityError,
	}
}

// declaredBefore reports whether any declaration of a name sits at or before the
// referencing statement.
//
// A name declared by the flow SIGNATURE carries an index below every statement's,
// so the implicit input satisfies this without a special case.
func declaredBefore(declared []NameRef, ref NameRef) bool {
	for _, d := range declared {
		if d.Stmt <= ref.Stmt {
			return true
		}
	}
	return false
}

// isSendTarget reports whether a reference is a send's TARGET rather than its
// source.
//
// The two are told apart by POSITION rather than by name, so `send x -> x`
// exempts only the target half. SendStmt's own documentation is the reason the
// exemption exists: its "target may be a node declared earlier or a loop label",
// which is precisely a backward reference.
func isSendTarget(flow *FlowSymbols, ref NameRef) bool {
	if ref.Stmt < 0 || ref.Stmt >= len(flow.Body) {
		return false
	}
	send, ok := flow.Body[ref.Stmt].(ast.SendStmt)
	return ok && send.Target.NamePos == ref.Pos
}

// insideBadSpan reports whether a position falls in a region the parser already
// reported on.
//
// Cascading resolution noise across a half-typed line is what the error-tolerant
// parse exists to avoid: the parser hands back a complete tree with the damage
// localized, and an analyzer that then reported every name inside the damage
// would undo that.
//
// AGAINST TODAY'S PARSER THIS NEVER FIRES, and saying so is more useful than
// letting a reader assume it is load bearing. A BadStmt holds the span of tokens
// the parser SKIPPED, and skipped tokens are never parsed into identifiers, so
// they contribute nothing to the consumer table — every position a diagnostic
// can carry comes from a statement the parser successfully built. Measured over
// both modules' corpora: 42 files parse to a tree, 7 of them carry a BadStmt,
// and 0 references sit inside one.
//
// It is kept as a guard on the parser's recovery CONTRACT rather than removed,
// because a recovery that one day retains partially-parsed elements inside a bad
// region would make it live immediately. TestBadSpanSuppressionDiscriminates
// exercises the predicate directly and records the corpus observation, so the
// guard is tested even though no fixture reaches it through an analyzer.
func insideBadSpan(flow *FlowSymbols, pos ast.Position) bool {
	for _, bad := range flow.Bad {
		if pos.Offset >= bad.Start.Offset && pos.Offset < bad.Stop.Offset {
			return true
		}
	}
	return false
}

// endOfName is the position just past an identifier, which ast.Ident computes
// the same way.
func endOfName(pos ast.Position, name string) ast.Position {
	return ast.Position{Offset: pos.Offset + len(name), Line: pos.Line, Col: pos.Col + len(name)}
}

// sortedFieldNames is a state-field table's keys in a stable order.
func sortedFieldNames(fields map[string]ast.StateField) []string {
	out := make([]string, 0, len(fields))
	for name := range fields {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sortInts orders a slice of statement indices in place.
func sortInts(v []int) { sort.Ints(v) }

// sortedNames is a name table's keys in a stable order.
func sortedNames(refs map[string][]NameRef) []string {
	out := make([]string, 0, len(refs))
	for name := range refs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
