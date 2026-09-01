// Package loader - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package loader

import (
	"errors"
	goast "go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"

	"golang.org/x/tools/go/packages"
)

// loadMode is what a caller of this module needs go/packages to fill in, and
// every bit of it is load-bearing rather than defensive.
//
// NeedModule is what makes cross-module work possible at all: flow-source
// discovery reads a package's Module.Dir to reach a DEPENDENCY's sources in the
// module cache, and without it that field is nil and the whole capability
// degrades silently to whatever happens to sit in the local tree. NeedDeps and
// NeedImports are what make an imported package's types resolvable rather than
// opaque. NeedSyntax and NeedTypesInfo are what let a consumer map a Go
// expression back to a type, which is what typing a flow node's Go span needs.
const loadMode = packages.NeedName |
	packages.NeedTypes |
	packages.NeedSyntax |
	packages.NeedTypesInfo |
	packages.NeedModule |
	packages.NeedDeps |
	packages.NeedImports

const (
	noPatterns    = "loader: Load needs at least one package pattern"
	cannotLoad    = "loader: cannot load packages in "
	noPackages    = "loader: no packages matched the given patterns in "
	unknownPath   = "loader: no loaded package has the import path "
	cannotResolve = "loader: cannot resolve the type spelling "
	ambiguous     = "loader: the type spelling "
)

// Packages is one generation run's loaded, type-checked package set.
//
// It is a VALUE THE CALLER OWNS AND KEEPS. This module holds no process-global
// cache, so the lifetime of a load is the caller's to decide.
type Packages struct {
	byPath map[string]*packages.Package
	paths  []string
}

// Load type-checks the packages the patterns name, rooted at dir.
//
// PACKAGE LOADING IS THE EXPENSIVE OPERATION IN THIS WHOLE TOOLCHAIN — seconds,
// not microseconds. Load is therefore called ONCE per generation run, over every
// package together; never once per node, once per file or once per type. A
// caller that loads per unit of work turns a one-off cost into a per-unit one
// and will feel it immediately at interactive speeds.
//
// THE CACHE LIFETIME IS THE CALLER'S, because this module holds no
// process-global cache. A long-lived consumer such as a language server keeps
// its own *Packages for as long as it is valid and calls Load again when go.mod
// changes; a one-shot generator loads once and drops the result when it exits.
//
// IT REFUSES RATHER THAN RETURNS AN EMPTY LOAD. An empty pattern set, a
// directory the toolchain cannot load, and a pattern set matching no package at
// all are each an error naming what could not be done. Nothing here hands back a
// usable-looking zero value beside a nil error.
//
// WHAT IT DOES NOT DO. A flow whose declaration carries no signature is not
// typed by this module — not here and not by any surface it exposes. Deciding
// what such a flow carries is inference over the flow graph, which belongs to
// the analysis layer ABOVE this module; the result reaches a consumer through
// the caller sitting above both, never through this module reaching sideways.
func Load(dir string, patterns []string) (*Packages, error) {
	if len(patterns) == 0 {
		return nil, errors.New(noPatterns)
	}

	roots, err := packages.Load(&packages.Config{Mode: loadMode, Dir: dir}, patterns...)
	if err != nil {
		return nil, errors.New(cannotLoad + dir + ": " + err.Error())
	}

	loaded := &Packages{byPath: map[string]*packages.Package{}}

	// Every REACHABLE package is indexed, not only the roots: a spelling in a
	// generated package routinely names a type an imported package declares, and
	// discovery reaches a dependency module through the same index.
	packages.Visit(roots, func(pkg *packages.Package) bool {
		loaded.byPath[pkg.PkgPath] = pkg

		return true
	}, nil)

	if len(loaded.byPath) == 0 {
		return nil, errors.New(noPackages + dir)
	}

	loaded.paths = make([]string, 0, len(loaded.byPath))
	for path := range loaded.byPath {
		loaded.paths = append(loaded.paths, path)
	}

	sort.Strings(loaded.paths)

	return loaded, nil
}

// Scope reports the package scope a spelling resolves against, and whether the
// package was loaded at all.
//
// The second result is the caller's discriminator between "this package declares
// nothing by that name" and "this package was never loaded", which are different
// problems with different fixes.
func (p *Packages) Scope(pkgPath string) (*types.Scope, bool) {
	pkg, ok := p.byPath[pkgPath]
	if !ok || pkg.Types == nil {
		return nil, false
	}

	return pkg.Types.Scope(), true
}

// Resolve turns a Go type SPELLING — the text a flow signature header carries —
// into the type it denotes inside the named package.
//
// THIS IS THE DEEPENING THE FLOW ANALYZERS COULD NOT DO. Comparing signatures as
// text equates nothing that is spelled two ways, and distinguishes nothing that
// is spelled one way and means two things. Evaluating the spelling in the
// package's OWN scope is what makes an alias, a dot-qualified name and a
// locally-renamed import resolve exactly as they would inside that package.
//
// THE SPELLING IS EVALUATED IN A FILE'S SCOPE, NOT THE PACKAGE'S, and that is a
// measured requirement rather than a preference. An import is declared in the
// FILE scope, so at the package scope's own position an import-qualified
// spelling resolves to nothing at all: evaluating "gob.GobEncoder" there reports
// `undefined: gob` even in a package whose files import encoding/gob. Since a
// flow signature header routinely names a type through its package qualifier,
// resolution walks EVERY file of the package, in filename order.
//
// A SPELLING ITS FILES DISAGREE ABOUT IS REFUSED, NOT DECIDED. Imports are
// per-file, so one alias can name two packages in two files of the same package
// — and since, say, encoding/gob and encoding/json both export Encoder, the
// spelling `codec.Encoder` can denote two different types in one package.
// Answering with the first file to resolve it would be deterministic and still
// wrong: it hands the caller one candidate and never discloses that another
// existed. The walk therefore continues past its first hit and refuses when two
// files resolve the spelling to types that are not identical, naming both
// candidates and the files they came from. Files that AGREE are not a
// disagreement, and resolution answers normally.
//
// EVERY REFUSAL NAMES ITS SUBJECT, and none has a fallback. A package that was
// never loaded is an error naming the path; a spelling that does not resolve is
// an error naming the spelling and the package it was resolved against; an
// ambiguous one names what it could not choose between. This function never
// hands back an invalid type beside a nil error — a caller cannot tell that
// shape from a real answer, which is exactly what makes it a defect rather than
// a convenience.
func (p *Packages) Resolve(pkgPath, spelling string) (types.Type, error) {
	pkg, ok := p.byPath[pkgPath]
	if !ok || pkg.Types == nil {
		return nil, errors.New(unknownPath + pkgPath)
	}

	reason := "it denotes no type"

	var (
		found types.Type
		from  string
	)

	for _, site := range evalSites(pkg) {
		evaluated, err := types.Eval(pkg.Fset, pkg.Types, site.pos, spelling)
		if err != nil {
			reason = err.Error()

			continue
		}

		if evaluated.Type == nil || evaluated.Type == types.Typ[types.Invalid] {
			continue
		}

		if found == nil {
			found, from = evaluated.Type, site.file

			continue
		}

		if !types.Identical(found, evaluated.Type) {
			return nil, errors.New(ambiguous + spelling + " is ambiguous in " + pkgPath + ": " +
				from + " resolves it to " + found.String() + ", " +
				site.file + " resolves it to " + evaluated.Type.String())
		}
	}

	if found == nil {
		return nil, errors.New(cannotResolve + spelling + " in " + pkgPath + ": " + reason)
	}

	return found, nil
}

// evalSite is one place a spelling can be evaluated: a position to evaluate at,
// and the file name to blame in a refusal.
type evalSite struct {
	file string
	pos  token.Pos
}

// evalSites reports the sites a spelling is evaluated at, in a stable order.
//
// One site per file, because file scope is where imports live, sorted by
// filename so that a package's files are always consulted in the same order and
// an ambiguity is reported with its candidates named the same way on every run.
// A package loaded without syntax — a dependency reached for its types alone —
// has only its package scope to offer, which still resolves every unqualified
// spelling, the only kind that could resolve without imports anyway.
func evalSites(pkg *packages.Package) []evalSite {
	if len(pkg.Syntax) == 0 {
		return []evalSite{{file: pkg.PkgPath, pos: pkg.Types.Scope().Pos()}}
	}

	files := make([]*goast.File, len(pkg.Syntax))
	copy(files, pkg.Syntax)
	sort.Slice(files, func(i, j int) bool {
		return pkg.Fset.Position(files[i].Pos()).Filename < pkg.Fset.Position(files[j].Pos()).Filename
	})

	at := make([]evalSite, 0, len(files))
	for _, file := range files {
		at = append(at, evalSite{
			file: filepath.Base(pkg.Fset.Position(file.Pos()).Filename),
			pos:  file.Name.End(),
		})
	}

	return at
}

// Errors reports the type-check problems of packages that loaded successfully.
//
// IT IS A DIFFERENT QUESTION FROM LOAD'S OWN ERROR. Load fails when the package
// set could not be loaded at all; this reports what went wrong INSIDE a set that
// loaded fine, which is the ordinary outcome for a generated package carrying a
// spelling that does not type-check.
//
// The order is deterministic. Package paths are walked through the sorted slice
// built at load time rather than through Go's randomized map order, so two runs
// over the same tree report the same problems in the same sequence.
func (p *Packages) Errors() []Diagnostic {
	var found []Diagnostic

	for _, path := range p.paths {
		for _, failure := range p.byPath[path].Errors {
			at, pos := splitPos(failure.Pos)
			found = append(found, Diagnostic{Path: at, Pos: pos, End: pos, Message: failure.Msg})
		}
	}

	return found
}
