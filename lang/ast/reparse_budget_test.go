// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import (
	"path/filepath"
	"testing"
	"time"
)

// The reparse budget. v1 implements incrementality as a CHEAP FULL REPARSE,
// which is admissible because the editor contract is responsiveness rather than
// a particular algorithm.
//
// THE BUDGET IS ANCHORED ON A MEASUREMENT. Go's own go/parser — a strictly
// harder grammar, with a full expression parser, comment attachment and scoping
// — was benchmarked at 53.7 microseconds for a 1730-byte file and 137.9
// microseconds for a 5720-byte one. Even at the larger size a one-millisecond
// budget carries roughly seven times headroom against a heavier parser, which
// leaves room for a CI runner several times slower than a developer machine.
const (
	reparseIterations = 200
	reparseBudget     = time.Millisecond
)

// TestFullReparseBudget measures a full reparse of the payments strawman and
// prints the figure whether it passes or fails, so the number lives in the log
// rather than only in a plan.
//
// NOTE ON THE TEST CACHE: this is the one measurement in this package whose
// validity depends on the run being fresh. A cached PASS is a valid pass of the
// assertion but it re-measures nothing, so a figure in the log from a cached run
// is the figure from whenever it last actually ran. That is stated here rather
// than defended with a cache-defeating flag, which would cost every unrelated
// run for no information.
func TestFullReparseBudget(t *testing.T) {
	path := filepath.Join(strawmanDir, "payments.flow")
	src := readFixture(t, path)

	if _, err := Parse(src); err != nil {
		t.Fatalf("CONTROL FAILED: the budget input does not parse clean: %v", err)
	}
	if len(src) < 1000 {
		t.Fatalf("CONTROL FAILED: the budget input is %d bytes, too small to measure anything", len(src))
	}

	start := time.Now()
	for range reparseIterations {
		file, err := Parse(src)
		if file == nil || err != nil {
			t.Fatalf("a reparse failed midway through the measurement: %v", err)
		}
	}
	elapsed := time.Since(start)
	mean := elapsed / reparseIterations

	t.Logf("full reparse of %s (%d bytes): mean %v over %d iterations, budget %v",
		filepath.Base(path), len(src), mean, reparseIterations, reparseBudget)

	if mean > reparseBudget {
		t.Fatalf("mean full reparse %v exceeds the %v budget", mean, reparseBudget)
	}
}
