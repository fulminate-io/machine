// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
)

// strawmanDir is the sibling module's canonical example set. It is READ as
// files, not imported, so this introduces no module dependency.
const strawmanDir = "../ast/testdata/strawman"

// strawmanExpectation is what one canonical example must produce.
//
// THE NUMBERS LIVE HERE AND NOWHERE ELSE. Pinning them in the criterion as well
// would mean re-deriving them in two places, and the two would drift.
type strawmanExpectation struct {
	// fixture is the base name under strawmanDir.
	fixture string
	// refusals are the diagnostics the assembler must produce, one entry per
	// diagnostic, each naming the source LINE and a fragment of the message.
	refusals []strawmanRefusal
}

type strawmanRefusal struct {
	line     int
	fragment string
}

// strawmanExpectations is the locked table, RE-DERIVED against the fixtures as
// they stand rather than carried from any earlier revision.
//
// WHAT CHANGED SINCE THE STEP WAS WRITTEN, and why the table says so. Two of the
// three examples used to be expected to produce a `checkpoint` diagnostic: first
// a refusal, then a not-yet-lowered held diagnostic. BOTH ARE RETIRED. The
// checkpoint clause is LOWERED now — it takes a codec operand and the generator
// emits WithCheckpoint — so payments and enrichment graph clean. An expectation
// carried forward from either earlier state would fail here, which is the point
// of re-deriving rather than copying.
//
// toy.flow's own expectation was also re-derived. An earlier revision recorded
// its `loop retry` label as having no sender; a later commit gave it one, so that
// refusal is gone and the single remaining one is a dangling output.
var strawmanExpectations = []strawmanExpectation{
	{
		fixture: "toy",
		refusals: []strawmanRefusal{
			// The transform at line 21 produces `archive`, and no later statement
			// consumes it. Every other output in the file IS consumed, which is
			// what makes this one refusal rather than a general failure to
			// resolve.
			{line: 21, fragment: "which nothing in this flow consumes"},
		},
	},
	{fixture: "payments", refusals: nil},
	{fixture: "enrichment", refusals: nil},
}

// TestStrawmanGraphExpectations proves each canonical example produces EXACTLY
// the diagnostics the table names, at the lines it names.
//
// IT IS AN EQUALITY, NOT A CONTAINMENT. A test asserting only that the named
// refusals appear would pass a builder that also produced ten spurious ones, and
// a builder producing NONE would pass the two entries whose expected set is
// empty. So the count is asserted first and every produced diagnostic is printed
// on a mismatch.
//
// THE STRAWMEN ARE ANOTHER LANE'S FIXTURES and have already been edited mid-plan
// once. When one changes under this table the failure names the fixture, the
// expected set and the set actually produced, so the next author corrects the
// table from evidence instead of guessing.
func TestStrawmanGraphExpectations(t *testing.T) {
	if len(strawmanExpectations) != 3 {
		t.Fatalf("the table names %d strawmen, want the three canonical ones", len(strawmanExpectations))
	}

	for _, want := range strawmanExpectations {
		t.Run(want.fixture, func(t *testing.T) {
			diags := strawmanDiagnostics(t, want.fixture)

			if len(diags) != len(want.refusals) {
				t.Fatalf("%s.flow produced %d diagnostics, want %d.\nproduced:\n%s",
					want.fixture, len(diags), len(want.refusals), strings.Join(messagesOf(diags), "\n"))
			}
			for i, expected := range want.refusals {
				got := diags[i]
				if got.Pos.Line != expected.line {
					t.Errorf("diagnostic %d is at line %d, want line %d: %s", i, got.Pos.Line, expected.line, got.Message)
				}
				if !strings.Contains(got.Message, expected.fragment) {
					t.Errorf("diagnostic %d reads %q, want a message containing %q", i, got.Message, expected.fragment)
				}
			}
		})
	}
}

// strawmanDiagnostics parses and builds one canonical example.
//
// It requires a CLEAN PARSE: these are the language's showcase files, and one
// that stopped parsing would be a fixture change this test must report rather
// than absorb.
func strawmanDiagnostics(t *testing.T, fixture string) []Diagnostic {
	t.Helper()
	path := filepath.Join(strawmanDir, fixture+".flow")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if len(src) == 0 {
		t.Fatalf("CONTROL FAILED: %s is empty, so any expectation over it is vacuous", path)
	}
	file, parseErr := ast.Parse(src)
	if parseErr != nil {
		t.Fatalf("%s no longer parses, so this table cannot be evaluated: %v", path, parseErr)
	}
	_, diags := buildFile(file)

	return diags
}

// TestStrawmanTableWouldCatchADroppedRefusal is the anti-vacuity guard for the
// two entries whose expected set is EMPTY.
//
// Those two entries pass over a builder that produces no diagnostics at all,
// which is exactly what dropping the refusal checks would do. This drives the
// same fixtures through a deliberately broken shape and requires the table's
// non-empty entry to notice — so at least one entry in the table is provably
// sensitive to the checks being present.
func TestStrawmanTableWouldCatchADroppedRefusal(t *testing.T) {
	diags := strawmanDiagnostics(t, "toy")
	if len(diags) == 0 {
		t.Fatal("toy.flow produced no diagnostics at all; the refusal checks are not running, " +
			"and the two empty table entries cannot tell the difference")
	}

	// And the empty entries are empty because the flows are clean, not because
	// nothing ran: the same builder over the same path produced a diagnostic above.
	for _, fixture := range []string{"payments", "enrichment"} {
		if got := strawmanDiagnostics(t, fixture); len(got) != 0 {
			t.Errorf("%s.flow now produces %d diagnostics:\n%s",
				fixture, len(got), strings.Join(messagesOf(got), "\n"))
		}
	}
}
