// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"testing"
)

// supportFixture supplies the Go declarations a generated file references but
// does not itself declare: the payload types and the node functions.
//
// A .flow source names Go it does not define — that is the whole point of the
// language — so compiling generated output means compiling it BESIDE the package
// the author would have written. This stands in for that package.
const supportFixture = `package generated

import machine "github.com/whitaker-io/machine/v4"

type Order struct {
	ID   string
	Kind string
}

type Receipt struct{ ID string }

func Poll() machine.EdgeFactory[Order] { return machine.Channel[Order](0) }

func Bill(f machine.Frame[Order]) Receipt { return Receipt{ID: f.Value().ID} }

func Store(f machine.Frame[Receipt]) Receipt { return f.Value() }
`

// TestAGeneratedFileCompilesAgainstTheRuntime is the instrument that proves the
// emitter produces REAL GO, not merely stable text.
//
// Every other assertion about emitted source is a text assertion: it proves the
// output has not drifted, and would go on passing over output that no longer
// builds. This one hands the emitted bytes to the compiler.
func TestAGeneratedFileCompilesAgainstTheRuntime(t *testing.T) {
	const src = "flow orders\n" +
		"state {\n  seen map[string]bool\n}\n" +
		"var attempt int\n" +
		"source ingest Poll()\n" +
		"transform charge Bill from ingest\n" +
		"  reads attempt  writes seen\n" +
		"sink done Store from charge\n"

	out := generateOf(t, src,
		map[string]string{"ingest": "Order", "charge": "Order", "done": "Receipt"},
		map[string]Boundary{"orders": {}})

	output, err := buildInWorkspace(t, map[string]string{
		"support.go":       supportFixture,
		"pipeline.flow.go": string(out.Source),
	})
	if err != nil {
		t.Fatalf("the generated file does not compile:\n%s\n--- generated ---\n%s", output, out.Source)
	}
	t.Log("the emitted file compiled against the real runtime")
}

// TestACheckpointingFlowCompiles covers the shape that pulls in the journal
// check, the codec options and the conditional fmt import at once.
func TestACheckpointingFlowCompiles(t *testing.T) {
	const src = "flow orders\n" +
		"source ingest Poll()\n" +
		"transform charge Bill from ingest\n" +
		"  checkpoint machine.GobCodec[Order]{}\n" +
		"sink done Store from charge\n"

	out := generateOf(t, src,
		map[string]string{"ingest": "Order", "charge": "Order", "done": "Receipt"},
		map[string]Boundary{"orders": {}})

	output, err := buildInWorkspace(t, map[string]string{
		"support.go":       supportFixture,
		"pipeline.flow.go": string(out.Source),
	})
	if err != nil {
		t.Fatalf("a checkpointing flow does not compile:\n%s\n--- generated ---\n%s", output, out.Source)
	}
	t.Log("a checkpointing flow compiled, journal check and successor codec included")
}
