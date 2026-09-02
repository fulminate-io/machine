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

// e2eToken is what the FIXTURE'S OWN func body prints when the flow has driven
// its data through.
//
// The harness never prints it. Asserting on the running program's stdout is what
// makes this a proof that data moved rather than that a file was written.
const e2eToken = "flow-e2e: processed=3"

// THE SUBJECT OF EVERY END-TO-END TEST IN THIS FILE IS THE SHIPPED BINARY.
//
// They used to construct a Driver in process with PackagePath and Boundary
// already set, which proved the generate-build-run path FOR THE LIBRARY and
// never for the command — and the two differed by exactly the wiring a user
// depends on. The shipped flowc registers four flags and set neither field, so
// every flow with a typed source failed with "needs its payload type" while
// every one of these tests stayed green. Driving the binary with nothing but the
// flags a user already had is what closes that gap; the fixtures, the host mains
// and the token assertions are unchanged.

// flowcBinary builds the shipped command into a scratch directory and answers
// its path.
//
// It is built per test rather than once for the package: the Go build cache makes
// every build after the first a link, and a per-test binary keeps the cleanup
// t.TempDir already does.
func flowcBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "flowc")
	cmd := exec.Command("go", "build", "-o", path, "./cmd/flowc")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the shipped flowc binary does not build: %v\n%s", err, out)
	}

	return path
}

// generateWithFlowc runs the shipped binary in dir with the four flags a user
// has, and NOTHING else.
//
// No -pkgpath and no injected facts: the package path is derived from the output
// directory's enclosing go.mod, and every analysis fact comes from the binary's
// own pre-generation gate. That is the whole point of driving the command.
func generateWithFlowc(t *testing.T, dir, qualifier string) {
	t.Helper()
	cmd := exec.Command(flowcBinary(t), "-in", ".", "-out", ".", "-package", "main", "-qualifier", qualifier)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the shipped flowc refused the fixture (%v):\n%s", err, out)
	}
}

// stageModule writes a scratch module wired to the local machine checkout.
func stageModule(t *testing.T, dir, module string, files map[string]string) {
	t.Helper()
	root := machineRoot(t)
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	write("go.mod", "module "+module+"\n\ngo 1.27\n")
	write("go.work", "go 1.27\n\nuse (\n\t.\n\t"+root+"\n)\n")
	if sum, err := os.ReadFile(filepath.Join(root, "go.sum")); err == nil {
		write("go.work.sum", string(sum))
	}
	for name, body := range files {
		write(name, body)
	}
}

// TestEndToEndPipelineRunsOnTheRuntime is the ticket's central claim, proved by
// RUNNING rather than by inspection.
//
// It generates from a .flow source, builds the result against the local machine
// checkout, executes the binary, and requires the program's own stdout to carry
// the locked token. It reds if the program fails to build, fails to start, or
// drives no data through — three different failures the same assertion catches,
// because none of them can produce the token.
//
// WHY A NEW FIXTURE RATHER THAN A CANONICAL STRAWMAN: none of the three strawmen
// can compile or run. Their acme imports do not resolve, and their transport
// spellings do not match the repo's own edge API. They are syntax fixtures and
// are gated as such elsewhere.
//
// EXPECT THIS TO BE THE SLOWEST TEST IN THE MODULE: it runs a full build and then
// the binary.
func TestEndToEndPipelineRunsOnTheRuntime(t *testing.T) {
	dir := t.TempDir()

	stageModule(t, dir, "flowe2e", map[string]string{
		"feed.go":       readFixtureFile(t, filepath.Join("testdata", "e2e", "feed.go.txt")),
		"pipeline.flow": readFixtureFile(t, filepath.Join("testdata", "e2e", "pipeline.flow")),
		"main.go":       e2eMain,
	})

	// GENERATE THROUGH THE SHIPPED BINARY, into the same directory, with the four
	// flags a user has.
	generateWithFlowc(t, dir, "flowe2e")

	generated, err := os.ReadFile(filepath.Join(dir, "pipeline.flow.go"))
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
	if !strings.Contains(got, e2eToken) {
		t.Fatalf("the program ran but drove no data through: its stdout does not carry %q.\n%s\n"+
			"--- generated ---\n%s", e2eToken, got, generated)
	}
	t.Logf("the generated flow built and ran against the runtime: %s", strings.TrimSpace(got))
}

// loopToken is what the LOOPING fixture's own func body prints once the datum has
// made its passes and left the loop.
//
// IT CARRIES THE ATTEMPT COUNT rather than a bare marker. A loop that ran once,
// or printed on the wrong pass, produces a different number and fails here — so
// the assertion proves the loop's arithmetic, not merely that something reached
// the sink.
const loopToken = "flow-loop: attempts=3"

// TestEndToEndTerminatingLoopAndSendRunOnTheRuntime is the second end-to-end leg,
// and it exists because the first one drives NEITHER of two constructs the step's
// list names.
//
// WHAT THE FIRST FIXTURE LEAVES UNPROVEN. TestEndToEndPipelineRunsOnTheRuntime
// runs a source, transform, branch, tee and two sinks. It contains no loop and no
// send, so Flow.Send and the loop label's re-entry are covered at that point only
// by lowering tests and by a golden file that is compiled but never executed.
// Compiling proves the emitted text is Go; it does not prove a datum survives a
// re-entry.
//
// WHY IT IS A SEPARATE FIXTURE RATHER THAN A CLAUSE ON THE FIRST. A running loop
// needs a loop that TERMINATES, which needs a payload counter, a predicate
// reading it and an outlet that leaves — a different data shape from the first
// fixture's. Bolting it on would have made one fixture prove two things less
// clearly than two fixtures each prove one.
//
// EXPECT THIS TO BE SLOW for the same reason the first one is: it runs a full
// build and then the binary.
func TestEndToEndTerminatingLoopAndSendRunOnTheRuntime(t *testing.T) {
	dir := t.TempDir()

	stageModule(t, dir, "flowloop", map[string]string{
		"feed.go":     readFixtureFile(t, filepath.Join("testdata", "e2e", "loopfeed.go.txt")),
		"looped.flow": readFixtureFile(t, filepath.Join("testdata", "e2e", "looped.flow")),
		"main.go":     loopMain,
	})

	generateWithFlowc(t, dir, "flowloop")

	generated, err := os.ReadFile(filepath.Join(dir, "looped.flow.go"))
	if err != nil {
		t.Fatalf("the driver wrote no generated file: %v", err)
	}

	// THE SEND MUST BE IN THE EMITTED WIRE. This is not the run assertion — the
	// stdout token below is — but it names WHICH construct the run exercised, so an
	// edit that quietly stopped emitting the re-entry fails here with a message
	// about the send rather than only as a timeout.
	if !strings.Contains(string(generated), ".Send(") {
		t.Fatalf("the generated wire contains no Send, so a passing run would not "+
			"exercise the loop's re-entry:\n%s", generated)
	}

	cmd := exec.Command("go", "run", "-buildvcs=false", ".")
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the generated program did not build and run: %v\n%s\n--- generated ---\n%s",
			err, output, generated)
	}

	got := string(output)
	if !strings.Contains(got, loopToken) {
		t.Fatalf("the program ran but its loop did not terminate at the expected pass: "+
			"its stdout does not carry %q.\n%s\n--- generated ---\n%s",
			loopToken, got, generated)
	}
	t.Logf("the generated flow looped, re-entered through Send and left the loop: %s",
		strings.TrimSpace(got))
}

// embedToken is what the EMBEDDED-SUBFLOW fixture's sink prints once a datum has
// travelled through the subflow's body.
const embedToken = "flow-embed: processed=3"

// TestEndToEndEmbeddedSubflowRunsOnTheRuntime is the third end-to-end leg, and it
// exists because the other two drive no `use` at all.
//
// WHAT THE OTHERS LEAVE UNPROVEN. Until this fixture the assembler's end-to-end
// programs carried no `use`, and its goldens compiled none either. That is the
// whole of why a signature-carrying subflow could emit a wiring function whose
// body read an unbound `in` — `undefined: in` — and reach a shipped command: no
// gate in this module ever compiled the emission path for one, let alone ran it.
//
// THE TOKEN IS REACHABLE ONLY THROUGH THE EMBEDDED BODY. Every datum passes the
// subflow's branch on its way to the sink, so a `use` that generated cleanly and
// routed nothing prints nothing and this reds. The count is three because the
// transport delivers two and the host pushes the third through the exported
// ingest, so the leg also fails if the ingest is typed but never wired.
//
// AND IT NEEDS NO BOUNDARY FROM THE HARNESS. The subflow's bindable outputs come
// from the binary's own pre-generation gate; supplying them here would put the
// test back to proving the library.
func TestEndToEndEmbeddedSubflowRunsOnTheRuntime(t *testing.T) {
	dir := t.TempDir()

	stageModule(t, dir, "flowembed", map[string]string{
		"feed.go":       readFixtureFile(t, filepath.Join("testdata", "e2e", "embedfeed.go.txt")),
		"embedded.flow": readFixtureFile(t, filepath.Join("testdata", "e2e", "embedded.flow")),
		"main.go":       embedMain,
	})

	generateWithFlowc(t, dir, "flowembed")

	generated, err := os.ReadFile(filepath.Join(dir, "embedded.flow.go"))
	if err != nil {
		t.Fatalf("the binary wrote no generated file: %v", err)
	}

	// THE SUBFLOW GETS NO WIRING FUNCTION OF ITS OWN. This is not the run
	// assertion — the stdout token below is — but it names WHICH property the run
	// depended on, so a regression that started emitting one again fails here with
	// a message about the wiring function rather than only as a compile error.
	if strings.Contains(string(generated), "func WireScreening(") {
		t.Fatalf("a signature-carrying subflow was given a wiring function of its own, "+
			"which reads an unbound in:\n%s", generated)
	}

	cmd := exec.Command("go", "run", "-buildvcs=false", ".")
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the generated program did not build and run: %v\n%s\n--- generated ---\n%s",
			err, output, generated)
	}

	got := string(output)
	if !strings.Contains(got, embedToken) {
		t.Fatalf("the program ran but no datum reached the sink through the embedded subflow: "+
			"its stdout does not carry %q.\n%s\n--- generated ---\n%s", embedToken, got, generated)
	}
	t.Logf("a datum travelled through the embedded subflow to the sink: %s", strings.TrimSpace(got))
}

// embedMain is e2eMain for the embedded-subflow fixture: same shape, different
// Wire function and machine name.
const embedMain = `package main

import (
	"context"
	"fmt"
	"os"
	"time"

	machine "github.com/whitaker-io/machine/v4"
)

func main() {
	m := machine.New("flow-embed")
	ingests, err := WireEmbedded(m)
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

	if err := ingests.Ingest(ctx, Order{ID: "order-pushed", Tags: []string{"a"}}); err != nil {
		fmt.Fprintln(os.Stderr, "push:", err)
		os.Exit(1)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		fmt.Fprintln(os.Stderr, "no datum reached the sink through the embedded subflow")
		os.Exit(1)
	}
}
`

// readFixtureFile reads one e2e fixture, refusing an empty one.
func readFixtureFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		t.Fatalf("CONTROL FAILED: %s is empty, so running it would prove nothing", path)
	}

	return string(body)
}

// e2eMain builds the machine, wires the generated flow onto it and waits for the
// fixture's own signal.
//
// IT WAITS ON THE DATUM RATHER THAN ON A TIMER. A sleep would pass on a fast
// machine whether or not anything moved, and would flake on a slow one; waiting
// on the channel the fixture closes means the program exits BECAUSE the data
// arrived.
const e2eMain = `package main

import (
	"context"
	"fmt"
	"os"
	"time"

	machine "github.com/whitaker-io/machine/v4"
)

func main() {
	m := machine.New("flow-e2e")
	ingests, err := WireE2e(m)
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

	// THE IN-PROCESS PUSH, and it is load bearing rather than decorative. The
	// transport delivers one datum FEWER than the token's count, so the count is
	// reached only if this push actually delivered into the running graph. An
	// ingest that is merely typed, or exported but never wired to the machine,
	// leaves the flow one short and the token is never printed.
	if err := ingests.Ingest(ctx, Order{ID: "order-pushed", Tags: []string{"a"}}); err != nil {
		fmt.Fprintln(os.Stderr, "push:", err)
		os.Exit(1)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		fmt.Fprintln(os.Stderr, "the flow drove no data through within its deadline")
		os.Exit(1)
	}
}
`

// loopMain is e2eMain for the looping fixture: same shape, different Wire
// function and machine name.
//
// THE DEADLINE IS THE NON-TERMINATION DETECTOR. A loop whose predicate never
// routes out would spin forever and the harness would hang; this exits non-zero
// with a message naming the loop instead, which is a failure a reader can act on.
const loopMain = `package main

import (
	"context"
	"fmt"
	"os"
	"time"

	machine "github.com/whitaker-io/machine/v4"
)

func main() {
	m := machine.New("flow-loop")
	// THE INGESTS ARE DELIBERATELY UNUSED HERE. This flow is fed by its
	// transport, and Wire returns the struct anyway because the signature is one
	// contract rather than two — a host that does not push simply ignores it,
	// which is what this main demonstrates.
	if _, err := WireLooped(m); err != nil {
		fmt.Fprintln(os.Stderr, "wiring:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		fmt.Fprintln(os.Stderr, "the loop did not terminate within its deadline")
		os.Exit(1)
	}
}
`
