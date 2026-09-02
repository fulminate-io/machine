// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package lsp

import (
	"context"

	"go.lsp.dev/protocol"
)

// Initialize declares what this server can do and scans the workspace once.
//
// THE ROOT COMES FROM WorkspaceFolders. InitializeParams.RootURI and .RootPath
// are both marked deprecated in the protocol — rootPath superseded by rootUri,
// and rootUri in turn by workspaceFolders — so reading either would draw SA1019
// under the shared lint config as well as being the wrong question to ask a
// modern client.
//
// A CLIENT SUPPLYING NO FOLDERS IS IN SINGLE-FILE MODE, which is a complete
// answer rather than an error or a degraded one: the open document still
// parses, analyzes and completes, and the cross-file capabilities simply have
// no other files to reach. The process working directory is NOT substituted —
// that would manufacture a workspace the client never declared.
func (s *Server) Initialize(_ context.Context, params *protocol.InitializeParams) (*protocol.InitializeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if folders, ok := params.WorkspaceFolders.Get(); ok {
		if err := s.scan(folders); err != nil {
			return nil, err
		}
	}
	return &protocol.InitializeResult{
		Capabilities: capabilities(),
		ServerInfo:   protocol.ServerInfo{Name: serverName},
	}, nil
}

// scan walks each workspace folder the client declared.
func (s *Server) scan(folders []protocol.WorkspaceFolder) error {
	for _, folder := range folders {
		if err := s.store.Scan(folder.URI.FsPath()); err != nil {
			return err
		}
	}
	return nil
}

// capabilities is what this version serves.
//
// PositionEncoding is declared EXPLICITLY rather than left to the protocol's
// default. utf-16 is the only value a server may return when the client offers
// no encodings, so the wire behavior would be the same either way — but
// declaring it is what makes the single conversion path in Mapper the only path
// there is. A utf-8 negotiation is deliberately not built: it would add a
// second, rarely-exercised conversion for no capability this server lacks.
//
// Sync is FULL-DOCUMENT because lang/ast publishes no incremental parse entry
// point and documents a cheap full reparse instead, so range-based sync would
// buy nothing this server could act on.
func capabilities() protocol.ServerCapabilities {
	return protocol.ServerCapabilities{
		PositionEncoding:   protocol.PositionEncodingKindUTF16,
		TextDocumentSync:   protocol.TextDocumentSyncKindFull,
		CompletionProvider: &protocol.CompletionOptions{},
		DefinitionProvider: protocol.Boolean(true),
	}
}

// Shutdown marks the server shut down.
//
// The mark is not decoration: the protocol requires a server to stop doing work
// once shutdown has been requested, so every document notification after this
// point is refused by name rather than quietly processed.
func (s *Server) Shutdown(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.down = true
	return nil
}

// Exit ends the session. Teardown belongs to whoever owns the connection, which
// for the stdio binary is the process itself.
func (*Server) Exit(_ context.Context) error {
	return nil
}
