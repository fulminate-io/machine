// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/whitaker-io/machine/lang/ast"
)

// The analysis budget, structured on lang/ast's reparse budget so the two are
// read together.
//
// THE BUDGET IS ANCHORED ON MEASUREMENT RATHER THAN INVENTION. One full
// structural walk of payments.flow costs about 22 nanoseconds and visits 46
// elements, while a parse plus one walk costs about 12 microseconds — the parse
// dominates by roughly 500x. Eleven analyzers each doing one walk therefore land
// the whole pass in the same tens-of-microseconds band as the parse it follows,
// leaving the one-millisecond editor budget with well over an order of magnitude
// of headroom.
//
// If the measured figure ever comes in above about 100 microseconds, that is a
// signal an analyzer has gone quadratic and should be investigated. It is NOT a
// signal to raise the budget.
const (
	analysisIterations = 200
	analysisBudget     = time.Millisecond
)

// TestFullAnalysisBudget measures a parse plus the full registered analyzer set
// over the payments strawman, and prints the figure whether it passes or fails,
// so the number lives in the run rather than only in a plan.
//
// NOTE ON THE TEST CACHE: this is the one measurement in this package whose
// validity depends on the run being fresh. A cached PASS is a valid pass of the
// assertion but it re-measures nothing, so a figure in the log from a cached run
// is the figure from whenever it last actually ran. That is stated here rather
// than defended with a cache-defeating flag, which would cost every unrelated run
// for no information — the same call lang/ast made.
func TestFullAnalysisBudget(t *testing.T) {
	path := filepath.Join(strawmanDir, "payments.flow")
	src, readErr := readFixture(t, path)
	if readErr != nil {
		t.Fatalf("cannot read %s: %v", path, readErr)
	}

	if _, err := ast.Parse(src); err != nil {
		t.Fatalf("CONTROL FAILED: the budget input does not parse clean: %v", err)
	}
	if len(src) < 1000 {
		t.Fatalf("CONTROL FAILED: the budget input is %d bytes, too small to measure anything", len(src))
	}

	analyzers := All()
	if len(analyzers) == 0 {
		t.Fatal("CONTROL FAILED: no analyzers are registered, so this would measure a parse alone")
	}

	start := time.Now()
	for range analysisIterations {
		file, err := ast.Parse(src)
		if file == nil || err != nil {
			t.Fatalf("a parse failed midway through the measurement: %v", err)
		}
		if _, err := Run([]Source{{Path: path, Src: src, File: file}}, analyzers); err != nil {
			t.Fatalf("an analysis failed midway through the measurement: %v", err)
		}
	}
	mean := time.Since(start) / analysisIterations

	t.Logf("full analysis of %s (%d bytes, %d analyzers): mean %v over %d iterations, budget %v",
		filepath.Base(path), len(src), len(analyzers), mean, analysisIterations, analysisBudget)

	// THE SAME FIGURE IN ASCII, for a gate to parse. The human line above renders
	// a duration, which means it can read "24.52µs" or "1.2ms" depending on
	// magnitude — a shape no grep can compare against a budget. This one is two
	// integers in nanoseconds and always has the same form.
	t.Logf("analysis budget: mean_ns=%d budget_ns=%d", mean.Nanoseconds(), analysisBudget.Nanoseconds())

	if mean > analysisBudget {
		t.Fatalf("mean full analysis %v exceeds the %v budget", mean, analysisBudget)
	}
}
