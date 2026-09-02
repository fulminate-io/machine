// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
	"github.com/whitaker-io/machine/lang/loader"
)

// loadedGolden writes one golden case into a temp module, loads it through
// lang/loader, and returns the Types view of it.
//
// THE LOAD HAPPENS ONCE and the caller owns it, which is the loader's own rule:
// it holds no process-global cache, and loading is the seconds-scale operation
// in this toolchain.
func loadedGolden(t *testing.T, c goldenCase) (*Types, string) {
	t.Helper()
	dir := t.TempDir()
	root := machineRoot(t)

	generated, err := os.ReadFile(filepath.Join(c.dir, "pipeline.flow.go"))
	if err != nil {
		t.Fatalf("reading the golden: %v", err)
	}
	for name, body := range map[string]string{
		"go.mod":           "module probe\n\ngo 1.27\n",
		"go.work":          "go 1.27\n\nuse (\n\t.\n\t" + root + "\n)\n",
		"support.go":       c.support,
		"pipeline.flow.go": string(generated),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	if sum, err := os.ReadFile(filepath.Join(root, "go.sum")); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "go.work.sum"), sum, 0o600); err != nil {
			t.Fatalf("writing go.work.sum: %v", err)
		}
	}

	pkgs, err := loader.Load(dir, []string{"./..."})
	if err != nil {
		t.Fatalf("loading the generated package: %v", err)
	}

	return NewTypes(pkgs, "probe", map[int]ast.Position{}), dir
}

// TestGeneratedPackageTypeChecks proves the emitted package type-checks with its
// imports resolved, and that the types a later phase needs are recoverable.
//
// THIS IS THE STEP'S WHOLE PURPOSE: the clone derivation cannot run on a type it
// cannot see, and a derivation over an unresolved type would silently produce a
// shallow copy — the exact defect the phase-3 refusals exist to avoid until this
// lands.
func TestGeneratedPackageTypeChecks(t *testing.T) {
	var pipeline goldenCase
	for _, c := range goldenCases(t) {
		if c.name == "pipeline" {
			pipeline = c
		}
	}
	if pipeline.name == "" {
		t.Fatal("CONTROL FAILED: the pipeline golden case is absent, so nothing here is exercised")
	}

	typed, _ := loadedGolden(t, pipeline)

	t.Run("the package type-checks with no errors", func(t *testing.T) {
		if diags := typed.Diagnostics(); len(diags) != 0 {
			t.Errorf("the generated package does not type-check:\n%s", strings.Join(messagesOf(diags), "\n"))
		}
	})

	t.Run("the scope is reachable", func(t *testing.T) {
		scope, ok := typed.Scope()
		if !ok || scope == nil {
			t.Fatal("the generated package has no scope, so no spelling can be resolved in it")
		}
		// CONTROL: the scope really holds the generated declarations, so a
		// successful resolve below is not a resolve against an empty scope.
		if scope.Lookup("WireOrders") == nil {
			t.Errorf("the scope does not hold the generated wiring function; it holds %v", scope.Names())
		}
	})

	t.Run("a node payload, a var and a state field all resolve to a types.Type", func(t *testing.T) {
		// The three the later phases need: a node's payload type, a var's type
		// and a state field's type. Each is read as a SPELLING and resolved.
		for _, spelling := range []string{"Order", "Receipt", "int"} {
			resolved, err := typed.Resolve(spelling)
			if err != nil {
				t.Errorf("%q did not resolve: %v", spelling, err)

				continue
			}
			if resolved == nil {
				t.Errorf("%q resolved to a nil type", spelling)
			}
		}
	})

	t.Run("a struct payload resolves to structure a derivation can walk", func(t *testing.T) {
		// A clone derivation type-switches on the underlying type, so recovering
		// a types.Type that is not walkable would satisfy a nil check and fail the
		// derivation.
		resolved, err := typed.Resolve("Order")
		if err != nil {
			t.Fatalf("Order did not resolve: %v", err)
		}
		structure, ok := resolved.Underlying().(*types.Struct)
		if !ok {
			t.Fatalf("Order's underlying type is %T, want a *types.Struct a derivation can walk", resolved.Underlying())
		}
		if structure.NumFields() == 0 {
			t.Error("Order resolved to a struct with no fields, which is not the fixture's shape")
		}
	})

	t.Run("an unresolvable spelling is an error naming it", func(t *testing.T) {
		_, diag := typed.PayloadOf("NoSuchType", ast.Position{Line: 7, Col: 3})
		if diag == nil {
			t.Fatal("an unresolvable spelling resolved successfully")
		}
		if !strings.Contains(diag.Message, "NoSuchType") {
			t.Errorf("the refusal %q does not name the spelling", diag.Message)
		}
		// IT IS POSITIONED IN THE AUTHOR'S FRAME. The caller supplies the .flow
		// position, so this refusal never needs the line map and must not be
		// re-mapped.
		if diag.Pos.Line != 7 {
			t.Errorf("the refusal is at line %d, want the caller's .flow line 7", diag.Pos.Line)
		}
	})
}

// TestTypeCheckErrorsAreMappedBackToTheFlowLine proves the two coordinate frames
// are kept apart.
//
// Errors() reports in the coordinates of the file go/types READ, which is the
// GENERATED Go. Only the emitter holds the map back to the .flow, so the mapping
// happens here and nowhere else. A diagnostic already in the author's frame —
// anything ResolveFlow produces — must NOT be mapped again, and a generated line
// no .flow produced must be reported where it is rather than guessed at.
func TestTypeCheckErrorsAreMappedBackToTheFlowLine(t *testing.T) {
	typed := &Types{lines: map[int]ast.Position{
		42: {Line: 7, Col: 3},
	}}

	t.Run("a mapped generated line becomes the flow line", func(t *testing.T) {
		got := typed.mapDiagnostic(loader.Diagnostic{
			Path: "pipeline.flow.go", Pos: ast.Position{Line: 42, Col: 9}, Message: "undefined: Bill",
		})
		if got.Pos.Line != 7 {
			t.Errorf("the diagnostic landed at line %d, want the .flow line 7", got.Pos.Line)
		}
		if got.Message != "undefined: Bill" {
			t.Errorf("the mapped message was rewritten to %q", got.Message)
		}
	})

	t.Run("an unmapped generated line is reported where it is, not guessed at", func(t *testing.T) {
		// The preamble and the package doc come from no .flow line at all, and a
		// type error in those is a real error about the GENERATOR. Inventing a
		// source position for it would send an author to a line that is fine.
		got := typed.mapDiagnostic(loader.Diagnostic{
			Path: "pipeline.flow.go", Pos: ast.Position{Line: 9, Col: 1}, Message: "undefined: machine",
		})
		if got.Pos.Line != 9 {
			t.Errorf("an unmapped line was relocated to %d", got.Pos.Line)
		}
		if !strings.Contains(got.Message, "which no .flow line produced") {
			t.Errorf("the diagnostic %q does not say it is in generated code", got.Message)
		}
	})
}
