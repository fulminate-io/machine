// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pushToken is what the PUSHED fixture's own func body prints once every datum
// the host pushed has arrived. Its transport delivers nothing, so this token is
// evidence about the ingest and nothing else.
const pushToken = "flow-push: delivered=2"

// stagePushedFixture writes the pushed fixture into a temp module, generates it
// through the real driver and returns the directory.
//
// Both legs below stage the SAME module, so the only thing that differs between
// a passing build and a failing one is the pushed value's type.
func stagePushedFixture(t *testing.T, mainBody string) string {
	t.Helper()
	root := machineRoot(t)
	dir := t.TempDir()

	flowSrc := readFixtureFile(t, filepath.Join("testdata", "e2e", "pushed.flow"))
	feed := readFixtureFile(t, filepath.Join("testdata", "e2e", "pushfeed.go.txt"))

	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	write("go.mod", "module flowpush\n\ngo 1.27\n")
	write("go.work", "go 1.27\n\nuse (\n\t.\n\t"+root+"\n)\n")
	if sum, err := os.ReadFile(filepath.Join(root, "go.sum")); err == nil {
		write("go.work.sum", string(sum))
	}
	write("feed.go", feed)
	write("pushed.flow", flowSrc)
	write("main.go", mainBody)

	driver := &Driver{
		Config:      Config{Package: "main", Qualifier: "flowpush"},
		PackagePath: "flowpush",
		Boundary:    map[string]Boundary{},
	}
	if err := driver.Generate(dir, dir); err != nil {
		t.Fatalf("generation failed: %v", err)
	}

	return dir
}

// TestGeneratedIngestIsTypedAtItsSourcePayload proves the exported ingest is
// instantiated at its source's payload type, and it proves it the only way a
// type-safety claim can be proved: THE GO TYPE SYSTEM REFUSES THE WRONG PUSH.
//
// THE EVIDENCE IS A NON-ZERO BUILD, not a test assertion about text. A generated
// field declared `machine.Ingest[any]`, or one erased to a func taking interface{},
// would accept the wrong push and this leg would go green while the property it
// names was false.
//
// IT CARRIES ITS OWN SAME-RUN CONTROL, and without it the leg is worthless: an
// identical module carrying a CORRECT-typed push must BUILD CLEAN in the same
// run. A leg asserting only that the wrong push fails is satisfied by a fixture
// that does not compile for an unrelated reason — a missing import, a bad module
// path, a generation that produced nothing — which is exactly how a type-safety
// claim goes vacuous.
func TestGeneratedIngestIsTypedAtItsSourcePayload(t *testing.T) {
	buildOf := func(mainBody string) (string, error) {
		dir := stagePushedFixture(t, mainBody)
		cmd := exec.Command("go", "build", "-buildvcs=false", "./...")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()

		return string(out), err
	}

	// THE CONTROL, RUN FIRST. If the correct-typed module does not build, the
	// refusal below proves nothing about types.
	out, err := buildOf(pushMainCorrect)
	if err != nil {
		t.Fatalf("CONTROL FAILED: the correct-typed push does not build, so a failing "+
			"wrong-typed build would prove nothing about the ingest's type: %v\n%s", err, out)
	}

	out, err = buildOf(pushMainWrongType)
	if err == nil {
		t.Fatalf("the wrong-typed push BUILT, so the exported ingest is not typed at its "+
			"source's payload type:\n%s", out)
	}
	// The failure must be the TYPE error, not a stray one. Naming the offending
	// argument is what separates "Go rejected the push" from "the module is
	// broken for some other reason".
	if !strings.Contains(out, "cannot use") && !strings.Contains(out, "IncompatibleAssign") {
		t.Errorf("the build failed, but not with a type error naming the pushed value:\n%s", out)
	}
	t.Logf("the wrong-typed push is refused by the compiler: %s", firstLine(out))
}

// TestGeneratedIngestDeliversAValueInProcess proves the exported ingest is WIRED,
// not merely typed.
//
// THE FIXTURE'S TRANSPORT DELIVERS NOTHING. Every datum that reaches the sink
// arrived through the ingest, so the program's own token is evidence about the
// ingest specifically. A struct field of the right type that was never connected
// to the machine satisfies the typing leg above, compiles, runs, and prints
// nothing — this is the leg that catches it.
func TestGeneratedIngestDeliversAValueInProcess(t *testing.T) {
	dir := stagePushedFixture(t, pushMainCorrect)

	generated, err := os.ReadFile(filepath.Join(dir, "pushed.flow.go"))
	if err != nil {
		t.Fatalf("the driver wrote no generated file: %v", err)
	}

	cmd := exec.Command("go", "run", "-buildvcs=false", ".")
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the generated program did not build and run: %v\n%s\n--- generated ---\n%s",
			err, output, generated)
	}

	got := string(output)
	if !strings.Contains(got, pushToken) {
		t.Fatalf("the program ran but no pushed datum reached the sink: its stdout does not "+
			"carry %q.\n%s\n--- generated ---\n%s", pushToken, got, generated)
	}
	t.Logf("the exported ingest delivered into the running graph: %s", strings.TrimSpace(got))
}

// TestTwoSourcesDifferingOnlyInFirstLetterCaseAreRefused proves the ONE collision
// the existing refusal set cannot see.
//
// WHY THE LANDED REFUSALS DO NOT COVER IT. `source a` and `source A` are DISTINCT
// node names, so the duplicate-node-name member never fires and the graph builds
// clean. Both upper-case to the ingest field `A`, so the generated struct would
// carry one field for two sources — and whichever the emitter wrote last would
// silently win, handing the host a field that feeds a flow it did not name.
//
// THE POSITIVE CONTROL IS NOT OPTIONAL. Two sources whose names differ by more
// than that case must still lower CLEAN, or the refusal would be indistinguishable
// from a lowering that rejects any two sources at all.
func TestTwoSourcesDifferingOnlyInFirstLetterCaseAreRefused(t *testing.T) {
	lowerOf := func(t *testing.T, src string, types map[string]string) []Diagnostic {
		t.Helper()
		file, err := parseFlowSource(src)
		if err != nil {
			t.Fatalf("the fixture does not parse, so the lowering was never reached: %v", err)
		}
		programs, diags := buildFile(file)
		if len(diags) != 0 {
			t.Fatalf("the fixture does not BUILD, so this would not be testing the ingest "+
				"refusal: %v", diags)
		}
		if len(programs) != 1 {
			t.Fatalf("the fixture built %d programs, want 1", len(programs))
		}
		// EVERY SOURCE NEEDS ITS PAYLOAD TYPE, or the lowering refuses for that
		// reason instead and the collision below is never reached. The control
		// above is what caught this.
		programs[0].InputTypes = types
		_, lowerDiags := lower(programs[0])

		return lowerDiags
	}

	// THE CONTROL, RUN FIRST: distinct field names lower clean.
	clean := lowerOf(t, "flow two\nsource alpha Poll\nsource beta Poll\n"+
		"sink done Store from alpha, beta\n",
		map[string]string{"alpha": "Order", "beta": "Order", "done": "Order"})
	if len(clean) != 0 {
		t.Fatalf("CONTROL FAILED: two ordinary sources were refused, so the collision "+
			"refusal below proves nothing: %v", clean)
	}

	got := lowerOf(t, "flow two\nsource a Poll\nsource A Poll\n"+
		"sink done Store from a, A\n",
		map[string]string{"a": "Order", "A": "Order", "done": "Order"})
	if len(got) == 0 {
		t.Fatal("two sources differing only in the case of their first letter lowered CLEAN, " +
			"so one ingest field would silently shadow the other")
	}
	// The diagnostic must NAME BOTH SOURCES. One naming only the survivor leaves
	// the author guessing which pair collided.
	message := got[0].Message
	for _, want := range []string{`"a"`, `"A"`} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal does not name source %s: %q", want, message)
		}
	}
	t.Logf("the colliding sources are refused: %s", message)
}

// pushMainCorrect pushes values of the source's own payload type.
const pushMainCorrect = `package main

import (
	"context"
	"fmt"
	"os"
	"time"

	machine "github.com/whitaker-io/machine/v4"
)

func main() {
	m := machine.New("flow-push")
	ingests, err := WirePushed(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wiring:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}

	for i := range deliveredCount {
		if err := ingests.Entry(ctx, Order{ID: fmt.Sprintf("pushed-%d", i)}); err != nil {
			fmt.Fprintln(os.Stderr, "push:", err)
			os.Exit(1)
		}
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		fmt.Fprintln(os.Stderr, "no pushed datum reached the sink within the deadline")
		os.Exit(1)
	}
}
`

// pushMainWrongType is pushMainCorrect with ONE difference: the pushed value is a
// string rather than the source's Order. Everything else — imports, module path,
// call shape, arity — is identical, so a failing build is attributable to the
// type and to nothing else.
const pushMainWrongType = `package main

import (
	"context"
	"fmt"
	"os"
	"time"

	machine "github.com/whitaker-io/machine/v4"
)

func main() {
	m := machine.New("flow-push")
	ingests, err := WirePushed(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wiring:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}

	for i := range deliveredCount {
		if err := ingests.Entry(ctx, fmt.Sprintf("pushed-%d", i)); err != nil {
			fmt.Fprintln(os.Stderr, "push:", err)
			os.Exit(1)
		}
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		fmt.Fprintln(os.Stderr, "no pushed datum reached the sink within the deadline")
		os.Exit(1)
	}
}
`
