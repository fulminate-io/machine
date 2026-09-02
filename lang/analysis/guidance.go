// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"errors"
	"reflect"
	"sort"

	"github.com/whitaker-io/machine/lang/ast"
)

// errNoGuidance is returned when a guidance build produced no table, which can
// only happen if the analyzer's result type changed without this file following.
var errNoGuidance = errors.New("the guidance analyzer produced no table")

// GuidanceAnalyzer computes what may legally be named at each point in a source.
//
// THIS MODULE COMPUTES GUIDANCE AND SERVES NOTHING. The ruling splits ownership:
// the analysis core holds the facts, and the LSP owns the wire surface that
// serves them to editors and to generating models. So this file ships a
// computation and a typed accessor, and nothing resembling a protocol, an
// endpoint, a serialization format or a server. A design question here that
// cannot be answered without knowing the wire format is a question for the LSP.
var GuidanceAnalyzer = &Analyzer{
	Name: "guidance",
	Doc: "guidance computes, for every point in a source, which identifiers are legal there: the " +
		"producer names in scope under the declare-before-use rule, the vars and state fields a " +
		"reads or writes clause may name, and the imports available to a Go reference. The table " +
		"is built ONCE per run and the accessor is a bounded lookup over it, because an editor " +
		"calls the accessor per keystroke rather than per pass — the module's usual one-walk-per- " +
		"analyzer shape is about pass latency and does not apply to a query path.",
	Requires:   []*Analyzer{SymbolsAnalyzer, FlowgraphAnalyzer},
	Run:        runGuidance,
	ResultType: reflect.TypeOf((*GuidanceTable)(nil)),
}

// Guidance is what may legally be named at one point in a flow.
//
// Producers holds the names already declared at that point, so a name declared
// LATER is absent — that is the declare-before-use rule expressed as a set
// rather than as a check. Storage holds the flow's vars and state fields, which
// are declared ahead of the body and are therefore all available at every point.
//
// Imports are the file's import declarations as written. An entry's Alias is set
// only when the source declared one: a package's NAME is not derivable from its
// PATH without go/types, and a last-path-segment guess yields "v82" for
// github.com/stripe/stripe-go/v82. Resolving that is the loader's job, so this
// reports what was declared and leaves the resolution to a consumer that has it.
type Guidance struct {
	Flow      string
	Producers []string
	Storage   []string
	Imports   []ImportRef
}

// GuidanceTable is the guidance analyzer's result, and the value an editor holds
// across keystrokes.
type GuidanceTable struct {
	files map[string]fileGuidance
}

// fileGuidance is one file's scopes, ordered by where they begin.
type fileGuidance struct {
	scopes []guidanceScope
}

// guidanceScope is the guidance in force from one statement's start until the
// next statement begins.
type guidanceScope struct {
	offset int
	value  Guidance
}

// At returns the guidance in force at a position in a source.
//
// THIS IS A BOUNDED LOOKUP OVER A PREBUILT TABLE, not a walk. A binary search
// over one file's scope offsets and a return of an already-built value, with no
// allocation on the query path — an editor calls this on every keystroke, and a
// per-call walk of the tree would make its cost track the file's length.
func (t *GuidanceTable) At(src Source, pos ast.Position) (Guidance, bool) {
	file, ok := t.files[src.Path]
	if !ok || len(file.scopes) == 0 {
		return Guidance{}, false
	}

	i := sort.Search(len(file.scopes), func(i int) bool { return file.scopes[i].offset > pos.Offset })
	if i == 0 {
		return Guidance{}, false
	}
	return file.scopes[i-1].value, true
}

// BuildGuidance runs the analysis once and hands back the prebuilt table.
//
// This is the seam an editor uses: build once when a file changes, then call At
// per keystroke. The capture rides an anonymous analyzer through the real
// driver rather than calling GuidanceAnalyzer.Run directly, so the prerequisite
// ordering is the driver's and not a second copy of it.
func BuildGuidance(srcs []Source) (*GuidanceTable, error) {
	var table *GuidanceTable
	capture := &Analyzer{
		Name:     "guidance-capture",
		Doc:      "captures the guidance table out of one driver run",
		Requires: []*Analyzer{GuidanceAnalyzer},
		Run: func(p *Pass) (any, error) {
			built, ok := p.ResultOf[GuidanceAnalyzer].(*GuidanceTable)
			if !ok {
				return nil, errNoGuidance
			}
			table = built
			return nil, nil
		},
	}

	if _, err := Run(srcs, []*Analyzer{capture}); err != nil {
		return nil, err
	}
	if table == nil {
		return nil, errNoGuidance
	}
	return table, nil
}

// runGuidance builds every file's scope table.
func runGuidance(p *Pass) (any, error) {
	symbols, ok := p.ResultOf[SymbolsAnalyzer].(*SymbolTable)
	if !ok {
		return nil, errNoSymbols
	}
	if _, ok := p.ResultOf[FlowgraphAnalyzer].(*GraphSet); !ok {
		return nil, errNoGraph
	}

	table := &GuidanceTable{files: make(map[string]fileGuidance, len(symbols.Files))}
	for f := range symbols.Files {
		file := &symbols.Files[f]
		table.files[file.Src.Path] = fileGuidance{scopes: fileScopes(file)}
	}
	return table, nil
}

// fileScopes builds one file's scopes, in offset order.
func fileScopes(file *FileSymbols) []guidanceScope {
	var out []guidanceScope
	for i := range file.Flows {
		out = append(out, flowScopes(file, &file.Flows[i])...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].offset < out[j].offset })
	return out
}

// flowScopes builds one flow's scopes: one at the flow's own start, then one per
// statement.
//
// The scope AT a statement holds the names declared by every EARLIER statement,
// which is exactly the declare-before-use rule read as availability rather than
// as a violation.
func flowScopes(file *FileSymbols, flow *FlowSymbols) []guidanceScope {
	declaredAt := declarationsByStatement(flow)
	storage := storageNames(flow)

	base := Guidance{Flow: flow.Name, Storage: storage, Imports: file.Imports}
	inScope := append([]string(nil), declaredAt[signatureStmt]...)
	sort.Strings(inScope)
	base.Producers = inScope

	out := make([]guidanceScope, 0, len(flow.Body)+1)
	out = append(out, guidanceScope{offset: flow.Pos.Offset, value: base})

	for i, stmt := range flow.Body {
		out = append(out, guidanceScope{
			offset: stmt.Pos().Offset,
			value:  Guidance{Flow: flow.Name, Producers: inScope, Storage: storage, Imports: file.Imports},
		})
		if len(declaredAt[i]) == 0 {
			continue
		}
		// A NEW SLICE PER STEP rather than an append in place: the scopes above
		// keep pointing at the prefix they were built with, and appending to a
		// shared backing array would let a later statement's declarations appear
		// in an earlier statement's scope.
		grown := make([]string, 0, len(inScope)+len(declaredAt[i]))
		grown = append(grown, inScope...)
		grown = append(grown, declaredAt[i]...)
		sort.Strings(grown)
		inScope = grown
	}
	return out
}

// declarationsByStatement inverts the producer table into the names each
// statement declares.
func declarationsByStatement(flow *FlowSymbols) map[int][]string {
	out := make(map[int][]string, len(flow.Body)+1)
	for _, name := range sortedNames(flow.Producers) {
		for _, ref := range flow.Producers[name] {
			out[ref.Stmt] = append(out[ref.Stmt], name)
		}
	}
	return out
}

// storageNames is every var and state field a reads or writes clause may name,
// sorted.
func storageNames(flow *FlowSymbols) []string {
	out := make([]string, 0, len(flow.Vars)+len(flow.State))
	for name := range flow.Vars {
		out = append(out, name)
	}
	for name := range flow.State {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
