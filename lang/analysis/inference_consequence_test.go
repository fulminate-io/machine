// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"bytes"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/whitaker-io/machine/lang/loader"
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

// assertExportedAndSignatureLess pins the two properties that make a fixture the
// SUBJECT of the retyping consequence rather than a counterexample to it.
//
// A signature-bearing flow is typed by its HEADER, so a dependency edit would not
// reach it and observing one would prove the opposite of the point. An unexported
// flow is nobody's dependency. Both are asserted rather than assumed, because a
// later edit to the fixture could quietly remove either.
func assertExportedAndSignatureLess(t *testing.T, src Source) {
	t.Helper()

	flow := firstFlow(t, src)
	if name := flow.Name.Name; name == "" || strings.ToUpper(name[:1]) != name[:1] {
		t.Fatalf("the subject flow %q is not exported, so it is nobody's dependency", name)
	}
	if flow.Signature != nil {
		t.Fatalf("the subject flow %s declares a signature, so its header would type it and this "+
			"observation would prove the opposite of the consequence", flow.Name.Name)
	}
}

// copyTree copies a directory tree into dst, so a test edits a COPY and never the
// checked-in fixture module.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()

	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(target, body, 0o644)
	})
	if err != nil {
		t.Fatalf("copying the fixture module failed: %v", err)
	}
}

// TestADependencyEditRetypesASignatureLessConsumer OBSERVES the ruling's accepted
// consequence rather than describing it.
//
// A signature-less exported flow takes its types from its BODY, so an edit inside
// a dependency it never mentions retypes it at the consumer's next generation.
// That is the accepted cost of not declaring a header, and the signature header is
// the author's opt-in contract against exactly this. If it did not hold — if the
// inference cached or otherwise pinned a type so a dependency edit never reached
// the consumer — the header's whole reason for existing would be unfounded, and no
// other gate in this plan looks at it.
//
// EVERY CONTROL HERE IS LOAD-BEARING. The subject must be exported and
// signature-less; the edit must be proven to have matched something, or the
// re-measurement proves nothing; and the .flow bytes must be IDENTICAL across both
// runs, which is what makes the retyping attributable to the DEPENDENCY rather
// than to the flow.
func TestADependencyEditRetypesASignatureLessConsumer(t *testing.T) {
	path := filepath.Join(inferenceDir, "Screening.flow")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read the subject flow: %v", err)
	}

	src := loadSource(t, path)
	assertExportedAndSignatureLess(t, src)

	// THE CHECKED-IN FIXTURE IS NEVER MUTATED: the edit lands in a copy.
	copied := t.TempDir()
	copyTree(t, inferenceSubject, copied)

	original, oerr := loader.Load(copied, []string{"./..."})
	if oerr != nil {
		t.Fatalf("the copied fixture module did not load: %v", oerr)
	}
	first, _, ferr := BuildInferredTypes([]Source{src}, original)
	if ferr != nil {
		t.Fatalf("the first inference failed: %v", ferr)
	}
	was, known := first.Name("Screening", "scored")
	if !known {
		t.Fatal("the subject name carries no type before the edit, so a change cannot be observed")
	}

	// THE DEPENDENCY-INTERNAL EDIT: Score now hands back a Receipt. The .flow file
	// says nothing about either type.
	pkgFile := filepath.Join(copied, "orderpkg", "v2", "orders.go")
	body, rerr := os.ReadFile(pkgFile)
	if rerr != nil {
		t.Fatalf("cannot read the copied dependency: %v", rerr)
	}
	const oldSig = "func Score(o Order) (Scored, error) { return Scored{Order: o}, nil }"
	const newSig = "func Score(o Order) (Receipt, error) { return Receipt{ID: o.ID}, nil }"
	edited := strings.Replace(string(body), oldSig, newSig, 1)
	if edited == string(body) {
		t.Fatal("CONTROL FAILED: the dependency edit matched nothing, so re-measuring proves nothing")
	}
	if werr := os.WriteFile(pkgFile, []byte(edited), 0o644); werr != nil {
		t.Fatalf("cannot write the edited dependency: %v", werr)
	}

	retyped, rlerr := loader.Load(copied, []string{"./..."})
	if rlerr != nil {
		t.Fatalf("the edited fixture module did not load: %v", rlerr)
	}
	second, _, serr := BuildInferredTypes([]Source{src}, retyped)
	if serr != nil {
		t.Fatalf("the second inference failed: %v", serr)
	}
	now, stillKnown := second.Name("Screening", "scored")
	if !stillKnown {
		t.Fatal("the subject name lost its type entirely after the edit")
	}

	// THE ATTRIBUTION CONTROL: the flow source did not move.
	after, aerr := os.ReadFile(path)
	if aerr != nil {
		t.Fatalf("cannot re-read the subject flow: %v", aerr)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the .flow source changed between the two runs, so the retyping is not attributable to the dependency")
	}

	if types.Identical(was, now) {
		t.Fatalf("a dependency edit did NOT retype the signature-less consumer; it is still %s",
			types.TypeString(was, nil))
	}
	t.Logf("a dependency-internal edit retyped the signature-less consumer: %s -> %s",
		types.TypeString(was, nil), types.TypeString(now, nil))
}
