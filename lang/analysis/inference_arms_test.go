// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestInferenceRefusalArms covers every way the inference declines to run.
//
// EACH ARM IS A STOP RATHER THAN A QUIET EMPTY RESULT. A missing prerequisite or
// an absent package set means the caller wired something wrong, and an inference
// that answered "nothing to report" under either condition would report a clean
// program about code it never looked at.
func TestInferenceRefusalArms(t *testing.T) {
	src := loadSource(t, filepath.Join(inferenceDir, "Screening.flow"))

	t.Run("BuildInferredTypes refuses a nil package set", func(t *testing.T) {
		table, diags, err := BuildInferredTypes([]Source{src}, nil)
		if err == nil {
			t.Fatal("BuildInferredTypes accepted a nil package set")
		}
		if !errors.Is(err, errNoPackages) {
			t.Errorf("the refusal is %v, want errNoPackages", err)
		}
		if table != nil || diags != nil {
			t.Errorf("BuildInferredTypes refused but handed back table %v and diagnostics %v", table, diags)
		}
	})

	t.Run("a source carrying no parsed tree is refused by the driver", func(t *testing.T) {
		untreed := Source{Path: "notree.flow", Src: src.Src}
		if _, _, err := BuildInferredTypes([]Source{untreed}, loadInferenceSubject(t)); err == nil {
			t.Fatal("a source with no parsed tree was accepted")
		}
	})

	// THE CONSTRUCTED ANALYZER REFUSES TOO, not just the convenience entry point:
	// TypeInferenceAnalyzer is exported and a caller may hand it straight to Run.
	t.Run("the analyzer itself refuses a nil package set", func(t *testing.T) {
		if _, err := Run([]Source{src}, []*Analyzer{TypeInferenceAnalyzer(nil)}); err == nil {
			t.Fatal("the constructed analyzer accepted a nil package set")
		}
	})

	t.Run("a missing symbols result is a stop", func(t *testing.T) {
		if _, err := runInference(&Pass{ResultOf: map[*Analyzer]any{}}, loadInferenceSubject(t)); err == nil {
			t.Fatal("the inference ran without a symbols result")
		} else if !errors.Is(err, errNoSymbols) {
			t.Errorf("the refusal is %v, want errNoSymbols", err)
		}
	})

	t.Run("a missing flowgraph result is a stop", func(t *testing.T) {
		pass := &Pass{ResultOf: map[*Analyzer]any{SymbolsAnalyzer: &SymbolTable{}}}
		if _, err := runInference(pass, loadInferenceSubject(t)); err == nil {
			t.Fatal("the inference ran without a flowgraph result")
		} else if !errors.Is(err, errNoGraphs) {
			t.Errorf("the refusal is %v, want errNoGraphs", err)
		}
	})
}

// TestInferenceLeavesUnbindableAndUnreferencedThingsAlone covers the answers the
// table gives about things the run never saw, and the import that binds nothing.
//
// AN HONEST ABSENCE IS THE POINT. Every one of these could be answered with a
// zero value beside a true, and a caller could not tell that shape from a real
// answer.
func TestInferenceLeavesUnbindableAndUnreferencedThingsAlone(t *testing.T) {
	table, diags := inferFixture(t, "unloaded-import.flow")

	if len(diags) == 0 {
		t.Error("an import naming a package that was never loaded produced no diagnostic at all")
	}
	if got, known := table.Name("unloaded", "ingest"); known {
		t.Errorf("a reference through an unloaded import was typed %v instead of refused", got)
	}

	t.Run("Flows lists only what the run saw", func(t *testing.T) {
		for _, flow := range table.Flows() {
			if flow != "unloaded" {
				t.Errorf("the table lists a flow %q the run never inferred", flow)
			}
		}
	})

	t.Run("Flow and Name answer honestly for what the run never saw", func(t *testing.T) {
		if names, ok := table.Flow("no-such-flow"); ok {
			t.Errorf("Flow invented an entry for a flow the run never saw: %v", names)
		}
		if got, ok := table.Name("no-such-flow", "ingest"); ok {
			t.Errorf("Name invented %v for a flow the run never saw", got)
		}
		if got, ok := table.Name("unloaded", "no-such-name"); ok {
			t.Errorf("Name invented %v for a name the flow never produced", got)
		}
	})
}

// TestInferenceBindsAnAliasedImportAndPassesThroughEveryRoutingShape covers the
// alias path and every routing shape that names no reference.
//
// ITS LAST LEG PINS THE FIXED POINT SPECIFICALLY. `spread` consumes the loop
// label and is declared ABOVE the send that feeds it, so a single forward pass in
// statement order leaves its outputs untyped and only an iterating walk types
// them. That is the one property no other test in this package would catch.
func TestInferenceBindsAnAliasedImportAndPassesThroughEveryRoutingShape(t *testing.T) {
	table, diags := inferFixture(t, "aliased-and-routing.flow")

	if len(diags) != 0 {
		t.Errorf("the aliased routing fixture produced diagnostics: %v", messages(diags))
	}

	// THE ALIAS: the package's own declared name is `orders`, and this file binds
	// it as `ord`. A consumer binding by the declared name would resolve nothing.
	assertInferred(t, table, "routes", "ingest", inferencePkg+".Order")
	assertInferred(t, table, "routes", "scored", inferencePkg+".Scored")

	// TEE passes its payload to every target.
	assertInferred(t, table, "routes", "keep", inferencePkg+".Scored")
	assertInferred(t, table, "routes", "toss", inferencePkg+".Scored")

	// SEND carries its source's type into the loop label declared above it.
	assertInferred(t, table, "routes", "retry", inferencePkg+".Scored")

	// THE FIXED POINT: typed only on a later round, because `spread` is above the
	// send that types `retry`.
	assertInferred(t, table, "routes", "again", inferencePkg+".Scored")
	assertInferred(t, table, "routes", "later", inferencePkg+".Scored")
}

// TestInferenceEdgeArmsOverRealSources covers three arms that each look like the
// absence of a check and are not.
func TestInferenceEdgeArmsOverRealSources(t *testing.T) {
	t.Run("an unresolvable declared output type is not a disagreement", func(t *testing.T) {
		_, diags := inferFixture(t, "unresolvable-declared-output.flow")
		for _, d := range errorsIn(diags) {
			if containsAll(d.Message, "declares the output") {
				t.Errorf("a declared type that does not resolve was reported as a disagreement: %s", d.Message)
			}
		}
	})

	t.Run("a bare reference naming no file-level func is reported rather than typed", func(t *testing.T) {
		table, diags := inferFixture(t, "bare-unknown.flow")
		if len(diags) != 1 {
			t.Fatalf("the unknown bare reference produced %d diagnostics, want 1: %v", len(diags), messages(diags))
		}
		if !containsAll(diags[0].Message, "NotAFileLevelFunc") {
			t.Errorf("the refusal does not name the reference: %s", diags[0].Message)
		}
		if got, known := table.Name("bareunknown", "scored"); known {
			t.Errorf("an unresolvable bare reference was typed %v", got)
		}
	})

	// THE ACCEPTING CONTROL for the disagreeing fan-in: two edges of ONE identity
	// are one identity, so the join is silent. Without this arm, "reports a
	// disagreement" would be satisfied by an implementation reporting every join.
	t.Run("a fan-in of two identical types is one identity and stays silent", func(t *testing.T) {
		_, diags := inferFixture(t, "identical-fan-in.flow")
		if len(diags) != 0 {
			t.Errorf("a fan-in whose inputs carry the same type was reported: %v", messages(diags))
		}
	})
}

// TestInferenceDistinguishesNothingCarriedFromNothingResolved covers the two arms
// that both leave a name untyped and mean opposite things.
//
// A REFERENCE THAT RESOLVES AND CARRIES NOTHING IS A CLEAN ANSWER; a reference
// that fails to resolve is a REFUSAL. Conflating them would either report a
// perfectly good error-returning reference as broken, or swallow a genuinely
// unresolvable one — and both leave the same empty entry in the table, so only a
// test that looks at the DIAGNOSTICS can tell them apart.
func TestInferenceDistinguishesNothingCarriedFromNothingResolved(t *testing.T) {
	table, diags := inferFixture(t, "error-only-ref.flow")

	if len(diags) != 0 {
		t.Errorf("a reference that resolves but yields only an error was reported: %v", messages(diags))
	}
	if got, known := table.Name("onlyerr", "failed"); known {
		t.Errorf("a reference carrying only an error typed its node %v", got)
	}

	// THE KNOWN POSITIVE, same run shape: the node whose reference DOES carry a
	// datum is typed, so this is the error result being skipped rather than the
	// inference having stopped.
	assertInferred(t, table, "onlyerr", "ingest", inferencePkg+".Order")
}

// TestInferenceSkipsADeclaredOutputTheBodyNeverProduces pins that the agreement
// check compares only what it actually has.
//
// A declared output no statement delivers is the SIGNATURE analyzer's report, not
// the inference's; comparing a declared type against an absent one here would
// invent a disagreement out of a missing value.
func TestInferenceSkipsADeclaredOutputTheBodyNeverProduces(t *testing.T) {
	table, diags := inferFixture(t, "declared-output-not-produced.flow")

	if len(errorsIn(diags)) != 0 {
		t.Errorf("a declared output the body never produces was reported by the inference: %v", messages(diags))
	}
	if got, known := table.Name("Missing", "ghost"); known {
		t.Errorf("an output no statement produces was typed %v", got)
	}

	// THE KNOWN POSITIVE: the output the body DOES produce is still typed and
	// still agreement-checked, so the skip above is scoped rather than total.
	assertInferred(t, table, "Missing", "scored", inferencePkg+".Scored")
}

// TestInferenceReportsAnUnresolvableReferenceOnEveryShapeThatCarriesOne pins that
// RESOLUTION and APPLICATION are separate questions.
//
// A branch routes on a predicate and a sink ends the line, so neither APPLIES its
// reference for typing — but both name real Go, and a reference that does not
// resolve is a defect whatever shape carries it. An implementation that resolved
// only the applying shapes would stay silent here, which is how a typo in a branch
// predicate ships.
//
// THE SECOND HALF IS THE ONE THAT WOULD BE EASY TO GET WRONG: a refusal on the
// branch must NOT poison the dataflow. Its targets still carry the branch's input
// type, because the predicate was never what typed them.
func TestInferenceReportsAnUnresolvableReferenceOnEveryShapeThatCarriesOne(t *testing.T) {
	table, diags := inferFixture(t, "unresolvable-sink-and-branch.flow")

	if len(diags) != 2 {
		t.Fatalf("an unresolvable branch predicate and sink writer produced %d diagnostics, want 2: %v",
			len(diags), messages(diags))
	}

	for _, want := range []string{"NoSuchPredicate", "NoSuchWriter"} {
		found := false
		for _, d := range diags {
			if containsAll(d.Message, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no diagnostic names the unresolvable reference %s: %v", want, messages(diags))
		}
	}

	// THE REFUSAL DOES NOT POISON THE DATAFLOW: the branch's targets still carry
	// the type flowing INTO the branch.
	assertInferred(t, table, "badrefs", "good", inferencePkg+".Order")
	assertInferred(t, table, "badrefs", "bad", inferencePkg+".Order")
}
