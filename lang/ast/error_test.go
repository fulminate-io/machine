// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import (
	"errors"
	goast "go/ast"
	goparser "go/parser"
	gotoken "go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// exportedTypeNames returns every exported type declared in the package's
// non-test sources.
func exportedTypeNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	fset := gotoken.NewFileSet()
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := goparser.ParseFile(fset, name, nil, goparser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		names = append(names, exportedTypesIn(file)...)
	}
	return names
}

// exportedTypesIn collects the exported type declarations of one file.
func exportedTypesIn(file *goast.File) []string {
	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*goast.GenDecl)
		if !ok || gen.Tok != gotoken.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			if typed, isType := spec.(*goast.TypeSpec); isType && typed.Name.IsExported() {
				names = append(names, typed.Name.Name)
			}
		}
	}
	return names
}

// TestParseReturnsPartialASTWithDiagnostics pins the contract error tolerance
// rests on: a source with a mistake in it still yields a usable tree.
//
// The statement COUNT is asserted rather than mere non-nilness, because a parser
// that returns an empty File satisfies non-nilness and has recovered nothing.
func TestParseReturnsPartialASTWithDiagnostics(t *testing.T) {
	src := []byte(`flow orders
source ingest Poll
transform bad
sink out Write from ingest
`)

	file, err := Parse(src)
	if file == nil {
		t.Fatalf("Parse returned a nil File; it must always return a tree")
	}
	if err == nil {
		t.Fatalf("Parse reported no error for a source with a missing from-list")
	}

	var parseErr *Error
	if !errors.As(err, &parseErr) {
		t.Fatalf("Parse returned %T, want *Error", err)
	}
	if len(parseErr.Diagnostics) == 0 {
		t.Fatalf("a non-nil error carried no diagnostics")
	}
	if parseErr.File != file {
		t.Errorf("the error carries a different tree than Parse returned")
	}

	for i, d := range parseErr.Diagnostics {
		if d.Message == "" {
			t.Errorf("diagnostic %d has no message", i)
		}
		if d.Pos.Line < 1 || d.Pos.Col < 1 {
			t.Errorf("diagnostic %d is not positioned: %+v", i, d.Pos)
		}
	}

	// THE RECOVERY ASSERTION. The broken transform sits between two good
	// statements, so a parser that gave up would return one statement and a
	// parser that resynchronized returns three.
	flow, ok := file.Decls[0].(FlowDecl)
	if !ok {
		t.Fatalf("first declaration is %T, want FlowDecl", file.Decls[0])
	}
	if len(flow.Body) != 3 {
		t.Fatalf("recovered %d statements, want 3 (the sink after the mistake must survive)", len(flow.Body))
	}
	if _, isSink := flow.Body[2].(SinkStmt); !isSink {
		t.Errorf("the statement after the mistake is %T, want SinkStmt", flow.Body[2])
	}

	// A clean source returns a nil error, so the error is a real signal rather
	// than something every parse produces.
	clean, cleanErr := Parse([]byte("flow orders\nsource ingest Poll\n"))
	if cleanErr != nil {
		t.Fatalf("CONTROL FAILED: a clean source reported %v", cleanErr)
	}
	if clean == nil || len(clean.Decls) != 1 {
		t.Fatalf("CONTROL FAILED: a clean source did not parse to one declaration")
	}
}

// TestErrorIsTheOnlyExportedErrorType walks the package's own sources so a
// second exported error type added later is caught here rather than by a caller
// discovering it has to type-switch.
func TestErrorIsTheOnlyExportedErrorType(t *testing.T) {
	exported := exportedTypeNames(t)
	if len(exported) == 0 {
		t.Fatalf("CONTROL FAILED: the walk found no exported types at all")
	}

	byType := packageMethods(t)
	var errorTypes []string
	for _, name := range exported {
		if slices.Contains(methodNames(byType[name]), "Error") {
			errorTypes = append(errorTypes, name)
		}
	}

	if len(errorTypes) != 1 || errorTypes[0] != "Error" {
		t.Fatalf("exported types implementing error: %v, want exactly [Error]", errorTypes)
	}
}
