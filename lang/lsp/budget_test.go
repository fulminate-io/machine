// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lsp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// The change-to-diagnostics budget, structured on lang/ast's reparse budget and
// lang/analysis's analysis budget so the three are read together.
//
// THIS IS HOW THE REPARSE CONTRACT WITH THE PARSER IS EXPRESSED. lang/ast ships
// no incremental entry point and documents a cheap full reparse instead, so the
// contract an editor actually needs is responsiveness rather than a particular
// algorithm. The budget constant is the SAME one millisecond its two siblings
// use: a third module measuring the same class of work against a looser bound
// would make the three numbers unreadable together.
//
// The measured band on a developer machine is tens of microseconds. A figure
// above about 100 microseconds for this corpus is a signal that something has
// gone quadratic and is worth investigating; it is NOT a signal to raise the
// budget. A figure that rises as the WORKSPACE grows is the documented
// property in ScalingDisclosure rather than a regression.
const (
	budgetIterations = 200
	changeBudget     = time.Millisecond
	astTestdata      = "../ast/testdata"
	strawmanDir      = astTestdata + "/strawman"
)

// strawmanFiles is the repository's largest real .flow set.
var strawmanFiles = []string{"payments.flow", "enrichment.flow", "toy.flow"}

// TestChangeToDiagnosticsBudget measures the WHOLE didChange path — the Store
// update, the single-document reparse, the analysis over the cached trees, the
// conversion through the Mapper and the publish payload — and prints the figure
// whether it passes or fails, so the number lives in the run rather than only
// in a plan.
//
// NO REPARSE OF UNCHANGED DOCUMENTS IS PART OF WHAT IS MEASURED. analyze takes
// documents carrying trees the Store already parsed; an implementation that
// re-parsed every source inside analyze would show up here rather than in any
// correctness test.
//
// NOTE ON THE TEST CACHE: a cached PASS is a valid pass of the assertion but it
// re-measures nothing, so a figure in the log from a cached run is the figure
// from whenever it last actually ran. That is stated here rather than defended
// with a cache-defeating flag, which would cost every unrelated run for no
// information — the same call lang/ast and lang/analysis both made.
func TestChangeToDiagnosticsBudget(t *testing.T) {
	s, client, ctx := newSession(t)
	target, targetSrc, total := openStrawman(t, s, ctx)

	if total < 3000 {
		t.Fatalf("CONTROL FAILED: the corpus is %d bytes, too small to measure anything", total)
	}
	if n := len(s.store.Documents()); n != len(strawmanFiles) {
		t.Fatalf("CONTROL FAILED: %d of %d corpus files reached the store", n, len(strawmanFiles))
	}
	before := len(client.publishes)
	if before == 0 {
		t.Fatal("CONTROL FAILED: opening the corpus published nothing, so the path under measurement is not wired")
	}

	start := time.Now()
	for range budgetIterations {
		if err := s.DidChange(ctx, changeOf(target, string(targetSrc))); err != nil {
			t.Fatalf("a change failed midway through the measurement: %v", err)
		}
	}
	mean := time.Since(start) / budgetIterations

	t.Logf("change to diagnostics over the %d-file strawman corpus (%d bytes): mean %v over %d iterations, budget %v",
		len(strawmanFiles), total, mean, budgetIterations, changeBudget)

	// THE SAME FIGURE IN ASCII, for a gate to parse. The human line above renders
	// a duration, which reads "24.52µs" or "1.2ms" depending on magnitude — a
	// shape no grep can compare against a budget.
	t.Logf("change budget: mean_ns=%d budget_ns=%d", mean.Nanoseconds(), changeBudget.Nanoseconds())

	// THE LOOP MUST HAVE DONE THE WORK. A Store that dropped the change, an
	// analyze over an empty document set, or a body the compiler elided all
	// complete in near-zero time and pass a budget with room to spare. Every
	// iteration republishes every known document, so the count is exact.
	want := before + budgetIterations*len(strawmanFiles)
	if got := len(client.publishes); got != want {
		t.Fatalf("CONTROL FAILED: %d publishes after the measurement, want %d; the loop body did not "+
			"run the path this budget claims to measure", got, want)
	}

	if mean > changeBudget {
		t.Fatalf("mean change-to-diagnostics %v exceeds the %v budget", mean, changeBudget)
	}
}

// openStrawman opens every strawman source through the server and reports the
// document the measurement will edit, its bytes, and the corpus size.
func openStrawman(t *testing.T, s *Server, ctx context.Context) (uri.URI, []byte, int) {
	t.Helper()
	var target uri.URI
	var targetSrc []byte
	total := 0
	for _, name := range strawmanFiles {
		path, err := filepath.Abs(filepath.Join(strawmanDir, name))
		if err != nil {
			t.Fatalf("cannot resolve %s: %v", name, err)
		}
		src, err := os.ReadFile(path) //nolint:gosec // a test reading its own corpus
		if err != nil {
			t.Fatalf("cannot read %s: %v", path, err)
		}
		total += len(src)
		u := uri.File(path)
		if openErr := s.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{URI: u, Text: string(src)},
		}); openErr != nil {
			t.Fatalf("opening %s failed: %v", name, openErr)
		}
		if name == strawmanFiles[0] {
			target, targetSrc = u, src
		}
	}
	return target, targetSrc, total
}

// changeOf is one full-document change notification.
func changeOf(u uri.URI, text string) *protocol.DidChangeTextDocumentParams {
	return &protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: u},
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			&protocol.TextDocumentContentChangeWholeDocument{Text: text},
		},
	}
}
