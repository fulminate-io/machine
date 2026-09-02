// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package lsp

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

const (
	// flowLaterProducer declares `later` AFTER the sink the cursor sits on, so
	// a handler offering every producer in the flow is separated from one
	// honoring declare-before-use.
	//
	// THE TRAILING SINK IS load-bearing and was added after the first draft was
	// found vacuous. Guidance scopes hold the names declared by EARLIER
	// statements, so a producer declared by the LAST statement appears in no
	// scope at all — the absence assertion would then hold against every
	// possible implementation. With a statement after it, `later` really is in
	// scope somewhere, and the test's own known-positive proves it.
	flowLaterProducer = `flow alpha
source early Poll
sink done Store from early
transform later Step from early
sink tail Store from later
`
	// flowStorageAndImports carries a state field, a var, an aliased import and
	// an UNALIASED import whose last path segment is "v82" — not a usable
	// identifier. That is the case the analysis core's own guidance doc names,
	// and it separates offering the declared path from guessing a package name.
	flowStorageAndImports = `flow alpha
import ledger "github.com/acme/ledger"
import "github.com/stripe/stripe-go/v82"
state {
  seen map[string]bool
}
var attempt int
source ingest Poll
sink done Store from ingest
`
)

// positionAt is the protocol position `plus` bytes past the first occurrence of
// needle in src.
//
// The fixtures it serves are ASCII, so a byte column is also a UTF-16 character
// index here. The byte-to-code-unit conversion itself is exercised by the
// Mapper's own tests against multi-byte runes; this helper is about locating a
// cursor, not about encoding.
func positionAt(t *testing.T, src, needle string, plus int) protocol.Position {
	t.Helper()
	at := strings.Index(src, needle)
	if at < 0 {
		t.Fatalf("the fixture does not contain %q", needle)
	}
	at += plus
	line := strings.Count(src[:at], "\n")
	col := at - (strings.LastIndex(src[:at], "\n") + 1)
	return protocol.Position{Line: uint32(line), Character: uint32(col)}
}

// openFor opens one document through the server and returns its URI.
func openFor(t *testing.T, s *Server, ctx context.Context, dir, name, src string) uri.URI {
	t.Helper()
	u := uri.File(filepath.Join(dir, name))
	if err := s.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: u, Text: src},
	}); err != nil {
		t.Fatalf("DidOpen of %s failed: %v", name, err)
	}
	return u
}

// completeAt asks for completions and returns the labels offered.
func completeAt(t *testing.T, s *Server, ctx context.Context, u uri.URI, pos protocol.Position) []string {
	t.Helper()
	got, err := s.Completion(ctx, &protocol.CompletionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: u},
			Position:     pos,
		},
	})
	if err != nil {
		t.Fatalf("Completion failed: %v", err)
	}
	items, ok := got.(protocol.CompletionItemSlice)
	if !ok {
		t.Fatalf("Completion answered with %T, want the CompletionItemSlice arm", got)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Label)
	}
	return out
}

func TestCompletionOffersProducersInScopeAndNotLaterOnes(t *testing.T) {
	s, _, ctx := newSession(t)
	u := openFor(t, s, ctx, t.TempDir(), "alpha.flow", flowLaterProducer)

	// The cursor sits on the sink, which is declared BEFORE the transform.
	labels := completeAt(t, s, ctx, u, positionAt(t, flowLaterProducer, "sink done", 0))

	// BOTH DIRECTIONS IN THE SAME RUN. Asserting only that `early` appears would
	// pass against a handler returning every producer in the flow — which is
	// exactly the declare-before-use rule being lost.
	if !slices.Contains(labels, "early") {
		t.Fatalf("completion did not offer `early`, declared before the cursor: %v", labels)
	}
	if slices.Contains(labels, "later") {
		t.Fatalf("completion offered `later`, which is declared AFTER the cursor: %v", labels)
	}

	// KNOWN POSITIVE FOR THE ABSENCE. `later` must be offerable SOMEWHERE, or
	// the assertion above holds against every implementation and measures
	// nothing. At the trailing sink it is genuinely in scope.
	tail := completeAt(t, s, ctx, u, positionAt(t, flowLaterProducer, "sink tail", 0))
	if !slices.Contains(tail, "later") {
		t.Fatalf("the control failed: `later` is offered at no position at all, so its absence "+
			"above is not evidence of scoping: %v", tail)
	}
}

func TestCompletionOffersStorageAndImports(t *testing.T) {
	s, _, ctx := newSession(t)
	u := openFor(t, s, ctx, t.TempDir(), "alpha.flow", flowStorageAndImports)

	labels := completeAt(t, s, ctx, u, positionAt(t, flowStorageAndImports, "sink done", 0))

	for _, want := range []string{"seen", "attempt"} {
		if !slices.Contains(labels, want) {
			t.Fatalf("completion did not offer the storage name %q: %v", want, labels)
		}
	}
	if !slices.Contains(labels, "ledger") {
		t.Fatalf("completion did not offer the declared import alias `ledger`: %v", labels)
	}

	// THE UNALIASED IMPORT. A handler guessing a package name from the path
	// would offer "v82"; the truthful answer is the path exactly as the author
	// wrote it, because a package's name is not derivable from its path.
	if slices.Contains(labels, "v82") {
		t.Fatalf("completion offered `v82`, a package name guessed from a versioned module path: %v", labels)
	}
	var offeredPath bool
	for _, label := range labels {
		if strings.Contains(label, "github.com/stripe/stripe-go/v82") {
			offeredPath = true
		}
	}
	if !offeredPath {
		t.Fatalf("completion offered nothing for the unaliased import; the path as written is the "+
			"truthful label: %v", labels)
	}
}

func TestCompletionWorksInADocumentThatDoesNotParse(t *testing.T) {
	s, _, ctx := newSession(t)
	u := openFor(t, s, ctx, t.TempDir(), "damaged.flow", flowMissingFromTarget)

	doc, ok := s.store.Get(u)
	if !ok || doc.ParseErr == nil {
		t.Fatal("the fixture parses cleanly, so this test would not be about a damaged document")
	}

	// The cursor is on the sink AFTER the damaged transform line. The parser
	// recovers `flow payments` with `source ingest Poll` ahead of it, so
	// `ingest` is legitimately in scope here.
	labels := completeAt(t, s, ctx, u, positionAt(t, flowMissingFromTarget, "sink out", 0))

	// THIS IS THE USER-VISIBLE PAYOFF OF ATTRIBUTION-TIME SUPPRESSION. An
	// implementation that withheld the damaged source from the analysis run
	// returns an empty list here, because the guidance table has no scopes for
	// a path absent from the symbol table.
	if len(labels) == 0 {
		t.Fatal("completion offered nothing inside a document that does not parse; the damaged source " +
			"was withheld from the run, which turns completion off in the buffer being typed in")
	}
	if !slices.Contains(labels, "ingest") {
		t.Fatalf("completion did not offer `ingest`, which the parser recovered ahead of the cursor: %v", labels)
	}
}

func TestCompletionRefusesAPositionThatDoesNotExist(t *testing.T) {
	s, _, ctx := newSession(t)
	u := openFor(t, s, ctx, t.TempDir(), "alpha.flow", flowLaterProducer)

	// A stale client asking about a line this version does not have gets an
	// empty list rather than a confident answer about somewhere else.
	if labels := completeAt(t, s, ctx, u, protocol.Position{Line: 900}); len(labels) != 0 {
		t.Fatalf("completion answered %v for a position the document does not have", labels)
	}

	// KNOWN POSITIVE in the same run: a position the document DOES have is
	// answered, so the empty result above is about the position.
	if labels := completeAt(t, s, ctx, u, positionAt(t, flowLaterProducer, "sink done", 0)); len(labels) == 0 {
		t.Fatal("the control failed: a real position also produced an empty list")
	}
}
