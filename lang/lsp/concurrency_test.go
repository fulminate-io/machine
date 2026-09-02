// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package lsp

import (
	"path/filepath"
	"sync"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// TestHandlersRunConcurrentlyAndDoNotRaceOnTheStore is the regression guard for
// a defect this module's design premise hid.
//
// EVERY OTHER TEST IN THIS PACKAGE DRIVES ONE HANDLER AT A TIME, which is why
// none of them saw it. go.lsp.dev/protocol wires each connection with
// protocol.Handlers, and that is CancelHandler(jsonrpc2.AsyncHandler(...)):
// AsyncHandler releases the read loop the instant a handler starts, so the
// library's documented default of inline dispatch does not apply to LSP. Two
// didChange notifications, or a didChange and a flow/guidance request, really
// do run at the same time.
//
// Before the Store took a lock and the Server serialized its notifications,
// this test reported a data race on Store.docs from two goroutines both under
// protocol.serverDispatch — an unsynchronized map write, which the Go runtime
// turns into a hard panic rather than a wrong answer. Run it with -race; it
// passes either way, and only -race sees the defect it exists for.
func TestHandlersRunConcurrentlyAndDoNotRaceOnTheStore(t *testing.T) {
	w := newWired(t)
	dir := t.TempDir()
	u := uri.File(filepath.Join(dir, "alpha.flow"))
	w.open(t, u, flowStorageAndImports)

	pos := positionAt(t, flowStorageAndImports, "sink done", 0)
	texts := []string{flowStorageAndImports, flowClean, flowLaterProducer}

	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_ = w.conn.Notify(w.ctx, protocol.MethodTextDocumentDidChange, &protocol.DidChangeTextDocumentParams{
				TextDocument: protocol.VersionedTextDocumentIdentifier{
					TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: u},
					Version:                int32(i),
				},
				ContentChanges: []protocol.TextDocumentContentChangeEvent{
					&protocol.TextDocumentContentChangeWholeDocument{Text: texts[i%len(texts)]},
				},
			})
		}()
		go func() {
			defer wg.Done()
			var out GuidanceResult
			_, _ = w.conn.Call(w.ctx, flowGuidanceMethod, GuidanceParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: u},
				Position:     pos,
			}, &out)
		}()
		go func() {
			defer wg.Done()
			var out AnalyzersResult
			_, _ = w.conn.Call(w.ctx, flowAnalyzersMethod, struct{}{}, &out)
		}()
	}
	wg.Wait()

	// The server must still be answering after the storm, or "no race" would be
	// a claim about a connection that had quietly died.
	if got := w.analyzers(t); len(got.Analyzers) == 0 {
		t.Fatal("the server stopped answering after concurrent traffic")
	}
	if _, ok := w.server.store.Get(u); !ok {
		t.Fatal("the document was lost during concurrent changes")
	}
}

func TestFlowAnalyzersAnswersOverTheWire(t *testing.T) {
	w := newWired(t)

	// The JSON round trip in analyzers_test proves the payload survives
	// encoding; this proves the METHOD is actually reachable through the
	// dispatcher's non-standard-method fall-through.
	got := w.analyzers(t)
	if len(got.Analyzers) == 0 {
		t.Fatal("flow/analyzers over a real connection reported no analyzers")
	}
	if got.Scaling != ScalingDisclosure {
		t.Fatalf("the scaling disclosure did not survive the wire:\n got  %q\n want %q",
			got.Scaling, ScalingDisclosure)
	}
	for _, a := range got.Analyzers {
		if a.Name == "" || a.Doc == "" {
			t.Fatalf("an analyzer arrived over the wire with an empty field: %+v", a)
		}
	}
}
