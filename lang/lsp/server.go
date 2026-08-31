// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lsp

import (
	"context"
	"io"
	"sync"

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
//
// HANDLERS RUN CONCURRENTLY AND ARE SERIALIZED HERE. protocol.Handlers wraps
// every handler in jsonrpc2.AsyncHandler, which releases the read loop the
// moment a handler begins, so a didChange and a completion request really do
// execute at the same time. The mutex is held for a whole document
// notification, INCLUDING the publish, for two reasons: without it the Store's
// maps race (observed under -race), and with a narrower lock two racing changes
// could still publish out of order, leaving an editor rendering diagnostics for
// text its author already replaced. Serializing costs nothing measurable —
// these operations are tens of microseconds — and it is what makes the serial
// design this module is built on true rather than assumed.
type Server struct {
	protocol.UnimplementedServer

	mu    sync.Mutex
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
