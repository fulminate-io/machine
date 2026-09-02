// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"strings"
	"testing"
)

// TestEmittedHelpersCarryTheNodeInputType COMPILES the preamble beside a node
// function and proves the helpers infer the node's input type.
//
// THAT INFERENCE IS THE WHOLE REASON THE HELPERS EXIST. machine.WithReads and
// machine.WithWrites take only KeyRefs, which name no payload type, and Go does
// not infer a generic call's type argument from the parameter it is passed into.
// The compiler says so in as many words — `cannot infer T`. A generator taking
// that at face value would have to SPELL every node's Go input type at every
// reads or writes clause, making the commonest clause in the language depend on
// full type resolution. Passing the node function as an ignored parameter carries
// the type for free.
//
// IT REDS IF A HELPER IS EMITTED WITH THE TYPE PARAMETER IN A POSITION GO CANNOT
// INFER, which no text assertion over the emitted source could detect.
func TestEmittedHelpersCarryTheNodeInputType(t *testing.T) {
	const callSite = `package generated

import machine "github.com/whitaker-io/machine/v4"

type Order struct{ Kind string }

// Charge is the node function. Its signature is the ONLY place the payload type
// appears at the call sites below.
func Charge(f machine.Frame[Order]) Order { return f.Value() }

// Screen is a branch predicate, a Filter rather than a Transformation.
func Screen(f machine.Frame[Order]) bool { return f.Value().Kind != "" }

var (
	seen    = machine.NewCell[int]("acme.orders.seen")
	attempt = machine.NewKey[int]("acme.orders.attempt", func(v int) int { return v })
)

// Wire exercises every helper in the preamble, with NO type argument written at
// any call site. If a helper's type parameter sat where Go cannot infer it, none
// of these would compile.
func Wire(m *machine.Machine) error {
	src, _ := m.Source[Order]("in")
	charged := src.Map("charge", Charge,
		flowReadsOf(Charge, attempt),
		flowWritesOf(Charge, seen),
	)
	kept, lost := charged.If("screen", Screen,
		flowReadsOfFilter(Screen, attempt),
		flowWritesOfFilter(Screen, seen),
	)
	kept.Drop("kept")
	lost.Drop("lost")

	return nil
}
`

	out, err := buildInWorkspace(t, map[string]string{
		"preamble.go": "package generated\n\nimport machine \"github.com/whitaker-io/machine/v4\"\n\n" + preamble,
		"wire.go":     callSite,
	})
	if err != nil {
		t.Fatalf("the emitted preamble does not compile beside a node function:\n%s", out)
	}
	t.Log("every preamble helper inferred the node's input type with no type argument written")
}

// TestTheHelpersAreWhatMakesInferenceWork is the discriminating control.
//
// The test above passes if the helpers work. It would ALSO pass if the runtime
// options had been inferable all along and the helpers were pointless ceremony.
// This drives the direct call the helpers replace and requires it to FAIL, so the
// preamble is demonstrated to be load-bearing rather than assumed to be.
func TestTheHelpersAreWhatMakesInferenceWork(t *testing.T) {
	const direct = `package generated

import machine "github.com/whitaker-io/machine/v4"

type Order struct{ Kind string }

func Charge(f machine.Frame[Order]) Order { return f.Value() }

var seen = machine.NewCell[int]("acme.direct.seen")

func Wire(m *machine.Machine) error {
	src, _ := m.Source[Order]("in")
	// NO HELPER, NO TYPE ARGUMENT. This is what the generator would emit if the
	// preamble did not exist.
	src.Map("charge", Charge, machine.WithReads(seen)).Drop("done")

	return nil
}
`

	out, err := buildInWorkspace(t, map[string]string{"wire.go": direct})
	if err == nil {
		t.Fatalf("machine.WithReads inferred its type parameter without a helper, "+
			"so the preamble is unnecessary ceremony:\n%s", out)
	}
	if !strings.Contains(out, "cannot infer") {
		t.Errorf("the direct call failed for some reason other than inference:\n%s", out)
	}
	t.Logf("the direct call fails as expected, which is why the helpers exist: %s", firstLine(out))
}

// firstLine returns a compiler output's first line, for a log that does not dump
// the whole build.
func firstLine(text string) string {
	if at := strings.IndexByte(text, '\n'); at >= 0 {
		return text[:at]
	}

	return text
}
