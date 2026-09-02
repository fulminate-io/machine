// Package loader - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package loader

import (
	"bytes"
	"encoding/gob"
	"go/types"
	"reflect"
	"testing"
)

// mixedMirror mirrors testdata/subject's Mixed so this test can hand a real
// value to a real gob encoder.
//
// The fixture module cannot be imported — it is a separate module under testdata
// — so the mirror is unavoidable, and an unverified mirror would test the
// derivation against this file's ASSUMPTION about the fixture rather than
// against the fixture. The shape control below ties the two together by
// comparing the mirror's exported fields against the loaded type's.
type mixedMirror struct {
	A int
	C chan int
	F func()
}

// TestTheDerivationDisagreesWithGobOnlyWhereGobIsSilent pins the derivation
// against the real codec at both sites.
//
// A TEST THAT ONLY ASSERTED AGREEMENT WOULD BE WORTHLESS HERE. gob is silent on
// the silent-drop class by construction, so a derivation that simply deferred to
// the codec would agree with it everywhere and pass. Proving the derivation is
// STRONGER than the codec requires manufacturing a disagreement and asserting
// both sides of it — which is what the Mixed case does.
func TestTheDerivationDisagreesWithGobOnlyWhereGobIsSilent(t *testing.T) {
	loaded, err := Load(subjectDir, []string{"./..."})
	if err != nil {
		t.Fatalf("the subject fixture module did not load: %v", err)
	}

	// EACH PROBE GETS A FRESH DERIVER. This test pins the derivation's VERDICT
	// at a site; the memo's behaviour under a reused Deriver is a separate
	// property with its own gate. Sharing one Deriver here would make this test
	// order-dependent and would quietly duplicate the memo gate, leaving the
	// memo's real failure mode covered by two tests that fail together instead
	// of by the one built to isolate it.
	for _, probe := range []struct {
		name string
		site Site
		want int
		why  string
	}{
		{"Plain", SiteConcrete, 0, "a plain struct is clean under its own static type"},
		{"Plain", SiteInterface, 1, "a named type in an interface slot must be registered"},
		{"Mixed", SiteConcrete, 2, "PLANTED DIVERGENCE: gob is silent, the derivation is not"},
		{"Nested", SiteConcrete, 2, "the drops are buried one level down, so a top-level-only walk misses them"},
		{"Escaped", SiteConcrete, 0, "the hatch owns the bytes, so the chan beneath it is not a drop"},
		{"Escaped", SiteInterface, 1, "THE HATCH DOES NOT EXEMPT REGISTRATION"},
		{"Collections", SiteConcrete, 4, "a slice, an array, a map value and a map KEY each hide the same drop"},
		{"NoFields", SiteConcrete, 1, "a struct the codec is allowed to see nothing of is refused outright"},
	} {
		typ, err := loaded.Resolve(subjectPath, probe.name)
		if err != nil {
			t.Fatalf("the fixture type %s did not resolve: %v", probe.name, err)
		}

		got := NewDeriver().Serializable(typ, probe.site)
		if len(got) != probe.want {
			t.Errorf("%s at site %d: derivation gave %d findings %v, want %d (%s)",
				probe.name, probe.site, len(got), got, probe.want, probe.why)

			continue
		}

		t.Logf("%s at site %d -> %v", probe.name, probe.site, got)
	}

	t.Run("the mirror really is the fixture", func(t *testing.T) {
		typ, err := loaded.Resolve(subjectPath, "Mixed")
		if err != nil {
			t.Fatalf("Mixed did not resolve: %v", err)
		}

		declared, ok := typ.Underlying().(*types.Struct)
		if !ok {
			t.Fatalf("Mixed is %v, not a struct", typ.Underlying())
		}

		mirror := reflect.TypeOf(mixedMirror{})
		if declared.NumFields() != mirror.NumField() {
			t.Fatalf("the mirror has %d fields and the fixture %d, so the gob half tests the wrong shape",
				mirror.NumField(), declared.NumFields())
		}

		for i := range declared.NumFields() {
			if declared.Field(i).Name() != mirror.Field(i).Name {
				t.Errorf("field %d is %q in the fixture and %q in the mirror",
					i, declared.Field(i).Name(), mirror.Field(i).Name)
			}
		}
	})

	t.Run("gob really does accept the silent-drop class", func(t *testing.T) {
		// THE CONTROL FOR THE PLANTED DIVERGENCE. If gob ever started refusing
		// this class, this fails loudly rather than the test quietly relaxing
		// back into an agreement test.
		var sink bytes.Buffer
		if err := gob.NewEncoder(&sink).Encode(mixedMirror{A: 42}); err != nil {
			t.Fatalf("CONTROL FAILED: gob refused the silent-drop class, so there is no divergence left to plant: %v", err)
		}

		typ, err := loaded.Resolve(subjectPath, "Mixed")
		if err != nil {
			t.Fatalf("Mixed did not resolve: %v", err)
		}

		found := NewDeriver().Serializable(typ, SiteConcrete)
		if len(found) == 0 {
			t.Fatalf("the derivation agreed with gob's silence on the silent-drop class: %d findings", len(found))
		}

		for _, one := range found {
			if one.Reason != ReasonSilentDrop {
				t.Errorf("%s is reported as reason %d, but gob's silence is the silent-drop class", one.Path, one.Reason)
			}
		}

		t.Logf("gob encoded %d bytes without complaint; the derivation reported %v", sink.Len(), found)
	})
}
