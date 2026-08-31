// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"path/filepath"
	"testing"
)

// TestSwitchesArmClassificationAndExhaustiveness covers both checks.
//
// THE THIRD LEG IS THE ONE THAT MATTERS. switch-no-else.flow sits in lang/ast's
// VALID corpus and this analyzer flags it, deliberately: that corpus asserts
// PARSEABILITY, not analysis cleanliness, and the two must not be conflated. A
// future reader who assumes the valid corpus should be analysis-clean finds the
// intent asserted here rather than having to infer it.
func TestSwitchesArmClassificationAndExhaustiveness(t *testing.T) {
	reject := loadSource(t, filepath.Join(sharedContractDir, "destructuring-arm.flow"))
	diags := withCode(analyze(t, SwitchesAnalyzer, reject), SwitchesAnalyzer.Name)
	if len(diags) != 1 {
		t.Fatalf("destructuring-arm.flow produced %d switches diagnostics, want 1: %v", len(diags), messages(diags))
	}
	if !containsAll(diags[0].Message, "destructuring pattern", "matched, not destructured") {
		t.Errorf("the diagnostic does not say why the arm is refused: %s", diags[0].Message)
	}
	t.Logf("destructuring-arm.flow: %v", messages(diags))

	withElse := loadSource(t, filepath.Join(astTestdata, "valid", "switch-with-else.flow"))
	if got := withCode(analyze(t, SwitchesAnalyzer, withElse), SwitchesAnalyzer.Name); len(got) != 0 {
		t.Errorf("switch-with-else.flow produced switches diagnostics: %v", messages(got))
	}

	noElse := loadSource(t, filepath.Join(astTestdata, "valid", "switch-no-else.flow"))
	missing := withCode(analyze(t, SwitchesAnalyzer, noElse), SwitchesAnalyzer.Name)
	if len(missing) != 1 {
		t.Fatalf("switch-no-else.flow produced %d switches diagnostics, want exactly the missing else: %v",
			len(missing), messages(missing))
	}
	if !containsAll(missing[0].Message, "has no else", "cannot prove the arms cover the subject") {
		t.Errorf("the diagnostic does not explain the v1 limitation: %s", missing[0].Message)
	}
	t.Logf("switch-no-else.flow, a VALID parse fixture that is an analysis reject: %v", messages(missing))
}

// TestSwitchesClassifiesEveryArmValue pins the classification the parser handed
// over, which the exhaustiveness legs above never exercise.
//
// switch-with-else.flow carries both kinds in one switch: two quoted literals
// and a call expression.
func TestSwitchesClassifiesEveryArmValue(t *testing.T) {
	src := loadSource(t, filepath.Join(astTestdata, "valid", "switch-with-else.flow"))
	got, _ := resultOf(t, SwitchesAnalyzer, src)
	classes, ok := got.(*ArmClassification)
	if !ok {
		t.Fatalf("the switches analyzer produced %T, want *ArmClassification", got)
	}

	seen := map[string]string{}
	for _, value := range classes.Values {
		seen[value.Text] = value.Class
	}
	if len(seen) == 0 {
		t.Fatal("no arm values were classified, so the assertions below check nothing")
	}
	for text, want := range map[string]string{`"card"`: armLiteral, `"wallet"`: armLiteral, "isRefund(ingest)": armPredicate} {
		if got := seen[text]; got != want {
			t.Errorf("the arm value %s classified as %q, want %q (all: %v)", text, got, want, seen)
		}
	}
	t.Logf("classified arm values: %v", seen)
}

// TestSwitchesIsSilentOnTheCanonicalCorpus records the strawman sweep. No
// strawman uses switch at all, so this analyzer has nothing to say about them —
// worth asserting rather than assuming, since a rule that fired on one would be
// evidence rather than noise.
func TestSwitchesIsSilentOnTheCanonicalCorpus(t *testing.T) {
	for name, diags := range sweepCorpus(t, SwitchesAnalyzer, strawmanDir) {
		if len(diags) != 0 {
			t.Errorf("strawman %s produced switches diagnostics: %v", name, messages(diags))
		}
	}
}
