// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"errors"
	"reflect"
	"sort"
	"strings"
)

// dependency-graph colors for the topological walk.
const (
	colorFresh = iota
	colorOpen
	colorDone
)

// factKey identifies one exported fact: which object it is about, and which fact
// type it is. Two analyzers may hold facts of different types about one object.
type factKey struct {
	obj string
	typ reflect.Type
}

// Run analyzes srcs with analyzers, plus every analyzer they transitively
// require, and returns the diagnostics they reported.
//
// The run is SERIAL, one walk per analyzer. That is a measurement rather than a
// preference: a full structural walk of a strawman costs 22ns against a 12.278µs
// parse, so twelve analyzers cost roughly 3% of the parse they follow and a
// worker pool would be pure overhead.
//
// A Requires cycle is returned as an error naming the analyzers in it, and no
// analyzer runs. An analyzer returning an error aborts the run under its own
// name; a partial result is not returned alongside a failure.
func Run(srcs []Source, analyzers []*Analyzer) ([]Diagnostic, error) {
	if err := checkSources(srcs); err != nil {
		return nil, err
	}
	order, err := topoOrder(analyzers)
	if err != nil {
		return nil, err
	}

	var diags []Diagnostic
	results := make(map[*Analyzer]any, len(order))
	facts := make(map[factKey]Fact)

	for _, a := range order {
		pass := &Pass{
			Analyzer:   a,
			Sources:    srcs,
			Report:     func(src Source, d Diagnostic) { diags = append(diags, stamp(a, src, d)) },
			ResultOf:   results,
			ImportFact: func(obj string, f Fact) bool { return importFact(facts, obj, f) },
			ExportFact: func(obj string, f Fact) { facts[factKey{obj: obj, typ: factType(f)}] = f },
		}
		res, rerr := a.Run(pass)
		if rerr != nil {
			return nil, errors.New("analysis: analyzer " + a.Name + " failed: " + rerr.Error())
		}
		results[a] = res
	}

	sortDiagnostics(diags)
	return diags, nil
}

// checkSources refuses a source carrying no parsed tree, naming it.
//
// Every analyzer reads Source.File, so a nil tree is a nil dereference deep in
// whichever analyzer happens to run first — a panic that names an internal
// walker rather than the input that caused it. It is refused here instead,
// once, at the entry point. A caller always has a tree to supply: ast.Parse
// returns one even for a file it reported diagnostics on, carried on the error
// alongside them.
func checkSources(srcs []Source) error {
	for _, src := range srcs {
		if src.File == nil {
			return errors.New("analysis: source " + describeSource(src) +
				" carries no parsed tree; ast.Parse returns one even for a file with diagnostics")
		}
	}
	return nil
}

// describeSource names a source for an error message, since an unnamed one
// would otherwise be reported as an empty string.
func describeSource(src Source) string {
	if src.Path == "" {
		return "(unnamed)"
	}
	return src.Path
}

// stamp fills in the two fields the driver owns rather than the analyzer: the
// reporting analyzer's Name as the rule Code, and the reported Source's path.
//
// Both are stamped here so neither can drift. An analyzer cannot emit under a
// foreign code, and cannot attribute a finding to a file it was not looking at.
func stamp(a *Analyzer, src Source, d Diagnostic) Diagnostic {
	d.Code = a.Name
	d.Path = src.Path
	return d
}

// sortDiagnostics puts the run's findings in a stable order so two runs over the
// same sources produce byte-identical output.
//
// THE KEY LEADS WITH PATH, which is what makes the order a function of the
// CONTENT rather than of the order the caller happened to list its sources in.
// Without it the key would tie for two findings at the same offset in different
// files — the ordinary case, since every parsed tree starts at offset zero — and
// the sort would fall back to arrival order, so reversing the Sources slice would
// reverse the output.
func sortDiagnostics(diags []Diagnostic) {
	sort.SliceStable(diags, func(i, j int) bool {
		if diags[i].Path != diags[j].Path {
			return diags[i].Path < diags[j].Path
		}
		if diags[i].Pos.Offset != diags[j].Pos.Offset {
			return diags[i].Pos.Offset < diags[j].Pos.Offset
		}
		if diags[i].Code != diags[j].Code {
			return diags[i].Code < diags[j].Code
		}
		return diags[i].Message < diags[j].Message
	})
}

// importFact copies a previously exported fact about obj into the value f points
// at, reporting whether one existed.
func importFact(facts map[factKey]Fact, obj string, f Fact) bool {
	stored, ok := facts[factKey{obj: obj, typ: factType(f)}]
	if !ok {
		return false
	}
	reflect.ValueOf(f).Elem().Set(reflect.ValueOf(stored).Elem())
	return true
}

// factType is a fact value's pointer type, which is the identity a fact is
// stored and retrieved under.
//
// A non-pointer fact PANICS. ImportFact fills the value its argument points at,
// so a fact passed by value could never be filled — accepting one would record
// an export nothing can ever import, which is worse than a stop.
func factType(f Fact) reflect.Type {
	t := reflect.TypeOf(f)
	if t == nil || t.Kind() != reflect.Pointer {
		panic("analysis: a Fact must be a pointer, so that ImportFact can fill it")
	}
	return t
}

// topoOrder returns the analyzers in dependency order: every analyzer appears
// after everything it Requires.
//
// The set is EXPANDED transitively, so naming one analyzer runs the ones it
// needs even when the caller did not list them.
func topoOrder(roots []*Analyzer) ([]*Analyzer, error) {
	t := &toposort{color: make(map[*Analyzer]int, len(roots))}
	for _, a := range roots {
		if err := t.visit(a); err != nil {
			return nil, err
		}
	}
	return t.order, nil
}

// toposort carries the depth-first walk's state.
type toposort struct {
	color map[*Analyzer]int
	stack []*Analyzer
	order []*Analyzer
}

// visit places a after everything it requires, or reports the cycle it sits in.
func (t *toposort) visit(a *Analyzer) error {
	if t.color[a] == colorDone {
		return nil
	}
	if t.color[a] == colorOpen {
		return cycleError(t.stack, a)
	}

	t.color[a] = colorOpen
	t.stack = append(t.stack, a)
	for _, req := range a.Requires {
		if err := t.visit(req); err != nil {
			return err
		}
	}
	t.stack = t.stack[:len(t.stack)-1]
	t.color[a] = colorDone
	t.order = append(t.order, a)
	return nil
}

// cycleError names every analyzer on the cycle, in the order the walk entered
// them, closing the loop back onto the one it re-entered.
//
// Naming them all is the point: "a dependency cycle" tells an author nothing,
// and the cycle is between analyzers whose Requires lists sit in different files.
func cycleError(stack []*Analyzer, repeated *Analyzer) error {
	names := make([]string, 0, len(stack)+1)
	for i, a := range stack {
		if a == repeated {
			names = append(names, namesOf(stack[i:])...)
			break
		}
	}
	names = append(names, repeated.Name)
	return errors.New("analysis: analyzers form a Requires cycle: " + strings.Join(names, " -> "))
}

// namesOf projects analyzers down to their names.
func namesOf(as []*Analyzer) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Name)
	}
	return out
}
