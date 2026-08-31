// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

// The stdlib source-walking packages are aliased because every one of them
// collides with a name this package already owns: `ast` is the package itself,
// `token` is its token type, and `parser` is its parser.
import (
	goast "go/ast"
	goparser "go/parser"
	gotoken "go/token"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// markerMethods seal the three node interfaces. Every node type declares isNode;
// the family markers say which interface it also satisfies.
var markerMethods = []string{"isNode", "isDecl", "isStmt"}

// methodDecl is one method as declared: its name and whether its receiver was
// written anonymously.
type methodDecl struct {
	name          string
	receiverNamed bool
}

// packageMethods returns every method in the package's non-test sources, keyed
// by the receiver type's name.
func packageMethods(t *testing.T) map[string][]methodDecl {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	fset := gotoken.NewFileSet()
	out := map[string][]methodDecl{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := goparser.ParseFile(fset, name, nil, goparser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		collectMethods(file, out)
	}
	return out
}

// collectMethods records every method declaration in one file.
func collectMethods(file *goast.File, into map[string][]methodDecl) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*goast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		recv := fn.Recv.List[0]
		typeName := receiverTypeName(recv.Type)
		if typeName == "" {
			continue
		}
		into[typeName] = append(into[typeName], methodDecl{
			name:          fn.Name.Name,
			receiverNamed: len(recv.Names) > 0,
		})
	}
}

// receiverTypeName unwraps a pointer receiver down to its type name.
func receiverTypeName(expr goast.Expr) string {
	switch typed := expr.(type) {
	case *goast.StarExpr:
		return receiverTypeName(typed.X)
	case *goast.Ident:
		return typed.Name
	case *goast.IndexExpr:
		return receiverTypeName(typed.X)
	}
	return ""
}

// methodNames projects a method list down to its names.
func methodNames(decls []methodDecl) []string {
	out := make([]string, 0, len(decls))
	for _, d := range decls {
		out = append(out, d.name)
	}
	return out
}

// TestEveryASTNodeIsSealedAndPositioned walks the package's OWN sources rather
// than keeping a hand-written inventory that would drift as node types are
// added.
//
// Four properties, and the receiver one is measured rather than stylistic:
// revive's unused-receiver rule is severity error in this repo and fires on
// every empty method with a NAMED receiver. Across the node types here that is
// fifty-odd error-severity findings, so it is asserted in the phase that writes
// the methods rather than left to the lint gate.
func TestEveryASTNodeIsSealedAndPositioned(t *testing.T) {
	byType := packageMethods(t)
	if len(byType) == 0 {
		t.Fatalf("CONTROL FAILED: the source walk found no methods at all")
	}

	var nodeTypes []string
	for typeName, decls := range byType {
		names := methodNames(decls)
		if !slices.Contains(names, "isNode") {
			continue
		}
		nodeTypes = append(nodeTypes, typeName)

		for _, want := range []string{"Pos", "End"} {
			if !slices.Contains(names, want) {
				t.Errorf("node type %s declares isNode but not %s; every node exposes both ends of its span", typeName, want)
			}
		}
		for _, decl := range decls {
			if !slices.Contains(markerMethods, decl.name) {
				continue
			}
			if unicode.IsUpper(rune(decl.name[0])) {
				t.Errorf("marker method %s.%s is exported, which unseals the interface", typeName, decl.name)
			}
			if decl.receiverNamed {
				t.Errorf("marker method %s.%s has a NAMED receiver; revive's unused-receiver rule is severity error here",
					typeName, decl.name)
			}
		}
	}

	if len(nodeTypes) == 0 {
		t.Fatalf("CONTROL FAILED: the walk found no node types, so it proved nothing about sealing")
	}
	sort.Strings(nodeTypes)
	t.Logf("%d node types are sealed and positioned: %v", len(nodeTypes), nodeTypes)
}

// TestFamilyMarkersOnlyAppearOnNodes asserts the family markers are never
// declared on a type that is not itself a node — a Decl or Stmt that is not a
// Node would satisfy neither interface and would be dead weight the compiler
// cannot see.
func TestFamilyMarkersOnlyAppearOnNodes(t *testing.T) {
	byType := packageMethods(t)
	if len(byType) == 0 {
		t.Fatalf("CONTROL FAILED: the source walk found no methods at all")
	}

	checked := 0
	for typeName, decls := range byType {
		names := methodNames(decls)
		for _, family := range []string{"isDecl", "isStmt"} {
			if !slices.Contains(names, family) {
				continue
			}
			checked++
			if !slices.Contains(names, "isNode") {
				t.Errorf("type %s declares %s but not isNode, so it satisfies neither interface", typeName, family)
			}
		}
	}
	if checked == 0 {
		t.Fatalf("CONTROL FAILED: no type declares a family marker, so this test asserted nothing")
	}
}
