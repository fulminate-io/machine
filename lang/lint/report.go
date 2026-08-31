// Package lint - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lint

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/whitaker-io/machine/lang/analysis"
)

// FormatText names the rendering meant for a person.
const FormatText = "text"

// FormatJSON names the rendering meant for a machine.
const FormatJSON = "json"

// Formats is the output vocabulary, declared once so the flag's help text and
// its refusal message cannot drift apart from what WriteJSON and WriteText
// actually offer.
//
// SARIF is not among them. The ticket asks for one machine-readable format, this
// one is a lossless projection of the Diagnostic vocabulary, and SARIF is
// derivable from it later by whoever needs it.
var Formats = []string{FormatText, FormatJSON}

// withheldReason is the account owed for every file kept away from the
// analyzers. Naming the file is not enough on its own: a reader needs to know
// the finding set is short by a file and why.
const withheldReason = ": not analyzed: it did not parse, and analyzer findings over a damaged tree " +
	"describe the damage rather than the program"

// crossFileIncomplete follows the summary whenever anything was withheld,
// because analysis facts travel between files: a missing file does not only cost
// its own findings, it costs the cross-file findings that needed it.
const crossFileIncomplete = " file(s) were withheld from the analyzers, so cross-file findings are incomplete"

// noProofOfCorrectness closes EVERY run, clean or not.
//
// THE WORDING IS LOAD-BEARING. `no problems found` would claim a proof this
// engine cannot supply: typeflow compares declared spellings and resolves
// nothing through go/types, state's bare-type check is a denylist of two retired
// spellings, switches mandates an else precisely because v1 cannot prove
// coverage, resolve ships no unimported-qualifier check, signature says nothing
// about a use it cannot resolve, and errorrouting's unhandled-failure finding is
// legal by the ruled supervisor default.
const noProofOfCorrectness = "no registered rule fired is not a proof of correctness; " +
	"run with -rules for each rule's own stated limits"

// lineWriter writes lines and REMEMBERS the first failure, so a caller checks
// once at the end instead of at every write.
//
// It is not a swallow: the error is returned to the caller, and a stream that
// has already failed is not written to again.
type lineWriter struct {
	w   io.Writer
	err error
}

// line writes one line unless the stream has already failed.
func (l *lineWriter) line(text string) {
	if l.err != nil {
		return
	}
	_, l.err = fmt.Fprintln(l.w, text)
}

// WriteText renders a result for a person: one line per finding in the
// compiler's own shape, one line per withheld file, then the summary.
//
// The trailing [code] on a finding is the emitting analyzer's Name, which the
// driver stamps, so a consumer can route or grep one rule without matching on
// message text.
func WriteText(w io.Writer, result Result) error {
	out := &lineWriter{w: w}

	for _, d := range result.Diagnostics {
		out.line(d.Path + ":" + d.Pos.String() + ": " + d.Severity.String() + ": " + d.Message + " [" + d.Code + "]")
	}
	for _, path := range result.Damaged {
		out.line(path + withheldReason)
	}

	writeSummary(out, result)
	return out.err
}

// writeSummary states the verdict, what it was judged against, and what the
// verdict does not prove.
func writeSummary(out *lineWriter, result Result) {
	counts := map[analysis.Severity]int{}
	for _, d := range result.Diagnostics {
		counts[d.Severity]++
	}

	out.line(strconv.Itoa(result.Failing) + " at or above " + result.Threshold.String() +
		" (" + strconv.Itoa(counts[analysis.SeverityError]) + " error, " +
		strconv.Itoa(counts[analysis.SeverityWarning]) + " warning, " +
		strconv.Itoa(counts[analysis.SeverityHint]) + " hint)")

	if withheld := len(result.Damaged); withheld > 0 {
		out.line(strconv.Itoa(withheld) + crossFileIncomplete)
	}
	out.line(noProofOfCorrectness)
}

// wireReport is the JSON document's top level.
type wireReport struct {
	Threshold   string           `json:"threshold"`
	Failing     int              `json:"failing"`
	Damaged     []string         `json:"damaged"`
	Diagnostics []wireDiagnostic `json:"diagnostics"`
}

// wireDiagnostic is one analysis.Diagnostic renamed into the spelling a JSON
// consumer expects. Nothing is derived and nothing is dropped.
//
// BOTH POSITIONS ARE CARRIED WHOLE — line, col AND offset at each end — because
// ast.Position.Col counts BYTES rather than runes, which lang/ast/position.go
// states explicitly. A consumer mapping onto UTF-16 code units needs the offset
// to do it, and dropping it would silently make that consumer impossible.
type wireDiagnostic struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Col       int    `json:"col"`
	Offset    int    `json:"offset"`
	EndLine   int    `json:"end_line"`
	EndCol    int    `json:"end_col"`
	EndOffset int    `json:"end_offset"`
	Severity  string `json:"severity"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// WriteJSON renders a result as a lossless projection of the Diagnostic
// vocabulary, so a consumer of this format sees exactly what a consumer of the
// pass framework sees.
func WriteJSON(w io.Writer, result Result) error {
	doc := wireReport{
		Threshold: result.Threshold.String(),
		Failing:   result.Failing,
		// An empty list rather than null: a null is a second spelling of empty
		// that every consumer would have to special-case.
		Damaged:     []string{},
		Diagnostics: make([]wireDiagnostic, 0, len(result.Diagnostics)),
	}
	doc.Damaged = append(doc.Damaged, result.Damaged...)

	for _, d := range result.Diagnostics {
		doc.Diagnostics = append(doc.Diagnostics, wireDiagnostic{
			Path:      d.Path,
			Line:      d.Pos.Line,
			Col:       d.Pos.Col,
			Offset:    d.Pos.Offset,
			EndLine:   d.End.Line,
			EndCol:    d.End.Col,
			EndOffset: d.End.Offset,
			Severity:  d.Severity.String(),
			Code:      d.Code,
			Message:   d.Message,
		})
	}

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(doc)
}

// WriteRules prints every registered analyzer's name and its own Doc, VERBATIM.
//
// Verbatim is the whole point. The limits each analyzer states about itself are
// the only honest account of what a clean run means, and a paraphrase here would
// rot the moment an analyzer's own text changed. Printing out of analysis.All()
// rather than from a list held here means an analyzer added to the registry
// cannot be silently omitted either.
func WriteRules(w io.Writer) error {
	out := &lineWriter{w: w}
	for _, a := range analysis.All() {
		out.line(a.Name)
		out.line(a.Doc)
		out.line("")
	}
	return out.err
}
