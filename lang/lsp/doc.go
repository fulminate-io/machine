// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// Package lsp serves the flow language's analysis over the Language Server
// Protocol: diagnostics, completion, go-to-definition, and two flow-specific
// endpoints that hand an editor or a generating model what the analyzers know.
//
// NO ANALYSIS LOGIC LIVES HERE. Every answer this server gives is a lookup over
// a table lang/analysis built, or a conversion of a value it produced. The
// module owns exactly three things the analysis core deliberately does not: the
// byte-to-UTF-16 column conversion the protocol requires, the document state an
// editor's unsaved buffers create, and the wire surface itself.
//
// THE AST IS CONSUMED IN VALUE FORM, for the reason lang/analysis records at
// length: lang/ast declares every node's interface methods on a VALUE receiver,
// so a pointer-form type switch compiles clean and matches nothing forever.
// ast.Parse's own return is the exception — the File is a pointer while the
// declarations and statements inside it are values.
package lsp

// ScalingDisclosure states what this server's per-change cost is a function of.
//
// IT IS A CONSTANT RATHER THAN A COMMENT, for the reason lang/analysis puts its
// own truthfulness disclosures in Analyzer.Doc: a Go comment is invisible to
// every consumer at runtime, and the consumers who most need this one are an
// editor deciding how much workspace to open and a generating model deciding
// how far to trust a latency it just observed. Served over flow/analyzers.
const ScalingDisclosure string = "This server's cost per document change is LINEAR IN TOTAL WORKSPACE BYTES, " +
	"not in the size of the change. lang/analysis walks every source in the workspace on every run and publishes " +
	"no incremental entry point, so a keystroke in a one-line file still costs a full analysis of every open " +
	"document. Only the changed document is reparsed; the analysis that follows is not incremental and cannot be " +
	"made so from this side. READ A MEASUREMENT ACCORDINGLY: a change-to-diagnostics figure that rises as the " +
	"workspace grows is this documented property and not a regression, while a figure that rises for a FIXED " +
	"workspace is a defect signal worth reporting."
