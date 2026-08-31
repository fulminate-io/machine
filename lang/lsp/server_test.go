// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lsp

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// flowClean draws ZERO diagnostics — every node routes its failure to a
// handler, which is what silences the error-routing hints. Measured against
// the analysis core rather than assumed; it is the "after" state the republish
// test edits toward.
const flowClean = `flow alpha
source ingest Poll
  on error Handle
sink done Store from ingest
  on error Handle
`

// publishRecord is one PublishDiagnostics call, kept whole so a test can tell
// NO PUBLISH from an EMPTY PUBLISH — a distinction three of the diagnostics
// tests depend on and which a map of URI to count would destroy.
type publishRecord struct {
	URI         uri.URI
	Diagnostics []protocol.Diagnostic
}

// capturingClient stands in for an editor. It is a double for a DEPENDENCY,
// never for the code under test: it records what the server sent and asserts
// nothing itself.
type capturingClient struct {
	protocol.UnimplementedClient

	mu        sync.Mutex
	publishes []publishRecord
}

func (c *capturingClient) PublishDiagnostics(_ context.Context, params *protocol.PublishDiagnosticsParams) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.publishes = append(c.publishes, publishRecord{URI: params.URI, Diagnostics: params.Diagnostics})
	return nil
}

// forURI is every publish the server sent about one document, in order.
func (c *capturingClient) forURI(u uri.URI) []publishRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []publishRecord
	for _, p := range c.publishes {
		if p.URI == u {
			out = append(out, p)
		}
	}
	return out
}

// newSession returns a server, a capturing client, and a context wired the way
// protocol.NewServer wires one — the client reached through the context rather
// than held in a field, because that is how the real handlers find it.
func newSession(t *testing.T) (*Server, *capturingClient, context.Context) {
	t.Helper()
	client := &capturingClient{}
	return NewServer(NewStore()), client, protocol.WithClient(t.Context(), client)
}

func TestInitializeDeclaresUTF16FullSyncCompletionAndDefinition(t *testing.T) {
	s, _, ctx := newSession(t)

	got, err := s.Initialize(ctx, &protocol.InitializeParams{})
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	caps := got.Capabilities

	// THE MUTANT IS LEAVING THE FIELD ZERO, which behaves as utf-16 on the wire
	// by the spec's default — so this asserts the DECLARATION, by name.
	if caps.PositionEncoding != protocol.PositionEncodingKindUTF16 {
		t.Fatalf("PositionEncoding is %q, want %q (an omitted field defaults to utf-16 on the wire "+
			"but leaves the encoding undeclared)", caps.PositionEncoding, protocol.PositionEncodingKindUTF16)
	}
	if caps.TextDocumentSync != protocol.TextDocumentSyncKindFull {
		t.Fatalf("TextDocumentSync is %v, want full-document sync", caps.TextDocumentSync)
	}
	if caps.CompletionProvider == nil {
		t.Fatal("no completion provider is declared, so no editor will ask for completions")
	}
	if caps.DefinitionProvider != protocol.Boolean(true) {
		t.Fatalf("DefinitionProvider is %v, want a declared provider", caps.DefinitionProvider)
	}
	if got.ServerInfo.Name != serverName {
		t.Fatalf("ServerInfo.Name is %q, want %q", got.ServerInfo.Name, serverName)
	}
}

func TestUnimplementedMethodsStayUnimplemented(t *testing.T) {
	s, _, ctx := newSession(t)

	// THE MUTANT IS A STUB RETURNING AN EMPTY RESULT, which tells an editor the
	// feature exists and then produces nothing forever. Hover is a method v1
	// deliberately does not serve.
	got, err := s.Hover(ctx, &protocol.HoverParams{})
	if err == nil {
		t.Fatalf("Hover returned (%v, nil); a method this server does not serve must report that "+
			"rather than answer with an empty result", got)
	}
	if got != nil {
		t.Fatalf("Hover returned a non-nil result %v alongside its error", got)
	}

	// KNOWN POSITIVE through the same instrument: a method v1 DOES serve
	// answers without an error, so the failure above is about Hover and not
	// about every method on this server.
	if _, err := s.Initialize(ctx, &protocol.InitializeParams{}); err != nil {
		t.Fatalf("the control failed: Initialize, which this server does serve, returned %v", err)
	}
}

func TestInitializeWithNoWorkspaceFoldersIsSingleFileMode(t *testing.T) {
	s, _, ctx := newSession(t)

	// A client that supports workspace folders but has none configured sends an
	// explicit null. That is single-file mode, not an error, and NOT a licence
	// to substitute the process working directory.
	if _, err := s.Initialize(ctx, &protocol.InitializeParams{
		WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
			WorkspaceFolders: protocol.NullNullable[[]protocol.WorkspaceFolder](),
		},
	}); err != nil {
		t.Fatalf("Initialize with a null workspaceFolders failed: %v", err)
	}
	if n := len(s.store.Documents()); n != 0 {
		t.Fatalf("a client declaring no workspace folders left %d documents in the store; "+
			"a workspace was manufactured that the client never declared", n)
	}
}

func TestInitializeScansEveryWorkspaceFolderTheClientDeclares(t *testing.T) {
	s, _, ctx := newSession(t)
	first, second := t.TempDir(), t.TempDir()
	writeFlow(t, first, "one.flow", flowAlpha)
	writeFlow(t, second, "two.flow", flowBeta)

	if _, err := s.Initialize(ctx, &protocol.InitializeParams{
		WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
			WorkspaceFolders: protocol.NewNullable([]protocol.WorkspaceFolder{
				{URI: uri.File(first), Name: "first"},
				{URI: uri.File(second), Name: "second"},
			}),
		},
	}); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// BOTH folders, not just the first: a loop that returns after one would
	// leave half the workspace invisible to every cross-file answer.
	if n := len(s.store.Documents()); n != 2 {
		t.Fatalf("the scan loaded %d documents from two folders, want 2", n)
	}
	for _, want := range []string{filepath.Join(first, "one.flow"), filepath.Join(second, "two.flow")} {
		if _, ok := s.store.Get(uri.File(want)); !ok {
			t.Fatalf("the scan did not load %s", want)
		}
	}
}
