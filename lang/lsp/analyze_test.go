// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package lsp

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/analysis"
	"github.com/whitaker-io/machine/lang/ast"
	"go.lsp.dev/uri"
)

const (
	// flowWithAFinding parses cleanly but leaves `idle` producing output that
	// nothing consumes, so the analyzers have something to say about it. The
	// Diagnostics leg needs a non-empty result or an unwired capture would
	// satisfy it with an empty slice.
	flowWithAFinding = `flow alpha
source ingest Poll
transform idle Step from ingest
sink done Store from ingest
`
	// flowMissingFromTarget is lang/ast/testdata/broken/missing-from-target.flow.
	// It is the damaged fixture for both suppression tests because it recovers
	// `flow payments` with `source ingest Poll` AHEAD of its damaged line, so
	// the guidance table still has a scope to report inside it.
	flowMissingFromTarget = `flow payments
source ingest Poll
transform charge billing.Charge from
sink out Write from ingest
`
)

// openDoc builds one Document through the Store, which is how every document
// the server analyzes is really produced.
func openDoc(t *testing.T, dir, name, src string) *Document {
	t.Helper()
	s := NewStore()
	u := uri.File(filepath.Join(dir, name))
	s.Open(u, []byte(src))
	doc, ok := s.Get(u)
	if !ok {
		t.Fatalf("the store did not keep the document it just opened for %s", name)
	}
	return doc
}

// analyzerDiagnosticsFor is every non-parse diagnostic attributed to a path.
func analyzerDiagnosticsFor(diags []analysis.Diagnostic, path string) []analysis.Diagnostic {
	var out []analysis.Diagnostic
	for _, d := range diags {
		if d.Path == path && d.Code != "parse" {
			out = append(out, d)
		}
	}
	return out
}

// parseDiagnosticsFor is every parse-code diagnostic attributed to a path.
func parseDiagnosticsFor(diags []analysis.Diagnostic, path string) []analysis.Diagnostic {
	var out []analysis.Diagnostic
	for _, d := range diags {
		if d.Path == path && d.Code == "parse" {
			out = append(out, d)
		}
	}
	return out
}

func TestOneRunYieldsDiagnosticsAndBothTables(t *testing.T) {
	dir := t.TempDir()
	docs := []*Document{openDoc(t, dir, "alpha.flow", flowWithAFinding)}

	snap, err := analyze(docs)
	if err != nil {
		t.Fatalf("analyze failed: %v", err)
	}

	if len(snap.Diagnostics) == 0 {
		t.Fatal("one run produced no diagnostics at all; the fixture is meant to draw at least one, " +
			"so an empty slice here cannot be told apart from a capture wired to nothing")
	}
	if snap.Symbols == nil {
		t.Fatal("one run produced no symbol table; go-to-definition has nothing to read")
	}
	if snap.Guidance == nil {
		t.Fatal("one run produced no guidance table; completion has nothing to read")
	}

	// The tables must be about THIS run, not merely non-nil.
	if len(snap.Symbols.Files) != 1 {
		t.Fatalf("the symbol table holds %d files, want the 1 in the run", len(snap.Symbols.Files))
	}
	if _, ok := snap.Symbols.Flow("alpha"); !ok {
		t.Fatal("the symbol table has no entry for the flow the fixture declares")
	}
}

func TestAParseFailureArrivesAsAParseCodeDiagnostic(t *testing.T) {
	dir := t.TempDir()
	damaged := openDoc(t, dir, "damaged.flow", flowMissingFromTarget)
	if damaged.ParseErr == nil {
		t.Fatal("the fixture parses cleanly, so this test could not observe a parse failure")
	}

	snap, err := analyze([]*Document{damaged})
	// The mutant is returning the parse error instead of converting it, which
	// leaves the editor with nothing to show for a file being typed in.
	if err != nil {
		t.Fatalf("analyze returned the parse error rather than converting it to a diagnostic: %v", err)
	}

	got := parseDiagnosticsFor(snap.Diagnostics, damaged.Path)
	if len(got) == 0 {
		t.Fatalf("no parse-code diagnostic for %s; the snapshot carries %d diagnostics",
			damaged.Path, len(snap.Diagnostics))
	}
	for _, d := range got {
		if d.Severity != analysis.SeverityError {
			t.Fatalf("a parse diagnostic arrived at severity %v, want error", d.Severity)
		}
	}

	// KNOWN POSITIVE: a clean document in the same run draws no parse-code
	// diagnostic, so the presence above is a property of the damaged file.
	clean := openDoc(t, dir, "clean.flow", flowWithAFinding)
	snap, err = analyze([]*Document{damaged, clean})
	if err != nil {
		t.Fatalf("analyze failed on the mixed set: %v", err)
	}
	if n := len(parseDiagnosticsFor(snap.Diagnostics, clean.Path)); n != 0 {
		t.Fatalf("the control failed: a cleanly parsing document drew %d parse diagnostics", n)
	}
}

func TestADamagedDocumentGetsNoAnalyzerFindings(t *testing.T) {
	dir := t.TempDir()
	damaged := openDoc(t, dir, "damaged.flow", flowMissingFromTarget)
	clean := openDoc(t, dir, "clean.flow", flowWithAFinding)
	docs := []*Document{clean, damaged}

	// LEG (a), THE KNOWN POSITIVE. Run the analyzers over the same source set
	// with no suppression at all and confirm the damaged document DOES draw an
	// analyzer finding. Without this the assertion below is green for a fixture
	// that could never have failed it.
	srcs, _ := sourceSet(docs)
	unsuppressed, err := analysis.Run(srcs, analysis.All())
	if err != nil {
		t.Fatalf("the unsuppressed control run failed: %v", err)
	}
	control := analyzerDiagnosticsFor(unsuppressed, damaged.Path)
	if len(control) == 0 {
		t.Fatalf("the control failed: unsuppressed, the analyzers say nothing about %s, "+
			"so leg (b) below would pass against an implementation with no suppression at all",
			damaged.Path)
	}
	t.Logf("control: unsuppressed, the analyzers report %d findings about the damaged document (%s)",
		len(control), control[0].Code)

	// LEG (b). Through analyze, the damaged document keeps its parse
	// diagnostics and loses every analyzer-attributed one.
	snap, err := analyze(docs)
	if err != nil {
		t.Fatalf("analyze failed: %v", err)
	}
	if n := len(parseDiagnosticsFor(snap.Diagnostics, damaged.Path)); n == 0 {
		t.Fatal("the damaged document lost its parse diagnostics too; it is meant to keep exactly those")
	}
	if leaked := analyzerDiagnosticsFor(snap.Diagnostics, damaged.Path); len(leaked) != 0 {
		t.Fatalf("%d analyzer findings survived for a document that failed to parse (first: %s %q); "+
			"the prepend landed but the suppression did not", len(leaked), leaked[0].Code, leaked[0].Message)
	}

	// The healthy document must keep everything, or "suppression" would just be
	// dropping analyzer findings for the whole run.
	if n := len(analyzerDiagnosticsFor(snap.Diagnostics, clean.Path)); n == 0 {
		t.Fatal("the cleanly parsing document lost its analyzer findings as well; " +
			"suppression is meant to be keyed on the damaged path alone")
	}
}

func TestADamagedDocumentStillAppearsInBothTables(t *testing.T) {
	dir := t.TempDir()
	damaged := openDoc(t, dir, "damaged.flow", flowMissingFromTarget)

	snap, err := analyze([]*Document{damaged})
	if err != nil {
		t.Fatalf("analyze failed: %v", err)
	}

	// The symbol table must hold the damaged file, or go-to-definition inside
	// the buffer being typed answers nothing.
	found := false
	for i := range snap.Symbols.Files {
		if snap.Symbols.Files[i].Src.Path == damaged.Path {
			found = true
		}
	}
	if !found {
		t.Fatalf("the symbol table has no entry for %s; the damaged source was withheld from the run",
			damaged.Path)
	}

	// And the guidance table must resolve INSIDE it. The earliest scope sits at
	// the flow name's offset, never at zero, so the probe position is taken
	// from the recovered source line rather than from the top of the file.
	at := strings.Index(flowMissingFromTarget, "source ingest")
	if at < 0 {
		t.Fatal("the fixture no longer carries the recovered line the probe position is taken from")
	}
	src := analysis.Source{Path: damaged.Path, Src: damaged.Src, File: damaged.File}
	guidance, ok := snap.Guidance.At(src, ast.Position{Offset: at})
	if !ok {
		t.Fatalf("the guidance table resolves nothing at offset %d inside a damaged document; "+
			"completion is off in the buffer the author is typing in", at)
	}
	if guidance.Flow != "payments" {
		t.Fatalf("the guidance at offset %d names flow %q, want payments", at, guidance.Flow)
	}
}
