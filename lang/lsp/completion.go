// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lsp

import (
	"context"

	"github.com/whitaker-io/machine/lang/analysis"
	"github.com/whitaker-io/machine/lang/ast"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// Completion offers what may legally be named at the request position.
//
// THIS HANDLER DOES NO ANALYSIS. The guidance table was built once when the
// document last changed, and answering is a binary search over one file's
// prebuilt scope offsets — which is the division the analysis core designed
// for, its own doc noting that an editor calls the accessor per keystroke
// rather than per pass.
//
// An empty list is the truthful answer in three cases: the document is unknown,
// the position does not exist in this version of it, and no scope covers the
// offset. None of them is a reason to guess.
func (s *Server) Completion(_ context.Context, params *protocol.CompletionParams) (protocol.CompletionResult, error) {
	guidance, ok := s.guidanceAt(params.TextDocument.URI, params.Position)
	if !ok {
		return protocol.CompletionItemSlice{}, nil
	}
	return completionItems(guidance), nil
}

// guidanceAt resolves a protocol position to the guidance in force there.
//
// It takes the lock itself: it is reached from Completion and from the
// flow/guidance handler, neither of which holds it, and the snapshot it reads
// is replaced by whichever document notification is running alongside.
func (s *Server) guidanceAt(u uri.URI, pos protocol.Position) (analysis.Guidance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, ok := s.store.Get(u)
	if !ok || s.snap == nil || s.snap.Guidance == nil {
		return analysis.Guidance{}, false
	}
	offset, ok := doc.Mapper.ToOffset(pos)
	if !ok {
		return analysis.Guidance{}, false
	}
	return s.snap.Guidance.At(source(doc), ast.Position{Offset: offset})
}

// completionItems renders one guidance value as completion items, with a kind
// per origin so producers, storage and imports stay distinguishable in an
// editor's list rather than arriving as one undifferentiated pile.
func completionItems(g analysis.Guidance) protocol.CompletionItemSlice {
	out := make(protocol.CompletionItemSlice, 0, len(g.Producers)+len(g.Storage)+len(g.Imports))
	for _, name := range g.Producers {
		out = append(out, protocol.CompletionItem{Label: name, Kind: protocol.CompletionItemKindValue})
	}
	for _, name := range g.Storage {
		out = append(out, protocol.CompletionItem{Label: name, Kind: protocol.CompletionItemKindVariable})
	}
	for _, ref := range g.Imports {
		out = append(out, protocol.CompletionItem{Label: importLabel(ref), Kind: protocol.CompletionItemKindModule})
	}
	return out
}

// importLabel is the alias the source declared, or the import path exactly as
// written where it declared none.
//
// NEVER A GUESS FROM THE PATH. A package's name is not derivable from its path
// without go/types — a last-segment guess yields "v82" for
// github.com/stripe/stripe-go/v82 — so offering an invented identifier would be
// manufacturing an answer this server cannot determine. The path as written is
// the truthful thing to show.
func importLabel(ref analysis.ImportRef) string {
	if ref.Alias != "" {
		return ref.Alias
	}
	return ref.Path
}
