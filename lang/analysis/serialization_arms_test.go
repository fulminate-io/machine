// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/loader"
)

// unknownReason is a loader.Reason value this contract does not know, standing
// in for the NEXT value the separately-versioned loader adds. It has already
// grown one.
const unknownReason = loader.Reason(99)

// TestEveryReasonReachesItsOwnChannel drives all five arms and proves they are
// five channels rather than one.
//
// THE CLASS TOKENS ARE ASSERTED MUTUALLY DISTINCT BEFORE THEY ARE USED. Four of
// the five arms report at SeverityError with zero requirements, so severity and
// the requirement count cannot separate them — without a distinct class per arm
// the contract's central claim would be observable only in prose.
func TestEveryReasonReachesItsOwnChannel(t *testing.T) {
	reasons := []loader.Reason{
		loader.ReasonSilentDrop,
		loader.ReasonNoExportedFields,
		loader.ReasonNeedsRegistration,
		loader.ReasonDepthExceeded,
		unknownReason,
	}

	seen := map[string]loader.Reason{}

	for _, reason := range reasons {
		class := verdictFor(reason).class
		if prior, duplicate := seen[class]; duplicate {
			t.Fatalf("reasons %d and %d share the class %q, so they are one channel and not two", prior, reason, class)
		}

		seen[class] = reason
	}

	if len(seen) != len(reasons) {
		t.Fatalf("the five arms produced %d distinct classes, want %d", len(seen), len(reasons))
	}

	for _, reason := range reasons {
		reportOneArm(t, reason)
	}
}

// reportOneArm routes one synthetic finding and logs the channel it reached.
//
// The finding is synthetic because a loader.Reason this contract does not know
// cannot be provoked from a fixture: it does not exist in the loader yet, which
// is the whole reason the default arm is mandatory.
func reportOneArm(t *testing.T, reason loader.Reason) {
	t.Helper()

	run := newSerializationRun(nil, serializationPkg)
	decl := declaredType{
		flow: "Screening", kind: kindStateField, name: "tally",
		spelling: "Mixed", boundary: stateBoundary,
	}

	reported := run.diagnose(decl, loader.Finding{Path: ".C", Type: "chan int", Reason: reason})
	routed := verdictFor(reason)

	t.Logf("reason %d -> severity=%s requirements=%d class=%s", reason, reported.Severity, len(run.required), routed.class)

	if reason == loader.ReasonNeedsRegistration && len(run.required) != 1 {
		t.Errorf("the registration arm recorded %d requirements, want 1; a requirement the generator never "+
			"reads is a refusal wearing a hint's severity", len(run.required))
	}

	if reason == loader.ReasonDepthExceeded && !strings.Contains(reported.Message, strconv.Itoa(loader.MaxDepth)) {
		t.Errorf("the depth arm does not name the bound it was refused by: %s", reported.Message)
	}
}

// TestTheAnalyzerRefusesWithoutAPackageSet gates BOTH boundaries the shared
// sentinel has to survive.
//
// The pair is what distinguishes a broken analyzer from a broken seam. A SECOND
// sentinel produces a byte-identical message and breaks errors.Is at the
// analyzer; a driver that concatenates rather than wrapping produces a
// byte-identical message and breaks it at the seam. Neither is visible in the
// text, so only errors.Is at both boundaries can see either.
func TestTheAnalyzerRefusesWithoutAPackageSet(t *testing.T) {
	src := loadSource(t, filepath.Join(serializationDir, "site-dependence.flow"))

	refusals := []struct {
		label    string
		analyzer *Analyzer
	}{
		{"serialization", SerializationAnalyzer(nil, serializationPkg)},
		{"type inference", TypeInferenceAnalyzer(nil)},
	}

	for _, one := range refusals {
		_, err := one.analyzer.Run(&Pass{})
		if err == nil {
			t.Fatalf("%s accepted a nil package set", one.label)
		}

		t.Logf("no package set: %s -> %q errors.Is=%t", one.label, err.Error(), errors.Is(err, errNoPackages))

		if !errors.Is(err, errNoPackages) {
			t.Errorf("%s does not carry the shared errNoPackages sentinel", one.label)
		}
	}

	for _, one := range refusals {
		_, err := Run([]Source{src}, []*Analyzer{one.analyzer})
		if err == nil {
			t.Fatalf("the driver accepted %s with a nil package set", one.label)
		}

		t.Logf("through the driver: %s -> errors.Is=%t", one.label, errors.Is(err, errNoPackages))

		if !errors.Is(err, errNoPackages) {
			t.Errorf("the sentinel does not survive the driver, so a caller cannot compare it: %v", err)
		}
	}
}

// TestTheAnalyzerStaysOutOfTheRegisteredSet gates the registration ban.
//
// A registered analyzer receives its Pass from the driver, which has no channel
// for a caller-supplied *loader.Packages — so an entry in analyzers.go's init
// would produce an analyzer that always refuses, and the ban is structural
// rather than stylistic.
func TestTheAnalyzerStaysOutOfTheRegisteredSet(t *testing.T) {
	registered := All()
	if len(registered) == 0 {
		t.Fatal("CONTROL FAILED: the registry is empty, so this gate would pass vacuously")
	}

	names := make([]string, 0, len(registered))

	for _, a := range registered {
		names = append(names, a.Name)

		if a.Name == serializationName {
			t.Errorf("the serialization analyzer is registered, so the driver would build its Pass and it " +
				"could never receive a package set")
		}
	}

	t.Logf("All() reports %d analyzers and none of them serialization: %s", len(names), strings.Join(names, ", "))
}
