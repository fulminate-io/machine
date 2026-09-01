// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lsp

import (
	"errors"

	"github.com/whitaker-io/machine/lang/analysis"
)

// captureName is the anonymous analyzer's name. It is never registered, so it
// cannot collide with the module's own twelve.
const captureName = "lsp-capture"

var (
	// errNoSymbolTable reports a symbols result that is not a SymbolTable,
	// which can only happen if the analyzer's result type changed under us.
	errNoSymbolTable = errors.New("lsp: the symbols analyzer produced no table")
	// errNoGuidanceTable is the same for the guidance analyzer.
	errNoGuidanceTable = errors.New("lsp: the guidance analyzer produced no table")
)

// snapshot is everything one analysis run produces that the server serves:
// what to publish, and the two tables every later answer is a lookup over.
type snapshot struct {
	Diagnostics []analysis.Diagnostic
	Symbols     *analysis.SymbolTable
	Guidance    *analysis.GuidanceTable
}

// analyze runs the whole analysis core over the documents ONCE and returns the
// diagnostics to publish alongside both result tables.
//
// IT TAKES DOCUMENTS RATHER THAN analysis.Source VALUES because a Source has
// nowhere to carry a parse error, and the parse errors are needed here. Taking
// sources would leave only one route to them — re-parsing every document on
// every change — which is both wasted work the Store already did and a direct
// contradiction of reparsing only the document that changed.
//
// A DAMAGED DOCUMENT STAYS IN THE RUN. Its parse diagnostics are prepended and
// the analyzers' findings ABOUT IT are dropped at attribution, but its tree
// still feeds the symbol and guidance tables — so completion and navigation
// keep working inside the buffer the author is typing in, which is the buffer
// least likely to parse and the one where they matter most.
func analyze(docs []*Document) (*snapshot, error) {
	srcs, damaged := sourceSet(docs)
	snap := &snapshot{}
	diags, err := analysis.Run(srcs, append(analysis.All(), captureAnalyzer(snap)))
	if err != nil {
		return nil, err
	}
	snap.Diagnostics = append(parseDiagnostics(docs), attributable(diags, damaged)...)
	return snap, nil
}

// sourceSet projects the documents onto the analysis core's source shape and
// collects the paths whose parse failed.
func sourceSet(docs []*Document) ([]analysis.Source, map[string]bool) {
	srcs := make([]analysis.Source, 0, len(docs))
	damaged := make(map[string]bool)
	for _, doc := range docs {
		srcs = append(srcs, source(doc))
		if doc.ParseErr != nil {
			damaged[doc.Path] = true
		}
	}
	return srcs, damaged
}

// source projects one document onto the analysis core's source shape, dropping
// the parse error the core has nowhere to put.
func source(doc *Document) analysis.Source {
	return analysis.Source{Path: doc.Path, Src: doc.Src, File: doc.File}
}

// parseDiagnostics converts every document's parse error into the framework's
// diagnostics, which is what an editor shows while a file is mid-keystroke.
func parseDiagnostics(docs []*Document) []analysis.Diagnostic {
	out := make([]analysis.Diagnostic, 0, len(docs))
	for _, doc := range docs {
		if doc.ParseErr != nil {
			out = append(out, analysis.ParseDiagnostics(doc.Path, doc.ParseErr)...)
		}
	}
	return out
}

// attributable drops analyzer findings ABOUT a document that failed to parse.
//
// A partial tree makes the analyzers report artifacts of the DAMAGE rather than
// facts about the program — the signature analyzer concatenates a recovered
// Ident's empty Name straight into its message, so a half-typed line yields a
// complaint about an output with no name. Suppression is keyed on Path at
// ATTRIBUTION time rather than by withholding the source, because the source is
// what keeps that document's scopes in the tables.
func attributable(diags []analysis.Diagnostic, damaged map[string]bool) []analysis.Diagnostic {
	out := make([]analysis.Diagnostic, 0, len(diags))
	for _, d := range diags {
		if !damaged[d.Path] {
			out = append(out, d)
		}
	}
	return out
}

// captureAnalyzer builds the anonymous analyzer that lifts both result tables
// out of ONE driver run.
//
// This extends the analysis core's own idiom rather than reinventing it:
// BuildGuidance declares an anonymous analyzer whose Requires names what it
// wants and reads Pass.ResultOf, so the prerequisite ordering stays the
// driver's. Requiring both analyzers here is what makes a single Run yield the
// diagnostics, the symbols and the guidance together — calling BuildGuidance
// separately would run all twelve analyzers a second time for a table this run
// already built.
func captureAnalyzer(snap *snapshot) *analysis.Analyzer {
	return &analysis.Analyzer{
		Name: captureName,
		Doc: "lsp-capture reads the symbol and guidance tables out of one driver run so the language " +
			"server pays for a single analysis per document change rather than one per answer it serves.",
		Requires: []*analysis.Analyzer{analysis.SymbolsAnalyzer, analysis.GuidanceAnalyzer},
		Run:      func(p *analysis.Pass) (any, error) { return nil, snap.read(p) },
	}
}

// read pulls both tables out of the pass's shared results.
func (s *snapshot) read(p *analysis.Pass) error {
	symbols, ok := p.ResultOf[analysis.SymbolsAnalyzer].(*analysis.SymbolTable)
	if !ok {
		return errNoSymbolTable
	}
	guidance, ok := p.ResultOf[analysis.GuidanceAnalyzer].(*analysis.GuidanceTable)
	if !ok {
		return errNoGuidanceTable
	}
	s.Symbols, s.Guidance = symbols, guidance
	return nil
}
