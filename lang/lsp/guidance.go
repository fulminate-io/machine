// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lsp

import (
	"errors"

	"github.com/whitaker-io/machine/lang/analysis"
	"go.lsp.dev/protocol"
)

// flowGuidanceMethod is the JSON-RPC method a decode-time monitor queries.
const flowGuidanceMethod = "flow/guidance"

// errGuidanceParams reports a flow/guidance request whose params did not arrive
// as the opaque JSON the dispatcher passes through.
var errGuidanceParams = errors.New("lsp: flow/guidance params did not arrive as JSON")

// GuidanceParams asks what may legally be named at one position.
//
// It mirrors the standard LSP position-request shape rather than inventing one,
// so a client that can build a completion request can build this.
type GuidanceParams struct {
	TextDocument protocol.TextDocumentIdentifier `json:"textDocument"`
	Position     protocol.Position               `json:"position"`
}

// GuidanceResult is the guidance in force at that position.
//
// IT IS THE TABLE'S OWN VALUE PROJECTED ONTO WIRE TYPES, field for field, with
// no filtering, ranking or augmentation. Computation stays in lang/analysis;
// this module serves. A consumer wanting ranked guidance is asking for a
// computation change, and it belongs in the analysis core.
//
// An empty result IS the refusal. There is no separate boolean: a request
// naming a document the server does not know, or a position that resolves to no
// scope, comes back with an empty Flow and empty slices rather than another
// document's guidance dressed up as this one's.
type GuidanceResult struct {
	Flow      string       `json:"flow"`
	Producers []string     `json:"producers"`
	Storage   []string     `json:"storage"`
	Imports   []ImportInfo `json:"imports"`
}

// ImportInfo is one import as the source declared it. Alias is empty where the
// source declared none — this server does not invent one, because a package's
// name is not derivable from its path.
type ImportInfo struct {
	Alias string `json:"alias"`
	Path  string `json:"path"`
}

// guidance answers a flow/guidance request.
//
// THE SAME BOUNDED LOOKUP COMPLETION USES. The table was built when the
// document last changed, so a monitor querying this on every decoded token pays
// a binary search rather than an analysis.
func (s *Server) guidance(params any) (any, error) {
	raw, ok := params.(protocol.LSPAny)
	if !ok {
		return nil, errGuidanceParams
	}
	var p GuidanceParams
	if err := protocol.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	found, ok := s.guidanceAt(p.TextDocument.URI, p.Position)
	if !ok {
		return GuidanceResult{}, nil
	}
	return guidanceResult(found), nil
}

// guidanceResult projects one Guidance value onto the wire types.
func guidanceResult(g analysis.Guidance) GuidanceResult {
	imports := make([]ImportInfo, 0, len(g.Imports))
	for _, ref := range g.Imports {
		imports = append(imports, ImportInfo{Alias: ref.Alias, Path: ref.Path})
	}
	return GuidanceResult{
		Flow:      g.Flow,
		Producers: g.Producers,
		Storage:   g.Storage,
		Imports:   imports,
	}
}
