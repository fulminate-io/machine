// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"fmt"
	"go/types"

	"github.com/whitaker-io/machine/lang/ast"
	"github.com/whitaker-io/machine/lang/loader"
)

// Types is this module's view of a loaded package set.
//
// IT CONSUMES lang/loader AND LOADS NOTHING ITSELF. Package loading is
// owner-exclusive to lang/loader — no x/tools, no go/importer, no packages.Config
// and no NeedTypes mode belongs here — and a landed ownership census reds by name
// on any of those appearing in this module's direct imports. What this module
// legitimately names is go/types itself, the vocabulary the loader hands back,
// because a clone derivation has to type-switch on *types.Struct, *types.Slice,
// *types.Map and *types.Pointer and Resolve's own return type is types.Type.
//
// THE LOAD HAPPENS ONCE PER GENERATION RUN and its lifetime belongs to the
// caller: the loader holds no process-global cache and says so. Loading is the
// seconds-scale operation in this toolchain, so a per-file or per-node call turns
// a one-off cost into a per-unit one. The driver owns the single *loader.Packages
// and hands it here.
type Types struct {
	pkgs *loader.Packages
	// pkgPath is the import path of the generated package, which is the scope
	// every spelling is resolved in.
	pkgPath string
	// lines maps a generated line back to the .flow position it came from.
	lines map[int]ast.Position
}

// NewTypes wraps a package set the DRIVER loaded.
//
// It takes the loaded set rather than a directory precisely so it cannot load
// one itself.
func NewTypes(pkgs *loader.Packages, pkgPath string, lines map[int]ast.Position) *Types {
	return &Types{pkgs: pkgs, pkgPath: pkgPath, lines: lines}
}

// Resolve turns a declared Go type SPELLING into a types.Type.
//
// The spelling is evaluated in a FILE's scope rather than the package's, because
// imports are per-file — that is the loader's rule and this only calls it. A
// spelling two files of one package disagree about is REFUSED by the loader
// naming both candidates, and that refusal is passed through untouched: it
// reports a genuine ambiguity in the author's own source, and papering over it
// would pick one of two meanings silently.
func (t *Types) Resolve(spelling string) (types.Type, error) {
	if t == nil || t.pkgs == nil {
		return nil, fmt.Errorf("no package set was loaded, so %q cannot be resolved", spelling)
	}

	return t.pkgs.Resolve(t.pkgPath, spelling)
}

// Scope returns the generated package's scope.
func (t *Types) Scope() (*types.Scope, bool) {
	if t == nil || t.pkgs == nil {
		return nil, false
	}

	return t.pkgs.Scope(t.pkgPath)
}

// Diagnostics maps the loader's type-check errors back into the author's frame.
//
// TWO COORDINATE FRAMES MEET HERE AND MIXING THEM SILENTLY RELOCATES A
// DIAGNOSTIC. Errors() reports go/types problems in the coordinates of the file
// go/types actually READ, which for a generation run is the GENERATED Go. Mapping
// one back to the .flow line the author wrote is THE EMITTER'S job, because only
// the emitter holds the line map — which is why this method takes it from the
// emitter and the loader does not attempt the mapping at all.
//
// IT IS APPLIED TO Errors() RESULTS AND TO NOTHING ELSE. ResolveFlow reports
// reference refusals ALREADY in the author's frame, taken from the caller's own
// position arguments, and re-mapping an already-mapped position is exactly the
// defect this separation exists to prevent.
func (t *Types) Diagnostics() []Diagnostic {
	if t == nil || t.pkgs == nil {
		return nil
	}
	var out []Diagnostic
	for _, d := range t.pkgs.Errors() {
		out = append(out, t.mapDiagnostic(d))
	}

	return out
}

// mapDiagnostic re-points one loader diagnostic at the .flow line that produced
// the generated line it names.
//
// A LINE WITH NO MAPPING IS REPORTED WHERE IT IS, not silently dropped and not
// guessed at: generated code carries a preamble and a package doc that came from
// no .flow line at all, and a type error in those is a real error about the
// generator rather than about the author's source. Saying so is more useful than
// inventing a position.
func (t *Types) mapDiagnostic(d loader.Diagnostic) Diagnostic {
	at, mapped := t.lines[d.Pos.Line]
	if !mapped {
		return Diagnostic{
			Pos: d.Pos, End: d.End,
			Message: d.Message + " (in generated code at " + d.Path + ", which no .flow line produced)",
		}
	}

	return Diagnostic{Pos: at, End: at, Message: d.Message}
}

// PayloadOf reads a node's payload type off a resolved spelling.
//
// FAIL LOUD. An unresolvable spelling is an error naming it and the .flow line
// that declared it; there is no fallback to an unresolved type, because a clone
// derivation over one would silently produce a shallow copy.
func (t *Types) PayloadOf(spelling string, at ast.Position) (types.Type, *Diagnostic) {
	resolved, err := t.Resolve(spelling)
	if err != nil {
		return nil, &Diagnostic{
			Pos: at, End: at,
			Message: fmt.Sprintf("the type %q could not be resolved: %v", spelling, err),
		}
	}

	return resolved, nil
}
