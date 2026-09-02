// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ast

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// updateGoldens regenerates the broken-corpus goldens in place. It is the
// sanctioned way to move them: hand-editing a golden and hand-editing the parser
// are two ways to make the same mistake.
var updateGoldens = flag.Bool("update", false, "regenerate the broken-corpus goldens")

// brokenCorpusDir holds the mid-edit sources — what a file looks like while
// someone is typing, rather than synthetic malformations.
const brokenCorpusDir = "testdata/broken"

// brokenKinds is the LOCKED list of breakage kinds. THE FILE NAME IS THE KIND,
// so the coverage test reads the directory rather than a hand-kept manifest that
// could drift away from what is actually on disk.
//
// There is deliberately no unclosed-flow-brace kind: a flow body is braceless
// and ends at the next `flow` or `func` line or at end of file, so the failure
// it would name cannot occur.
var brokenKinds = []string{
	"unterminated-note",
	"unclosed-state-brace",
	"unclosed-switch-brace",
	"truncated-statement",
	"truncated-func-body",
	"unknown-leading-keyword",
	"missing-from-target",
	"missing-arrow-target",
	"partial-flow-header",
}

// countStatements totals the statements across every flow in a file.
func countStatements(file *File) int {
	total := 0
	for _, decl := range file.Decls {
		if flow, ok := decl.(FlowDecl); ok {
			total += len(flow.Body)
		}
	}
	return total
}

// renderRecovery renders what a parse recovered: the shape of the partial tree
// first, then one line per diagnostic.
//
// The COUNTS are on the first line because a recovery record that held only
// diagnostics could not tell a parser that recovered from one that gave up and
// returned an empty file.
func renderRecovery(file *File, err error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "decls=%d stmts=%d\n", len(file.Decls), countStatements(file))
	var parseErr *Error
	if err != nil {
		var ok bool
		if parseErr, ok = err.(*Error); !ok {
			fmt.Fprintf(&b, "UNEXPECTED ERROR TYPE %T\n", err)
			return b.String()
		}
		for _, d := range parseErr.Diagnostics {
			fmt.Fprintf(&b, "%s: %s\n", d.Pos, d.Message)
		}
	}
	return b.String()
}

// brokenCorpusFiles lists the .flow sources of the broken corpus.
func brokenCorpusFiles(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(brokenCorpusDir, "*.flow"))
	if err != nil {
		t.Fatalf("reading %s: %v", brokenCorpusDir, err)
	}
	slices.Sort(matches)
	return matches
}

// TestBrokenCorpusRecoversWithExpectedDiagnostics parses every mid-edit source
// and diffs the recovery against its golden.
func TestBrokenCorpusRecoversWithExpectedDiagnostics(t *testing.T) {
	files := brokenCorpusFiles(t)
	if len(files) == 0 {
		t.Fatalf("CONTROL FAILED: %s holds no .flow sources", brokenCorpusDir)
	}

	recoveredStatements := 0
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}

			file, parseErr := Parse(src)
			if file == nil {
				t.Fatalf("Parse returned a nil File; a broken source still yields a tree")
			}
			if parseErr == nil {
				t.Fatalf("%s parsed clean, so it is not a broken source at all", path)
			}
			recoveredStatements += countStatements(file)

			// Every fixture declares at least a flow, so a parse that recovered
			// nothing at all cannot match its golden by both sides being empty.
			if len(file.Decls) == 0 {
				t.Fatalf("recovered no declarations; the parse gave up rather than recovering")
			}

			got := renderRecovery(file, parseErr)
			golden := path + ".want"
			if *updateGoldens {
				if writeErr := os.WriteFile(golden, []byte(got), 0o600); writeErr != nil {
					t.Fatalf("writing %s: %v", golden, writeErr)
				}
				return
			}

			want, readErr := os.ReadFile(golden)
			if readErr != nil {
				t.Fatalf("reading %s (regenerate with -update): %v", golden, readErr)
			}
			if got != string(want) {
				t.Errorf("recovery differs from the golden:\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}

	// THE CORPUS-LEVEL KNOWN POSITIVE. Per-file goldens would all match a parser
	// that recovered nothing if the goldens had been regenerated from it, so the
	// corpus as a whole must show real statements surviving real breakage.
	if !*updateGoldens && recoveredStatements == 0 {
		t.Fatalf("the whole corpus recovered zero statements; every golden records an empty tree")
	}
}

// TestBrokenCorpusCoversEveryNamedBreakageKind asserts set equality between the
// locked kind list and the files on disk, so a kind cannot be quietly dropped
// and an unlisted file cannot be quietly added.
func TestBrokenCorpusCoversEveryNamedBreakageKind(t *testing.T) {
	files := brokenCorpusFiles(t)
	if len(files) == 0 {
		t.Fatalf("CONTROL FAILED: %s holds no .flow sources", brokenCorpusDir)
	}

	present := make([]string, 0, len(files))
	for _, path := range files {
		present = append(present, strings.TrimSuffix(filepath.Base(path), ".flow"))
	}

	want := slices.Clone(brokenKinds)
	if len(want) != 9 {
		t.Fatalf("the locked breakage list states %d kinds; the plan locks 9", len(want))
	}
	slices.Sort(want)
	slices.Sort(present)

	if !slices.Equal(want, present) {
		for _, kind := range want {
			if !slices.Contains(present, kind) {
				t.Errorf("breakage kind %q has no fixture", kind)
			}
		}
		for _, kind := range present {
			if !slices.Contains(want, kind) {
				t.Errorf("fixture %q.flow is not a locked breakage kind", kind)
			}
		}
		t.Fatalf("corpus (%d files) diverges from the locked kind list (%d)", len(present), len(want))
	}
}

// TestBrokenCorpusPinsTheTruncatedStatementCursor guards the one fixture whose
// exact bytes are load bearing.
//
// truncated-statement.flow ends at `transform charge billing.Charge from ` with
// the cursor there. The TRAILING SPACE is what the author has typed so far, and
// a whitespace-stripping tool run over the tree would silently change which
// token the parser is looking at when the file runs out.
func TestBrokenCorpusPinsTheTruncatedStatementCursor(t *testing.T) {
	path := filepath.Join(brokenCorpusDir, "truncated-statement.flow")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	const want = "transform charge billing.Charge from "
	if !strings.HasSuffix(string(src), want) {
		t.Fatalf("%s ends %q, want it to end %q with the trailing space intact",
			path, string(src[max(0, len(src)-40):]), want)
	}
}
