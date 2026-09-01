// Package loader - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package loader

import (
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
)

const damagedDir = "testdata/damaged"
const damagedPath = "example.com/damaged"

// TestResolveFlowRefusesAbsentAndPrivateDistinctlyAndResolvesExported covers the
// export predicate and the whole of ResolveFlow's refusal set.
//
// The refusals must stay DISTINCT, which is the property most easily lost: an
// implementation that answers "not found" for a module-private flow refuses both
// cases, passes any gate that only checks for a refusal, and tells the author to
// capitalize the first letter of a flow that does not exist.
func TestResolveFlowRefusesAbsentAndPrivateDistinctlyAndResolvesExported(t *testing.T) {
	t.Run("the export predicate is Go's own rule, one case per axis", func(t *testing.T) {
		for _, probe := range []struct {
			name string
			want bool
			why  string
		}{
			{"Orders", true, "ASCII upper"},
			{"orders", false, "ASCII lower"},
			{"Ärger", true, "NON-ASCII upper: a byte test over 'A'-'Z' calls this private"},
			{"ärger", false, "non-ASCII lower"},
			{"日本語", false, "a caseless script has no uppercase form, so it is never exported — Go's own consequence"},
			{"_hidden", false, "an underscore is not an uppercase letter"},
			{"", false, "the predicate must be TOTAL on the empty name rather than panic"},
		} {
			if got := Exported(probe.name); got != probe.want {
				t.Errorf("Exported(%q) = %v, want %v — %s", probe.name, got, probe.want, probe.why)
			}
		}
	})

	loaded, err := Load(subjectDir, []string{"./..."})
	if err != nil {
		t.Fatalf("the subject fixture module did not load: %v", err)
	}

	// CONTROL: the fixture must really declare the flows the arms below refer to,
	// or a refusal is just a walk that found nothing and proves nothing.
	declared := map[string]Flow{}

	flows, err := loaded.Flows(subjectPath)
	if err != nil {
		t.Fatalf("the subject module's flows did not enumerate: %v", err)
	}

	for _, flow := range flows {
		declared[flow.Name] = flow
	}

	for _, want := range []string{"Orders", "Deep", "hidden"} {
		if _, ok := declared[want]; !ok {
			t.Fatalf("CONTROL FAILED: the fixture does not declare flow %q, so nothing below is evidence (got %v)", want, declared)
		}
	}

	at := ast.Position{Offset: 12, Line: 3, Col: 5}
	const from = "consumer.flow"

	t.Run("an exported flow WITH a signature resolves and carries its declared outputs", func(t *testing.T) {
		flow, bad := loaded.ResolveFlow(subjectPath, "Orders", at, from)
		if bad != nil {
			t.Fatalf("an exported flow was refused: %v", bad.Message)
		}

		if !flow.HasSignature {
			t.Fatal("Orders declares a signature but resolved with HasSignature false")
		}

		if len(flow.Outputs) == 0 {
			t.Fatal("Orders declares outputs but resolved with none")
		}

		t.Logf("Orders resolved with outputs %v from %s", flow.Outputs, flow.File)
	})

	t.Run("an exported flow WITHOUT a signature also resolves, and is returned untyped", func(t *testing.T) {
		flow, bad := loaded.ResolveFlow(subjectPath, "Deep", at, from)
		if bad != nil {
			t.Fatalf("a signature-less exported flow was refused; the signature is OPTIONAL: %v", bad.Message)
		}

		if flow.HasSignature {
			t.Fatal("Deep carries no signature but resolved with HasSignature true")
		}

		if len(flow.Outputs) != 0 {
			t.Fatalf("a signature-less flow came back carrying outputs %v, which this module cannot know", flow.Outputs)
		}
		// Nothing is asserted about what Deep CARRIES. That inference belongs to
		// the type-flow above this module, and a claim here would be invented.
	})

	t.Run("an absent flow and a module-private one are refused DIFFERENTLY", func(t *testing.T) {
		_, absent := loaded.ResolveFlow(subjectPath, "Nonexistent", at, from)
		if absent == nil {
			t.Fatal("a flow that does not exist resolved")
		}

		_, private := loaded.ResolveFlow(subjectPath, "hidden", at, from)
		if private == nil {
			t.Fatal("a lowercase flow was resolvable from another module")
		}

		if absent.Message == private.Message {
			t.Fatalf("the absent and private refusals are identical, so one of them is wrong: %q", absent.Message)
		}

		if !strings.Contains(absent.Message, "Nonexistent") {
			t.Errorf("the absent refusal does not name the missing flow: %q", absent.Message)
		}

		if strings.Contains(absent.Message, "capitalize") {
			t.Errorf("the absent refusal tells the author to capitalize a flow that does not exist: %q", absent.Message)
		}

		if !strings.Contains(private.Message, "capitalize") {
			t.Errorf(`the refusal does not name "capitalize", so it does not say how to fix it: %q`, private.Message)
		}

		for _, bad := range []*Diagnostic{absent, private} {
			if bad.Path != from || bad.Pos != at {
				t.Errorf("a refusal is positioned at %s %v rather than at the reference %s %v", bad.Path, bad.Pos, from, at)
			}
		}

		t.Logf("absent:  %s", absent.Message)
		t.Logf("private: %s", private.Message)
	})

	t.Run("a module carrying an unparseable source refuses by naming the FILE", func(t *testing.T) {
		damaged, err := Load(damagedDir, []string{"./..."})
		if err != nil {
			t.Fatalf("the damaged fixture module failed to LOAD, which is a different failure: %v", err)
		}

		// CONTROL: Flows must genuinely refuse this module. If it returned a
		// partial set instead, the file was skipped silently and nothing below
		// proves anything.
		if _, err := damaged.Flows(damagedPath); err == nil {
			t.Fatal("CONTROL FAILED: Flows returned no error for a module carrying an unparseable source, so the file was skipped silently and nothing here proves a partition")
		}

		_, bad := damaged.ResolveFlow(damagedPath, "Anything", at, from)
		if bad == nil {
			t.Fatal("a reference into a module with an unparseable source resolved")
		}

		if !strings.Contains(bad.Message, "broken.flow") {
			t.Errorf("the refusal does not name the offending file: %q", bad.Message)
		}

		if strings.Contains(bad.Message, noFlowNamed) {
			t.Errorf("an unparseable source was reported as a mere absence, which sends the author after the wrong thing: %q", bad.Message)
		}

		t.Logf("unparseable: %s", bad.Message)
	})
}
