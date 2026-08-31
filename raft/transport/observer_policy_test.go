package transport

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// blockingObserverCalls reports every raft.NewObserver call in file whose
// blocking argument is not the literal false.
//
// THE RULE IS AN ALLOWLIST OF ONE, NOT A DENYLIST OF ONE. Flagging the literal
// true would pass anything spelled another way — a named constant, a config
// field, a function call — and a config-driven spelling is precisely how a
// later lane would write it. Requiring the argument to BE the literal false
// makes every other spelling, including ones nobody has thought of, fail the
// census and require a deliberate decision.
//
// A blocking observer wedges the raft instance the moment its channel fills:
// measured against v1.7.3, a capacity-1 channel left undrained parked
// handleCommand for the whole run, so replication on that group stops. The
// ruled semantic for this repo is the dropping observer, and this census is
// what keeps it true as the module grows.
//
// WHAT IT DOES NOT COVER: it gates the CONSTRUCTION of an observer. A
// hand-rolled blocking read on an observer channel is invisible to it, and
// that negative space is a review duty rather than a check.
func blockingObserverCalls(fset *token.FileSet, f *ast.File) []token.Position {
	var found []token.Position
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewObserver" {
			return true
		}
		if lit, ok := call.Args[1].(*ast.Ident); !ok || lit.Name != "false" {
			found = append(found, fset.Position(call.Pos()))
		}
		return true
	})
	return found
}

func TestNoBlockingObserverIsConstructedInThisModule(t *testing.T) {
	// Known-positive control: the detector must fire on the shape it forbids.
	fset := token.NewFileSet()
	bad, err := parser.ParseFile(fset, "bad.go", `package p
import "github.com/hashicorp/raft"
func f(r *raft.Raft, ch chan raft.Observation) { r.RegisterObserver(raft.NewObserver(ch, true, nil)) }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := blockingObserverCalls(fset, bad); len(got) != 1 {
		t.Fatalf("CONTROL FAILED: the detector found %d blocking observers in the bad fixture, want 1", len(got))
	}
	// The evasion fixture: a named constant carrying true reads as an ordinary
	// identifier, so a detector keyed on the literal true would pass it.
	evasive, err := parser.ParseFile(fset, "evasive.go", `package p
import "github.com/hashicorp/raft"
const blockingObservers = true
func f(r *raft.Raft, ch chan raft.Observation) { r.RegisterObserver(raft.NewObserver(ch, blockingObservers, nil)) }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := blockingObserverCalls(fset, evasive); len(got) != 1 {
		t.Fatalf("CONTROL FAILED: a constant-spelled blocking observer evaded the census (found %d, want 1)", len(got))
	}
	// Known-negative control: the dropping form is legitimate and must not fire.
	good, err := parser.ParseFile(fset, "good.go", `package p
import "github.com/hashicorp/raft"
func f(r *raft.Raft, ch chan raft.Observation) { r.RegisterObserver(raft.NewObserver(ch, false, nil)) }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := blockingObserverCalls(fset, good); len(got) != 0 {
		t.Fatalf("CONTROL FAILED: the detector fired on the dropping form at %v", got)
	}

	scanned := 0
	var offenders []token.Position
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		scanned++
		offenders = append(offenders, blockingObserverCalls(fset, f)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("CONTROL FAILED: the walk parsed no Go files, so a clean result would mean nothing")
	}
	if len(offenders) != 0 {
		t.Fatalf("blocking observers constructed at %v: a full channel wedges the group", offenders)
	}
	t.Logf("scanned %d Go files under the raft module; no blocking observer is constructed", scanned)
}
