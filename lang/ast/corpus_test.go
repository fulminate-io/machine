// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The four corpora. Three buckets rather than two, because there are three
// genuinely different outcomes and collapsing any two produces a gate that can
// only pass by making the parser wrong.
const (
	validCorpusDir    = "testdata/valid"
	analysisRejectDir = "testdata/analysis-rejects"
	invalidCorpusDir  = "testdata/invalid"
	strawmanDir       = "testdata/strawman"
)

// mandatoryValidFixtures are the eleven files the valid corpus must carry. The
// corpus itself stays OPEN; only these names are required.
//
// This is a SUBSET gate rather than a set-equality one, and it exists because
// without it the mandatory list is a suggestion — measured on an earlier
// revision, two-flows.flow and node-on-error-continuation.flow could both be
// deleted with every other gate still green.
var mandatoryValidFixtures = []string{
	"const-and-param",
	"switch-with-else",
	"switch-no-else",
	"subflow-and-use",
	"two-flows",
	"node-on-error-continuation",
	"func-before-use",
	"func-after-use",
	"func-go-aware-spans",
	"func-nested-literals",
	// The canonical `checkpoint <codec>` form. It was testdata/invalid's
	// checkpoint-with-argument fixture, which existed to prove an operand was
	// REFUSED; the clause now requires one, so the fixture inverted rather than
	// being deleted, and it is mandatory so the corpus always demonstrates the
	// shape authors are meant to write.
	"checkpoint-with-codec",
}

// lockedInvalidFixtures is the closed set of forms the parser rejects.
var lockedInvalidFixtures = []string{
	"map-as-shape-keyword",
	"node-prefixed-statement",
	"retry-keyword",
	"error-edge-arrow",
	"switch-with-no-arms",
	"else-not-last",
	"drop-with-from",
	"checkpoint-without-codec",
	"over-without-transport",
	"duplicate-clause",
	"func-missing-name",
	"func-unterminated-body",
	"headless-statement",
	"via-not-over",
}

// lockedAnalysisRejectFixtures is the closed set of files that PARSE CLEAN and
// are wrong at a later layer.
var lockedAnalysisRejectFixtures = []string{
	"declare-after-use-loop",
	"wrapper-type-state",
	"destructuring-arm",
	"traversal-wide-var",
}

// corpusFiles lists the .flow sources of one corpus, refusing an empty read.
func corpusFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.flow"))
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	if len(matches) == 0 {
		t.Fatalf("CONTROL FAILED: %s holds no .flow sources", dir)
	}
	slices.Sort(matches)
	return matches
}

// fixtureNames projects paths down to their bare fixture names.
func fixtureNames(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, strings.TrimSuffix(filepath.Base(p), ".flow"))
	}
	slices.Sort(out)
	return out
}

// parseCorpusFile reads and parses one fixture.
func parseCorpusFile(t *testing.T, path string) (*File, error) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	file, parseErr := Parse(src)
	if file == nil {
		t.Fatalf("%s: Parse returned a nil File", path)
	}
	return file, parseErr
}

// TestValidCorpusParsesWithNoDiagnostics asserts every correct source parses
// clean.
func TestValidCorpusParsesWithNoDiagnostics(t *testing.T) {
	for _, path := range corpusFiles(t, validCorpusDir) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if _, err := parseCorpusFile(t, path); err != nil {
				t.Fatalf("a valid source produced diagnostics: %v", err)
			}
		})
	}
}

// TestAnalysisRejectCorpusParsesClean asserts every deliberately-wrong-later
// source parses with ZERO diagnostics.
//
// A parse diagnostic here would mean the parser has started resolving names or
// types, which is a layer below this package.
func TestAnalysisRejectCorpusParsesClean(t *testing.T) {
	for _, path := range corpusFiles(t, analysisRejectDir) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			file, err := parseCorpusFile(t, path)
			if err != nil {
				t.Fatalf("the parser rejected a source only a later layer should reject: %v", err)
			}
			if len(file.Decls) == 0 {
				t.Fatalf("parsed clean but produced an empty tree")
			}
		})
	}
}

// TestRejectedFormsAreRejectedWithAPositionedDiagnostic walks the invalid corpus
// and checks each fixture against the expected position and message in its
// sibling .want file.
func TestRejectedFormsAreRejectedWithAPositionedDiagnostic(t *testing.T) {
	for _, path := range corpusFiles(t, invalidCorpusDir) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			_, err := parseCorpusFile(t, path)
			if err == nil {
				t.Fatalf("a rejected form parsed clean")
			}
			parseErr, ok := err.(*Error)
			if !ok {
				t.Fatalf("Parse returned %T, want *Error", err)
			}

			wantRaw, readErr := os.ReadFile(path + ".want")
			if readErr != nil {
				t.Fatalf("reading %s.want: %v", path, readErr)
			}
			wantPos, wantMessage := splitExpectation(t, strings.TrimSpace(string(wantRaw)))

			for _, d := range parseErr.Diagnostics {
				if d.Pos.String() == wantPos && strings.Contains(d.Message, wantMessage) {
					return
				}
			}
			t.Fatalf("no diagnostic at %s containing %q; got %v", wantPos, wantMessage, parseErr.Diagnostics)
		})
	}
}

// splitExpectation splits a `line:col: message` expectation.
func splitExpectation(t *testing.T, want string) (string, string) {
	t.Helper()
	line, rest, ok := strings.Cut(want, ":")
	if !ok {
		t.Fatalf("expectation %q is not `line:col: message`", want)
	}
	col, message, ok := strings.Cut(rest, ":")
	if !ok {
		t.Fatalf("expectation %q is not `line:col: message`", want)
	}
	return line + ":" + col, strings.TrimSpace(message)
}

// TestInvalidCorpusCoversEveryLockedFixture asserts SET EQUALITY: the invalid
// corpus is a closed set.
func TestInvalidCorpusCoversEveryLockedFixture(t *testing.T) {
	assertSetEquality(t, invalidCorpusDir, lockedInvalidFixtures, 14)
}

// TestAnalysisRejectCorpusCoversEveryLockedFixture asserts SET EQUALITY: the
// analysis-reject corpus is a closed set.
func TestAnalysisRejectCorpusCoversEveryLockedFixture(t *testing.T) {
	assertSetEquality(t, analysisRejectDir, lockedAnalysisRejectFixtures, 4)
}

// assertSetEquality checks a closed corpus against its locked name list.
func assertSetEquality(t *testing.T, dir string, locked []string, wantCount int) {
	t.Helper()
	if len(locked) != wantCount {
		t.Fatalf("the locked list for %s states %d names, want %d", dir, len(locked), wantCount)
	}
	present := fixtureNames(corpusFiles(t, dir))
	want := slices.Clone(locked)
	slices.Sort(want)

	if slices.Equal(want, present) {
		return
	}
	for _, name := range want {
		if !slices.Contains(present, name) {
			t.Errorf("%s is missing the locked fixture %q", dir, name)
		}
	}
	for _, name := range present {
		if !slices.Contains(want, name) {
			t.Errorf("%s carries %q, which is not a locked fixture", dir, name)
		}
	}
	t.Fatalf("%s holds %d fixtures, the locked list %d", dir, len(present), len(want))
}

// TestValidCorpusCoversEveryMandatoryFixture asserts the eleven mandatory names
// are present. The valid corpus stays OPEN, so this is a subset gate.
func TestValidCorpusCoversEveryMandatoryFixture(t *testing.T) {
	if len(mandatoryValidFixtures) != 11 {
		t.Fatalf("the mandatory list states %d fixtures; the plan locks 11", len(mandatoryValidFixtures))
	}
	present := fixtureNames(corpusFiles(t, validCorpusDir))
	for _, name := range mandatoryValidFixtures {
		if !slices.Contains(present, name) {
			t.Errorf("the valid corpus is missing the mandatory fixture %q", name)
		}
	}
}

// TestCorpusFilesEndWithATrailingNewline enforces what the grammar requires.
//
// Every Entry and FlowEntry alternative terminates in the newline terminal, so a
// file whose last line lacks one DOES NOT DERIVE — and the conformance gate
// would then report a recognizer/parser divergence rather than the missing byte
// it actually is. The broken corpus is excluded on purpose: a mid-edit file
// ending without a newline is exactly what it is for.
func TestCorpusFilesEndWithATrailingNewline(t *testing.T) {
	checked := 0
	for _, dir := range []string{validCorpusDir, analysisRejectDir, strawmanDir} {
		for _, path := range corpusFiles(t, dir) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			checked++
			if len(src) == 0 || src[len(src)-1] != '\n' {
				t.Errorf("%s does not end with a newline, so it does not derive under the grammar", path)
			}
		}
	}
	if checked == 0 {
		t.Fatalf("CONTROL FAILED: the scan read no corpus files at all")
	}
}

// strawmanShape is what each canonical example must parse to.
//
// The FuncDecl count is the executable form of the no-splicing invariant: toy
// and payments were ratified WITHOUT funcs, and enrichment is the ratified
// example for the amendment.
//
// The counts and the zero-diagnostics leg catch different defects. A parser
// whose flow-body terminator set omits `func` meets a func token at the
// statement dispatcher and emits a diagnostic — the zero-diagnostics leg catches
// that. The DECLARATION count catches the subtler variant: a parser that handles
// `func` but parents it INSIDE the flow as a body entry, which parses clean,
// reports nothing, and is wrong only in shape.
var strawmanShape = map[string]struct {
	decls, stmts, funcs int
}{
	"toy.flow":        {decls: 3, stmts: 11, funcs: 0},
	"payments.flow":   {decls: 6, stmts: 12, funcs: 0},
	"enrichment.flow": {decls: 6, stmts: 7, funcs: 2},
}

// TestStrawmenParseClean asserts all three canonical examples parse to a non-nil
// File with zero diagnostics, and pins the shape each parses to.
func TestStrawmenParseClean(t *testing.T) {
	files := corpusFiles(t, strawmanDir)
	if len(files) != 3 {
		t.Fatalf("the strawman corpus holds %d files, want the 3 canonical examples", len(files))
	}

	for _, path := range files {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			want, known := strawmanShape[name]
			if !known {
				t.Fatalf("%s is not one of the three canonical examples", name)
			}

			file, err := parseCorpusFile(t, path)
			if err != nil {
				t.Fatalf("a canonical example produced diagnostics: %v", err)
			}

			funcs, stmts := 0, 0
			for _, decl := range file.Decls {
				switch typed := decl.(type) {
				case FuncDecl:
					funcs++
				case FlowDecl:
					stmts += len(typed.Body)
				}
			}

			if len(file.Decls) != want.decls {
				t.Errorf("parsed %d declarations, want %d", len(file.Decls), want.decls)
			}
			if stmts != want.stmts {
				t.Errorf("parsed %d statements, want %d", stmts, want.stmts)
			}
			if funcs != want.funcs {
				t.Errorf("parsed %d func declarations, want %d", funcs, want.funcs)
			}
		})
	}
}
