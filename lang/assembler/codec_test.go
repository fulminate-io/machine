// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
)

// TestCheckpointCodecFamilyIsDerivedOrRefusedByName covers both halves of the
// rule on the shape that makes it matter.
//
// THE DEFECT THIS EXISTS FOR is a successor codec emitted at the WRONG TYPE. A
// completion-anchored checkpoint journals with its SUCCESSOR's codec, so the
// generator has to carry the operand's family to the successor's input type. If
// it reused the operand verbatim the emitted code would TYPE-CHECK ON BOTH SIDES
// and nothing downstream could catch it — the damage appears only when a recovery
// unmarshals with a codec built for another type. So the positive leg asserts the
// emitted option names the SUCCESSOR's type and not the operand's, which is the
// one assertion a verbatim-reuse implementation fails.
func TestCheckpointCodecFamilyIsDerivedOrRefusedByName(t *testing.T) {
	t.Run("re-instantiated at the successor's input type", func(t *testing.T) {
		program := graphOf(t, "flow orders\n"+
			"source ingest Poll\n"+
			"transform charge Bill from ingest\n"+
			"  checkpoint machine.GobCodec[Order]{}\n"+
			"sink done Store from charge\n")
		// The DRIVER supplies these; the generator derives neither.
		program.InputTypes = map[string]string{"ingest": "Order", "charge": "Order", "done": "Receipt"}

		plan, diags := lower(program)
		if len(diags) != 0 {
			t.Fatalf("lowering refused a well-formed operand: %v", messagesOf(diags))
		}

		option := optionOn(t, plan, "done")
		const want = "machine.WithCodec(machine.GobCodec[Receipt]{})"
		if option != want {
			t.Errorf("the successor carries %q, want %q", option, want)
		}
		// THE DISCRIMINATING ASSERTION. A generator that reused the operand
		// verbatim would emit GobCodec[Order] here, which compiles and is wrong.
		if strings.Contains(option, "[Order]") {
			t.Errorf("the successor's codec is instantiated at the CHECKPOINTED node's type, not its own: %q", option)
		}
	})

	t.Run("an arrival anchor needs no successor codec", func(t *testing.T) {
		// The known positive for the negative case: marking the node idempotent
		// moves the anchor to arrival, where the node marshals its own input with
		// its own codec, so no successor option is emitted at all.
		program := graphOf(t, "flow orders\n"+
			"source ingest Poll\n"+
			"transform charge Bill from ingest\n"+
			"  checkpoint machine.GobCodec[Order]{}  idempotent\n"+
			"sink done Store from charge\n")
		program.InputTypes = map[string]string{"ingest": "Order", "charge": "Order", "done": "Receipt"}

		plan, diags := lower(program)
		if len(diags) != 0 {
			t.Fatalf("lowering refused an arrival-anchored checkpoint: %v", messagesOf(diags))
		}
		for _, op := range plan.Ops {
			if op.Node == "done" && strings.Contains(strings.Join(op.Options, " "), "WithCodec") {
				t.Errorf("an arrival-anchored checkpoint put a codec on its successor: %v", op.Options)
			}
		}
	})

	t.Run("a branching successor carries it on both outlets", func(t *testing.T) {
		program := graphOf(t, "flow orders\n"+
			"source ingest Poll\n"+
			"transform charge Bill from ingest\n"+
			"  checkpoint machine.GobCodec[Order]{}\n"+
			"branch split Ok from charge -> good, bad\n"+
			"sink kept Store from good\n"+
			"sink lost Store from bad\n")
		program.InputTypes = map[string]string{
			"ingest": "Order", "charge": "Order", "split": "Receipt", "kept": "Receipt", "lost": "Receipt",
		}
		plan, diags := lower(program)
		if len(diags) != 0 {
			t.Fatalf("lowering refused: %v", messagesOf(diags))
		}
		if got := optionOn(t, plan, "split"); !strings.Contains(got, "GobCodec[Receipt]") {
			t.Errorf("the branching successor carries %q", got)
		}
	})

	// THE SIX REFUSED FORMS, each for its own reason. Every one is a shape an
	// author can write, and the inferred-generic call is the one they most
	// plausibly write.
	for name, tc := range map[string]struct{ operand, why string }{
		"bare identifier":       {"orderCodec", "names a value"},
		"non-generic call":      {"codecs.NewOrder()", "no written type argument"},
		"inferred generic call": {"codecs.New(order)", "no written type argument"},
		"unparseable":           {"GobCodec[Order", "not a Go expression"},
		"bare type name":        {"machine.GobCodec", "names a value"},
	} {
		t.Run("refuses a "+name, func(t *testing.T) {
			family, why := codecFamilyOf(tc.operand)
			if why == "" {
				t.Fatalf("%q was admitted as family %+v", tc.operand, family)
			}
			if !strings.Contains(why, tc.why) {
				t.Errorf("%q was refused as %q, want a reason about %q", tc.operand, why, tc.why)
			}
		})
	}

	// The empty operand is the sixth form, reached through a bare clause rather
	// than through a written expression.
	t.Run("refuses the empty operand", func(t *testing.T) {
		if _, why := codecFamilyOf("   "); why == "" {
			t.Fatal("an empty operand was admitted")
		}
	})

	t.Run("a refusal is positioned on the operand's own line", func(t *testing.T) {
		program := graphOf(t, "flow orders\n"+
			"source ingest Poll\n"+
			"transform charge Bill from ingest\n"+
			"  checkpoint codecs.New(order)\n"+
			"sink done Store from charge\n")
		program.InputTypes = map[string]string{"ingest": "Order", "charge": "Order", "done": "Receipt"}

		_, diags := lower(program)
		if len(diags) == 0 {
			t.Fatal("an inferred-generic operand was lowered")
		}
		d := diags[0]
		// THE POSITION IS PART OF THE ASSERTION. A refusal at line 0 sends the
		// author to the top of the file rather than to the clause they wrote.
		if d.Pos.Line != 4 {
			t.Errorf("the refusal is at line %d, want the operand's own line 4: %s", d.Pos.Line, d.Message)
		}
		if !strings.Contains(d.Message, "codecs.New(order)") {
			t.Errorf("the refusal %q does not name the offending operand", d.Message)
		}
		// AND IT NAMES WHAT CAN BE LOWERED, so the author has somewhere to go.
		for _, want := range []string{"machine.GobCodec[Order]{}", "codecs.New[Order]()"} {
			if !strings.Contains(d.Message, want) {
				t.Errorf("the refusal %q does not name the admissible form %q", d.Message, want)
			}
		}
	})

	t.Run("both admitted forms round-trip", func(t *testing.T) {
		for operand, want := range map[string]string{
			"machine.GobCodec[Order]{}": "machine.GobCodec[Receipt]{}",
			"codecs.New[Order]()":       "codecs.New[Receipt]()",
		} {
			family, why := codecFamilyOf(operand)
			if why != "" {
				t.Fatalf("%q was refused: %s", operand, why)
			}
			if got := family.instantiate("Receipt"); got != want {
				t.Errorf("%q re-instantiates to %q, want %q", operand, got, want)
			}
		}
	})
}

// optionOn returns the single option a named node's first op carries, failing
// when the node is absent.
func optionOn(t *testing.T, plan *Plan, node string) string {
	t.Helper()
	for _, op := range plan.Ops {
		if op.Node != node {
			continue
		}
		for _, option := range op.Options {
			if strings.Contains(option, "WithCodec") {
				return option
			}
		}
	}
	t.Fatalf("no op for node %q carries a codec option; plan is %v", node, planNodes(plan))

	return ""
}

// planNodes lists the node names in plan order.
func planNodes(plan *Plan) []string {
	out := make([]string, 0, len(plan.Ops))
	for _, op := range plan.Ops {
		out = append(out, op.Method+":"+op.Node)
	}

	return out
}

// TestASuccessorTypeThatIsMissingIsReportedNotGuessed proves the generator says
// so rather than emitting a codec at some default type.
//
// A generator that fell back to the operand's own type here would produce exactly
// the wrong-type defect the family rule exists to prevent, and it would do it
// silently.
func TestASuccessorTypeThatIsMissingIsReportedNotGuessed(t *testing.T) {
	program := graphOf(t, "flow orders\n"+
		"source ingest Poll\n"+
		"transform charge Bill from ingest\n"+
		"  checkpoint machine.GobCodec[Order]{}\n"+
		"sink done Store from charge\n")
	// The successor's type is deliberately absent.
	program.InputTypes = map[string]string{"ingest": "Order", "charge": "Order"}

	plan, diags := lower(program)
	if len(diags) == 0 {
		t.Fatal("a missing successor type was lowered rather than reported")
	}
	if !strings.Contains(diags[0].Message, "successor") {
		t.Errorf("the diagnostic %q does not say what is missing", diags[0].Message)
	}
	for _, op := range plan.Ops {
		if strings.Contains(strings.Join(op.Options, " "), "WithCodec") {
			t.Errorf("a codec was emitted anyway, at a guessed type: %v", op.Options)
		}
	}
}

// TestClausePositionsSurviveIntoDiagnostics guards the operand span itself.
func TestClausePositionsSurviveIntoDiagnostics(t *testing.T) {
	program := graphOf(t, "flow orders\nsource ingest Poll\n"+
		"transform charge Bill from ingest\n  checkpoint machine.GobCodec[Order]{}\nsink done Store from charge\n")
	charge := nodeNamed(t, program, "charge")
	if charge.Clauses.Checkpoint == nil {
		t.Fatal("the checkpoint clause did not reach the graph")
	}
	if charge.Clauses.Checkpoint.Start.Line != 4 {
		t.Errorf("the operand span starts at line %d, want 4", charge.Clauses.Checkpoint.Start.Line)
	}
	if charge.Clauses.Checkpoint.Text != "machine.GobCodec[Order]{}" {
		t.Errorf("the operand text is %q", charge.Clauses.Checkpoint.Text)
	}
	var _ ast.Position = charge.Clauses.Checkpoint.Start
}
