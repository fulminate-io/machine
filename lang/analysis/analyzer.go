// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import "reflect"

// Fact is a value one analyzer exports about a named flow-level object and
// another imports.
//
// Facts are how information crosses FILE boundaries within one run: a `use`
// statement may reference a flow declared in a different file, and the signature
// analyzer learns that flow's declared outputs only because the symbols analyzer
// exported them as a fact keyed on the flow's name.
//
// The object key is a plain string — the flow-qualified name — rather than an
// object model, because a flow-level name is already unique within a run and an
// object model would be a second name space to keep in agreement with the first.
type Fact interface {
	// AFact distinguishes a fact from every other value. It is never called.
	AFact()
}

// Analyzer is one analysis pass: what it is called, what it needs, and what it
// does.
//
// Name is also the Code every diagnostic this analyzer reports carries, so a
// consumer suppresses or routes a rule by identity rather than by matching
// message text. The driver stamps it, so the two cannot drift apart.
//
// Requires names the analyzers whose results this one reads out of Pass.ResultOf.
// The driver runs them first; a cycle among them is an error, never a dropped
// edge.
//
// ResultType and FactTypes are declarations rather than machinery: they document
// what a consumer may assert about the value Run returns and which fact types
// this analyzer participates in.
type Analyzer struct {
	Name       string
	Doc        string
	Requires   []*Analyzer
	Run        func(*Pass) (any, error)
	ResultType reflect.Type
	FactTypes  []Fact
}

// String returns the analyzer's name, so an analyzer formats readably inside an
// error naming a dependency cycle.
func (a *Analyzer) String() string { return a.Name }

// Pass is one analyzer's view of one run.
//
// Sources is every file in the run rather than one file at a time, because the
// cross-file questions — does this `use` reference a flow declared elsewhere,
// does that flow deliver the outputs it declares — cannot be asked one file at a
// time.
//
// Report takes the SOURCE the finding is about alongside the Diagnostic, and the
// driver stamps both Code and Path from them. Passing the source rather than
// letting an analyzer fill in a Path field is what makes a forgotten file
// attribution a COMPILE ERROR instead of an empty string and a silently
// degraded sort.
//
// ImportFact fills the value pointed at by f with a fact previously exported for
// obj under that same pointer type, reporting whether one was found. ExportFact
// records one. Both take a POINTER to a fact value; anything else is a
// programming error and panics rather than silently recording nothing.
type Pass struct {
	Analyzer   *Analyzer
	Sources    []Source
	Report     func(Source, Diagnostic)
	ResultOf   map[*Analyzer]any
	ImportFact func(obj string, f Fact) bool
	ExportFact func(obj string, f Fact)
}
