// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"
)

// generationBudget is the ceiling for one whole parse-to-emit pass.
//
// IT IS THE SAME BUDGET lang/ast uses for PARSE ALONE, and the headroom is the
// point rather than an accident. Generation is strictly more work than parsing
// but the same order — three linear passes over a few dozen nodes plus one
// format.Source call — and a locally measured parse of a comparable fixture sits
// in the ten-microsecond band. Two orders of magnitude of headroom is what leaves
// room for a CI runner several times slower than a developer machine, and it is
// deliberately far enough above the measurement that ordinary run-to-run spread
// cannot flap it.
//
// IF A MEASURED MEAN EVER LANDS WITHIN A FACTOR OF TWO OF THIS, the answer is to
// report it, not to widen the budget: a generator that close to a millisecond is
// evidence of a cost this design did not intend.
const generationBudget = time.Millisecond

// budgetIterations is the sample count within one reading.
const budgetIterations = 100

// budgetReadings is how many readings are taken, of which the BEST is judged.
//
// A SINGLE READING OF THIS MEASUREMENT IS AN INSTRUMENT ARTIFACT rather than a
// measurement, and that was observed rather than assumed: three consecutive runs
// of an unchanged fixture on an unchanged tree read 380µs, 1.31ms and 316µs — a
// 4x spread, with the middle one over budget purely because the machine was busy.
//
// THE MINIMUM IS JUDGED, NOT THE MEAN, and that does not weaken the gate. A
// perturbed batch can only ever read SLOWER, so the minimum is the least-polluted
// sample; and a genuinely slower generator raises the minimum exactly as it
// raises every other reading, so nothing real can hide behind it. Judging a
// single batch instead would make this gate fail on unrelated machine load, which
// is a flaky gate rather than a strict one.

const budgetReadings = 5

// TestGenerationBudget measures the full parse, graph, lower and emit path over
// a fixture that exists for no other purpose.
//
// IT USED TO MEASURE WHICHEVER GOLDEN WAS LARGEST, and that coupled a
// performance gate to an editorial decision nobody could see from either end.
// The largest golden is also the canonical one a reader consults, so it is where
// explanatory prose accumulates — and prose is source bytes. Measured across one
// branch: two commits that added a comment and a note paragraph to that fixture,
// for entirely documentary reasons, grew it from 839 to 1510 bytes and this
// measurement from 303.820µs to 384.525µs, spending 27% of the margin to the
// half-budget tripwire. Nothing was wrong with either the prose or the gate; the
// SELECTION RULE was wrong. A superlative over a mutable set measures the set as
// much as the code, and it does so monotonically, because fixtures only grow.
//
// SO THE INPUT IS ITS OWN FIXTURE AND ITS BYTES ARE PINNED. testdata/budget
// belongs to this test, is named in no other one, and carries no prose beyond
// the minimum: a comment added to it fails the hash below rather than silently
// moving the number. Changing it is then a deliberate act with a new baseline,
// which is the only honest way to move a performance measurement's input.
//
// THE NEW INPUT COSTS MORE THAN THE OLD ONE, and that is a trade rather than a
// regression. It carries seven statements where the golden it replaced carried
// three, so it generates 6424 bytes of Go against 4709 — and format.Source over
// the GENERATED file is ~92% of this path, so cost tracks that number rather
// than the source size. Measured by ALTERNATING the two inputs on one host in
// one window, which is what cancels load drift: the old input read 292, 418, 513
// and 389µs while this one read 455, 556, 481 and 487µs. Roughly 20% dearer,
// bought with four more statements' worth of coverage.
//
// THAT SAME MEASUREMENT SHOWS THE GATE ALREADY FLAPPED, independently of any of
// this: the OLD input failed its own tripwire at 513µs in round three. On a host
// under a load average near 300 the best-of-five statistic is not stable to
// within the margin either input has. Nine readings were tried and did not help,
// so the sampling was left at five rather than shipping a lever that measured
// nothing. This is reported, not compensated for, and the budget is untouched.
//
// THE FIXTURE IS STILL A REAL, GENERATED ONE rather than a canonical strawman: a
// strawman is refused before emission, so a budget over one would be measuring a
// refusal. It carries the constructs a generator actually meets — import, const,
// param, state, var, flow- and node-level error handlers, an `over` transport
// clause, verbatim funcs, source, transform, branch, a three-arm switch, two
// sinks and a drop.
//
// IT CARRIES NO TEE, and that is a harness limit rather than a choice: a tee
// needs a duplicator derived from its payload's STRUCTURE, and this harness
// supplies type spellings rather than structure, so a fixture with one is
// refused at emission. The e2e fixtures exercise the tee by running.
//
// A CACHED PASS RE-MEASURES NOTHING. Go's test cache will report this green
// without running it whenever its inputs are unchanged, which is correct and
// wanted; a deliberate re-measurement defeats the cache at the command line
// rather than here, because a cache-defeating flag stored in the gate would tax
// every unrelated run for no information.
func TestGenerationBudget(t *testing.T) {
	subject := budgetCase(t)

	// CONTROL: it must generate CLEAN, or the timing would be of a refusal path.
	generateCase(t, subject)

	best, worst := time.Duration(1<<62), time.Duration(0)
	for range budgetReadings {
		start := time.Now()
		for range budgetIterations {
			generateCase(t, subject)
		}
		mean := time.Since(start) / budgetIterations
		best = min(best, mean)
		worst = max(worst, mean)
	}

	// THE FIGURES ARE LOGGED WHETHER THIS PASSES OR FAILS, so they live in the
	// run's output rather than only in whatever prose accompanied the change. The
	// SPREAD is logged beside the best reading deliberately: a figure quoted to
	// three significant digits from one sample is quoting noise, and a reader
	// comparing runs needs to see how wide the noise is.
	t.Logf("parse-to-emit over %s (%d bytes): best mean %v, worst %v, over %d readings of %d iterations; budget %v",
		subject.name, len(subject.source), best, worst, budgetReadings, budgetIterations, generationBudget)

	if best > generationBudget {
		t.Errorf("the generation path means %v at best, over the %v budget", best, generationBudget)
	}
	if best > generationBudget/2 {
		t.Errorf("the generation path means %v at best, within a factor of two of the %v budget; "+
			"report this rather than widening the budget", best, generationBudget)
	}
}

// TestWhereTheGenerationCostIs records the breakdown, because the budget's own
// premise turned out to be wrong about it.
//
// THE PLAN REASONED that generation is the same order as parsing — three linear
// passes over a few dozen nodes — and set the budget with two orders of magnitude
// of headroom on that basis. MEASURED, the passes are indeed cheap and the
// premise is still wrong: gofmt is ~92% of the whole path.
//
//	parse            12.5µs
//	graph + lower     5.8µs
//	format.Source   211µs      839 source bytes -> 4709 generated bytes
//
// THOSE THREE TIMINGS WERE TAKEN ON THE OLD INPUT, the 839-byte golden this
// measurement used before it got a fixture of its own, and they are left as
// written rather than re-quoted against the new one: a breakdown nobody re-ran
// should not be dressed up in fresh numbers. The RATIO is the durable claim.
// What this test re-measures every run is the byte expansion the ratio rests on,
// and it logs it: the pinned fixture generates several times its own size in Go.
//
// So the headroom is one order of magnitude rather than two, and it belongs to
// the standard library's formatter rather than to anything this package does.
func TestWhereTheGenerationCostIs(t *testing.T) {
	subject := budgetCase(t)
	out := generateCase(t, subject)

	// The generated file is several times the source it came from, which is the
	// reason formatting dominates: the formatter parses and prints THAT, not the
	// .flow.
	if len(out.Source) <= len(subject.source) {
		t.Errorf("the generated file is %d bytes from %d source bytes; the cost model assumes it is larger",
			len(out.Source), len(subject.source))
	}
	t.Logf("%s: %d source bytes generate %d bytes of Go", subject.name, len(subject.source), len(out.Source))
}

// budgetDir holds the one fixture the budget measures, and nothing else reads it.
const budgetDir = "testdata/budget"

// budgetFixtureSHA256 PINS THE MEASUREMENT'S INPUT.
//
// A performance number is only comparable across runs if the thing measured is
// the same thing, so the input is hashed rather than trusted. Editing
// testdata/budget/budget.flow fails here with both hashes printed, which is the
// point: the edit is legal, but it invalidates every recorded baseline and has to
// be an act someone took deliberately and re-based, not a comment that drifted in.
//
// TO CHANGE IT: edit the fixture, run this test, paste the "got" hash here, and
// record the new baseline mean beside the old one wherever the old one is cited.
const budgetFixtureSHA256 = "b0cca3d4dc8e028ae809c14f3468704feb62655cf0369db7ae46fb0e68664856"

// budgetFixtureFloor is the size below which this measurement stops meaning
// anything: a budget over a two-line fixture measures function call overhead.
const budgetFixtureFloor = 256

// budgetCase loads the budget's own fixture as a case the golden machinery can
// generate, WITHOUT it being a golden. It is deliberately not discovered by
// goldenCases: a golden owes a checked-in .flow.go expectation and takes part in
// the drift, determinism and compile sweeps, and this fixture owes none of that.
// It owes exactly one thing, which is to stay byte-identical.
func budgetCase(t *testing.T) goldenCase {
	t.Helper()

	source := readGoldenFile(t, filepath.Join(budgetDir, "budget.flow"))

	sum := sha256.Sum256([]byte(source))
	if got := hex.EncodeToString(sum[:]); got != budgetFixtureSHA256 {
		t.Fatalf("the budget fixture changed, so every recorded baseline for this measurement is stale.\n"+
			"  got  %s (%d bytes)\n want  %s\n"+
			"If the change is deliberate, paste the got hash into budgetFixtureSHA256 and record the new baseline mean beside the old one.",
			got, len(source), budgetFixtureSHA256)
	}

	// CONTROL: the pin above proves the bytes did not move; this proves they were
	// worth pinning. A hash over an empty or trivial file is equally stable and
	// equally meaningless.
	if len(source) < budgetFixtureFloor {
		t.Fatalf("CONTROL FAILED: the budget fixture is %d bytes, under the %d-byte floor; a measurement over that is function call overhead",
			len(source), budgetFixtureFloor)
	}

	return goldenCase{
		name:     "budget",
		dir:      budgetDir,
		source:   source,
		types:    readTypes(t, filepath.Join(budgetDir, "types.txt")),
		boundary: map[string]Boundary{},
	}
}
