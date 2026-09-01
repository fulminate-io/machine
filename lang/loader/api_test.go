// Package loader - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package loader

import (
	"go/types"
	"strings"
	"testing"
)

const (
	subjectDir  = "testdata/subject"
	subjectPath = "example.com/subject"
	brokenDir   = "testdata/broken"
)

// TestTheLoadingSurfaceRefusesLoudlyAndSplitsReceivers exercises the four
// behaviours the assembler and lane F build on, against real modules on disk.
//
// It is one test rather than four because the legs share an expensive load and
// because leg 3's control — the CLEAN module reporting zero problems in the same
// run as the broken one reports some — is only a control if both are measured
// together. A run where the clean module were absent could not distinguish a
// working diagnostic surface from one that always answers.
func TestTheLoadingSurfaceRefusesLoudlyAndSplitsReceivers(t *testing.T) {
	t.Run("an empty pattern set is refused rather than loaded as nothing", func(t *testing.T) {
		if _, err := Load(subjectDir, nil); err == nil {
			t.Fatal("Load accepted an empty pattern set instead of refusing it")
		}

		if _, err := Load(subjectDir, []string{}); err == nil {
			t.Fatal("Load accepted an empty pattern slice instead of refusing it")
		}
	})

	loaded, err := Load(subjectDir, []string{"./..."})
	if err != nil {
		t.Fatalf("the clean fixture module did not load: %v", err)
	}

	t.Run("what cannot be resolved is refused by name", func(t *testing.T) {
		unloaded, err := loaded.Resolve("example.com/never-loaded", "Payload")
		if err == nil {
			t.Fatal("Resolve invented an answer for a package that was never loaded")
		}

		if unloaded != nil {
			t.Fatalf("Resolve refused but still handed back a type: %v", unloaded)
		}

		if !strings.Contains(err.Error(), "example.com/never-loaded") {
			t.Fatalf("the refusal does not name the package it could not find: %v", err)
		}

		absent, err := loaded.Resolve(subjectPath, "NoSuchTypeHere")
		if err == nil {
			t.Fatal("Resolve invented an answer for a spelling that denotes no type")
		}

		if absent != nil {
			t.Fatalf("Resolve refused but still handed back a type: %v", absent)
		}

		if !strings.Contains(err.Error(), "NoSuchTypeHere") {
			t.Fatalf("the refusal does not name the spelling it could not resolve: %v", err)
		}
	})

	t.Run("a package that does not type-check yields positioned diagnostics", func(t *testing.T) {
		if clean := loaded.Errors(); len(clean) != 0 {
			t.Fatalf("the clean fixture module reported %d type-check problems: %v", len(clean), clean)
		}

		damaged, err := Load(brokenDir, []string{"./..."})
		if err != nil {
			t.Fatalf("the broken fixture module failed to LOAD, which is a different failure: %v", err)
		}

		found := damaged.Errors()
		if len(found) == 0 {
			t.Fatal("a package that does not type-check reported no diagnostics at all")
		}

		first := found[0]
		t.Logf("type-check diagnostic: %s:%d:%d: %s", first.Path, first.Pos.Line, first.Pos.Col, first.Message)

		if !strings.HasSuffix(first.Path, "broken.go") {
			t.Fatalf("the diagnostic does not name the offending file: %q", first.Path)
		}

		if first.Pos.Line == 0 || first.Pos.Col == 0 {
			t.Fatalf("the diagnostic carries no position: line=%d col=%d", first.Pos.Line, first.Pos.Col)
		}
	})

	t.Run("the value and pointer method sets are two separate answers", func(t *testing.T) {
		scope, ok := loaded.Scope(subjectPath)
		if !ok {
			t.Fatalf("the subject package %q was not loaded, so an answer about it would prove nothing", subjectPath)
		}

		if scope.Lookup("Payload") == nil {
			t.Fatal("the subject package does not declare Payload, so a false answer could come from an empty scope")
		}

		payload, err := loaded.Resolve(subjectPath, "Payload")
		if err != nil {
			t.Fatalf("the subject type did not resolve: %v", err)
		}

		encoder := iface(t, loaded, "gob.GobEncoder")
		decoder := iface(t, loaded, "gob.GobDecoder")

		if value, pointer := Carries(payload, encoder); !value || !pointer {
			t.Fatalf("GobEncode has a VALUE receiver so both sets carry it: value=%v pointer=%v", value, pointer)
		}

		if value, pointer := Carries(payload, decoder); value || !pointer {
			t.Fatalf("GobDecode has a POINTER receiver so only the pointer set carries it: value=%v pointer=%v", value, pointer)
		}
	})
}

// iface resolves an interface by its spelling in the subject package's own
// scope, so the test measures the loader's own resolution rather than an
// interface it hand-built and then agreed with.
func iface(t *testing.T, loaded *Packages, spelling string) *types.Interface {
	t.Helper()

	resolved, err := loaded.Resolve(subjectPath, spelling)
	if err != nil {
		t.Fatalf("%s did not resolve: %v", spelling, err)
	}

	under, ok := resolved.Underlying().(*types.Interface)
	if !ok {
		t.Fatalf("%s resolved to %v, which is not an interface", spelling, resolved)
	}

	return under
}
