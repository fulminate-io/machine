// Package lint - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lint

import (
	"errors"
	"sort"
	"strings"

	"github.com/whitaker-io/machine/lang/analysis"
)

// Thresholds is the fail-on vocabulary, in severity order.
//
// The names are taken from analysis.Severity.String rather than written out, so
// the parser, the refusal message and the rendered level cannot disagree. The
// ORDER is load-bearing too: ParseThreshold returns the index as the Severity,
// which is exact because the severity constants are an iota in this same order.
//
// There is deliberately no `never`. The threshold can be made stricter than its
// default and no looser, so no invocation of this tool can turn a red green.
var Thresholds = []string{
	analysis.SeverityError.String(),
	analysis.SeverityWarning.String(),
	analysis.SeverityHint.String(),
}

// Result is one run's verdict: everything reported, everything withheld, the
// threshold it was judged against and how much of it failed.
type Result struct {
	Diagnostics []analysis.Diagnostic
	Damaged     []string
	Threshold   analysis.Severity
	Failing     int
}

// ParseThreshold resolves a fail-on level name to its severity.
//
// An unknown name is REFUSED, naming both the value and the vocabulary. It never
// coerces to a default: a misspelled level that silently became the strictest or
// the loosest setting is a build whose gate nobody chose.
func ParseThreshold(name string) (analysis.Severity, error) {
	for i, level := range Thresholds {
		if level == name {
			return analysis.Severity(i), nil
		}
	}
	return analysis.SeverityError, errors.New("lint: unknown fail-on level " + name +
		"; the levels are " + strings.Join(Thresholds, ", "))
}

// Check runs the registered analyzers over the batch's clean sources, merges
// what they report with the parse diagnostics the batch already carries, and
// counts everything at or above the threshold.
//
// EVERY REGISTERED ANALYZER RUNS, always: analysis.All() with no selection. There
// is no --only, no --exclude, no configuration file and no suppression comment.
// Each of those is a lever for making a finding disappear without fixing it.
//
// An analyzer error is RETURNED rather than absorbed. A run that could not be
// performed is not a run that found nothing.
func Check(batch Batch, threshold analysis.Severity) (Result, error) {
	result := Result{Damaged: batch.Damaged, Threshold: threshold}
	result.Diagnostics = append(result.Diagnostics, batch.Parse...)

	// The analyzers are given only what parsed. An empty source set means every
	// file was withheld, which is a real state and not an error here — the
	// batch's own refusals already rejected an input that could not be filled.
	if len(batch.Sources) > 0 {
		found, err := analysis.Run(batch.Sources, analysis.All())
		if err != nil {
			return Result{}, err
		}
		result.Diagnostics = append(result.Diagnostics, found...)
	}

	// Not redundant with the driver's sort: analysis.Run orders what IT returns,
	// and the parse diagnostics are merged in afterwards, arriving outside it.
	sortDiagnostics(result.Diagnostics)

	// THE DIRECTION MATTERS. Severity is an iota where error is 0 and hint is 2,
	// so numerically lower is louder and `<=` means at-or-above in loudness.
	for _, d := range result.Diagnostics {
		if d.Severity <= threshold {
			result.Failing++
		}
	}
	return result, nil
}

// sortDiagnostics puts the merged findings in a stable, content-derived order so
// two runs over the same inputs produce byte-identical output.
//
// The key leads with path because every parsed tree starts at offset zero, so an
// offset-first key ties constantly across files and falls back to arrival order.
func sortDiagnostics(diags []analysis.Diagnostic) {
	sort.SliceStable(diags, func(i, j int) bool {
		if diags[i].Path != diags[j].Path {
			return diags[i].Path < diags[j].Path
		}
		if diags[i].Pos.Offset != diags[j].Pos.Offset {
			return diags[i].Pos.Offset < diags[j].Pos.Offset
		}
		if diags[i].Code != diags[j].Code {
			return diags[i].Code < diags[j].Code
		}
		return diags[i].Message < diags[j].Message
	})
}
