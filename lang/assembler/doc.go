// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// Package assembler turns parsed .flow sources into generated Go that builds and
// runs against the machine runtime.
//
// GENERATED CODE IS A BUILD ARTIFACT. Users commit .flow files; the Go this
// package emits is regenerated whole on every run and is never committed. The
// repository's .gitignore enforces that, with one deliberate exception for this
// module's own golden fixtures, which have to be readable by CI to be evidence.
//
// The pipeline is four stages, each refusing rather than degrading:
//
//	parse   lang/ast produces one ast.File per source; this package consumes them
//	graph   statements become a positioned node graph, edges derived from the
//	        names the AST already carries
//	lower   every statement shape maps onto the runtime's CLOSED builder method
//	        set; a shape with no lowering is a positioned refusal, never a skip
//	emit    the plan becomes a gofmt-stable Go package
//
// NOTHING DEGRADES SILENTLY. Every construct the runtime cannot express is
// refused with a positioned diagnostic naming the .flow line that wrote it. That
// is the whole contract this package offers a caller: what it emits builds, and
// what it cannot emit it says so about.
package assembler
