// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package lsp

import (
	"context"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

const (
	// flowConsumerAndProducer declares `ingest` on one line and references it
	// on another, so the declaration position and the request position differ
	// — which is what separates a real jump from a handler echoing the cursor.
	flowConsumerAndProducer = `flow alpha
source ingest Poll
transform step Step from ingest
sink done Store from step
`
	// flowDeclaringScreening lives in the file the editor never opens.
	flowDeclaringScreening = `flow screening (Order) -> ok OkResult, bad ErrResult
branch check fraud.Clean from in -> ok, bad
`
	// flowUsingScreening names that flow from another file. The `use` line is
	// the whole point: UseStmt.Flow is tabled nowhere, so a handler built only
	// on the producer/consumer tables answers nothing here.
	flowUsingScreening = `flow main
source ingest Poll
use screen screening from ingest -> clean, suspect
sink store warehouse.Insert from clean
sink hold fraud.Quarantine from suspect
`
)

// definitionAt asks where the name at pos is declared.
func definitionAt(t *testing.T, s *Server, ctx context.Context, u uri.URI, pos protocol.Position) *protocol.Location {
	t.Helper()
	got, err := s.Definition(ctx, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: u},
			Position:     pos,
		},
	})
	if err != nil {
		t.Fatalf("Definition failed: %v", err)
	}
	if got == nil {
		return nil
	}
	loc, ok := got.(*protocol.Location)
	if !ok {
		t.Fatalf("Definition answered with %T, want the *Location arm", got)
	}
	return loc
}

func TestDefinitionJumpsFromAConsumerToItsProducer(t *testing.T) {
	s, _, ctx := newSession(t)
	u := openFor(t, s, ctx, t.TempDir(), "alpha.flow", flowConsumerAndProducer)

	// The cursor sits on `ingest` where the TRANSFORM consumes it.
	request := positionAt(t, flowConsumerAndProducer, "Step from ingest", len("Step from "))
	loc := definitionAt(t, s, ctx, u, request)
	if loc == nil {
		t.Fatal("Definition found nothing for a name whose producer is two lines above it")
	}

	// The target is the `ingest` on the SOURCE line.
	want := positionAt(t, flowConsumerAndProducer, "source ingest", len("source "))
	if loc.Range.Start != want {
		t.Fatalf("Definition landed at %+v, want the declaration at %+v", loc.Range.Start, want)
	}
	// THE MUTANT IS RETURNING THE CURSOR'S OWN POSITION, so the two must differ
	// and the test would be vacuous if they did not.
	if request == want {
		t.Fatal("the fixture puts the reference and the declaration at the same position, " +
			"so a handler echoing the cursor would pass")
	}
	if loc.Range.Start == request {
		t.Fatalf("Definition returned the request position %+v rather than the declaration", request)
	}
	if loc.URI != u {
		t.Fatalf("Definition landed in %s, want the same document %s", loc.URI, u)
	}
}

func TestDefinitionCrossesAFileBoundary(t *testing.T) {
	s, _, ctx := newSession(t)
	dir := t.TempDir()
	declaring := writeFlow(t, dir, "screening.flow", flowDeclaringScreening)
	writeFlow(t, dir, "main.flow", flowUsingScreening)

	// The workspace scan is what puts the DECLARING file in the run; the editor
	// opens only the consuming one.
	if _, err := s.Initialize(ctx, &protocol.InitializeParams{
		WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
			WorkspaceFolders: protocol.NewNullable([]protocol.WorkspaceFolder{{URI: uri.File(dir), Name: "w"}}),
		},
	}); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	u := openFor(t, s, ctx, dir, "main.flow", flowUsingScreening)
	if _, opened := s.store.Get(u); !opened {
		t.Fatal("the consuming document is not open")
	}

	// THE CURSOR MUST BE ON THE FLOW NAME, not on the instance name `screen`
	// and not on a binding. On any of those this silently degrades into a
	// path-one test that a path-one-only handler passes.
	request := positionAt(t, flowUsingScreening, "use screen screening", len("use screen "))
	loc := definitionAt(t, s, ctx, u, request)
	if loc == nil {
		t.Fatal("Definition found nothing for the flow a `use` statement names. Two implementations " +
			"fail here: one that looks only in the producer and consumer tables, which never hold " +
			"UseStmt.Flow, and one that analyzes only open documents, which has no entry for the " +
			"unopened file that declares the flow")
	}
	if loc.URI != uri.File(declaring) {
		t.Fatalf("Definition landed in %s, want the unopened declaring file %s", loc.URI, uri.File(declaring))
	}

	want := positionAt(t, flowDeclaringScreening, "flow screening", len("flow "))
	if loc.Range.Start != want {
		t.Fatalf("Definition landed at %+v in the declaring file, want the flow name at %+v",
			loc.Range.Start, want)
	}

	// CONTROL: the declaring file really is only on disk and was never opened,
	// so the jump above genuinely crossed out of the editor's open set.
	if s.store.overlay[declaring] {
		t.Fatal("the control failed: the declaring file was opened as a buffer, so this test did not " +
			"exercise the workspace scan")
	}
}

func TestDefinitionAnswersNothingForAnUndefinedName(t *testing.T) {
	s, _, ctx := newSession(t)
	u := openFor(t, s, ctx, t.TempDir(), "alpha.flow", flowConsumerAndProducer)

	// A position on a keyword names no flow-level symbol. The truthful answer
	// is no location, never the nearest declaration.
	if loc := definitionAt(t, s, ctx, u, positionAt(t, flowConsumerAndProducer, "transform step", 0)); loc != nil {
		t.Fatalf("Definition answered %+v for a position naming no reference", loc)
	}

	// KNOWN POSITIVE through the same call: a real reference still resolves, so
	// the nil above is about the position rather than a handler that never
	// answers.
	resolvable := positionAt(t, flowConsumerAndProducer, "Store from step", len("Store from "))
	if loc := definitionAt(t, s, ctx, u, resolvable); loc == nil {
		t.Fatal("the control failed: a real reference also produced no location")
	}
}
