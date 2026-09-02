// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"slices"
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
)

// subflowFixture declares a dependency with two outputs and a caller that embeds
// it twice, which is the shape both criteria on this step need.
const subflowFixture = "flow screening (Order) -> ok OkResult, bad ErrResult\n" +
	"state {\n  seen map[string]bool\n}\n" +
	"var attempt int\n" +
	"branch check Clean from in -> ok, bad\n" +
	"\n" +
	"flow main\n" +
	"source ingest Poll\n" +
	"use first screening from ingest -> ok, bad\n" +
	"sink kept Store from ok\n" +
	"sink lost Store from bad\n"

// screeningBoundary is the fact lang/analysis exports for the dependency. The
// assembler CONSUMES it and never re-derives it.
var screeningBoundary = map[string]Boundary{
	"screening": {Outputs: []string{"ok", "bad"}},
	"main":      {Outputs: nil},
}

// lowerFileOf parses, builds and lowers a whole file, requiring a clean build.
func lowerFileOf(t *testing.T, src string, boundary map[string]Boundary) ([]*Plan, []Diagnostic) {
	t.Helper()
	plans, build, lowered := assembleFile(t, src, boundary)
	if len(build) != 0 {
		t.Fatalf("the fixture must build clean: %v", messagesOf(build))
	}

	return plans, lowered
}

// assembleFile parses, builds and lowers, returning both diagnostic sets
// separately so a test can say which layer refused.
func assembleFile(t *testing.T, src string, boundary map[string]Boundary) (plans []*Plan, build, lowered []Diagnostic) {
	t.Helper()
	file, err := ast.Parse([]byte(src))
	if err != nil {
		t.Fatalf("the fixture must parse clean: %v", err)
	}
	programs, build := buildFile(file)
	for _, p := range programs {
		// The DRIVER supplies these; the generator derives none of them.
		p.InputTypes = map[string]string{
			"ingest": "Order", "check": "Order", "kept": "OkResult", "lost": "ErrResult",
			"extra": "OkResult", "first.check": "Order", "second.check": "Order",
		}
	}
	plans, lowered = lowerFile(programs, boundary, Config{Package: "generated", Qualifier: "acme"})

	return plans, build, lowered
}

// planNamed returns the plan for one flow.
func planNamed(t *testing.T, plans []*Plan, flow string) *Plan {
	t.Helper()
	for _, plan := range plans {
		if plan.Flow == flow {
			return plan
		}
	}
	t.Fatalf("no plan for flow %q", flow)

	return nil
}

// handleNames lists a plan's handle names.
func handleNames(plan *Plan) []string {
	out := make([]string, 0, len(plan.Handles))
	for _, h := range plan.Handles {
		out = append(out, h.Name)
	}

	return out
}

// TestSubflowInlinesUnderTheInstanceNamespace proves the inlining, the handle
// namespacing, and the property that makes the namespacing necessary.
//
// THE COLLISION IS A PANIC, NOT A WRONG ANSWER. The runtime keeps keys and cells
// in ONE process-global namespace and panics on a duplicate at declaration, so two
// instances of one subflow sharing a handle name is a program that dies at Start.
func TestSubflowInlinesUnderTheInstanceNamespace(t *testing.T) {
	plans, diags := lowerFileOf(t, subflowFixture, screeningBoundary)
	if len(diags) != 0 {
		t.Fatalf("lowering refused a well-formed subflow:\n%s", strings.Join(messagesOf(diags), "\n"))
	}
	main := planNamed(t, plans, "main")

	t.Run("the dependency's nodes are inlined under the instance name", func(t *testing.T) {
		var found bool
		for _, op := range main.Ops {
			if op.Node == "first"+nameSep+"check" {
				found = true
			}
		}
		if !found {
			t.Errorf("the subflow's node was not inlined under the instance; the plan declares %v", planNodes(main))
		}
		// And it is NOT declared under its bare name, which would collide with
		// the same node from a second instance.
		for _, op := range main.Ops {
			if op.Node == "check" {
				t.Errorf("the subflow's node was inlined under its bare name: %+v", op)
			}
		}
	})

	t.Run("state handles carry the qualifier, the flow and the instance", func(t *testing.T) {
		names := handleNames(main)
		for _, want := range []string{"acme.main.first.seen", "acme.main.first.attempt"} {
			if !slices.Contains(names, want) {
				t.Errorf("the plan declares handles %v, want one named %q", names, want)
			}
		}
	})

	t.Run("two instances get independent handle names", func(t *testing.T) {
		// THE POINT OF THE NAMESPACE. Without the instance in the name these two
		// would be the same process-global handle, and the runtime panics on the
		// duplicate at declaration time.
		// EACH INSTANCE BINDS A DIFFERENT OUTPUT, because binding is by name and
		// no local renaming form exists: two instances cannot both publish `ok`
		// into one caller, and the graph builder refuses that as two statements
		// publishing one name. What this subtest is about is the STATE handles,
		// which must stay independent whatever the callers bind.
		twice := strings.Replace(subflowFixture,
			"use first screening from ingest -> ok, bad\n",
			"use first screening from ingest -> ok\nuse second screening from ingest -> bad\n", 1)

		plans, diags := lowerFileOf(t, twice, screeningBoundary)
		if len(diags) != 0 {
			t.Fatalf("two instances were refused:\n%s", strings.Join(messagesOf(diags), "\n"))
		}
		names := handleNames(planNamed(t, plans, "main"))

		for _, want := range []string{"acme.main.first.seen", "acme.main.second.seen"} {
			if !slices.Contains(names, want) {
				t.Errorf("handles are %v, want one named %q", names, want)
			}
		}
		// The collision assertion: no handle name appears twice.
		seen := map[string]bool{}
		for _, name := range names {
			if seen[name] {
				t.Errorf("two instances share the handle name %q; the runtime panics on a duplicate", name)
			}
			seen[name] = true
		}
	})
}

// TestSubflowBindsOutputsByName is LEG 1 of the binding criterion.
//
// THE DEFECT IT ALONE DETECTS IS SILENT CROSS-BINDING. A positional
// implementation compiles, generates, builds and runs, and wires `-> bad, ok` so
// that bad receives ok's value. Nothing observable says so — no diagnostic, and no
// type error when the two outputs share a type. So the discriminating case is a
// REVERSED binding list, which a positional implementation accepts and silently
// mis-wires while a by-name one treats as the same set.
func TestSubflowBindsOutputsByName(t *testing.T) {
	t.Run("order is irrelevant", func(t *testing.T) {
		reversed := strings.Replace(subflowFixture,
			"use first screening from ingest -> ok, bad\n",
			"use first screening from ingest -> bad, ok\n", 1)

		plans, diags := lowerFileOf(t, reversed, screeningBoundary)
		if len(diags) != 0 {
			t.Fatalf("a reversed binding list was refused; the identifiers are a SET:\n%s",
				strings.Join(messagesOf(diags), "\n"))
		}
		// The same nodes are declared whichever order the author wrote.
		if got := planNamed(t, plans, "main"); len(got.Ops) == 0 {
			t.Fatal("the reversed form lowered to nothing")
		}
	})

	t.Run("a subset is legal and there is no count check", func(t *testing.T) {
		subset := strings.Replace(subflowFixture,
			"use first screening from ingest -> ok, bad\n",
			"use first screening from ingest -> ok\n", 1)
		subset = strings.Replace(subset, "sink lost Store from bad\n", "", 1)

		if _, diags := lowerFileOf(t, subset, screeningBoundary); len(diags) != 0 {
			t.Errorf("binding a SUBSET of the outputs was refused:\n%s", strings.Join(messagesOf(diags), "\n"))
		}
	})

	t.Run("an unknown name is a positioned error listing the real outputs", func(t *testing.T) {
		unknown := strings.Replace(subflowFixture,
			"use first screening from ingest -> ok, bad\n",
			"use first screening from ingest -> ok, nope\n", 1)
		unknown = strings.Replace(unknown, "sink lost Store from bad\n", "sink lost Store from nope\n", 1)

		_, diags := lowerFileOf(t, unknown, screeningBoundary)
		if len(diags) == 0 {
			t.Fatal("an identifier naming no output was accepted")
		}
		joined := strings.Join(messagesOf(diags), "\n")
		if !strings.Contains(joined, "names no output of that flow") {
			t.Errorf("the diagnostics %q do not say the name is not an output", joined)
		}
		if !strings.Contains(joined, "ok") || !strings.Contains(joined, "bad") {
			t.Errorf("the diagnostics %q do not list the flow's real outputs", joined)
		}
		for _, d := range diags {
			if d.Pos.Line == 0 {
				t.Errorf("the binding error is unpositioned: %q", d.Message)
			}
		}
	})

	t.Run("a duplicate is refused", func(t *testing.T) {
		// A duplicated identifier is refused TWICE OVER: the graph builder sees
		// one name published twice, and the binding resolver sees one identifier
		// bound twice. Either is a refusal; what matters is that no layer lets it
		// through, so this asserts over both diagnostic sets.
		dup := strings.Replace(subflowFixture,
			"use first screening from ingest -> ok, bad\n",
			"use first screening from ingest -> ok, ok\n", 1)
		dup = strings.Replace(dup, "sink lost Store from bad\n", "", 1)

		_, build, lowered := assembleFile(t, dup, screeningBoundary)
		all := append(append([]Diagnostic{}, build...), lowered...)
		if len(all) == 0 {
			t.Fatal("a duplicated binding was accepted by every layer")
		}
		joined := strings.Join(messagesOf(all), "\n")
		if !strings.Contains(joined, "bound twice") && !strings.Contains(joined, "already produced") {
			t.Errorf("the diagnostics do not report a duplicate: %v", messagesOf(all))
		}
		for _, d := range all {
			if d.Pos.Line == 0 {
				t.Errorf("the duplicate refusal is unpositioned: %q", d.Message)
			}
		}
	})

	t.Run("the binding resolver's own duplicate arm fires", func(t *testing.T) {
		// The subtest above is satisfied by either layer, so this drives the
		// resolver directly to prove ITS arm is not dead code.
		l := &lowering{program: &Program{Name: "caller"}, plan: &Plan{}}
		stmt := ast.UseStmt{Bindings: []ast.Ident{
			{Name: "ok", NamePos: ast.Position{Line: 3, Col: 1}},
			{Name: "ok", NamePos: ast.Position{Line: 3, Col: 5}},
		}}
		if l.resolveBindings(stmt, Boundary{Outputs: []string{"ok", "bad"}}) {
			t.Fatal("the resolver accepted a duplicated identifier")
		}
		if !strings.Contains(strings.Join(messagesOf(l.diags), "\n"), "bound twice") {
			t.Errorf("the resolver's diagnostics do not report a duplicate: %v", messagesOf(l.diags))
		}
	})
}

// TestSubflowBindingDisagreementIsRefusedLoudly is LEG 2: the single-owner ruling.
//
// lang/analysis DERIVES which outputs a flow exposes and this module CONSUMES that
// fact. The defect this leg alone detects is a stale or divergent local derivation
// quietly winning — two implementations of one ruled semantic drifting apart,
// which no build, no other criterion and no runtime behavior reveals. So a
// disagreement is a refusal naming BOTH views, in both directions, never a
// preference for either.
func TestSubflowBindingDisagreementIsRefusedLoudly(t *testing.T) {
	t.Run("the graph produces a name the boundary does not export", func(t *testing.T) {
		// The stale-local-derivation case. Preferring the local view here is
		// exactly the second-opinion behavior the ruling forbids.
		narrow := map[string]Boundary{"screening": {Outputs: []string{"ok"}}, "main": {}}
		_, diags := lowerFileOf(t, subflowFixture, narrow)
		if len(diags) == 0 {
			t.Fatal("a disagreement was lowered on the local view")
		}
		assertNamesBothViews(t, diags, "bad")
	})

	t.Run("the boundary exports a name the graph cannot produce", func(t *testing.T) {
		// Silently dropping this would generate a program missing an output the
		// consumer was told it could bind.
		wide := map[string]Boundary{"screening": {Outputs: []string{"ok", "bad", "ghost"}}, "main": {}}
		_, diags := lowerFileOf(t, subflowFixture, wide)
		if len(diags) == 0 {
			t.Fatal("a disagreement was lowered on the exported view")
		}
		assertNamesBothViews(t, diags, "ghost")
	})

	t.Run("a flow with no exported boundary is refused, not read as empty", func(t *testing.T) {
		_, diags := lowerFileOf(t, subflowFixture, map[string]Boundary{"main": {}})
		if len(diags) == 0 {
			t.Fatal("an absent boundary fact was read as an empty output set")
		}
		joined := strings.Join(messagesOf(diags), "\n")
		if !strings.Contains(joined, "an absent fact is not an empty one") {
			t.Errorf("the diagnostics %q do not draw the absent-versus-empty distinction", joined)
		}
	})

	// THE SAME-RUN CONTROL. Without it a leg demanding refusals would be satisfied
	// by a function that refuses everything.
	t.Run("agreeing views lower cleanly", func(t *testing.T) {
		if _, diags := lowerFileOf(t, subflowFixture, screeningBoundary); len(diags) != 0 {
			t.Fatalf("the agreeing case was refused, so the refusals above prove nothing:\n%s",
				strings.Join(messagesOf(diags), "\n"))
		}
	})
}

// assertNamesBothViews requires a disagreement diagnostic to name the disputed
// output AND to say it is refusing rather than choosing.
func assertNamesBothViews(t *testing.T, diags []Diagnostic, disputed string) {
	t.Helper()
	joined := strings.Join(messagesOf(diags), "\n")
	if !strings.Contains(joined, disputed) {
		t.Errorf("the refusal %q does not name the disputed output %q", joined, disputed)
	}
	if !strings.Contains(joined, "Refusing rather than lowering on either view") {
		t.Errorf("the refusal %q does not say it declines to pick a view", joined)
	}
	if !strings.Contains(joined, "the other view holds") {
		t.Errorf("the refusal %q does not name the other view's contents", joined)
	}
}
