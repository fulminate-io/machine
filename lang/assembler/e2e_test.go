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
	root := machineRoot(t)
	dir := t.TempDir()

	flowSrc := readFixtureFile(t, filepath.Join("testdata", "e2e", "pipeline.flow"))
	feed := readFixtureFile(t, filepath.Join("testdata", "e2e", "feed.go.txt"))

	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	write("go.mod", "module flowe2e\n\ngo 1.27\n")
	write("go.work", "go 1.27\n\nuse (\n\t.\n\t"+root+"\n)\n")
	if sum, err := os.ReadFile(filepath.Join(root, "go.sum")); err == nil {
		write("go.work.sum", string(sum))
	}
	write("feed.go", feed)
	write("pipeline.flow", flowSrc)
	write("main.go", e2eMain)

	// GENERATE THROUGH THE REAL DRIVER, into the same directory, exactly as flowc
	// does.
	driver := &Driver{
		Config:      Config{Package: "main", Qualifier: "flowe2e"},
		PackagePath: "flowe2e",
		Boundary:    map[string]Boundary{},
	}
	if err := driver.Generate(dir, dir); err != nil {
		t.Fatalf("generation failed: %v", err)
	}

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
	root := machineRoot(t)
	dir := t.TempDir()

	flowSrc := readFixtureFile(t, filepath.Join("testdata", "e2e", "looped.flow"))
	feed := readFixtureFile(t, filepath.Join("testdata", "e2e", "loopfeed.go.txt"))

	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	write("go.mod", "module flowloop\n\ngo 1.27\n")
	write("go.work", "go 1.27\n\nuse (\n\t.\n\t"+root+"\n)\n")
	if sum, err := os.ReadFile(filepath.Join(root, "go.sum")); err == nil {
		write("go.work.sum", string(sum))
	}
	write("feed.go", feed)
	write("looped.flow", flowSrc)
	write("main.go", loopMain)

	driver := &Driver{
		Config:      Config{Package: "main", Qualifier: "flowloop"},
		PackagePath: "flowloop",
		Boundary:    map[string]Boundary{},
	}
	if err := driver.Generate(dir, dir); err != nil {
		t.Fatalf("generation failed: %v", err)
	}

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
