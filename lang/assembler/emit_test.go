// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	goast "go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
)

// generateOf parses, builds, lowers and emits one source, failing on any
// diagnostic.
func generateOf(t *testing.T, src string, types map[string]string, boundary map[string]Boundary) Generated {
	t.Helper()
	file, err := ast.Parse([]byte(src))
	if err != nil {
		t.Fatalf("the fixture must parse clean: %v", err)
	}
	programs, diags := buildFile(file)
	if len(diags) != 0 {
		t.Fatalf("the fixture must build clean: %v", messagesOf(diags))
	}
	for _, p := range programs {
		p.InputTypes = types
	}
	cfg := Config{Package: "generated", Qualifier: "acme"}
	plans, lowerDiags := lowerFile(programs, boundary, cfg)
	if len(lowerDiags) != 0 {
		t.Fatalf("the fixture must lower clean:\n%s", strings.Join(messagesOf(lowerDiags), "\n"))
	}
	out, emitDiags := Generate(file, programs, plans, cfg, "pipeline.flow", nil)
	if len(emitDiags) != 0 {
		t.Fatalf("emission reported:\n%s\n--- source ---\n%s",
			strings.Join(messagesOf(emitDiags), "\n"), out.Source)
	}

	return out
}

// handleFixture declares state and a var in a flow, so both handle kinds are
// emitted.
const handleFixture = "flow orders\n" +
	"state {\n  seen map[string]bool\n}\n" +
	"var attempt int\n" +
	"source ingest Poll\n" +
	"transform charge Bill from ingest\n" +
	"  reads attempt  writes seen\n" +
	"sink done Store from charge\n"

// TestStateHandleNamesAreQualified proves every emitted handle name carries the
// qualifier and the flow.
//
// THE RUNTIME KEEPS KEYS AND CELLS IN ONE PROCESS-GLOBAL NAMESPACE and panics on
// a duplicate at declaration, so an unqualified name is not a cosmetic problem:
// two generated flows in one binary that both declare a var called `attempt`
// would kill the process at startup. The names are read out of the emitted Go by
// PARSING it, not by grepping, so a re-indent or a reordering cannot change the
// answer and a file that stopped being valid Go fails loudly.
func TestStateHandleNamesAreQualified(t *testing.T) {
	out := generateOf(t, handleFixture,
		map[string]string{"ingest": "Order", "charge": "Order", "done": "Receipt"},
		map[string]Boundary{"orders": {}})

	names := handleNamesIn(t, string(out.Source))
	if len(names) != 2 {
		t.Fatalf("the generated file declares %d handles (%v), want one cell and one key", len(names), names)
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "acme.orders.") {
			t.Errorf("the handle %q is not qualified; two flows in one process would collide on it", name)
		}
		if !strings.Contains(name, ".") {
			t.Errorf("the handle %q carries no separator at all", name)
		}
	}
	t.Logf("every emitted handle name is qualified: %v", names)
}

// handleNamesIn parses generated Go and returns the string literal passed to
// every machine.NewKey and machine.NewCell call.
func handleNamesIn(t *testing.T, src string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "generated.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("the generated file is not valid Go: %v\n--- source ---\n%s", err, src)
	}

	var names []string
	goast.Inspect(file, func(n goast.Node) bool {
		call, ok := n.(*goast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		if !isHandleConstructor(call.Fun) {
			return true
		}
		if lit, ok := call.Args[0].(*goast.BasicLit); ok && lit.Kind == token.STRING {
			names = append(names, strings.Trim(lit.Value, `"`))
		}

		return true
	})

	return names
}

// isHandleConstructor reports whether a call expression is machine.NewKey or
// machine.NewCell, in either the bare or the explicitly instantiated form.
func isHandleConstructor(fun goast.Expr) bool {
	switch typed := fun.(type) {
	case *goast.IndexExpr:
		return isHandleConstructor(typed.X)
	case *goast.IndexListExpr:
		return isHandleConstructor(typed.X)
	case *goast.SelectorExpr:
		return typed.Sel.Name == "NewKey" || typed.Sel.Name == "NewCell"
	default:
		return false
	}
}

// TestTheGeneratedFileCarriesItsHeaderAndPackageDoc pins the two header lines and
// the host journal contract the package doc has to state.
func TestTheGeneratedFileCarriesItsHeaderAndPackageDoc(t *testing.T) {
	out := generateOf(t, handleFixture,
		map[string]string{"ingest": "Order", "charge": "Order", "done": "Receipt"},
		map[string]Boundary{"orders": {}})

	lines := strings.Split(string(out.Source), "\n")
	if len(lines) < 2 {
		t.Fatal("the generated file is shorter than its own header")
	}
	if lines[0] != generatedMarker {
		t.Errorf("line 1 is %q, want the exact generated-code marker", lines[0])
	}
	if lines[1] != "// source: pipeline.flow" {
		t.Errorf("line 2 is %q, want the .flow source stamp", lines[1])
	}
	if out.Name != "pipeline.flow.go" {
		t.Errorf("the generated file is named %q", out.Name)
	}

	// THE HOST JOURNAL CONTRACT IS PROSE A REVIEWER CHECKS, but its PRESENCE is
	// worth pinning: a package doc that lost it would leave a host with no
	// statement anywhere of who builds the journal.
	doc := string(out.Source)
	for _, want := range []string{"machine.OptionJournal", "THE HOST OWNS THE JOURNAL", "returns error"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the generated package doc does not mention %q", want)
		}
	}
}

// TestWireReturnsErrorUniformly proves the signature is one contract rather than
// two, and that a checkpointing flow checks for a journal before declaring
// anything.
func TestWireReturnsErrorUniformly(t *testing.T) {
	t.Run("a flow with no checkpoint still returns error", func(t *testing.T) {
		out := generateOf(t, handleFixture,
			map[string]string{"ingest": "Order", "charge": "Order", "done": "Receipt"},
			map[string]Boundary{"orders": {}})
		if !strings.Contains(string(out.Source), "func WireOrders(m *machine.Machine) error {") {
			t.Errorf("the wiring function does not return error:\n%s", out.Source)
		}
		if strings.Contains(string(out.Source), "HasJournal") {
			t.Error("a flow with no checkpoint emitted a journal check it does not need")
		}
	})

	t.Run("a checkpointing flow checks for a journal first", func(t *testing.T) {
		src := strings.Replace(handleFixture,
			"  reads attempt  writes seen\n",
			"  reads attempt  writes seen  checkpoint machine.GobCodec[Order]{}\n", 1)
		out := generateOf(t, src,
			map[string]string{"ingest": "Order", "charge": "Order", "done": "Receipt"},
			map[string]Boundary{"orders": {}})

		body := string(out.Source)
		if !strings.Contains(body, "m.HasJournal()") {
			t.Fatalf("a checkpointing flow emitted no journal check:\n%s", body)
		}
		// THE CHECK MUST PRECEDE THE FIRST BUILDER CALL, so a host that forgot the
		// option gets a clean refusal rather than a half-built machine.
		checkAt := strings.Index(body, "m.HasJournal()")
		firstCall := strings.Index(body, "m.Source[")
		if firstCall < 0 {
			t.Fatal("the generated wiring declares no source at all")
		}
		if checkAt > firstCall {
			t.Error("the journal check is emitted after the first node is declared, " +
				"so a refusal would leave the machine half-built")
		}
		if !strings.Contains(body, "machine.OptionJournal") {
			t.Error("the refusal does not name the option the host has to pass")
		}
	})
}
