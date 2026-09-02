// Package loader - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package loader

import (
	"bytes"
	"encoding/gob"
	"strings"
	"testing"
)

// mirrorPlain mirrors the fixture's named struct. The fixture module is separate
// and cannot be imported, and gob's rule reads the SHAPE, so a mirror of the same
// shape draws the same verdict from the codec.
//
// IT IS THE ONLY NAMED MIRROR, AND THAT IS LOAD-BEARING. Every other row samples
// an UNNAMED composite literal, because a named wrapper is refused by gob for
// BEING NAMED rather than for its shape — which would make a composite row pass
// for the wrong reason and, worse, make the bootstrapped rows fail. The control
// in the table caught exactly that when these were first written as named types.
type mirrorPlain struct{ A int }

// TestRegistrationAgreesWithRealGobAtInterfacePosition compares the derivation's
// registration verdict against the LIVE CODEC, one row per shape class.
//
// THE AGREEMENT IS MEASURED, NOT ASSUMED. Every row encodes its own sample
// through an interface slot — `Encode(&v)` where v is `any`, which is the shape a
// state entry actually reaches — and compares that outcome against what the walk
// says. A row whose stated premise disagrees with the codec fails as a CONTROL
// naming itself, so the fixture cannot drift away from gob's real behaviour
// without saying so.
//
// THE PLANTED DISAGREEMENT IS WITH THE NAIVE READING, NOT WITH GOB. A walk that
// descends into a composite without asking whether the composite ITSELF is in the
// interface slot agrees with gob on the two bootstrapped rows and is silently
// wrong on every other one. So agreement across ALL classes is what
// discriminates, and a table narrowed to the easy rows would pass while proving
// nothing.
func TestRegistrationAgreesWithRealGobAtInterfacePosition(t *testing.T) {
	loaded, err := Load(subjectDir, []string{"./..."})
	if err != nil {
		t.Fatalf("the subject fixture module did not load: %v", err)
	}

	const (
		classBootstrapped = "bootstrapped"
		classComposite    = "composite"
		classNamed        = "named"
		classRefused      = "already-refused"
	)

	rows := []struct {
		class      string
		fixture    string
		underlying bool
		sample     any
		wantNeeds  bool
	}{
		{classBootstrapped, "Counter", true, int(1), false},
		{classBootstrapped, "IntSlice", true, []int{1}, false},

		{classComposite, "ByType", true, map[string]int{"a": 1}, true},
		{classComposite, "Seen", true, map[string]bool{"a": true}, true},
		{classComposite, "IntArray", true, [3]int{1, 2, 3}, true},
		{classComposite, "NestedSlice", true, [][]int{{1}}, true},
		{classComposite, "PlainSlice", true, []mirrorPlain{{A: 1}}, true},

		{classNamed, "Plain", false, mirrorPlain{A: 1}, true},

		{classRefused, "Signal", true, nil, false},
	}

	counts := map[string]int{}

	for _, row := range rows {
		typ, err := loaded.Resolve(subjectPath, row.fixture)
		if err != nil {
			t.Fatalf("the fixture %s did not resolve: %v", row.fixture, err)
		}

		subject := typ
		if row.underlying {
			subject = typ.Underlying()
		}

		found := NewDeriver().Serializable(subject, SiteInterface)

		needs, drops := false, false

		for _, one := range found {
			switch one.Reason {
			case ReasonNeedsRegistration:
				needs = true
			case ReasonSilentDrop:
				drops = true
			case ReasonNoExportedFields, ReasonDepthExceeded:
			}
		}

		if row.class == classRefused {
			// gob refuses a chan for a reason that is NOT registration, so this
			// row asserts the arm added nothing rather than comparing verdicts.
			if needs {
				t.Errorf("%s (%s): the registration arm fired on a type already refused as a silent drop, which is a second reason for the same refusal",
					row.fixture, subject)
			}

			if !drops {
				t.Errorf("%s (%s): expected the silent-drop refusal that makes the registration arm's silence correct, got %v",
					row.fixture, subject, found)
			}

			counts[row.class]++

			continue
		}

		refuses, because := gobRefusesAtInterface(row.sample)

		// CONTROL: the row's stated premise must match the live codec, or the
		// fixture has drifted and every verdict below it is meaningless.
		if refuses != row.wantNeeds {
			t.Fatalf("CONTROL FAILED for %s (%s): this row asserts gob refuses=%v but the codec says %v (%v) — the fixture has drifted from gob",
				row.fixture, subject, row.wantNeeds, refuses, because)
		}

		if needs != refuses {
			t.Errorf("%s (%s): derivation says needs-registration=%v, real gob refuses=%v — the derivation disagrees with the codec",
				row.fixture, subject, needs, refuses)
		}

		counts[row.class]++
	}

	for _, class := range []string{classBootstrapped, classComposite, classNamed, classRefused} {
		if counts[class] == 0 {
			t.Errorf("no %s rows ran, so that shape class is unproved", class)

			continue
		}

		t.Logf("registration class agrees with gob: %s, %d rows", class, counts[class])
	}
}

// gobRefusesAtInterface reports whether the codec refuses a value for want of
// registration when it crosses an INTERFACE slot, which is the shape a state
// entry reaches — `Encode(&v)` with v declared as any.
//
// It distinguishes a registration refusal from every other encoding error, so a
// row cannot pass by provoking gob into failing for an unrelated reason.
func gobRefusesAtInterface(sample any) (bool, error) {
	var sink bytes.Buffer

	err := gob.NewEncoder(&sink).Encode(&sample)
	if err == nil {
		return false, nil
	}

	return strings.Contains(err.Error(), "not registered for interface"), err
}
