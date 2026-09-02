// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"go/types"

	"github.com/whitaker-io/machine/lang/ast"
	"github.com/whitaker-io/machine/lang/loader"
)

// Inference is the per-name type table lang/analysis exports.
//
// IT IS AN INTERFACE SO THE DRIVER'S RULE IS TESTABLE WITHOUT STANDING UP THE
// ANALYSIS ENGINE. Production passes the real *analysis.InferredTypes, whose
// Name method has exactly this shape; a test passes a recorder and observes WHICH
// FLOWS THE DRIVER ASKS ABOUT, which is the property under test.
type Inference interface {
	// Name returns the inferred type of one output of one flow.
	Name(flow, name string) (types.Type, bool)
}

// Source is one .flow file handed to Assemble.
type Source struct {
	// Path is the file's path, used for the generated file's source stamp and
	// for rendering diagnostics.
	Path string
	// Src is the file's bytes. The analysis module carries them alongside the
	// tree because its analyzers read them, so the bridge to it has nothing to
	// re-read from disk.
	Src []byte
	// File is the parsed source.
	File *ast.File
}

// Facts are the driver-supplied answers this package consumes and never derives.
//
// EVERY MEMBER IS SOMEONE ELSE'S ANSWER. lang/analysis owns which outputs a flow
// makes bindable and what a signature-less exported flow's boundary types are;
// lang/loader owns package loading. This package resolves, lowers and emits
// against those answers, and a disagreement is a loud refusal rather than a
// second opinion.
type Facts struct {
	// Boundary is the bindable-output fact per flow. An ABSENT entry means no
	// fact was exported, which is refused rather than read as an empty set.
	Boundary map[string]Boundary
	// Types is the loaded package set's view, or nil when none was loaded.
	Types *Types
	// Inferred is the analysis type table, or nil.
	Inferred Inference
	// Registrations are the gob registrations the generated package must emit,
	// deduplicated by spelling and sorted by it.
	Registrations []Registration
	// Imported are the flows resolved out of other modules, keyed by the
	// REFERENCE the author wrote.
	//
	// AN IMPORTED FLOW IS AVAILABLE TO INLINE AND IS NEVER LOWERED INTO A PLAN OF
	// ITS OWN. The dependency's own build generates its wiring; emitting a second
	// copy here would declare it twice in a program that already has one.
	Imported map[string]*Program
}

// Registration is one gob.Register call the generated package emits.
//
// IT CARRIES THE SPELLING THE AUTHOR WROTE, not the identity the derivation
// named. The derivation's identity is a go/types string and is not Go source; the
// spelling is text that already compiles in the package the .flow sits beside,
// which is the package the registration is emitted into.
type Registration struct {
	// Spelling is the Go type as the author wrote it.
	Spelling string
	// Flow and Name are the declaration that made the registration necessary,
	// kept so a wrong emission can be traced back to what asked for it.
	Flow string
	Name string
}

// Assemble turns parsed .flow sources into generated Go.
//
// IT IS A PURE FUNCTION of its inputs. It knows nothing about analysis, checks or
// diagnostics arriving from outside; what it takes are FACTS — values the driver
// gathered — rather than an analysis API.
//
// IT COLLECTS FROM EVERY FILE RATHER THAN STOPPING AT THE FIRST. A run over
// several sources reports every problem it found, because an author fixing one
// diagnostic at a time through repeated runs is paying for the tool's
// convenience.
//
// ON ANY DIAGNOSTIC IT RETURNS *Error WITH Partial CARRYING WHAT IT DID EMIT,
// mirroring the tolerant contract lang/ast documents at Parse: hand back a usable
// partial product alongside a non-nil error. Whether that partial is usable is
// the caller's decision; this package's own driver declines to write any of it.
func Assemble(sources []Source, cfg Config, facts Facts) ([]Generated, error) {
	var (
		generated []Generated
		diags     []Diagnostic
	)
	for _, source := range sources {
		out, fileDiags := assembleOne(source, cfg, facts)
		diags = append(diags, fileDiags...)
		if len(out.Source) > 0 {
			generated = append(generated, out)
		}
	}
	if len(diags) != 0 {
		return generated, &Error{Diagnostics: diags, Partial: generated}
	}

	return generated, nil
}

// assembleOne runs the whole pipeline over one file.
func assembleOne(source Source, cfg Config, facts Facts) (Generated, []Diagnostic) {
	programs, diags := buildFile(source.File)
	if len(diags) != 0 {
		return Generated{}, diags
	}
	for _, p := range programs {
		p.InputTypes = nodeTypes(p, facts)
	}
	plans, lowerDiags := lowerFile(programs, facts.Imported, facts.Boundary, cfg)
	if len(lowerDiags) != 0 {
		return Generated{}, lowerDiags
	}

	return Generate(Request{
		File: source.File, Programs: programs, Plans: plans,
		Config: cfg, Source: source.Path, Types: facts.Types,
		Registrations: facts.Registrations,
	})
}

// nodeTypes resolves every node's input type spelling.
//
// TWO SOURCES ANSWER TWO DIFFERENT QUESTIONS, and conflating them is what makes a
// driver ask the wrong authority. A node's OWN payload comes from its Go
// function's signature, which already states it; a flow's BOUNDARY types — what
// it exposes across a module edge — come from the three-way rule below. The
// boundary answer wins where both apply, because a flow's declared or inferred
// boundary is a statement about the edge and the local signature is not.
func nodeTypes(p *Program, facts Facts) map[string]string {
	out := map[string]string{}
	if facts.Types != nil {
		for _, n := range p.Nodes {
			if ref := refOf(n); ref != "" {
				if spelling, ok := facts.Types.PayloadOfRef(ref); ok {
					out[n.Name] = spelling
				}
			}
		}
	}
	propagateTypes(p, out)
	for name, spelling := range boundaryTypes(p, facts) {
		out[name] = spelling
	}

	return out
}

// propagateTypes carries a payload type along the graph to the nodes that name
// no Go function of their own.
//
// A TEE, A DROP AND A SEND APPLY NOTHING. The language says so — a tee is
// route-only — so there is no signature to read a payload off, and their payload
// is by definition whatever their input carries. Propagating along the declaring
// input is not an inference: a route-only node cannot change the type, so its
// producer's type IS its type.
//
// The walk repeats until it settles rather than assuming source order, because a
// loop puts a consumer before its producer.
func propagateTypes(p *Program, known map[string]string) {
	producer := map[string]string{}
	for _, n := range p.Nodes {
		for _, out := range n.Outputs {
			producer[out] = n.Name
		}
	}
	for range len(p.Nodes) {
		if settled := propagateOnce(p, known, producer); settled {
			return
		}
	}
}

// propagateOnce carries the type one hop, reporting whether anything moved.
func propagateOnce(p *Program, known, producer map[string]string) bool {
	settled := true
	for _, n := range p.Nodes {
		if _, have := known[n.Name]; have || len(n.Inputs) == 0 {
			continue
		}
		spelling, ok := known[producer[n.Inputs[0]]]
		if !ok {
			continue
		}
		known[n.Name] = spelling
		settled = false
	}

	return settled
}

// boundaryTypes resolves a flow's BOUNDARY type spellings, by the THREE-WAY RULE
// three landed rulings produce.
//
// THE DEFECT THIS SHAPE EXISTS TO AVOID is a driver asking the WRONG AUTHORITY,
// because both wrong answers compile, produce a types.Type and generate code.
//
//   - A FLOW DECLARING A SIGNATURE is typed from its own declared SPELLINGS, and
//     inference is NOT consulted for it. The author wrote the types down; reading
//     them from anywhere else would let a derived answer silently override a
//     stated one.
//   - A SIGNATURE-LESS EXPORTED FLOW takes its boundary types from INFERENCE.
//     HasSignature false means NO type claim at all, which is a different
//     statement from an empty one, so the driver has to go somewhere else rather
//     than read an absent claim as empty.
//   - A SIGNATURE-LESS UNEXPORTED FLOW is module-private and is never typed across
//     a boundary, so inference is not consulted for it either.
func boundaryTypes(p *Program, facts Facts) map[string]string {
	out := map[string]string{}
	if p.Signature != nil {
		for _, declared := range p.Signature.Outputs {
			out[declared.Name.Name] = declared.Type.Text
		}

		return out
	}
	if !loader.Exported(p.Name) {
		return out
	}
	if facts.Inferred == nil {
		return out
	}
	for _, n := range p.Nodes {
		for _, name := range n.Outputs {
			typ, ok := facts.Inferred.Name(p.Name, name)
			if !ok {
				continue
			}
			// AN INFERRED TYPE IS SPELLED AS GO SOURCE, THROUGH THE RENDERER THIS
			// PACKAGE ALREADY HAS. types.Type.String renders a named type by its
			// full import path, so a type declared in the generated package comes
			// back as example.com/app.Order and splicing it produces a file that
			// does not parse. Types.spell answers exactly this question for
			// PayloadOfRef, under a qualifier returning the empty string for the
			// generated package; a second renderer for one rule is a duplicate.
			if facts.Types == nil {
				continue
			}
			out[n.Name] = facts.Types.spell(typ)
		}
	}

	return out
}
