// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"sort"

	"github.com/whitaker-io/machine/lang/analysis"
	"github.com/whitaker-io/machine/lang/loader"
)

// gate runs the analysis module's whole gate over the run and splits its answer
// into the three things this package needs: the facts, what REFUSES, and what is
// merely DISCLOSED.
//
// THIS FILE DERIVES NOTHING. It converts sources into the shape lang/analysis
// takes, calls Gate, and re-keys the answers; every judgement in it belongs to
// the analysis module, which is the single-owner ruling this package works
// under.
func gate(sources []Source, pkgs *loader.Packages, pkgPath string) (Facts, []Diagnostic, []Diagnostic, error) {
	result, err := analysis.Gate(analysisSources(sources), pkgs, pkgPath)
	if err != nil {
		return Facts{}, nil, nil, err
	}

	refused, disclosed := partition(result.Diagnostics)
	facts := Facts{
		Boundary:      boundaryFacts(result.Boundaries),
		Inferred:      result.Inferred,
		Registrations: registrationFacts(result.Registrations),
	}

	return facts, refused, disclosed, nil
}

// registrationFacts re-keys the derivation's registration table into this
// package's own, DEDUPLICATED BY SPELLING AND SORTED BY IT.
//
// ONE TYPE DECLARED AT FOUR SITES IS ONE REGISTRATION. A generated file is a
// function of the analysis ANSWER rather than of how many declarations reached
// it, so two runs over the same sources emit byte-identical output. A duplicate
// gob.Register of an identical type does not panic, so this is determinism rather
// than safety.
func registrationFacts(required *analysis.Registrations) []Registration {
	if required == nil {
		return nil
	}

	seen := map[string]bool{}
	out := make([]Registration, 0, len(required.Required))
	for _, entry := range required.Required {
		if entry.Spelling == "" || seen[entry.Spelling] {
			continue
		}
		seen[entry.Spelling] = true
		out = append(out, Registration{Spelling: entry.Spelling, Flow: entry.Flow, Name: entry.Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spelling < out[j].Spelling })

	return out
}

// analysisSources converts this package's sources into the analysis module's.
//
// The bytes ride along because analysis.Source carries them beside the tree and
// its analyzers read them; discover already had them in hand.
func analysisSources(sources []Source) []analysis.Source {
	out := make([]analysis.Source, 0, len(sources))
	for _, source := range sources {
		out = append(out, analysis.Source{Path: source.Path, Src: source.Src, File: source.File})
	}

	return out
}

// boundaryFacts re-keys the analysis module's exported boundaries into this
// package's per-flow fact.
//
// IT IS A RE-KEYING OF SOMEONE ELSE'S ANSWER AND DERIVES NOTHING. Which names a
// flow makes bindable is lang/analysis's rule; reading it a second way here would
// be exactly the second opinion Facts' own doc forbids.
//
// A FLOW WITH NO EXPORTED BOUNDARY IS LEFT OUT of the map rather than entered
// empty, because an ABSENT entry is what this package refuses on and an empty one
// would read as a flow that legitimately binds nothing.
func boundaryFacts(boundaries *analysis.Boundaries) map[string]Boundary {
	out := map[string]Boundary{}
	for _, flow := range boundaries.Flows() {
		names, ok := boundaries.Names(flow)
		if !ok {
			continue
		}
		out[flow] = Boundary{Outputs: names}
	}

	return out
}

// partition splits the gate's findings at the line that refuses.
//
// THE LINE IS analysis.SeverityError AND IT IS FIXED, and that is MEASURED rather
// than preferred. The analysis module's own vocabulary defines a warning as
// "suspicious but not provably wrong" and a hint as "an observation an author may
// reasonably ignore"; run against this repository's own end-to-end fixture the
// fourteen analyzers report eleven findings, six of which say in their own text
// that the condition is legal and may be deliberate. Refusing on those refuses
// every legal program.
//
// THERE IS NO FLAG FOR IT. A threshold a caller can move is a lever for
// generating a program an analyzer already refused, which is the silence this
// whole gate exists to remove.
//
// NOTHING IS DROPPED. What does not refuse is returned for the caller to print.
func partition(diags []analysis.Diagnostic) (refused, disclosed []Diagnostic) {
	for _, d := range diags {
		converted := Diagnostic{Pos: d.Pos, End: d.End, Message: d.Message, Path: d.Path}
		if d.Severity == analysis.SeverityError {
			refused = append(refused, converted)

			continue
		}
		disclosed = append(disclosed, converted)
	}

	return refused, disclosed
}
