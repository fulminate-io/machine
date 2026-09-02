// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"path/filepath"
	"testing"
)

// reachabilityDir holds this module's own orphan-shape fixtures.
//
// They live here rather than in lang/ast because no canonical fixture carries
// the shape any more: toy.flow's orphaned retry chain was a real wiring defect
// and was repaired, and a rule with no fixture is a rule nobody notices
// breaking.
var reachabilityDir = filepath.Join("testdata", "reachability")

// TestReachabilityFlagsDeadNodesAndSparesStrawmen covers two of the rule's three
// outcomes plus the corpus sweep.
//
// The first fixture is the case a "any targeted loop label is a root" shortcut
// gets wrong: the label IS targeted, but only from inside the cycle it feeds.
// The second is the untargeted label, which is the shape the canonical corpus
// carried before its repair.
func TestReachabilityFlagsDeadNodesAndSparesStrawmen(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		dead    []string
	}{
		{fixture: "unreachable-send-cycle.flow", dead: []string{"retry", "redo", "redo"}},
		{fixture: "untargeted-loop-chain.flow", dead: []string{"stranded", "lonely", "out"}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			src := loadSource(t, filepath.Join(reachabilityDir, tc.fixture))
			diags := errorsIn(withCode(analyze(t, ReachabilityAnalyzer, src), ReachabilityAnalyzer.Name))
			if len(diags) != len(tc.dead) {
				t.Fatalf("got %d dead-node errors, want %d: %v", len(diags), len(tc.dead), messages(diags))
			}
			for i, d := range diags {
				if !containsAll(d.Message, "no data can reach the", tc.dead[i]) {
					t.Errorf("dead-node error %d does not name %s: %s", i, tc.dead[i], d.Message)
				}
			}
			t.Logf("%s: %v", tc.fixture, messages(diags))
		})
	}

	// THE STRAWMAN SWEEP. Against the PRE-AMENDMENT toy this rule reds all
	// three, which is what surfaced the wiring defect in the first place. A
	// SeverityError here at the amended tree is either a second real defect or a
	// mis-specified rule, and neither is a number to adjust.
	for name, diags := range sweepCorpus(t, ReachabilityAnalyzer, strawmanDir) {
		if errs := errorsIn(diags); len(errs) != 0 {
			t.Errorf("strawman %s produced reachability ERRORS: %v", name, messages(errs))
		}
		t.Logf("strawman %s reachability: %v", name, messages(diags))
	}
}

// TestReachabilityRootsFollowTargetedLoopLabels is the rule's third outcome, and
// the leg a single-pass implementation fails.
//
// The send that targets the label sits BELOW it, so a walk in declaration order
// meets the label while its name is still unavailable. Only a fixpoint promotes
// the label after the send is itself shown reachable.
func TestReachabilityRootsFollowTargetedLoopLabels(t *testing.T) {
	src := loadSource(t, filepath.Join(reachabilityDir, "downstream-send-targets-loop.flow"))
	diags := withCode(analyze(t, ReachabilityAnalyzer, src), ReachabilityAnalyzer.Name)
	if errs := errorsIn(diags); len(errs) != 0 {
		t.Errorf("a loop targeted by a reachable downstream send was reported dead: %v", messages(errs))
	}

	// THE KNOWN POSITIVE, in the same run. Without it, "no errors" is equally
	// satisfied by an analyzer that never reports anything at all.
	orphan := loadSource(t, filepath.Join(reachabilityDir, "untargeted-loop-chain.flow"))
	control := errorsIn(withCode(analyze(t, ReachabilityAnalyzer, orphan), ReachabilityAnalyzer.Name))
	if len(control) == 0 {
		t.Fatal("the known positive produced no dead-node error, so the silence above proves nothing")
	}
	t.Logf("known positive still fires: %v", messages(control))
}

// TestReachabilityHintsTheUnconsumedOutput pins the ruled hint severity, and
// pins toy.flow's `archive` as the corpus's single unconsumed producer.
//
// The severity is the assertion that matters: at error severity this would red a
// canonical program, and the ruling put it at hint precisely so it does not.
func TestReachabilityHintsTheUnconsumedOutput(t *testing.T) {
	src := loadSource(t, filepath.Join(strawmanDir, "toy.flow"))
	diags := withCode(analyze(t, ReachabilityAnalyzer, src), ReachabilityAnalyzer.Name)
	if len(diags) != 1 {
		t.Fatalf("toy.flow produced %d reachability diagnostics, want exactly the archive hint: %v",
			len(diags), messages(diags))
	}
	if diags[0].Severity != SeverityHint {
		t.Errorf("the unconsumed-output diagnostic carries severity %s, want hint", diags[0].Severity)
	}
	if !containsAll(diags[0].Message, "archive", "no statement in flow orders consumes") {
		t.Errorf("the hint does not name the unconsumed producer: %s", diags[0].Message)
	}

	// The other two strawmen carry none, which is what makes toy's the corpus's
	// only one rather than one of several.
	for _, name := range []string{"enrichment.flow", "payments.flow"} {
		other := loadSource(t, filepath.Join(strawmanDir, name))
		if got := withCode(analyze(t, ReachabilityAnalyzer, other), ReachabilityAnalyzer.Name); len(got) != 0 {
			t.Errorf("%s produced reachability diagnostics: %v", name, messages(got))
		}
	}
}

// TestReachabilityDoesNotHintADeclaredSignatureOutput pins the correction the
// standing corpus sweep forced.
//
// A flow signature's declared outputs are consumed by the CALLER, so an analysis
// looking only inside the flow must not report them as dead ends. Before this,
// lang/ast's VALID subflow-and-use.flow attracted two hints naming `ok` and
// `bad` — its own declared outputs, and the entire point of declaring them.
//
// The fixture carries a KNOWN POSITIVE in the same file: `stray` is neither
// declared nor read, so it still draws a hint. Without it, this test would pass
// against an analyzer that stopped hinting inside signature flows altogether.
func TestReachabilityDoesNotHintADeclaredSignatureOutput(t *testing.T) {
	src := loadSource(t, filepath.Join(reachabilityDir, "declared-output-and-stray.flow"))
	diags := withCode(analyze(t, ReachabilityAnalyzer, src), ReachabilityAnalyzer.Name)

	if len(diags) != 1 {
		t.Fatalf("got %d reachability diagnostics, want exactly the stray hint: %v", len(diags), messages(diags))
	}
	if !containsAll(diags[0].Message, "stray") {
		t.Errorf("the single hint does not name stray: %s", diags[0].Message)
	}
	for _, d := range diags {
		for _, declared := range []string{" ok,", " bad,"} {
			if containsAll(d.Message, declared) {
				t.Errorf("a declared signature output was reported as unconsumed: %s", d.Message)
			}
		}
	}

	// The fixture this correction came from, asserted directly so the collision
	// cannot come back unnoticed.
	valid := loadSource(t, filepath.Join(astTestdata, "valid", "subflow-and-use.flow"))
	if got := withCode(analyze(t, ReachabilityAnalyzer, valid), ReachabilityAnalyzer.Name); len(got) != 0 {
		t.Errorf("subflow-and-use.flow is a VALID fixture but produced reachability diagnostics: %v", messages(got))
	}
}

// TestReachabilityAcceptsASignatureFlowWithNoSource pins that a flow whose only
// entry is its declared signature is not reported dead.
//
// A flow with a signature declares no source: FlowSignature's own documentation
// says "the body consumes an implicit `in`". A root rule reading only SourceStmt
// would call every statement in such a flow unreachable, and subflow-and-use.flow
// is in lang/ast's VALID corpus.
func TestReachabilityAcceptsASignatureFlowWithNoSource(t *testing.T) {
	src := loadSource(t, filepath.Join(astTestdata, "valid", "subflow-and-use.flow"))
	diags := errorsIn(withCode(analyze(t, ReachabilityAnalyzer, src), ReachabilityAnalyzer.Name))
	if len(diags) != 0 {
		t.Errorf("subflow-and-use.flow is a VALID fixture but produced reachability errors: %v", messages(diags))
	}
}
