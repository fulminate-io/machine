// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lsp

import (
	"path/filepath"
	"testing"

	"github.com/whitaker-io/machine/lang/analysis"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

func TestOpeningADocumentPublishesItsDiagnostics(t *testing.T) {
	s, client, ctx := newSession(t)
	u := uri.File(filepath.Join(t.TempDir(), "alpha.flow"))

	if err := s.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: u, Text: flowWithAFinding},
	}); err != nil {
		t.Fatalf("DidOpen failed: %v", err)
	}

	got := client.forURI(u)
	if len(got) != 1 {
		t.Fatalf("opening a document produced %d publishes for it, want exactly 1", len(got))
	}
	// The fixture is chosen to draw findings, so an empty array here would mean
	// the publish fired without the analysis behind it.
	if len(got[0].Diagnostics) == 0 {
		t.Fatal("the publish carried an empty diagnostic array for a fixture that draws findings")
	}
	first := got[0].Diagnostics[0]
	if first.Source != protocol.NewOptional(serverName) {
		t.Fatalf("a published diagnostic names source %v, want %q", first.Source, serverName)
	}
	if first.Message == nil {
		t.Fatal("a published diagnostic carries no message")
	}
}

func TestChangingADocumentRepublishes(t *testing.T) {
	s, client, ctx := newSession(t)
	u := uri.File(filepath.Join(t.TempDir(), "alpha.flow"))

	if err := s.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: u, Text: flowWithAFinding},
	}); err != nil {
		t.Fatalf("DidOpen failed: %v", err)
	}
	if n := len(client.forURI(u)[0].Diagnostics); n == 0 {
		t.Fatal("the fixture drew no diagnostics to begin with, so clearing them proves nothing")
	}

	// EDIT TO A STATE WITH NO FINDINGS. Asserting only that a publish arrived
	// would pass against a server that never recomputes; asserting the array is
	// EMPTY is what proves the recompute happened and that clearing works.
	if err := s.DidChange(ctx, &protocol.DidChangeTextDocumentParams{
		TextDocument:   protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: u}},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{&protocol.TextDocumentContentChangeWholeDocument{Text: flowClean}},
	}); err != nil {
		t.Fatalf("DidChange failed: %v", err)
	}

	got := client.forURI(u)
	if len(got) != 2 {
		t.Fatalf("after one change the document has %d publishes, want 2", len(got))
	}
	if n := len(got[1].Diagnostics); n != 0 {
		t.Fatalf("the second publish carried %d diagnostics for a source with none; "+
			"either the change was not reanalyzed or the earlier findings were never cleared", n)
	}
	if got[1].Diagnostics == nil {
		t.Fatal("the clearing publish carried a nil slice, which marshals as JSON null; " +
			"LSP clears a file's diagnostics with an empty ARRAY")
	}
}

func TestClosingADocumentClearsItsDiagnostics(t *testing.T) {
	s, client, ctx := newSession(t)
	u := uri.File(filepath.Join(t.TempDir(), "alpha.flow"))

	if err := s.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: u, Text: flowWithAFinding},
	}); err != nil {
		t.Fatalf("DidOpen failed: %v", err)
	}
	before := len(client.forURI(u))

	if err := s.DidClose(ctx, &protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: u},
	}); err != nil {
		t.Fatalf("DidClose failed: %v", err)
	}

	got := client.forURI(u)
	// SILENCE IS THE PLAUSIBLE-AND-WRONG IMPLEMENTATION: it leaves the editor
	// rendering stale squiggles for a file nobody has open. So an empty publish
	// and no publish are separated explicitly.
	if len(got) != before+1 {
		t.Fatalf("closing produced %d publishes for the document, want one more than the %d before; "+
			"silence leaves the editor showing diagnostics for a closed file", len(got)-before, before)
	}
	last := got[len(got)-1]
	if len(last.Diagnostics) != 0 {
		t.Fatalf("the closing publish carried %d diagnostics, want an empty array", len(last.Diagnostics))
	}
	if last.Diagnostics == nil {
		t.Fatal("the closing publish carried a nil slice, which marshals as JSON null rather than []")
	}
}

func TestEachAnalysisSeverityMapsToItsOwnLSPSeverity(t *testing.T) {
	// THIS IS A DIRECT CALL BY NECESSITY. No analyzer in the core emits
	// SeverityWarning — censused at zero emission sites — so no fixture can
	// reach the warning arm through a driver run, and the coverage floor cannot
	// keep it honest either: a collapsed arm simply has fewer statements.
	cases := []struct {
		in   analysis.Severity
		want protocol.DiagnosticSeverity
	}{
		{analysis.SeverityError, protocol.DiagnosticSeverityError},
		{analysis.SeverityWarning, protocol.DiagnosticSeverityWarning},
		{analysis.SeverityHint, protocol.DiagnosticSeverityHint},
	}

	seen := map[protocol.DiagnosticSeverity]analysis.Severity{}
	for _, c := range cases {
		got := lspSeverity(c.in)
		if got != c.want {
			t.Fatalf("lspSeverity(%v) = %v, want %v", c.in, got, c.want)
		}
		if prior, dup := seen[got]; dup {
			t.Fatalf("lspSeverity maps both %v and %v onto %v; the mutant that promotes the "+
				"warning arm is exactly this collapse", prior, c.in, got)
		}
		seen[got] = c.in
	}
	if len(seen) != len(cases) {
		t.Fatalf("three analysis severities produced %d distinct LSP severities", len(seen))
	}
}

func TestAnAnalysisFailureNeitherPublishesEmptyNorReturnsNil(t *testing.T) {
	s, client, ctx := newSession(t)
	dir := t.TempDir()
	u := uri.File(filepath.Join(dir, "alpha.flow"))

	if err := s.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: u, Text: flowWithAFinding},
	}); err != nil {
		t.Fatalf("DidOpen failed: %v", err)
	}
	published := client.forURI(u)
	if len(published) != 1 || len(published[0].Diagnostics) == 0 {
		t.Fatalf("the precondition failed: the document must already have a NON-EMPTY published set, "+
			"got %d publishes", len(published))
	}

	// A REAL, NON-CONTRIVED FAILURE. The analysis driver refuses a Source that
	// carries no parsed tree, naming it — so a Document with a nil File drives
	// analyze to a genuine error with no test hook in the production path.
	broken := filepath.Join(dir, "broken.flow")
	s.store.docs[broken] = &Document{URI: uri.File(broken), Path: broken, Src: []byte(flowAlpha), Mapper: NewMapper(nil)}

	err := s.DidChange(ctx, &protocol.DidChangeTextDocumentParams{
		TextDocument:   protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: u}},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{&protocol.TextDocumentContentChangeWholeDocument{Text: flowClean}},
	})

	// BOTH HALVES, in the same run. Asserting only the return would pass a
	// mutant that publishes empty and then returns the error; asserting only
	// the publish would pass one that swallows the error and returns nil.
	if err == nil {
		t.Fatal("an analysis failure returned nil; the handler reported success for work it could not do")
	}
	after := client.forURI(u)
	if len(after) != len(published) {
		t.Fatalf("an analysis failure produced %d further publishes for the document (the last carrying "+
			"%d diagnostics); on failure the server must publish nothing and leave the previous state standing",
			len(after)-len(published), len(after[len(after)-1].Diagnostics))
	}
	if _, cleared := s.store.Get(uri.File(broken)); !cleared {
		t.Fatal("the control failed: the injected document is not in the store, so analyze was never made to fail")
	}
}

func TestASyncHandlerWithNoClientInContextReturnsAnError(t *testing.T) {
	s := NewServer(NewStore())
	bare := t.Context() // deliberately carries no Client
	u := uri.File(filepath.Join(t.TempDir(), "alpha.flow"))

	if _, ok := protocol.ClientFromContext(bare); ok {
		t.Fatal("the fixture context already carries a client, so this test could not observe its absence")
	}

	open := func() error {
		return s.DidOpen(bare, &protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{URI: u, Text: flowWithAFinding},
		})
	}
	if err := open(); err == nil {
		t.Fatal("DidOpen with no client in context returned nil; it produced diagnostics nobody will see " +
			"and reported success for it")
	}

	change := s.DidChange(bare, &protocol.DidChangeTextDocumentParams{
		TextDocument:   protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: u}},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{&protocol.TextDocumentContentChangeWholeDocument{Text: flowClean}},
	})
	if change == nil {
		t.Fatal("DidChange with no client in context returned nil")
	}
	if closeErr := s.DidClose(bare, &protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: u},
	}); closeErr == nil {
		t.Fatal("DidClose with no client in context returned nil")
	}

	// KNOWN POSITIVE: the same call through a context that DOES carry a client
	// succeeds, so the errors above are about the missing client rather than
	// about the handler refusing everything.
	_, client, ctx := newSession(t)
	s = NewServer(NewStore())
	if err := s.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: u, Text: flowWithAFinding},
	}); err != nil {
		t.Fatalf("the control failed: DidOpen with a client in context returned %v", err)
	}
	if len(client.forURI(u)) == 0 {
		t.Fatal("the control failed: DidOpen with a client in context published nothing")
	}
}

func TestARangedChangeAgainstFullSyncIsRefused(t *testing.T) {
	s, _, ctx := newSession(t)
	u := uri.File(filepath.Join(t.TempDir(), "alpha.flow"))

	if err := s.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: u, Text: flowWithAFinding},
	}); err != nil {
		t.Fatalf("DidOpen failed: %v", err)
	}

	// The server declared full-document sync, so a ranged change means the two
	// sides disagree about the contract. Guessing would corrupt the buffer the
	// analysis then runs over.
	err := s.DidChange(ctx, &protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: u}},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			&protocol.TextDocumentContentChangePartial{Text: "x"},
		},
	})
	if err == nil {
		t.Fatal("a ranged content change was accepted by a server that declared full-document sync")
	}

	// And an empty change list is refused rather than treated as an empty file.
	if err := s.DidChange(ctx, &protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: u}},
	}); err == nil {
		t.Fatal("a didChange carrying no content change was accepted")
	}
}

func TestDocumentWorkAfterShutdownIsRefused(t *testing.T) {
	s, _, ctx := newSession(t)
	u := uri.File(filepath.Join(t.TempDir(), "alpha.flow"))

	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	if err := s.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: u, Text: flowWithAFinding},
	}); err == nil {
		t.Fatal("a document notification after shutdown was processed; the shutdown mark is inert")
	}

	// KNOWN POSITIVE: the identical call on a server that has NOT been shut
	// down succeeds, so the refusal is the mark's doing.
	fresh, _, freshCtx := newSession(t)
	if err := fresh.DidOpen(freshCtx, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: u, Text: flowWithAFinding},
	}); err != nil {
		t.Fatalf("the control failed: DidOpen before shutdown returned %v", err)
	}
}
