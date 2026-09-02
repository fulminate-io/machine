// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	goast "go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"testing"
)

// runtimeSources are the two files the builder vocabulary is derived from. They
// are READ, not imported, so this adds no module dependency.
var runtimeSources = []string{
	filepath.Join("..", "..", "flow.go"),
	filepath.Join("..", "..", "machine.go"),
}

// TestEmittedMethodsMatchTheRuntimeBuilderSet derives the runtime's builder
// vocabulary FROM ITS SOURCE and asserts the emitter's equals it.
//
// THE DERIVATION RULE, stated here because it is the whole content of the gate:
// a builder method is an EXPORTED method whose receiver is Flow[...], plus
// Machine.Source. Nothing is hand-listed and nothing is special-cased.
//
// The rule produces two exclusions on its own rather than by exception, and both
// are checked below so a future edit cannot quietly widen it:
//   - *Machine's other exported methods — Host, Name, Start, HasJournal — are not
//     builder calls, and the rule reaches none of them because it admits exactly
//     one method on that receiver by name.
//   - HostState.Load and HostState.Save are methods on HostState, so the receiver
//     clause never reaches them at all.
//
// IT REDS IN BOTH DIRECTIONS. A method the emitter names and the runtime does not
// export is Go that will not compile. A builder method the runtime gained and the
// emitter does not know about is a shape being lowered the worse way forever, in
// silence. Neither is detectable from inside this module.
func TestEmittedMethodsMatchTheRuntimeBuilderSet(t *testing.T) {
	derived := deriveBuilderMethods(t)

	// CONTROL: the derivation must have read something. An empty derived set
	// would make the comparison below vacuous in one direction and is the shape a
	// moved file or a changed receiver spelling would produce.
	if len(derived) == 0 {
		t.Fatal("CONTROL FAILED: the derivation found no builder methods at all, so this gate proves nothing")
	}

	slices.Sort(derived)
	emitted := slices.Clone(builderMethods)
	slices.Sort(emitted)

	for _, name := range emitted {
		if !slices.Contains(derived, name) {
			t.Errorf("the emitter names %q, which the runtime does not export as a builder method; "+
				"generated code calling it would not compile.\nruntime exports: %v", name, derived)
		}
	}
	for _, name := range derived {
		if !slices.Contains(emitted, name) {
			t.Errorf("the runtime exports the builder method %q, which the emitter does not know about; "+
				"a shape that should lower to it is being lowered some other way.\nemitter names: %v", name, emitted)
		}
	}

	t.Logf("the runtime's builder vocabulary is %v, derived from %v", derived, runtimeSources)
}

// deriveBuilderMethods reads the runtime's source and returns the builder method
// names, by the rule stated on the test above.
func deriveBuilderMethods(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, path := range runtimeSources {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*goast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || !fn.Name.IsExported() {
				continue
			}
			if isBuilderReceiver(fn) {
				names = append(names, fn.Name.Name)
			}
		}
	}

	return names
}

// isBuilderReceiver reports whether a method belongs to the builder chain.
//
// A method on Flow[...] always does: Flow is the chain. A method on *Machine does
// only when it is Source, which is where a chain begins; the machine's other
// exported methods configure or run a machine rather than declare a node.
func isBuilderReceiver(fn *goast.FuncDecl) bool {
	switch receiverName(fn.Recv.List[0].Type) {
	case "Flow":
		return true
	case "Machine":
		return fn.Name.Name == MethodSource
	default:
		return false
	}
}

// receiverName reduces a receiver type expression to its bare type name,
// stripping the pointer and any type parameters.
func receiverName(expr goast.Expr) string {
	switch typed := expr.(type) {
	case *goast.StarExpr:
		return receiverName(typed.X)
	case *goast.IndexExpr:
		return receiverName(typed.X)
	case *goast.IndexListExpr:
		return receiverName(typed.X)
	case *goast.Ident:
		return typed.Name
	default:
		return ""
	}
}

// TestTheDerivationExcludesNonBuilderMethods pins the two exclusions the rule is
// supposed to produce ON ITS OWN.
//
// Without this, a derivation that admitted every exported *Machine method would
// still pass the equality test if someone widened builderMethods to match it. This
// asserts the runtime's non-builder methods exist AND are absent from the derived
// set, so the exclusion is a measured property rather than a claim in a comment.
func TestTheDerivationExcludesNonBuilderMethods(t *testing.T) {
	derived := deriveBuilderMethods(t)
	everyExported := everyExportedMethod(t)

	for _, name := range []string{"Name", "Host", "Start", "HasJournal"} {
		// The known positive: the method really is exported by the runtime, so
		// its absence below is an exclusion rather than a typo.
		if !slices.Contains(everyExported, name) {
			t.Fatalf("CONTROL FAILED: the runtime exports no method named %q, "+
				"so its absence from the builder set proves nothing.\nexported: %v", name, everyExported)
		}
		if slices.Contains(derived, name) {
			t.Errorf("%q reached the builder vocabulary; it configures or runs a machine "+
				"rather than declaring a node", name)
		}
	}

	// HostState's accessors are reached by no receiver clause at all.
	for _, name := range []string{"Load", "Save"} {
		if slices.Contains(derived, name) {
			t.Errorf("HostState.%s reached the builder vocabulary; its receiver is neither Flow nor Machine", name)
		}
	}
}

// everyExportedMethod lists every exported method in the runtime sources,
// whatever its receiver. It is the known-positive population for the exclusion
// test above.
func everyExportedMethod(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, path := range runtimeSources {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*goast.FuncDecl)
			if !ok || fn.Recv == nil || !fn.Name.IsExported() {
				continue
			}
			names = append(names, fn.Name.Name)
		}
	}

	return names
}
