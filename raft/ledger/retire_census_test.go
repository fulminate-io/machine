// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ledger

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// retireCensusDirs are the packages that construct journal entries by hand.
//
// THE CENSUS IS THE CLASS GATE. A retire entry names TWO keys — Entry.Path is the
// datum's checkpoint and Entry.Value carries its claim — and an entry that names
// only the first retires no claim at all, silently, forever. That defect shipped
// once: the arm deleted claims at the CHECKPOINT key while every claim was written
// at the disjoint claim key, and the test that was supposed to catch it keyed both
// halves at one bare string so it could not see which map the arm reached.
//
// IT IS A CENSUS RATHER THAN AN ASSERTION ABOUT ONE SITE, because a point fix leaves
// the next author free to write the one-key form in a new place.
var retireCensusDirs = []string{".", "../recovery"}

// TestEveryRetireEntryNamesBothKeys walks the source of every package that builds
// journal entries and fails on a KindRetire composite literal with no Value.
func TestEveryRetireEntryNamesBothKeys(t *testing.T) {
	sites, retires := 0, 0
	for _, dir := range retireCensusDirs {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", dir, err)
		}
		for _, pkg := range pkgs {
			for name, file := range pkg.Files {
				ast.Inspect(file, func(n ast.Node) bool {
					lit, ok := n.(*ast.CompositeLit)
					if !ok || !isEntryLit(lit) {
						return true
					}
					kind, hasValue := "", false
					for _, elt := range lit.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, _ := kv.Key.(*ast.Ident)
						if key == nil {
							continue
						}
						switch key.Name {
						case "Kind":
							kind = exprText(kv.Value)
						case "Value":
							hasValue = true
						}
					}
					if kind != "KindRetire" {
						return true
					}
					retires++
					if !hasValue {
						sites++
						t.Errorf("%s:%d builds a KindRetire entry with no Value; it names the checkpoint key "+
							"and NOT the claim key, so it retires no claim at all",
							filepath.Base(name), fset.Position(lit.Pos()).Line)
					}

					return true
				})
			}
		}
	}

	// CONTROL: the walk actually reached KindRetire entries. Without it a parse that
	// found nothing — a renamed constant, a moved package, a directory that does not
	// exist — would satisfy the zero above.
	if retires == 0 {
		t.Fatal("CONTROL FAILED: the census found no KindRetire entry at all, so its zero is a measurement " +
			"of the walk rather than of the code")
	}
	t.Logf("retire-entry census: %d KindRetire entries across %v, %d naming only one key",
		retires, retireCensusDirs, sites)
}

// isEntryLit reports whether a composite literal builds a ledger Entry, in either
// the bare or the package-qualified spelling.
func isEntryLit(lit *ast.CompositeLit) bool {
	switch t := lit.Type.(type) {
	case *ast.Ident:
		return t.Name == "Entry"
	case *ast.SelectorExpr:
		return t.Sel.Name == "Entry"
	}

	return false
}

// exprText renders the trailing identifier of a possibly-qualified name, so
// KindRetire and ledger.KindRetire read alike.
func exprText(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	}

	return ""
}
