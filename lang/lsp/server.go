// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lsp

import (
	"context"
	"io"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

// serverName identifies this server to a client in InitializeResult.ServerInfo
// and as the Source on every diagnostic it publishes.
const serverName = "flowlsp"

// Server serves the flow language's analysis over the Language Server Protocol.
//
// IT EMBEDS UnimplementedServer RATHER THAN IMPLEMENTING THE INTERFACE. The
// protocol.Server interface declares 75 methods; the library ships an
// embeddable default whose un-overridden request methods return a method-not-
// found error and whose un-overridden notifications return nil. This version
// overrides exactly ten and inherits the rest, so a method this server does not
// serve reports that honestly instead of returning an empty result that tells
// an editor the feature exists and produces nothing.
//
// THE CLIENT IS NOT A FIELD HERE. protocol.NewServer builds the Client itself
// out of the connection it is given, which means the Server value must exist
// before any Client does. Handlers reach it through protocol.ClientFromContext
// and fail loudly when it is absent.
type Server struct {
	protocol.UnimplementedServer

	store *Store
	snap  *snapshot
	down  bool
}

// NewServer returns a server backed by store.
func NewServer(store *Store) *Server {
	return &Server{store: store}
}

// Serve runs one LSP session over rw until the connection terminates.
//
// It takes any duplex stream rather than reaching for os.Stdin and os.Stdout
// itself, which is what lets the whole server be driven in-process over an
// io.Pipe with no subprocess anywhere in the tests.
func Serve(ctx context.Context, rw io.ReadWriteCloser) error {
	_, conn, _ := protocol.NewServer(ctx, NewServer(NewStore()), jsonrpc2.NewStream(rw))
	defer func() { _ = conn.Close() }()

	<-conn.Done()
	return conn.Err()
}
