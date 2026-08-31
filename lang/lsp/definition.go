// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lsp

import (
	"context"
	"strings"

	"github.com/whitaker-io/machine/lang/analysis"
	"github.com/whitaker-io/machine/lang/ast"
	"go.lsp.dev/protocol"
)

// Definition answers where the name under the cursor is declared.
//
// THERE ARE TWO PATHS, because the symbol table holds one of the two edges and
// not the other.
//
// Path one is flow-level names, where the table holds both sides: Consumers
// maps a name to the statements referencing it and Producers to the statement
// declaring it, so a lookup answers directly.
//
// Path two is the edge from a `use` statement to the flow it names, which is
// tabled NOWHERE. The symbols collector passes a use statement's clauses, its
// instance name and its bindings to collector methods; UseStmt.Flow is passed
// to none of them, so the referenced flow's name has no recorded position
// anywhere. A handler following path one alone answers nothing for every `use`
// — including every cross-file jump, which is the case this capability is most
// wanted for.
//
// When neither path finds a target, no location is returned. A nearest guess
// would send an author somewhere they did not ask to go.
func (s *Server) Definition(_ context.Context, params *protocol.DefinitionParams) (protocol.DefinitionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, ok := s.store.Get(params.TextDocument.URI)
	if !ok || s.snap == nil || s.snap.Symbols == nil {
		return nil, nil
	}
	offset, ok := doc.Mapper.ToOffset(params.Position)
	if !ok {
		return nil, nil
	}
	return s.locate(doc, offset), nil
}

// locate runs the two paths in order, cheapest first. The caller holds s.mu.
func (s *Server) locate(doc *Document, offset int) protocol.DefinitionResult {
	file, ok := fileSymbols(s.snap.Symbols, doc.Path)
	if !ok {
		return nil
	}
	for i := range file.Flows {
		flow := &file.Flows[i]
		if loc, found := producerJump(doc, flow, offset); found {
			return loc
		}
		if loc, found := s.useFlowJump(flow, offset); found {
			return loc
		}
	}
	return nil
}

// producerJump answers a reference to a flow-level name with the position of
// the statement that declares it.
func producerJump(doc *Document, flow *analysis.FlowSymbols, offset int) (protocol.DefinitionResult, bool) {
	name, ok := consumerUnder(flow, offset)
	if !ok {
		return nil, false
	}
	refs := flow.Producers[name]
	if len(refs) == 0 {
		return nil, false
	}
	return locationIn(doc, refs[0].Pos), true
}

// useFlowJump answers a reference to the flow a `use` statement names, which
// may be declared in any file in the run — including one the editor never
// opened, which is why the workspace is scanned rather than only the buffers.
func (s *Server) useFlowJump(flow *analysis.FlowSymbols, offset int) (protocol.DefinitionResult, bool) {
	path, ok := useFlowRefUnder(flow, offset)
	if !ok {
		return nil, false
	}
	target, ok := s.snap.Symbols.Flow(dottedName(path))
	if !ok {
		return nil, false
	}
	return s.locationOfFlow(target)
}

// locationOfFlow finds the document a flow was declared in and converts the
// declaration's position through THAT document's mapper, since a position is
// only meaningful against the bytes it was measured in.
func (s *Server) locationOfFlow(target *analysis.FlowSymbols) (protocol.DefinitionResult, bool) {
	for f := range s.snap.Symbols.Files {
		file := &s.snap.Symbols.Files[f]
		for i := range file.Flows {
			if &file.Flows[i] != target {
				continue
			}
			doc, ok := s.store.byPath(file.Src.Path)
			if !ok {
				return nil, false
			}
			return locationIn(doc, target.Pos), true
		}
	}
	return nil, false
}

// consumerUnder is the flow-level name referenced at offset, if any.
func consumerUnder(flow *analysis.FlowSymbols, offset int) (string, bool) {
	for name, refs := range flow.Consumers {
		for _, ref := range refs {
			if covers(ref.Pos, name, offset) {
				return name, true
			}
		}
	}
	return "", false
}

// useFlowRefUnder finds the dotted flow reference a `use` statement names at
// offset, by walking the flow's body.
//
// THE WALK IS THE ONLY ROUTE. No table holds this edge, so there is nothing to
// look up. It is bounded by one flow's statement count and it runs only after
// path one found nothing, so the common case pays nothing for it.
func useFlowRefUnder(flow *analysis.FlowSymbols, offset int) ([]ast.Ident, bool) {
	for _, stmt := range flow.Body {
		use, ok := stmt.(ast.UseStmt)
		if !ok {
			continue
		}
		for _, id := range use.Flow {
			if covers(id.NamePos, id.Name, offset) {
				return use.Flow, true
			}
		}
	}
	return nil, false
}

// dottedName joins a use statement's reference path the way the analysis core
// keys it, so the two agree on what a reference names.
//
// A MULTI-SEGMENT REFERENCE RESOLVES TO NOTHING TODAY, and that is faithful
// rather than a gap: SymbolTable.Flow matches a flow's declared name, which is
// a single identifier, so a namespaced reference finds no entry in the core
// either. Falling back to the last segment would answer a question the system
// cannot yet decide.
func dottedName(path []ast.Ident) string {
	parts := make([]string, 0, len(path))
	for _, id := range path {
		parts = append(parts, id.Name)
	}
	return strings.Join(parts, ".")
}

// covers reports whether offset falls inside the name written at pos.
func covers(pos ast.Position, name string, offset int) bool {
	return offset >= pos.Offset && offset < pos.Offset+len(name)
}

// fileSymbols is one file's entry in the run's symbol table.
func fileSymbols(table *analysis.SymbolTable, path string) (*analysis.FileSymbols, bool) {
	for f := range table.Files {
		if table.Files[f].Src.Path == path {
			return &table.Files[f], true
		}
	}
	return nil, false
}

// locationIn renders a parser position as a protocol Location in a document,
// as a zero-width range at the declaration's name.
func locationIn(doc *Document, pos ast.Position) protocol.DefinitionResult {
	at := doc.Mapper.ToLSP(pos)
	return &protocol.Location{URI: doc.URI, Range: protocol.Range{Start: at, End: at}}
}
