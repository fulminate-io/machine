// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"reflect"
	"strings"
)

// retiredStateTypes are the two wrapper spellings a state field may no longer
// use. This is the WHOLE of the bare-type check.
var retiredStateTypes = []string{"cell[", "key["}

// StateAnalyzer checks how a flow reaches its vars and its state block.
var StateAnalyzer = &Analyzer{
	Name: "state",
	Doc: "state checks four things about a flow's declared storage. Every reads and writes clause " +
		"must name a declared var or state field; a name read while NOTHING anywhere writes it is " +
		"an error; a VAR written and never read is an error; and a state field's type must not use " +
		"a retired wrapper spelling. READING BEFORE THE FIRST WRITE IS LEGAL and is never " +
		"reported — flow-level vars carry Go's zero-value semantics, so the rule is " +
		"ordering-free. The written-never-read rule is scoped to VARS and never to state fields: " +
		"a var is fresh per datum and copied per tee branch, so a write nobody reads evaporates, " +
		"while shared state is written precisely so something outside the flow can observe it. " +
		"THE BARE-TYPE CHECK IS A DENYLIST OF TWO RETIRED SPELLINGS, cell[ and key[, and NOT type " +
		"validation: this module does no Go-type resolution, so it cannot tell whether a state " +
		"field's type is a real Go type at all, and full validation arrives when the loader lands " +
		"go/types feeding. A clean state result therefore does not mean every state type is valid.",
	Requires:   []*Analyzer{SymbolsAnalyzer},
	Run:        runState,
	ResultType: reflect.TypeOf((*struct{})(nil)),
}

// runState checks every flow in every source.
func runState(p *Pass) (any, error) {
	table, ok := p.ResultOf[SymbolsAnalyzer].(*SymbolTable)
	if !ok {
		return nil, errNoSymbols
	}
	for f := range table.Files {
		for i := range table.Files[f].Flows {
			flow := &table.Files[f].Flows[i]
			src := table.Files[f].Src
			reportUndeclaredAccess(p, src, flow)
			reportUnwrittenReads(p, src, flow)
			reportUnreadVarWrites(p, src, flow)
			reportRetiredStateTypes(p, src, flow)
		}
	}
	return nil, nil
}

// declares reports whether a name is a declared var or state field.
func declares(flow *FlowSymbols, name string) bool {
	if _, isVar := flow.Vars[name]; isVar {
		return true
	}
	_, isField := flow.State[name]
	return isField
}

// reportUndeclaredAccess reports a reads or writes clause naming something the
// flow never declared.
func reportUndeclaredAccess(p *Pass, src Source, flow *FlowSymbols) {
	for _, clause := range []map[string][]NameRef{flow.Reads, flow.Writes} {
		for _, name := range sortedNames(clause) {
			if declares(flow, name) {
				continue
			}
			for _, ref := range clause[name] {
				p.Report(src, Diagnostic{
					Pos:      ref.Pos,
					End:      endOfName(ref.Pos, name),
					Message:  "flow " + flow.Name + " declares no var or state field named " + name,
					Severity: SeverityError,
				})
			}
		}
	}
}

// reportUnwrittenReads reports a declared name some statement reads while no
// statement anywhere writes it.
//
// THIS IS NOT AN ORDERING CHECK, and the distinction is the ruling. Reading a
// var before the first write to it is LEGAL — vars carry Go's zero-value
// semantics — so a rule asking "does anyone write this before that node reads
// it" would fire on correct programs. What is flagged is the ordering-free case:
// a value nothing ever produces.
func reportUnwrittenReads(p *Pass, src Source, flow *FlowSymbols) {
	for _, name := range sortedNames(flow.Reads) {
		if !declares(flow, name) || len(flow.Writes[name]) > 0 {
			continue
		}
		ref := flow.Reads[name][0]
		p.Report(src, Diagnostic{
			Pos:      ref.Pos,
			End:      endOfName(ref.Pos, name),
			Message:  name + " is read in flow " + flow.Name + " but no statement anywhere writes it",
			Severity: SeverityError,
		})
	}
}

// reportUnreadVarWrites reports a VAR written by some statement and read by
// none.
//
// SCOPED TO VARS, NEVER TO STATE FIELDS, and the scoping is load bearing rather
// than convenient. A var is fresh per datum and copied per tee branch, so a
// write nobody reads is a value that evaporates. A state entry is shared across
// datums and is written precisely so something outside the flow can observe it,
// so the same shape there is the normal case — every written-never-read name in
// the canonical corpus is a state field, and a rule including them would red all
// three canonical programs.
func reportUnreadVarWrites(p *Pass, src Source, flow *FlowSymbols) {
	for _, name := range sortedNames(flow.Writes) {
		if _, isVar := flow.Vars[name]; !isVar {
			continue
		}
		if len(flow.Reads[name]) > 0 {
			continue
		}
		ref := flow.Writes[name][0]
		p.Report(src, Diagnostic{
			Pos: ref.Pos,
			End: endOfName(ref.Pos, name),
			Message: "the var " + name + " is written in flow " + flow.Name + " and never read; a var is fresh " +
				"per datum and copied per tee branch, so a value accumulating across datums belongs in the state block",
			Severity: SeverityError,
		})
	}
}

// reportRetiredStateTypes reports a state field spelled with a retired wrapper.
//
// A DENYLIST OF TWO NAMES, NOT TYPE VALIDATION. Without go/types this module
// cannot tell a real Go type from a typo, so it checks the one thing it can see:
// that the declared type does not begin with a spelling the language retired.
func reportRetiredStateTypes(p *Pass, src Source, flow *FlowSymbols) {
	for _, name := range sortedFieldNames(flow.State) {
		field := flow.State[name]
		spelling := strings.TrimSpace(field.Type.Text)
		for _, retired := range retiredStateTypes {
			if !strings.HasPrefix(spelling, retired) {
				continue
			}
			p.Report(src, Diagnostic{
				Pos: field.Type.Start,
				End: field.Type.Stop,
				Message: "the state field " + name + " uses the retired wrapper spelling " + retired +
					"...]; state entries hold bare Go types, and a per-datum value belongs in a flow-body var",
				Severity: SeverityError,
			})
		}
	}
}
