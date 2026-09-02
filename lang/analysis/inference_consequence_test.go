// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"path/filepath"
	"testing"
	"time"
)

// The inference budget, structured on TestFullAnalysisBudget's so the two are
// read together and a gate can parse both the same way.
//
// THE ITERATION COUNT IS LOWER because each iteration evaluates real go/types
// expressions rather than walking a parsed tree: one simple selector costs about
// 1.7µs and a generic instantiation call about 13µs, against roughly 22ns for a
// full structural walk.
//
// As with the analysis budget, a measured figure above about 100 microseconds is
// a signal the pass has gone quadratic and is NOT a signal to raise the budget.
const (
	inferenceIterations = 50
	inferenceBudget     = time.Millisecond
)

// TestInferenceBudget measures the inference pass against the one-millisecond
// editor budget.
//
// THE LOAD IS OUTSIDE THE MEASURED REGION, deliberately. loader.Load is the
// caller's one-off — seconds of work done once per generation run, measured at
// 65ms for a two-package fixture — and folding it into the loop would measure
// go/packages rather than this pass. What is measured is what a caller pays per
// analysis once the packages are in hand.
//
// NOTE ON THE TEST CACHE: as with TestFullAnalysisBudget, a cached PASS is a
// valid pass of the assertion but re-measures nothing, so a figure in the log
// from a cached run is the figure from whenever it last actually ran. That is
// stated rather than defended with a cache-defeating flag.
func TestInferenceBudget(t *testing.T) {
	path := filepath.Join(inferenceDir, "Screening.flow")
	src := loadSource(t, path)

	if len(src.Src) == 0 {
		t.Fatal("CONTROL FAILED: the budget input is empty, so this would measure nothing")
	}

	pkgs := loadInferenceSubject(t)

	// THE SECOND CONTROL: one clean run BEFORE the timer starts. A loop whose
	// every iteration failed would otherwise report a very fast mean.
	table, _, err := BuildInferredTypes([]Source{src}, pkgs)
	if err != nil {
		t.Fatalf("CONTROL FAILED: the budget input does not infer clean: %v", err)
	}
	if len(table.Flows()) == 0 {
		t.Fatal("CONTROL FAILED: the budget input inferred no flows at all")
	}

	start := time.Now()
	for range inferenceIterations {
		if _, _, err := BuildInferredTypes([]Source{src}, pkgs); err != nil {
			t.Fatalf("an inference failed midway through the measurement: %v", err)
		}
	}
	mean := time.Since(start) / inferenceIterations

	t.Logf("inference of %s (%d bytes): mean %v over %d iterations, budget %v",
		filepath.Base(path), len(src.Src), mean, inferenceIterations, inferenceBudget)

	// THE SAME FIGURE IN ASCII, for a gate to parse — two integers in nanoseconds,
	// always the same form, where a rendered duration reads "24.52µs" or "1.2ms"
	// depending on magnitude and defeats comparison.
	t.Logf("inference budget: mean_ns=%d budget_ns=%d", mean.Nanoseconds(), inferenceBudget.Nanoseconds())

	if mean > inferenceBudget {
		t.Fatalf("mean inference %v exceeds the %v budget", mean, inferenceBudget)
	}
}
