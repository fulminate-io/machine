// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/whitaker-io/machine/lang/loader"
)

// serializationBudget is the nanoseconds one serialization pass over the budget
// fixture may cost.
//
// IT IS AN ABSOLUTE BOUND RATHER THAN A RATIO, and the ratio was tried first. A
// gate asserting that forty declarations of one type cost under 12x one
// declaration read 21.14x and 11.92x across two runs of the SAME code state,
// because the absolute times are microseconds and run-to-run noise swamps the
// effect. No constant separates the memoized implementation from the unmemoized
// one, so the cardinality of the memo is asserted deterministically instead and
// this bound is kept only because it is noise-tolerant at a millisecond.
const serializationBudget = 1000000

// budgetPasses is how many passes the mean is taken over.
const budgetPasses = 20

// TestResolutionIsPerSpellingWhileReportingIsPerDeclaration pins the cost SHAPE:
// one resolution-and-walk per (spelling, site) per run, while reporting stays
// per declaration.
//
// THE DEFECT CLASS IT ALONE DETECTS is a per-declaration resolve. It compiles,
// it passes every behavioral gate in the package, and it turns a one-per-run
// types.Eval into a one-per-declaration cost that a corpus declaring one type in
// forty places pays forty times.
func TestResolutionIsPerSpellingWhileReportingIsPerDeclaration(t *testing.T) {
	src := loadSource(t, filepath.Join(serializationDir, "one-spelling-forty-times.flow"))
	pkgs := loadSerializationSubject(t)

	run := newSerializationRun(pkgs, serializationPkg)

	got, _ := resultOf(t, run.analyzer(), src)

	table, ok := got.(*Registrations)
	if !ok {
		t.Fatalf("the serialization analyzer produced %T, want *Registrations", got)
	}

	t.Logf("declarations=%d resolutions=%d requirements=%d", run.declarations, run.resolutions, len(table.Required))

	assertCostShape(t, run, table)

	mean := measureSerializationPass(t, src, pkgs)

	t.Logf("serialization budget: mean_ns=%d budget_ns=%d", mean, serializationBudget)

	if mean <= 0 {
		t.Fatalf("the measured mean is %d ns, which means the measurement stopped measuring", mean)
	}

	if mean > serializationBudget {
		t.Errorf("the serialization pass measured %d ns against a budget of %d ns", mean, serializationBudget)
	}
}

// assertCostShape is the deciding leg: the memo's cardinality rather than a
// ratio of measured times.
func assertCostShape(t *testing.T, run *serializationRun, table *Registrations) {
	t.Helper()

	if run.declarations != 40 {
		t.Fatalf("CONTROL FAILED: the fixture presented %d declarations, want 40; the cost shape below is only "+
			"meaningful over many declarations of one spelling", run.declarations)
	}

	if run.resolutions != 1 {
		t.Errorf("40 declarations of ONE spelling performed %d resolutions, want 1; resolution is not memoized "+
			"per run", run.resolutions)
	}

	if len(table.Required) != 40 {
		t.Errorf("40 declarations produced %d requirements, want 40; reporting is per DECLARATION even though "+
			"resolution is per spelling", len(table.Required))
	}
}

// measureSerializationPass is the mean nanoseconds one COLD pass costs.
//
// THE LOAD IS OUTSIDE THE MEASURED REGION, deliberately: loader.Load is the
// expensive operation in this whole toolchain, it happens once per generation
// run, and including it would measure go/packages rather than this contract. A
// FRESH analyzer is constructed per pass so every pass pays for its own memo;
// reusing one would measure a warm cache and report a number no first run ever
// sees.
func measureSerializationPass(t *testing.T, src Source, pkgs *loader.Packages) int64 {
	t.Helper()

	start := time.Now()

	for range budgetPasses {
		if _, err := Run([]Source{src}, []*Analyzer{SerializationAnalyzer(pkgs, serializationPkg)}); err != nil {
			t.Fatalf("the measured pass failed: %v", err)
		}
	}

	return time.Since(start).Nanoseconds() / budgetPasses
}
