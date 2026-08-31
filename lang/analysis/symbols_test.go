// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
)

// TestSymbolsTablesStrawmanProducers asserts the name model two ways.
//
// THE POSITIVE FLOOR catches the value-form trap. A walk written in pointer form
// compiles clean, matches nothing, and tables zero producers for every file —
// which no absence-style assertion can tell apart from a clean parse. Requiring a
// strictly positive count per strawman is what makes the golden below meaningful
// rather than a comparison of two empty tables.
//
// THE GOLDEN catches a wrong name model, and it is regenerated from the fixtures
// with -update rather than typed from prose.
func TestSymbolsTablesStrawmanProducers(t *testing.T) {
	var lines []string
	for _, name := range strawmanFiles {
		src := loadSource(t, filepath.Join(strawmanDir, name))
		table, diags := symbolsOf(t, src)
		if len(diags) != 0 {
			t.Errorf("%s produced symbols diagnostics: %v", name, messages(diags))
		}
		if len(table.Files) != 1 {
			t.Fatalf("%s tabled %d files, want 1", name, len(table.Files))
		}
		lines = append(lines, strawmanLines(t, name, table.Files[0])...)
	}
	checkGolden(t, "producers.txt", lines)
}

// strawmanLines renders one file's tables, and enforces the positive floor while
// it walks them.
func strawmanLines(t *testing.T, fixture string, file FileSymbols) []string {
	t.Helper()

	var out []string
	for i := range file.Flows {
		flow := &file.Flows[i]
		if len(flow.Producers) == 0 {
			t.Fatalf("%s flow %s tabled ZERO producers; a pointer-form walk looks exactly like this",
				fixture, flow.Name)
		}
		out = append(out, fmt.Sprintf("%s %s statements %d producers %d consumers %d routing %d",
			fixture, flow.Name, len(flow.Body), len(flow.Producers), len(flow.Consumers), len(flow.Routing)))
		out = append(out, refLines(fixture, flow.Name, "producer", flow.Producers)...)
		out = append(out, refLines(fixture, flow.Name, "consumer", flow.Consumers)...)
	}
	return out
}

// refLines renders one name table in sorted order.
func refLines(fixture, flow, kind string, refs map[string][]NameRef) []string {
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]string, 0, len(names))
	for _, name := range names {
		at := make([]string, 0, len(refs[name]))
		for _, ref := range refs[name] {
			at = append(at, ref.Pos.String())
		}
		sort.Strings(at)
		out = append(out, fmt.Sprintf("%s %s %s %s %v", fixture, flow, kind, name, at))
	}
	return out
}

// TestRoutingNodeNamesAreNotProducers pins the half of the name model that is
// easiest to get wrong: a branch, tee, switch or use statement's OWN name
// identifies the node and is never consumable, while the names it routes to are.
//
// The identifiers are ENUMERATED FROM THE FIXTURES by walking the tree directly
// rather than read out of the symbols table's own Routing map, which would make
// the assertion circular — the table would be checked against itself.
func TestRoutingNodeNamesAreNotProducers(t *testing.T) {
	var routingSeen int
	for _, name := range strawmanFiles {
		src := loadSource(t, filepath.Join(strawmanDir, name))
		table, _ := symbolsOf(t, src)
		flow := firstFlow(t, src)

		routing := routingNames(flow)
		routingSeen += len(routing)
		t.Logf("%s routing node names: %v", name, routing)

		for _, ident := range routing {
			if refs, produced := table.Files[0].Flows[0].Producers[ident]; produced {
				t.Errorf("%s tables the %s node name %q as a producer at %v; a routing name is not consumable",
					name, "branch/tee/switch/use", ident, refs)
			}
		}
	}

	// THE POSITIVE FLOOR. Without it, "no routing node name is a producer" is
	// satisfied by a scan that found no routing statements at all.
	if routingSeen == 0 {
		t.Fatal("no branch, tee, switch or use statement was found in any strawman; the scan found nothing to check")
	}
	t.Logf("checked %d routing node names across %d strawmen", routingSeen, len(strawmanFiles))
}

// routingNames walks a flow and collects every branch, tee, switch and use
// statement's own name.
func routingNames(flow ast.FlowDecl) []string {
	var out []string
	for _, stmt := range flow.Body {
		switch s := stmt.(type) {
		case ast.BranchStmt:
			out = append(out, s.Name.Name)
		case ast.TeeStmt:
			out = append(out, s.Name.Name)
		case ast.SwitchStmt:
			out = append(out, s.Name.Name)
		case ast.UseStmt:
			out = append(out, s.Instance.Name)
		default:
		}
	}
	sort.Strings(out)
	return out
}

// TestSymbolsRefusesAPointerFormStatement is the known positive for the walk's
// default arm, and the direct test of the trap the whole module is shaped
// around.
//
// Every ast node declares its interface methods on a VALUE receiver, so *T
// satisfies ast.Stmt exactly as T does while matching no value-form case. If the
// walk silently ignored what it could not match, an engine that matched NOTHING
// would report a clean program — so the default arm reports, and this test is
// what proves the arm is reachable rather than decorative.
func TestSymbolsRefusesAPointerFormStatement(t *testing.T) {
	src := parseSource(t, "one.flow", "flow one\nsource ingest Poll\nsink done audit.Store from ingest\n")
	flow := firstFlow(t, src)
	source, ok := flow.Body[0].(ast.SourceStmt)
	if !ok {
		t.Fatalf("the first statement is %T, want ast.SourceStmt", flow.Body[0])
	}

	// The pointer form satisfies ast.Stmt and is exactly what a wrongly-written
	// analyzer would be switching on.
	pointer := &source
	var _ ast.Stmt = pointer

	broken := ast.FlowDecl{Name: flow.Name, Body: []ast.Stmt{pointer}}
	src.File = &ast.File{Decls: []ast.Decl{broken}}

	_, diags := symbolsOf(t, src)
	if len(diags) != 1 {
		t.Fatalf("a pointer-form statement produced %d diagnostics, want 1: %v", len(diags), messages(diags))
	}
	if diags[0].Severity != SeverityError {
		t.Errorf("the unknown-shape diagnostic carries severity %s, want error", diags[0].Severity)
	}
	if got := diags[0].Message; !containsAll(got, "does not know the statement shape", "*ast.SourceStmt") {
		t.Errorf("the diagnostic does not name the offending shape: %q", got)
	}
}

// TestSymbolsTablesTheImplicitSignatureInput pins that a flow with a signature
// tables the `in` its body consumes but no statement declares.
func TestSymbolsTablesTheImplicitSignatureInput(t *testing.T) {
	src := loadSource(t, filepath.Join(astTestdata, "valid", "subflow-and-use.flow"))
	table, _ := symbolsOf(t, src)

	screening, ok := table.Flow("screening")
	if !ok {
		t.Fatal("subflow-and-use.flow tables no flow named screening")
	}
	if !screening.HasSignature {
		t.Error("screening declares a signature but the table does not record one")
	}
	if len(screening.Outputs) != 2 {
		t.Errorf("screening tables %d declared outputs, want 2", len(screening.Outputs))
	}
	refs, produced := screening.Producers[implicitInput]
	if !produced {
		t.Fatalf("screening does not table the implicit input; its producers are %v", sortedKeys(screening.Producers))
	}
	if len(refs) != 1 || refs[0].Stmt != signatureStmt {
		t.Errorf("the implicit input is tabled at %v, want one reference carrying the signature index", refs)
	}

	main, ok := table.Flow("main")
	if !ok {
		t.Fatal("subflow-and-use.flow tables no flow named main")
	}
	if main.HasSignature {
		t.Error("main declares no signature but the table records one")
	}
	if _, produced := main.Producers[implicitInput]; produced {
		t.Error("main tables an implicit input, but only a flow with a signature has one")
	}
}
