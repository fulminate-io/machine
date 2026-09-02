// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"fmt"

	"github.com/whitaker-io/machine/lang/loader"
)

// Boundaries is every flow's BINDABLE OUTPUT NAMES, keyed by flow.
//
// It is the signature analyzer's own answer lifted out of a run rather than a
// second derivation: the names a `use` may bind are the DECLARED outputs when the
// flow has a header and everything its body produces when it does not, and
// bindableNames is where that rule lives.
type Boundaries struct {
	flows map[string][]string
	order []string
}

// Names returns one flow's bindable output names, and whether that flow was seen.
//
// ABSENCE IS THE FALSE SECOND RESULT, never an empty slice. A flow nobody
// exported a boundary for and a flow whose boundary is genuinely empty are
// different facts, and a caller that cannot tell them apart generates against an
// absent one as though it were empty — which is the silence this whole seam
// exists to remove. InferredTypes.Name reports absence the same way.
func (b *Boundaries) Names(flow string) ([]string, bool) {
	if b == nil {
		return nil, false
	}
	names, ok := b.flows[flow]

	return names, ok
}

// Flows lists every flow a boundary was exported for, in the order the run
// walked them.
func (b *Boundaries) Flows() []string {
	if b == nil {
		return nil
	}

	return b.order
}

// GateResult is everything one gate run produced.
//
// THE DIAGNOSTICS ARE NOT SPLIT HERE. Which of them REFUSE and which are merely
// disclosed is the consumer's policy, and this module states the severity rather
// than acting on it.
type GateResult struct {
	Diagnostics []Diagnostic
	Inferred    *InferredTypes
	Boundaries  *Boundaries
	// Registrations is what the generated code must register with encoding/gob.
	//
	// IT IS THE DERIVATION'S RESULT RATHER THAN ITS DIAGNOSTICS, and a gate that
	// consumed only the diagnostics would leave it unreachable — which is the
	// state that shipped: a named struct at an interface site passed silently and
	// the generated file carried no registration at all.
	Registrations *Registrations
}

// Gate runs EVERY analyzer this module has over srcs, in one driver run, and
// lifts out the two tables a generation driver consumes.
//
// FOURTEEN ANALYZERS, ONE WALK. The twelve All() registers plus the two that are
// constructed because they need a caller-supplied package set — type inference
// and the serialization derivation, neither of which is registered and neither of
// which this function registers. Running the three sets separately would walk the
// sources three times and derive the symbol table three times; naming them all in
// one capture's Requires lets the driver's own topological order do it once.
//
// THIS IS THE SEAM BuildGuidance ESTABLISHED AND BuildInferredTypes FOLLOWED: an
// anonymous capture analyzer names what it wants, runs through the REAL driver,
// and lifts the built values out of Pass.ResultOf, so the prerequisite ordering
// stays the driver's rather than becoming a second copy of it. The capture is
// placed last by that same order, which is what lets it read every result.
//
// A NIL PACKAGE SET IS REFUSED, wrapping the shared errNoPackages sentinel so
// errors.Is holds for a caller, on the same terms as BuildInferredTypes. A run
// that produces no inferred table is an ERROR rather than an empty result beside
// a nil error, because a caller cannot tell those two apart.
//
// PERF SHAPE, from the driver's own measurement: a full structural walk costs
// 22ns against a 12.278µs parse, so fourteen analyzers cost a few percent of the
// parse that precedes them. Serial, one pass, no pool.
func Gate(srcs []Source, pkgs *loader.Packages, pkgPath string) (*GateResult, error) {
	if pkgs == nil {
		return nil, fmt.Errorf("analysis gate: %w", errNoPackages)
	}

	inference := TypeInferenceAnalyzer(pkgs, pkgPath)
	serialization := SerializationAnalyzer(pkgs, pkgPath)
	out := &GateResult{Boundaries: &Boundaries{flows: map[string][]string{}}}

	capture := &Analyzer{
		Name:     "gate-capture",
		Doc:      "captures the inferred types, the registrations and the exported boundaries out of one driver run",
		Requires: gateRequires(inference, serialization),
		Run:      func(p *Pass) (any, error) { return nil, out.capture(p, inference, serialization) },
	}

	diags, err := Run(srcs, []*Analyzer{capture})
	if err != nil {
		return nil, err
	}
	if out.Inferred == nil {
		return nil, errNoInferredTypes
	}
	if out.Registrations == nil {
		return nil, errNoRegistrations
	}
	out.Diagnostics = diags

	return out, nil
}

// gateRequires names every analyzer one gate run walks.
//
// THE TWO PREREQUISITES ARE NAMED EXPLICITLY even though All() already carries
// them. The capture reads the SymbolTable out of Pass.ResultOf and imports the
// facts the signature analyzer exports, so it depends on both directly; relying
// on All() to keep supplying them would make this capture's correctness a
// property of a registration list it does not own.
func gateRequires(inference, serialization *Analyzer) []*Analyzer {
	required := All()

	return append(required, inference, serialization, SymbolsAnalyzer, SignatureAnalyzer)
}

// capture lifts the inferred table, the registration table and the per-flow
// boundaries out of one pass.
//
// EACH ABSENCE IS ITS OWN SENTINEL rather than an empty value beside a nil error.
// A run that required no registration and a run whose registration table was
// never produced are different facts, and a caller cannot tell them apart from an
// empty slice.
func (g *GateResult) capture(p *Pass, inference, serialization *Analyzer) error {
	table, ok := p.ResultOf[inference].(*InferredTypes)
	if !ok {
		return errNoInferredTypes
	}
	g.Inferred = table

	required, ok := p.ResultOf[serialization].(*Registrations)
	if !ok {
		return errNoRegistrations
	}
	g.Registrations = required

	symbols, ok := p.ResultOf[SymbolsAnalyzer].(*SymbolTable)
	if !ok {
		return errNoSymbols
	}
	for f := range symbols.Files {
		for i := range symbols.Files[f].Flows {
			g.Boundaries.add(p, symbols.Files[f].Flows[i].Name)
		}
	}

	return nil
}

// add imports one flow's exported outputs fact and records what a use may bind.
//
// THE RULE IS bindableNames', NOT A RESTATEMENT OF IT. A flow whose fact was
// never exported is left ABSENT rather than recorded empty, which is the
// distinction Names exists to preserve.
func (b *Boundaries) add(p *Pass, flow string) {
	if _, seen := b.flows[flow]; seen {
		return
	}

	var outputs flowOutputsFact
	if !p.ImportFact(flow, &outputs) {
		return
	}

	b.flows[flow] = bindableNames(outputs)
	b.order = append(b.order, flow)
}
