// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"os"
	"strings"

	"github.com/whitaker-io/machine/lang/ast"
	"github.com/whitaker-io/machine/lang/loader"
)

// Imported is one flow resolved out of another module, ready to inline.
type Imported struct {
	// Ref is the dotted reference the author wrote, which is the key every
	// downstream lookup uses.
	Ref string
	// Source is the DEPENDENCY's file, carried so a diagnostic about it names
	// the module the author imported rather than the one they are writing.
	Source Source
	// Program is the resolved flow's graph, built with its declaration renamed
	// to Ref.
	Program *Program
}

// resolveImports resolves every dotted `use` in the run through the loader.
//
// THE RESOLUTION IS THE LOADER'S AND THIS FILE REACHES NOTHING OF ITS OWN. It
// maps a reference's qualifier to the import the file declares, hands the import
// path and the flow name to Packages.ResolveFlow with the use statement's own
// position, and carries that module's answer — or its refusal — back verbatim. A
// second resolution mechanism is how two answers to one question appear.
func resolveImports(sources []Source, pkgs *loader.Packages) ([]Imported, []Diagnostic) {
	if pkgs == nil {
		return nil, nil
	}

	return resolveImportsWith(sources, pkgs.ResolveFlow)
}

// flowResolver turns an import path and a flow name into that module's
// declaration, or into that module's refusal.
//
// IT IS A FUNCTION RATHER THAN THE PACKAGE SET ITSELF for exactly one reason:
// the LOOKUP is lang/loader's and everything after it — reading the resolved
// file, renaming the declaration, building its graph, splicing its funcs and
// dropping a flow-only import — is this package's, and the second half is what
// the golden fixtures compile. Naming the seam lets a fixture exercise the whole
// of this package's half without standing up a package load per golden case.
// Production passes Packages.ResolveFlow and nothing else does.
type flowResolver func(pkgPath, name string, at ast.Position, from string) (loader.Flow, *loader.Diagnostic)

// resolveImportsWith is resolveImports over a named resolver.
func resolveImportsWith(sources []Source, resolve flowResolver) ([]Imported, []Diagnostic) {
	var (
		imported []Imported
		diags    []Diagnostic
	)
	for _, source := range sources {
		fileImported, fileDiags := resolveFileImports(source, resolve)
		imported = append(imported, fileImported...)
		diags = append(diags, fileDiags...)
	}

	return imported, diags
}

// resolveFileImports resolves one file's dotted references and prunes the
// imports that served only them.
func resolveFileImports(source Source, resolve flowResolver) ([]Imported, []Diagnostic) {
	uses := dottedUses(source.File)
	if len(uses) == 0 {
		return nil, nil
	}

	paths := importPaths(source.File)

	var (
		imported []Imported
		diags    []Diagnostic
		consumed = map[string]int{}
	)
	for _, use := range uses {
		one, useDiags := resolveOne(source, resolve, paths, use)
		diags = append(diags, useDiags...)
		if one == nil {
			continue
		}
		consumed[use.Flow[0].Name]++
		imported = append(imported, *one)
		spliceDependency(source.File, one.Source.File)
	}
	dropFlowOnlyImports(source, consumed)

	return imported, diags
}

// dottedUses is every `use` in a file whose reference carries a dot.
//
// A SINGLE-PART REFERENCE IS NOT ONE. It names a flow declared in this same file
// and is bound by the lowering's ordinary dependency set, which this path does
// not touch.
func dottedUses(file *ast.File) []ast.UseStmt {
	var out []ast.UseStmt
	for _, decl := range file.Decls {
		flow, ok := decl.(ast.FlowDecl)
		if !ok {
			continue
		}
		for _, stmt := range flow.Body {
			if use, isUse := stmt.(ast.UseStmt); isUse && len(use.Flow) > 1 {
				out = append(out, use)
			}
		}
	}

	return out
}

// resolveOne resolves a single dotted reference.
//
// TWO REFUSALS BELONG TO THIS FILE, each positioned at the use statement. The
// grammar's FlowRef admits any number of dots and only the two-part form names a
// module and a flow; and a qualifier no import declares names nothing at all.
// Everything past those two is the loader's answer.
func resolveOne(
	source Source, resolve flowResolver, paths map[string]string, use ast.UseStmt,
) (*Imported, []Diagnostic) {
	ref := dottedRef(identNames(use.Flow))
	if len(use.Flow) != 2 {
		return nil, []Diagnostic{diagnosticAt(use.Start, use.Stop,
			"the use of %q names a flow through %d parts; a cross-module reference is written <import>.<Flow>",
			ref, len(use.Flow))}
	}

	path, declared := paths[use.Flow[0].Name]
	if !declared {
		return nil, []Diagnostic{diagnosticAt(use.Start, use.Stop,
			"the use of %q names the import %q, which this file does not declare",
			ref, use.Flow[0].Name)}
	}

	flow, refusal := resolve(path, use.Flow[1].Name, use.Start, source.Path)
	if refusal != nil {
		// THE LOADER'S REFUSAL IS CARRIED VERBATIM — path, positions and message
		// unchanged — because it is that module's answer and rewording it would
		// make this package a second opinion on a question it does not own.
		return nil, []Diagnostic{{Pos: refusal.Pos, End: refusal.End, Message: refusal.Message, Path: refusal.Path}}
	}

	return buildImported(ref, flow, use)
}

// buildImported reads the resolved flow's own file, renames its declaration to
// the reference, and builds its graph.
//
// EVERY FAILURE HERE CARRIES THE DEPENDENCY'S PATH. The author needs to know the
// problem is in the module they imported rather than in the file they are
// writing, and a diagnostic positioned in their own file would send them looking
// in the wrong place.
func buildImported(ref string, flow loader.Flow, use ast.UseStmt) (*Imported, []Diagnostic) {
	body, err := os.ReadFile(flow.File)
	if err != nil {
		return nil, []Diagnostic{{Path: flow.File, Pos: flow.Pos, End: flow.Pos,
			Message: "the flow " + ref + " could not be read: " + err.Error()}}
	}

	file, parseErr := ast.Parse(body)
	if parseErr != nil {
		return nil, []Diagnostic{{Path: flow.File, Pos: flow.Pos, End: flow.Pos,
			Message: "the flow " + ref + " is in a file that does not parse: " + parseErr.Error()}}
	}

	if !renameFlow(file, flow.Name, ref) {
		return nil, []Diagnostic{{Path: flow.File, Pos: flow.Pos, End: flow.Pos,
			Message: "the flow " + ref + " is no longer declared in " + flow.File}}
	}

	dep := Source{Path: flow.File, Src: body, File: file}
	programs, buildDiags := buildFile(file)
	if len(buildDiags) != 0 {
		return nil, pathed(buildDiags, flow.File)
	}

	for _, program := range programs {
		if program.Name == ref {
			return &Imported{Ref: ref, Source: dep, Program: program}, nil
		}
	}

	return nil, []Diagnostic{diagnosticAt(use.Start, use.Stop,
		"the flow %q resolved but its graph could not be built", ref)}
}

// renameFlow renames one flow declaration to the reference, so every downstream
// key — the dependency set, the boundary fact, the inlined node names — is the
// reference the author wrote rather than the name the dependency chose.
func renameFlow(file *ast.File, from, to string) bool {
	for i, decl := range file.Decls {
		flow, ok := decl.(ast.FlowDecl)
		if !ok || flow.Name.Name != from {
			continue
		}
		flow.Name.Name = to
		file.Decls[i] = flow

		return true
	}

	return false
}

// pathed stamps a dependency's path onto diagnostics raised against it.
func pathed(diags []Diagnostic, path string) []Diagnostic {
	out := make([]Diagnostic, 0, len(diags))
	for _, d := range diags {
		d.Path = path
		out = append(out, d)
	}

	return out
}

// spliceDependency pastes the dependency's imports and funcs into the consuming
// file's declarations.
//
// AN INLINED BODY IS PASTED INTO THE CONSUMER'S GENERATED PACKAGE, so its Go
// references have to resolve there. Its FLOW declarations are deliberately NOT
// spliced: those would become plans of this file, and the dependency's own build
// already generates its wiring.
func spliceDependency(into, dep *ast.File) {
	for _, decl := range dep.Decls {
		switch d := decl.(type) {
		case ast.ImportDecl:
			if !declaresImport(into, d.Path) {
				into.Decls = append(into.Decls, d)
			}
		case ast.FuncDecl:
			if !declaresFunc(into, d.Name.Name) {
				into.Decls = append(into.Decls, d)
			}
		}
	}
}

// declaresImport reports whether a file already imports a path.
func declaresImport(file *ast.File, path string) bool {
	for _, decl := range file.Decls {
		if imp, ok := decl.(ast.ImportDecl); ok && imp.Path == path {
			return true
		}
	}

	return false
}

// declaresFunc reports whether a file already declares a func by name.
//
// A COLLISION KEEPS THE CONSUMER'S OWN. Pasting a second declaration under one
// name emits Go that does not compile, and the file the author is writing is the
// one whose meaning they control.
func declaresFunc(file *ast.File, name string) bool {
	for _, decl := range file.Decls {
		if fn, ok := decl.(ast.FuncDecl); ok && fn.Name.Name == name {
			return true
		}
	}

	return false
}

// dropFlowOnlyImports removes an import that named a module for a flow reference
// and for nothing else.
//
// THE FLOW LANGUAGE'S `import` SERVES TWO PURPOSES — qualifying Go names, and
// naming the module a dotted `use` reaches — and only the FIRST belongs in the
// emitted Go. MEASURED: leaving it in produced `"example.com/upstream" imported
// and not used` against a generated line the author never wrote.
//
// USE IS DECIDED BY SCANNING THE FILE'S OWN BYTES for the qualifier in reference
// position, and subtracting the occurrences the resolved references account for.
// What remains is a Go reference the import is still needed for.
func dropFlowOnlyImports(source Source, consumed map[string]int) {
	if len(consumed) == 0 {
		return
	}

	kept := make([]ast.Decl, 0, len(source.File.Decls))
	for _, decl := range source.File.Decls {
		imp, ok := decl.(ast.ImportDecl)
		if ok && flowOnly(source.Src, qualifierOf(imp), consumed) {
			continue
		}
		kept = append(kept, decl)
	}
	source.File.Decls = kept
}

// flowOnly reports whether every occurrence of a qualifier in the file is
// accounted for by a resolved flow reference.
func flowOnly(src []byte, qualifier string, consumed map[string]int) bool {
	resolved, referenced := consumed[qualifier]
	if !referenced || resolved == 0 {
		return false
	}

	return strings.Count(string(src), qualifier+".") <= resolved
}

// qualifierOf is the name an import declaration binds: its alias when one is
// written, and the last segment of its path otherwise, which is Go's own rule.
func qualifierOf(imp ast.ImportDecl) string {
	if imp.Alias != nil {
		return imp.Alias.Name
	}

	path := strings.Trim(imp.Path, `"`)
	if at := strings.LastIndex(path, "/"); at >= 0 {
		return path[at+1:]
	}

	return path
}

// dottedRef renders a use REFERENCE with the dot the author wrote.
//
// IT IS A SEPARATE FUNCTION FROM renderChain ON PURPOSE, rather than one renderer
// taking a separator: a future message must not be able to pick the wrong render
// by reaching for the nearest helper. A reference is a name; a chain is a
// sequence of hops.
func dottedRef(parts []string) string {
	return strings.Join(parts, ".")
}
