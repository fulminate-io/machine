// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lsp

import (
	"context"
	"net"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// wired is one end-to-end session: a real Server serving a real jsonrpc2
// connection, and the client end of that same connection.
//
// THE TESTS BELOW GO OVER THE WIRE rather than calling handlers, because the
// wire surface IS the subject of this endpoint — a handler that answers
// correctly in-process and drops a field in transport would pass a direct call.
type wired struct {
	server *Server
	conn   jsonrpc2.Conn
	client *capturingClient
	ctx    context.Context
}

func newWired(t *testing.T) *wired {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	srv := NewServer(NewStore())
	client := &capturingClient{}
	ctx := t.Context()

	protocol.NewServer(ctx, srv, jsonrpc2.NewStream(serverSide))
	_, conn, _ := protocol.NewClient(ctx, client, jsonrpc2.NewStream(clientSide))

	t.Cleanup(func() {
		_ = conn.Close()
		_ = serverSide.Close()
	})
	return &wired{server: srv, conn: conn, client: client, ctx: ctx}
}

// open sends a real didOpen notification and waits for the server to publish
// that document's diagnostics, which is the only honest signal the handler ran.
//
// A REQUEST-RESPONSE ROUND TRIP IS NOT A BARRIER HERE, and that was measured
// rather than assumed. protocol.Handlers wraps handlers in
// jsonrpc2.AsyncHandler, which releases the read loop as soon as a handler
// starts, so later messages overtake earlier ones: sending two didOpens and one
// request was observed leaving the server's snapshot holding only the FIRST
// document, one run in ten. The publish is emitted from inside the handler
// after the snapshot is installed, so observing it means the work is done.
func (w *wired) open(t *testing.T, u uri.URI, src string) {
	t.Helper()
	err := w.conn.Notify(w.ctx, protocol.MethodTextDocumentDidOpen, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: u, Text: src},
	})
	if err != nil {
		t.Fatalf("didOpen over the connection failed: %v", err)
	}
	w.awaitPublish(t, u)
}

// awaitPublish blocks until the client has received a diagnostic publish for u,
// and FAILS LOUDLY rather than proceeding on a timeout — a test that carried on
// against a half-built snapshot would report a defect that is not there.
func (w *wired) awaitPublish(t *testing.T, u uri.URI) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(w.client.forURI(u)) > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the server never published diagnostics for %s, so its handler did not complete", u)
}

// askGuidance issues a real flow/guidance request and decodes the response.
func (w *wired) askGuidance(t *testing.T, u uri.URI, pos protocol.Position) GuidanceResult {
	t.Helper()
	var out GuidanceResult
	_, err := w.conn.Call(w.ctx, flowGuidanceMethod, GuidanceParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: u},
		Position:     pos,
	}, &out)
	if err != nil {
		t.Fatalf("flow/guidance over the connection failed: %v", err)
	}
	return out
}

// analyzers issues a real flow/analyzers request over the connection.
//
// The empty params object is required rather than decorative: the dispatcher
// unmarshals a non-standard method's params BEFORE routing to Server.Request,
// so a request carrying no params member fails with a JSON parse error before
// the handler is ever reached.
func (w *wired) analyzers(t *testing.T) AnalyzersResult {
	t.Helper()
	var out AnalyzersResult
	if _, err := w.conn.Call(w.ctx, flowAnalyzersMethod, struct{}{}, &out); err != nil {
		t.Fatalf("flow/analyzers over the connection failed: %v", err)
	}
	return out
}

func TestFlowGuidanceServesTheTableAtAPosition(t *testing.T) {
	w := newWired(t)
	dir := t.TempDir()
	u := uri.File(filepath.Join(dir, "alpha.flow"))

	w.open(t, u, flowStorageAndImports)

	pos := positionAt(t, flowStorageAndImports, "sink done", 0)
	got := w.askGuidance(t, u, pos)

	// THE EXPECTATION COMES FROM THE TABLE IN THE SAME RUN, not from a literal.
	// A literal would be an identity check whose subject supplies its own answer
	// key, and it would false-red the moment the fixture changed.
	want, ok := w.server.guidanceAt(u, pos)
	if !ok {
		t.Fatal("the guidance table resolves nothing at the fixture position, so this test " +
			"would compare two empty results")
	}

	if got.Flow != want.Flow {
		t.Fatalf("the wire reported flow %q, the table says %q", got.Flow, want.Flow)
	}
	if !reflect.DeepEqual(got.Producers, want.Producers) {
		t.Fatalf("producers were altered in transport:\n got  %v\n want %v", got.Producers, want.Producers)
	}
	if !reflect.DeepEqual(got.Storage, want.Storage) {
		t.Fatalf("storage names were altered in transport:\n got  %v\n want %v", got.Storage, want.Storage)
	}
	if len(got.Imports) != len(want.Imports) {
		t.Fatalf("the wire reported %d imports, the table holds %d", len(got.Imports), len(want.Imports))
	}
	for i, ref := range want.Imports {
		if got.Imports[i].Alias != ref.Alias || got.Imports[i].Path != ref.Path {
			t.Fatalf("import %d was altered in transport: got %+v, want alias %q path %q",
				i, got.Imports[i], ref.Alias, ref.Path)
		}
	}
	// The fixture must actually carry each field, or "unaltered" is a claim
	// about three empty lists.
	if want.Flow == "" || len(want.Producers) == 0 || len(want.Storage) == 0 || len(want.Imports) == 0 {
		t.Fatalf("the fixture leaves a field empty, so transport fidelity is untested for it: %+v", want)
	}
}

func TestFlowGuidanceRefusesAnUnknownDocument(t *testing.T) {
	w := newWired(t)
	dir := t.TempDir()

	// TWO DOCUMENTS ARE OPEN, so a handler defaulting to "the first entry in
	// the table" or "the most recently opened document" has something to
	// wrongly return. Without them the mutant has no wrong answer available.
	known := uri.File(filepath.Join(dir, "alpha.flow"))
	other := uri.File(filepath.Join(dir, "beta.flow"))
	w.open(t, known, flowStorageAndImports)
	w.open(t, other, flowLaterProducer)

	// AND THE POSITION MUST BE ONE AT WHICH THOSE DOCUMENTS RESOLVE. At offset
	// zero the table returns false for EVERY file — the earliest scope sits at
	// a flow's name position, never at zero — so a request at 0,0 would make
	// the defaulting mutant return empty exactly as a correct server does, and
	// this test would pass while proving nothing.
	pos := positionAt(t, flowLaterProducer, "sink done", 0)
	for _, live := range []uri.URI{known, other} {
		if _, ok := w.server.guidanceAt(live, pos); !ok {
			t.Fatalf("the control failed: %s resolves no guidance at %+v, so a handler defaulting to "+
				"another document would return empty here and be indistinguishable from a correct one",
				live, pos)
		}
	}

	unknown := uri.File(filepath.Join(dir, "never-opened.flow"))
	got := w.askGuidance(t, unknown, pos)

	if got.Flow != "" || len(got.Producers) != 0 || len(got.Storage) != 0 || len(got.Imports) != 0 {
		t.Fatalf("a request naming a document the server has never seen came back populated: %+v; "+
			"that is a confident, well-formed answer about the wrong file", got)
	}
}
