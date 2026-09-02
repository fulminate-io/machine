// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"errors"
	"go/types"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
	"github.com/whitaker-io/machine/lang/loader"
)

// driverFlow is a minimal .flow source the driver can run end to end.
const driverFlow = "flow orders\n" +
	"source ingest Poll()\n" +
	"sink done Store from ingest\n"

// driverDir stages a directory of .flow sources BESIDE A LOADABLE GO PACKAGE.
//
// The Go has to be there: a node's payload type is read off its own function's
// signature, so a directory of .flow files with no Go beside them declares nodes
// whose types nothing states. That is the real shape of a flow-bearing directory.
func driverDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	root := machineRoot(t)

	staged := map[string]string{
		"go.mod":     "module probe\n\ngo 1.27\n",
		"go.work":    "go 1.27\n\nuse (\n\t.\n\t" + root + "\n)\n",
		"support.go": driverSupport,
	}
	if sum, err := os.ReadFile(filepath.Join(root, "go.sum")); err == nil {
		staged["go.work.sum"] = string(sum)
	}
	for name, body := range files {
		staged[name] = body
	}
	for name, body := range staged {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	return dir
}

// driverSupport is the Go an author writes beside driverFlow.
const driverSupport = `package probe

import machine "github.com/whitaker-io/machine/v4"

type Order struct{ ID string }

func Poll() machine.EdgeFactory[Order] { return machine.Channel[Order](0) }

func Store(f machine.Frame[Order]) Order { return f.Value() }
`

// countingLoader wraps the REAL loader and counts the calls.
//
// IT IS THE REAL LOAD, not a double: the property under test is how MANY times
// the driver loads, and a double that returned nothing would also have to fake
// every type the run then needs.
func countingLoader(count *int) func(string, []string) (*loader.Packages, error) {
	return func(dir string, patterns []string) (*loader.Packages, error) {
		*count++

		return loader.Load(dir, patterns)
	}
}

// countingDriver builds a driver whose loads are counted.
func countingDriver(count *int) *Driver {
	return &Driver{
		Config:      Config{Package: "probe", Qualifier: "acme"},
		PackagePath: "probe",
		Load:        countingLoader(count),
		Boundary:    map[string]Boundary{},
	}
}

// TestDriverLoadsPackagesOncePerGenerationRun is LEG 1 of the cost criterion.
//
// THE DEFECT CLASS: a driver that calls loader.Load per flow or per file. That
// shape compiles, passes every behavioural test, and produces byte-identical
// generated output — the only thing that changes is that a one-off
// seconds-scale cost becomes a per-unit one.
//
// THE INSTRUMENT IS A COUNTER, NOT A TIMER. A wall-clock budget would be
// satisfiable by a fast machine and would confound loading with everything else.
func TestDriverLoadsPackagesOncePerGenerationRun(t *testing.T) {
	files := map[string]string{}
	for _, name := range []string{"a.flow", "b.flow", "c.flow", "d.flow", "e.flow"} {
		files[name] = driverFlow
	}
	in := driverDir(t, files)
	out := t.TempDir()

	loads := 0
	driver := countingDriver(&loads)
	if err := driver.Generate(in, out); err != nil {
		t.Fatalf("the run failed: %v", err)
	}

	// THE SAME-RUN CONTROL COMES FIRST. A run that generated nothing would also
	// report one load, so the count means nothing until the work is shown.
	written, err := filepath.Glob(filepath.Join(out, "*.flow.go"))
	if err != nil {
		t.Fatalf("reading the output directory: %v", err)
	}
	if len(written) != len(files) {
		t.Fatalf("the run generated %d files from %d sources, so the load count below covers nothing",
			len(written), len(files))
	}

	if loads != 1 {
		t.Errorf("the driver loaded the package set %d times over %d files, want exactly 1; "+
			"loading is a seconds-scale one-off and a per-unit call turns it into a per-unit cost",
			loads, len(files))
	}
}

// TestGenerationConcurrencyStaysUnderItsCeiling is LEG 2.
//
// THE KNOWN POSITIVE IS THE FIXTURE SIZE. The file count must EXCEED the ceiling
// or the bound can never engage and the assertion is vacuous, so the fixture is
// floored and the control fails loudly when it is not.
func TestGenerationConcurrencyStaysUnderItsCeiling(t *testing.T) {
	ceiling := generationConcurrency()
	files := map[string]string{}
	for i := range ceiling * 4 {
		files[fileName(i)] = driverFlow
	}
	if len(files) <= ceiling {
		t.Fatalf("CONTROL FAILED: %d files against a ceiling of %d; the bound can never engage",
			len(files), ceiling)
	}

	in := driverDir(t, files)
	out := t.TempDir()

	var (
		mu   sync.Mutex
		peak int
	)
	loads := 0
	driver := countingDriver(&loads)
	driver.Observe = func(live int) {
		mu.Lock()
		defer mu.Unlock()
		peak = max(peak, live)
	}

	if err := driver.Generate(in, out); err != nil {
		t.Fatalf("the run failed: %v", err)
	}

	// EVERY FILE WAS EMITTED, so the observed peak covers the work it claims to.
	written, err := filepath.Glob(filepath.Join(out, "*.flow.go"))
	if err != nil {
		t.Fatalf("reading the output directory: %v", err)
	}
	if len(written) != len(files) {
		t.Fatalf("the run generated %d files from %d sources", len(written), len(files))
	}

	if peak > ceiling {
		t.Errorf("the driver ran %d concurrent emissions against a ceiling of %d over %d files",
			peak, ceiling, len(files))
	}
	t.Logf("peak %d concurrent emissions against a ceiling of %d (GOMAXPROCS %d) over %d files",
		peak, ceiling, runtime.GOMAXPROCS(0), len(files))
}

// fileName renders a numbered fixture name.
func fileName(i int) string {
	return "flow" + string(rune('a'+i%26)) + itoa(i) + ".flow"
}

// TestFlowcRefusesWhenAPreGenerationCheckReports gates the whole of the analysis
// seam.
//
// THE CHECK'S TYPE IS DECLARED INSIDE THIS PACKAGE AND NAMES NO ANALYSIS CONCEPT,
// so this constrains nothing about the unruled analysis API. What it does pin is
// the two properties that matter: a reporting check means NOTHING is written, and
// the same run with no reporting check DOES generate.
func TestFlowcRefusesWhenAPreGenerationCheckReports(t *testing.T) {
	t.Run("a reporting check writes nothing and fails", func(t *testing.T) {
		in := driverDir(t, map[string]string{"a.flow": driverFlow})
		out := t.TempDir()

		loads := 0
		driver := countingDriver(&loads)
		driver.Checks = []Check{func([]Source) []Diagnostic {
			return []Diagnostic{{Pos: ast.Position{Line: 2, Col: 1}, Message: "a pre-generation check reported"}}
		}}

		err := driver.Generate(in, out)
		if err == nil {
			t.Fatal("a reporting check did not fail the run")
		}
		written, globErr := filepath.Glob(filepath.Join(out, "*.flow.go"))
		if globErr != nil {
			t.Fatalf("reading the output directory: %v", globErr)
		}
		if len(written) != 0 {
			t.Errorf("a refused run wrote %v", written)
		}
		// AND IT REFUSED BEFORE LOADING, which is what "before any file is
		// written" means in cost terms as well as in effect.
		if loads != 0 {
			t.Errorf("the driver loaded the package set %d times on a run it refused", loads)
		}
	})

	// THE SAME RUN WITH NO REPORTING CHECK DOES GENERATE. Without this a driver
	// that refused everything would pass the leg above.
	t.Run("a silent check does not block generation", func(t *testing.T) {
		in := driverDir(t, map[string]string{"a.flow": driverFlow})
		out := t.TempDir()

		loads := 0
		driver := countingDriver(&loads)
		driver.Checks = []Check{func([]Source) []Diagnostic { return nil }}

		if err := driver.Generate(in, out); err != nil {
			t.Fatalf("a silent check blocked generation: %v", err)
		}
		written, globErr := filepath.Glob(filepath.Join(out, "*.flow.go"))
		if globErr != nil || len(written) != 1 {
			t.Errorf("the run generated %v", written)
		}
	})
}

// TestFlowcRemovesOrphanedOutput is LEG 1: regeneration is always WHOLE.
//
// An orphan surviving here would go on compiling forever, declaring a flow whose
// source nobody can find.
func TestFlowcRemovesOrphanedOutput(t *testing.T) {
	in := driverDir(t, map[string]string{"a.flow": driverFlow, "b.flow": driverFlow})
	out := t.TempDir()

	loads := 0
	if err := countingDriver(&loads).Generate(in, out); err != nil {
		t.Fatalf("the first run failed: %v", err)
	}
	// THE CONTROL: the orphan existed before it was expected to be removed, so
	// its absence below is a removal rather than a file that was never written.
	if _, err := os.Stat(filepath.Join(out, "b.flow.go")); err != nil {
		t.Fatalf("the first run did not generate b.flow.go, so removing it proves nothing: %v", err)
	}

	if err := os.Remove(filepath.Join(in, "b.flow")); err != nil {
		t.Fatalf("removing the source: %v", err)
	}
	loads = 0
	if err := countingDriver(&loads).Generate(in, out); err != nil {
		t.Fatalf("the second run failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "b.flow.go")); err == nil {
		t.Error("the generated file for a deleted .flow survived regeneration")
	}
	if _, err := os.Stat(filepath.Join(out, "a.flow.go")); err != nil {
		t.Errorf("regeneration removed a file whose source still exists: %v", err)
	}
}

// TestFlowcRefusalWritesNothing is LEG 2.
//
// A refusal writes NOTHING and reports POSITIONED diagnostics. A run that wrote
// what it could and reported the rest would leave a tree that is neither the old
// program nor the new one.
func TestFlowcRefusalWritesNothing(t *testing.T) {
	in := driverDir(t, map[string]string{
		"broken.flow": "flow broken\nsource in Poll()\nsink out Store from nowhere\n",
	})
	out := t.TempDir()

	loads := 0
	err := countingDriver(&loads).Generate(in, out)
	if err == nil {
		t.Fatal("an unassemblable source generated successfully")
	}

	written, globErr := filepath.Glob(filepath.Join(out, "*.flow.go"))
	if globErr != nil {
		t.Fatalf("reading the output directory: %v", globErr)
	}
	if len(written) != 0 {
		t.Errorf("a refused run wrote %v", written)
	}

	// THE DIAGNOSTICS ARE POSITIONED, so an author is sent to the line they wrote.
	var assembly *Error
	if !errors.As(err, &assembly) {
		t.Fatalf("the refusal is %T, want an *Error carrying diagnostics", err)
	}
	if len(assembly.Diagnostics) == 0 {
		t.Fatal("the refusal carries no diagnostics")
	}
	for _, d := range assembly.Diagnostics {
		if d.Pos.Line == 0 {
			t.Errorf("a refusal diagnostic is unpositioned: %q", d.Message)
		}
	}
}

// TestFlowcLeavesOutputUntouchedOnMidEmissionFailure is LEG 3: THE CRASH WINDOW.
//
// This is the leg an implementer under pressure drops, because the other two need
// only ordinary fixtures while this one needs a run that fails AFTER some work
// succeeded. A directory half-regenerated by a failed run compiles without
// complaint and is neither program.
func TestFlowcLeavesOutputUntouchedOnMidEmissionFailure(t *testing.T) {
	in := driverDir(t, map[string]string{"a.flow": driverFlow})
	out := t.TempDir()

	loads := 0
	if err := countingDriver(&loads).Generate(in, out); err != nil {
		t.Fatalf("the first run failed: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(out, "a.flow.go"))
	if err != nil {
		t.Fatalf("reading the first run's output: %v", err)
	}

	// THE SECOND RUN PARTLY SUCCEEDS: a.flow still assembles, and the new source
	// does not. Everything that CAN emit does, and then the run fails.
	if err := os.WriteFile(filepath.Join(in, "broken.flow"),
		[]byte("flow broken\nsource in Poll()\nsink out Store from nowhere\n"), 0o600); err != nil {
		t.Fatalf("writing the broken source: %v", err)
	}
	loads = 0
	if err := countingDriver(&loads).Generate(in, out); err == nil {
		t.Fatal("a run over an unassemblable source succeeded")
	}

	after, err := os.ReadFile(filepath.Join(out, "a.flow.go"))
	if err != nil {
		t.Fatalf("the failed run removed a file it should not have touched: %v", err)
	}
	if string(before) != string(after) {
		t.Error("the failed run rewrote an existing generated file")
	}
	if _, err := os.Stat(filepath.Join(out, "broken.flow.go")); err == nil {
		t.Error("the failed run wrote output for the source that failed")
	}

	// AND IT LEFT NO STAGING DIRECTORY BEHIND, which is the mechanism that makes
	// the property above true rather than a coincidence.
	leftovers, err := filepath.Glob(filepath.Join(out, ".flowc-staging-*"))
	if err != nil {
		t.Fatalf("reading the output directory: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("the failed run left staging directories behind: %v", leftovers)
	}
}

// recordingInference records which flows the driver asks about.
//
// IT IS A DEPENDENCY DOUBLE, NOT A DOUBLE FOR THE CODE UNDER TEST. The subject is
// the DRIVER'S RULE about which authority to ask; production passes the real
// analysis table behind the same locked shape.
type recordingInference struct {
	asked map[string]bool
	types map[string]types.Type
}

func (r *recordingInference) Name(flow, name string) (types.Type, bool) {
	if r.asked == nil {
		r.asked = map[string]bool{}
	}
	r.asked[flow] = true
	typ, ok := r.types[name]

	return typ, ok
}

// TestDriverTypesSignatureLessExportedFlowsFromInference pins the three-way rule.
//
// THE DEFECT CLASS: a driver that asks the WRONG AUTHORITY for a flow's boundary
// types. Both wrong answers compile, produce a types.Type and generate code, so
// nothing downstream can catch it.
func TestDriverTypesSignatureLessExportedFlowsFromInference(t *testing.T) {
	inference := &recordingInference{types: map[string]types.Type{"out": types.Typ[types.String]}}

	// LEG 1 AND THE SAME-RUN CONTROL. A signature-less EXPORTED flow takes its
	// boundary types from inference, and this hit is what proves the recorder was
	// reached at all — without it the two negative legs below would be satisfied
	// by a recorder that could never fire.
	exported := programFor(t, "flow Orders\nsource in Poll()\ntransform out Bill from in\nsink done Store from out\n")
	boundaryTypes(exported, Facts{Inferred: inference})
	if !inference.asked["Orders"] {
		t.Fatal("the driver did not consult inference for a signature-less EXPORTED flow, " +
			"so the negative legs below prove nothing")
	}

	// LEG 2. A flow declaring a SIGNATURE is typed from its own spellings, and
	// inference is not consulted: the author wrote the types down, and reading
	// them from anywhere else would let a derived answer override a stated one.
	signatured := programFor(t,
		"flow Screening (Order) -> ok OkResult\nbranch check Clean from in -> ok, bad\n")
	got := boundaryTypes(signatured, Facts{Inferred: inference})
	if inference.asked["Screening"] {
		t.Error("the driver consulted inference for a flow that DECLARES its types")
	}
	if got["ok"] != "OkResult" {
		t.Errorf("the signatured flow's output typed as %q, want its declared spelling OkResult", got["ok"])
	}

	// LEG 3. A signature-less UNEXPORTED flow is module-private and is never
	// typed across a boundary.
	unexported := programFor(t, "flow orders\nsource in Poll()\nsink done Store from in\n")
	boundaryTypes(unexported, Facts{Inferred: inference})
	if inference.asked["orders"] {
		t.Error("the driver consulted inference for an UNEXPORTED flow, which has no cross-module boundary")
	}
}

// programFor builds the first flow of a source.
func programFor(t *testing.T, src string) *Program {
	t.Helper()
	file, err := ast.Parse([]byte(src))
	if err != nil {
		t.Fatalf("the fixture must parse clean: %v", err)
	}
	programs, _ := buildFile(file)
	if len(programs) == 0 {
		t.Fatal("the fixture declares no flow")
	}

	return programs[0]
}

// TestRenderReportsColumnsAsBytes pins the rendering contract.
func TestRenderReportsColumnsAsBytes(t *testing.T) {
	got := Render("pipeline.flow", Diagnostic{Pos: ast.Position{Line: 7, Col: 13}, Message: "a refusal"})
	if want := "pipeline.flow:7:13: a refusal"; got != want {
		t.Errorf("Render produced %q, want %q", got, want)
	}
	if strings.Count(got, ":") != 3 {
		t.Errorf("the rendered form %q is not <file>:<line>:<col>: <message>", got)
	}
}
