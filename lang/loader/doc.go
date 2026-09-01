// Package loader - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// Package loader is the repository's single owner of go/types: it loads and
// type-checks Go packages once per run, resolves a flow signature's declared Go
// spelling to a types.Type, answers the method-set question separately for a
// type's value and pointer sets, finds the .flow sources a resolved module
// declares, and derives whether a type survives serialization at a given
// position.
//
// IT IS THE ONLY MODULE HERE THAT IMPORTS go/types, go/importer OR
// golang.org/x/tools, and that is a fenced property rather than a habit. A
// go/types walk needs a type switch over types.Type, which the root module's
// erasure gate forbids, and x/tools is barred from the root module's direct
// requirements, which are OTel-only. Homing the loading surface in one module
// keeps both true while giving every consumer one implementation to call
// instead of one each.
//
// THE DEPENDENCY DIRECTION IS ONE-WAY. This module imports lang/ast and
// golang.org/x/tools and nothing else from this repository. It must never
// import lang/analysis, lang/lint or lang/lsp: analysis depends on the loader
// for its go/types deepening, and an edge in the other direction would make
// that a cycle.
//
// WHAT IT DOES NOT DO IS AS LOAD-BEARING AS WHAT IT DOES. This package resolves
// and refuses; it infers nothing. A flow whose declaration carries no signature
// is resolved without complaint and without a type claim of any kind — deciding
// what such a flow carries is type inference over the flow graph, which belongs
// to lang/analysis above this module, never to a reach sideways from inside it.
package loader
