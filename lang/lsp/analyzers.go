// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lsp

import (
	"context"

	"github.com/whitaker-io/machine/lang/analysis"
)

// flowAnalyzersMethod is the JSON-RPC method that answers with the analyzer
// roster and this server's scaling disclosure.
const flowAnalyzersMethod = "flow/analyzers"

// AnalyzerInfo is one analyzer's identity and its documented behavior, VERBATIM.
//
// Doc is never truncated, summarized or reformatted on the way out. Six of the
// analyzers carry required truthfulness disclosures in exactly this field, put
// there by the analysis core because a Go comment is invisible to a consumer at
// runtime — the starkest being typeflow's, which states outright that it is not
// type checking and that its silence is not agreement. Shortening the payload
// would delete the very thing the field exists to carry.
type AnalyzerInfo struct {
	Name string `json:"name"`
	Doc  string `json:"doc"`
}

// AnalyzersResult is what flow/analyzers answers with.
//
// Scaling carries ScalingDisclosure verbatim, so a consumer driving this server
// over a large workspace can READ that per-change cost is linear in total
// workspace bytes rather than having to infer it from latency.
type AnalyzersResult struct {
	Analyzers []AnalyzerInfo `json:"analyzers"`
	Scaling   string         `json:"scaling"`
}

// Request answers this server's flow-specific methods.
//
// Non-standard methods route here: the library's dispatcher reports a method it
// does not recognize as unhandled and falls through to Server.Request with the
// method name and opaque params. Anything this server does not answer goes back
// to the embedded default, which refuses it as method-not-found rather than
// inventing a reply.
func (s *Server) Request(ctx context.Context, method string, params any) (any, error) {
	switch method {
	case flowAnalyzersMethod:
		return analyzerRoster(), nil
	case flowGuidanceMethod:
		return s.guidance(params)
	default:
		return s.UnimplementedServer.Request(ctx, method, params)
	}
}

// analyzerRoster reports whatever the registry holds, rather than a list copied
// into this file — so a twelfth analyzer appears here without an edit.
func analyzerRoster() AnalyzersResult {
	all := analysis.All()
	out := make([]AnalyzerInfo, 0, len(all))
	for _, a := range all {
		out = append(out, AnalyzerInfo{Name: a.Name, Doc: a.Doc})
	}
	return AnalyzersResult{Analyzers: out, Scaling: ScalingDisclosure}
}
