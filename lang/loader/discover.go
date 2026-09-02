// Package loader - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package loader

import (
	"errors"
	"fmt"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/whitaker-io/machine/lang/ast"
)

// flowExt is the suffix a directory walk collects.
//
// IT IS DECLARED HERE RATHER THAN IMPORTED, and that is a dependency decision
// rather than an oversight. lang/lint exports its own Extension and lang/lsp
// declares its own copy; both are CONSUMERS of this module, so importing either
// would point the loader at something above it and create the cycle the module's
// dependency direction exists to prevent. lang/lsp set the same precedent for
// the same reason.
const flowExt = ".flow"

const (
	cannotRead   = "loader: cannot read "
	cannotParse  = "loader: cannot parse "
	noModuleDir  = "loader: no resolved module directory for package "
	noSuchModule = "loader: no loaded package has a module for import path "
)

// The three refusal messages ResolveFlow reports. They carry no "loader:" prefix
// because they are DIAGNOSTICS shown against the author's own source, not errors
// returned to a program, and they must read as compiler output.
const (
	noFlowNamed   = "no flow named "
	inModule      = " in module "
	flowPrefix    = "flow "
	notExported   = " is not exported from module "
	capitalizeFix = "; capitalize its first letter to export it"
)

// Module is the identity and RESOLVED LOCATION of the Go module a package
// belongs to.
//
// Dir is what makes cross-module flow discovery possible: for a dependency it is
// that module's directory in the module cache, which is a place no walk of the
// local tree would ever reach.
type Module struct {
	Path    string
	Dir     string
	Version string
}

// Module reports the module a loaded package belongs to.
//
// The second result distinguishes "no such package was loaded" and "this package
// belongs to no module the toolchain resolved" from a module that genuinely
// carries empty fields, so a caller never reads a zero value as an answer.
func (p *Packages) Module(pkgPath string) (Module, bool) {
	pkg, ok := p.byPath[pkgPath]
	if !ok || pkg.Module == nil {
		return Module{}, false
	}

	return Module{Path: pkg.Module.Path, Dir: pkg.Module.Dir, Version: pkg.Module.Version}, true
}

// Sources collects the flow sources a package's module declares, at any depth.
//
// THE WALK ROOTS AT THE RESOLVED MODULE DIRECTORY, NEVER AT THE PROCESS WORKING
// DIRECTORY, and that distinction is the whole capability rather than a detail.
// A walk rooted at "." returns the same files whenever the module happens to sit
// under the current directory, which is exactly what happens during development
// — so a wrongly-rooted implementation passes every local test and fails only in
// production, on a dependency in the module cache, which is the case this
// function exists for.
//
// DISCOVERY IS BY EXTENSION, with no ignore file and no vendor rule. Nothing
// marks a module as flow-bearing, just as nothing marks a Go module as
// Go-bearing, and a silent exclusion is how a file stops being checked without
// anyone deciding it should.
//
// Results are sorted, so a run's output does not depend on the order the
// filesystem hands back directory entries. A module whose directory the
// toolchain did not resolve is an error naming the package; this never returns
// an empty slice beside a nil error for a module it could not reach.
func (p *Packages) Sources(pkgPath string) ([]string, error) {
	mod, ok := p.Module(pkgPath)
	if !ok {
		return nil, errors.New(noSuchModule + pkgPath)
	}

	if mod.Dir == "" {
		return nil, errors.New(noModuleDir + pkgPath)
	}

	var found []string

	err := filepath.WalkDir(mod.Dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !entry.IsDir() && filepath.Ext(path) == flowExt {
			found = append(found, path)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s%s: %w", cannotRead, mod.Dir, err)
	}

	sort.Strings(found)

	return found, nil
}

// Flow is one flow declaration a module publishes.
//
// IT CARRIES NO Exported FIELD. Export is derived from Name through the Exported
// predicate whenever it is asked for; a stored boolean sitting beside the name it
// was derived from is a second authority, and two authorities eventually
// disagree.
//
// OUTPUTS CARRIES THE DECLARED TYPE SPELLINGS, not the output names, because the
// declared TYPES are what a signature contributes across a module boundary. The
// names travel the other way: a consumer's `use` statement NAMES the outputs it
// binds, order carries no meaning, and whether those names are outputs of the
// flow at all is checked above this module. Each entry is the Go type text
// exactly as the flow author wrote it,
// which is what Resolve turns into a types.Type. HasSignature false means this
// module makes NO type claim about the flow at all, which is a different
// statement from an empty output list.
type Flow struct {
	Name         string
	File         string
	Pos          ast.Position
	HasSignature bool
	Outputs      []string
}

// Flows reports every flow a package's module declares, with its signature
// header read off the declaration.
//
// A SOURCE THAT DOES NOT PARSE IS A LOUD REFUSAL, NEVER A PARTIAL RESULT. This
// is the one place in discovery where the alternative failure is INVISIBLE:
// silently skipping a damaged file makes a later reference report `no flow named
// X in module M`, which sends the author hunting for a flow that exists and
// parses fine, in a file the loader quietly dropped. A wrong diagnostic is worse
// than no diagnostic, so the file is named and the parser's own positioned
// message is carried out whole.
//
// WHAT IT READS AND DOES NOT DERIVE: HasSignature and Outputs are facts read off
// the declaration. No types are derived for a signature-less flow here or
// anywhere in this module.
func (p *Packages) Flows(pkgPath string) ([]Flow, error) {
	sources, err := p.Sources(pkgPath)
	if err != nil {
		return nil, err
	}

	var found []Flow

	for _, path := range sources {
		// The path came from this module's own walk of a resolved module
		// directory, not from a caller.
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s%s: %w", cannotRead, path, err)
		}

		file, err := ast.Parse(src)
		if err != nil {
			return nil, fmt.Errorf("%s%s: %w", cannotParse, path, err)
		}

		for _, decl := range file.Decls {
			// The AST is consumed in VALUE form: lang/ast declares every node's
			// interface methods on a value receiver, so a pointer-form assertion
			// compiles clean and matches nothing, forever.
			if flow, ok := decl.(ast.FlowDecl); ok {
				found = append(found, declaredFlow(flow, path))
			}
		}
	}

	return found, nil
}

// declaredFlow reads one declaration into the value this module publishes.
func declaredFlow(decl ast.FlowDecl, path string) Flow {
	flow := Flow{Name: decl.Name.Name, File: path, Pos: decl.Pos()}

	if decl.Signature == nil {
		return flow
	}

	flow.HasSignature = true
	flow.Outputs = make([]string, 0, len(decl.Signature.Outputs))

	for _, out := range decl.Signature.Outputs {
		flow.Outputs = append(flow.Outputs, out.Type.Text)
	}

	return flow
}

// Exported reports whether a flow name is exported, by GO'S OWN RULE.
//
// IT DELEGATES RATHER THAN REIMPLEMENTS, and that is the whole of its
// correctness. The ruling names Go's rule, so calling Go's own function is the
// only implementation that cannot drift from it. The natural hand-rolled version
// — testing the first BYTE against 'A' and 'Z' — compiles, passes every ASCII
// fixture, and silently calls every non-ASCII uppercase name module-private.
//
// Delegation inherits Go's consequences deliberately, including the ones Go
// cannot express: a name opening in a caseless script has no uppercase form and
// is therefore never exported, exactly as the equivalent Go identifier is
// unexportable. Matching Go means matching what Go cannot do too.
func Exported(name string) bool {
	return token.IsExported(name)
}

// ResolveFlow resolves a cross-module flow reference, or refuses it with a
// diagnostic positioned at the reference that wrote it.
//
// THREE REFUSALS, AND KEEPING THEM DISTINCT IS THE POINT OF THE FUNCTION. A name
// that names no flow reports that no such flow exists. A name that DOES name a
// flow, but a module-private one, reports that it is not exported and says how to
// export it. A module carrying a source that does not parse reports the parse
// failure and names the file. Collapsing any pair still refuses every case, and
// still compiles — and then tells an author to capitalize the first letter of a
// flow that does not exist, or sends them looking for a flow that is sitting
// perfectly well in a file the loader could not read.
//
// A SIGNATURE-LESS EXPORTED FLOW RESOLVES, WITH NO REFUSAL AND NO TYPING. The
// signature is optional on an exported flow: present, its declared types are the
// cross-module contract; absent, the flow comes back with HasSignature false and
// no outputs, and this module asserts NOTHING about what it carries. That is not
// a gap being papered over — deciding what such a flow carries is inference over
// the flow body, which belongs to the analysis layer ABOVE this module. A caller
// that needs those types asks the type-flow, and the answer reaches it through
// the caller sitting above both, never through this module reaching sideways.
//
// NO FALLBACK RESOLUTION PATHS. A reference that resolves to nothing produces a
// diagnostic. There is no same-module retry, no nearest-name suggestion, and
// above all no case-insensitive retry — which would defeat the export rule
// itself, the rule being exactly a case distinction.
//
// THE POSITION IS THE REFERENCE'S, not the declaration's: a refusal belongs where
// the author wrote the thing being refused, which is why this takes `at` and
// `from` at all.
func (p *Packages) ResolveFlow(pkgPath, name string, at ast.Position, from string) (Flow, *Diagnostic) {
	flows, err := p.Flows(pkgPath)
	if err != nil {
		return Flow{}, refusal(at, from, err.Error())
	}

	for _, flow := range flows {
		if flow.Name != name {
			continue
		}

		if !Exported(name) {
			return Flow{}, refusal(at, from, flowPrefix+name+notExported+pkgPath+capitalizeFix)
		}

		return flow, nil
	}

	return Flow{}, refusal(at, from, noFlowNamed+name+inModule+pkgPath)
}

// refusal builds a diagnostic positioned at the reference that caused it.
//
// End equals Pos because the caller supplies the one position it has — the point
// the reference sits at. Inventing an end would be a claim about extent that
// nothing measured.
func refusal(at ast.Position, from, message string) *Diagnostic {
	return &Diagnostic{Path: from, Pos: at, End: at, Message: message}
}
