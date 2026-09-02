// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"strings"
	"testing"
)

// isolateRegistry swaps in an empty registry for the duration of one test and
// restores the real one afterwards.
//
// Registration has no undo, so a test that registered into the live registry
// would leave its throwaway analyzers visible to All() for the rest of the
// binary — and the contract suite asks All() what the module ships.
func isolateRegistry(t *testing.T) {
	t.Helper()

	registryMu.Lock()
	savedRegistry, savedNames := registry, registered
	registry, registered = nil, map[string]bool{}
	registryMu.Unlock()

	t.Cleanup(func() {
		registryMu.Lock()
		defer registryMu.Unlock()
		registry, registered = savedRegistry, savedNames
	})
}

// TestRegisterKeepsRegistrationOrder pins that All reports what was registered,
// in the order it was registered.
func TestRegisterKeepsRegistrationOrder(t *testing.T) {
	isolateRegistry(t)

	var seen []string
	third := recorder("probe-third", &seen)
	first := recorder("probe-first", &seen)
	second := recorder("probe-second", &seen)
	Register(third)
	Register(first)
	Register(second)

	var got []string
	for _, a := range All() {
		got = append(got, a.Name)
	}
	if want := "probe-third probe-first probe-second"; strings.Join(got, " ") != want {
		t.Errorf("All() reported [%s], want [%s]", strings.Join(got, " "), want)
	}
}

// TestAllReturnsACopy pins that a caller reordering the returned slice cannot
// reorder the registry.
func TestAllReturnsACopy(t *testing.T) {
	isolateRegistry(t)

	var seen []string
	Register(recorder("probe-one", &seen))
	Register(recorder("probe-two", &seen))

	taken := All()
	taken[0], taken[1] = taken[1], taken[0]

	if again := All(); again[0].Name != "probe-one" {
		t.Errorf("reordering All()'s result reordered the registry: first is now %s", again[0].Name)
	}
}

// TestRegisterRefusesADuplicateName pins that a second analyzer under one name
// stops rather than shadowing the first — a Code that identified two rules would
// make suppression ambiguous.
func TestRegisterRefusesADuplicateName(t *testing.T) {
	isolateRegistry(t)

	var seen []string
	Register(recorder("probe-dup", &seen))

	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("Register accepted a duplicate name")
		}
		if msg, ok := got.(string); !ok || !strings.Contains(msg, "probe-dup") {
			t.Errorf("the panic does not name the duplicate: %v", got)
		}
	}()
	Register(recorder("probe-dup", &seen))
}

// TestRegisterRefusesAnUnusableAnalyzer pins the two other bad inputs, each of
// which would otherwise register an entry no consumer can address.
func TestRegisterRefusesAnUnusableAnalyzer(t *testing.T) {
	for _, tc := range []struct {
		name string
		arg  *Analyzer
		want string
	}{
		{name: "nil analyzer", arg: nil, want: "nil analyzer"},
		{name: "empty name", arg: &Analyzer{Doc: "unnamed"}, want: "no Name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateRegistry(t)
			defer func() {
				got := recover()
				if got == nil {
					t.Fatalf("Register accepted a %s", tc.name)
				}
				if msg, ok := got.(string); !ok || !strings.Contains(msg, tc.want) {
					t.Errorf("the panic does not mention %q: %v", tc.want, got)
				}
			}()
			Register(tc.arg)
		})
	}
}

// TestExportFactRefusesANonPointerFact pins the stop that keeps a fact nothing
// can ever import from being recorded as though it worked.
func TestExportFactRefusesANonPointerFact(t *testing.T) {
	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("a by-value fact was accepted")
		}
		if msg, ok := got.(string); !ok || !strings.Contains(msg, "must be a pointer") {
			t.Errorf("the panic does not explain the pointer requirement: %v", got)
		}
	}()

	bad := &Analyzer{
		Name: "bad-fact",
		Doc:  "exports a fact by value",
		Run: func(p *Pass) (any, error) {
			p.ExportFact("obj", valueFact{})
			return nil, nil
		},
	}
	_, _ = Run(nil, []*Analyzer{bad})
}

// valueFact satisfies Fact on a VALUE receiver, so a value of it is a Fact and
// the pointer check is the only thing standing between it and a silent no-op.
type valueFact struct{}

// AFact marks valueFact as a Fact.
func (valueFact) AFact() {}
