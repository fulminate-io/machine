// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// Package analysis is the flow language's analysis core: a pass framework in the
// STYLE of go/analysis, the analyzers built on it, and the Mermaid renderer that
// draws what they derive.
//
// The framework is our own rather than an import of golang.org/x/tools/go/analysis
// because that package's Pass hard-binds Go's own trees — Files []*go/ast.File,
// Pkg *types.Package, TypesInfo *types.Info — with no seam through which a flow
// tree could be supplied. Style is the only available reuse.
//
// THE AST IS CONSUMED IN VALUE FORM. lang/ast declares every node's interface
// methods on a VALUE receiver, so both T and *T satisfy ast.Stmt and a
// pointer-form type switch compiles clean while matching nothing, forever. Parse
// emits the value form. Every type switch in this package therefore writes
// `case ast.TransformStmt:` and carries a default arm. The one exception is
// ast.Parse's own return: the FILE is a pointer while the declarations and
// statements inside it are values.
//
// TEXT SCANS OVER .flow SOURCE ARE FORBIDDEN IN THIS MODULE. A note block is an
// unconstrained raw region, so any scan over source text matches prose inside one
// — a grep census for unconsumed producers once reported a file clean because the
// name it was looking for appeared in that file's own note. Every analyzer keys
// on the parsed tree.
package analysis
