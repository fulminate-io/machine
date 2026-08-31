// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import (
	"strings"
	"testing"
)

// mustParse parses a source that is expected to be clean, failing loudly with
// every diagnostic when it is not.
func mustParse(t *testing.T, src string) *File {
	t.Helper()
	file, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("unexpected diagnostics for:\n%s\n%v", src, err)
	}
	if file == nil {
		t.Fatalf("Parse returned a nil File")
	}
	return file
}

// flowAt returns the nth flow declaration of a file.
func flowAt(t *testing.T, file *File, n int) FlowDecl {
	t.Helper()
	seen := 0
	for _, decl := range file.Decls {
		flow, ok := decl.(FlowDecl)
		if !ok {
			continue
		}
		if seen == n {
			return flow
		}
		seen++
	}
	t.Fatalf("file carries %d flows, wanted flow %d", seen, n)
	return FlowDecl{}
}

// diagnosticsFor parses a source expected to be broken and returns its
// diagnostics, failing when the parse reported none.
func diagnosticsFor(t *testing.T, src string) []Diagnostic {
	t.Helper()
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatalf("expected diagnostics for:\n%s\nbut the parse was clean", src)
	}
	parseErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("Parse returned %T, want *Error", err)
	}
	return parseErr.Diagnostics
}

// requireDiagnostic asserts some diagnostic mentions the given fragment.
func requireDiagnostic(t *testing.T, diags []Diagnostic, fragment string) Diagnostic {
	t.Helper()
	for _, d := range diags {
		if strings.Contains(d.Message, fragment) {
			return d
		}
	}
	t.Fatalf("no diagnostic mentions %q; got %v", fragment, diags)
	return Diagnostic{}
}

// TestParseFileLevelDeclarations covers every declaration legal before or
// between flows, plus the flow-level entries.
//
// It uses a TWO-FLOW file deliberately: a braceless flow body ends at the next
// `flow` line, and a single-flow fixture cannot show that boundary at all.
func TestParseFileLevelDeclarations(t *testing.T) {
	file := mustParse(t, `import ratelimit "github.com/example/ratelimit"
const retries = 3
param window time.Duration = 5 * time.Second

flow first
on error LogAndDrop
state {
	seen map[string]bool
}
var span *ops.Span clone ops.CloneSpan
var h func(int) error
source ingest Poll
drop ingest

flow second
source other Gen
`)

	if len(file.Decls) != 5 {
		t.Fatalf("got %d file-level declarations, want 5", len(file.Decls))
	}

	imported, ok := file.Decls[0].(ImportDecl)
	if !ok {
		t.Fatalf("declaration 0 is %T, want ImportDecl", file.Decls[0])
	}
	if imported.Alias == nil || imported.Alias.Name != "ratelimit" {
		t.Errorf("import alias is %v, want ratelimit", imported.Alias)
	}
	if imported.Path != `"github.com/example/ratelimit"` {
		t.Errorf("import path is %q and should be captured verbatim, quotes included", imported.Path)
	}

	constant, ok := file.Decls[1].(ConstDecl)
	if !ok {
		t.Fatalf("declaration 1 is %T, want ConstDecl", file.Decls[1])
	}
	if constant.Type != nil {
		t.Errorf("const with no declared type carries type %q", constant.Type.Text)
	}
	if constant.Value.Text != "3" {
		t.Errorf("const value is %q, want 3", constant.Value.Text)
	}

	param, ok := file.Decls[2].(ParamDecl)
	if !ok {
		t.Fatalf("declaration 2 is %T, want ParamDecl", file.Decls[2])
	}
	if param.Type.Text != "time.Duration" || param.Default.Text != "5 * time.Second" {
		t.Errorf("param parsed as type %q default %q", param.Type.Text, param.Default.Text)
	}

	first := flowAt(t, file, 0)
	if first.OnError == nil || first.OnError.Handler.Text != "LogAndDrop" {
		t.Errorf("flow-level on error is %v", first.OnError)
	}
	if first.State == nil || len(first.State.Fields) != 1 {
		t.Fatalf("state block is %v", first.State)
	}
	if got := first.State.Fields[0].Type.Text; got != "map[string]bool" {
		t.Errorf("state field type is %q, want map[string]bool", got)
	}

	if len(first.Vars) != 2 {
		t.Fatalf("got %d vars, want 2", len(first.Vars))
	}
	if first.Vars[0].Clone == nil || first.Vars[0].Clone.Text != "ops.CloneSpan" {
		t.Errorf("clone override is %v", first.Vars[0].Clone)
	}
	// THE TRAP CASE at the declaration level: the type spelling begins with the
	// `func` keyword, which parses only because `func` is absent from the Go
	// span's stop set.
	if first.Vars[1].Type.Text != "func(int) error" {
		t.Errorf("var h type is %q, want %q", first.Vars[1].Type.Text, "func(int) error")
	}
	if first.Vars[1].Clone != nil {
		t.Errorf("var h has no clone override but parsed one: %v", first.Vars[1].Clone)
	}

	if len(first.Body) != 2 {
		t.Fatalf("flow first has %d statements, want 2", len(first.Body))
	}
	second := flowAt(t, file, 1)
	if len(second.Body) != 1 {
		t.Fatalf("flow second has %d statements, want 1 — the first flow's body swallowed it", len(second.Body))
	}
}

// goAwareFuncBody exercises all five forms in which a brace is text rather than
// structure, inside one func declaration.
const goAwareFuncBody = "() error {\n" +
	"\ts := \"}\"\n" +
	"\t// }\n" +
	"\t/* } */\n" +
	"\tr := '}'\n" +
	"\traw := `}`\n" +
	"\t_, _, _ = s, r, raw\n" +
	"\treturn nil\n" +
	"}"

// TestParseFuncDeclaration covers the name extraction, the span boundary, both
// placements, and the one way a func declaration fails syntactically.
func TestParseFuncDeclaration(t *testing.T) {
	file := mustParse(t, "func Before() error { return nil }\n"+
		"\nflow one\nsource ingest Poll\n"+
		"\nfunc Between"+goAwareFuncBody+"\n"+
		"\nflow two\nsource other Gen\ndrop other\n")

	before, ok := file.Decls[0].(FuncDecl)
	if !ok {
		t.Fatalf("declaration 0 is %T, want a FuncDecl declared before any flow", file.Decls[0])
	}
	if before.Name.Name != "Before" {
		t.Errorf("func name is %q, want Before", before.Name.Name)
	}

	var between FuncDecl
	found := false
	for _, decl := range file.Decls {
		if fn, isFunc := decl.(FuncDecl); isFunc && fn.Name.Name == "Between" {
			between, found = fn, true
		}
	}
	if !found {
		t.Fatalf("the func declared between two flows was not parsed as a file-level declaration")
	}
	if between.Body.Text != goAwareFuncBody {
		t.Fatalf("func body span:\n got %q\nwant %q", between.Body.Text, goAwareFuncBody)
	}

	// A func line ENDS the preceding flow body, so the flow after it must own
	// its own statements rather than losing them to the flow before.
	if got := len(flowAt(t, file, 0).Body); got != 1 {
		t.Errorf("flow one has %d statements, want 1", got)
	}
	if got := len(flowAt(t, file, 1).Body); got != 2 {
		t.Errorf("flow two has %d statements, want 2 — the func boundary did not hold", got)
	}

	// The name is the only thing this production requires the parser to find,
	// so its absence is the only way the declaration fails.
	diags := diagnosticsFor(t, "func () error { return nil }\n")
	d := requireDiagnostic(t, diags, "needs a name")
	if d.Pos.Line != 1 || d.Pos.Col != 1 {
		t.Errorf("the unnamed-func diagnostic is at %s, want it on the func keyword at 1:1", d.Pos)
	}
}

// TestParseFlowSignatureAndOutputs covers the signature header.
//
// The MULTI-OUTPUT case is the one that matters: an implementation handling only
// a single output passes every other assertion in this file, and the second
// output's type span is exactly where a naive stop set swallows the rest of the
// header.
func TestParseFlowSignatureAndOutputs(t *testing.T) {
	file := mustParse(t, `flow simple (Order) -> out Result
source in Poll

flow enrich (Order) -> ok EnrichResult, retryable Order, failed error
source other Gen
`)

	simple := flowAt(t, file, 0)
	if simple.Signature == nil {
		t.Fatalf("flow simple parsed no signature")
	}
	if simple.Signature.Input.Text != "Order" {
		t.Errorf("input type is %q, want Order", simple.Signature.Input.Text)
	}
	if len(simple.Signature.Outputs) != 1 {
		t.Fatalf("flow simple has %d outputs, want 1", len(simple.Signature.Outputs))
	}

	enrich := flowAt(t, file, 1)
	if enrich.Signature == nil {
		t.Fatalf("flow enrich parsed no signature")
	}
	outputs := enrich.Signature.Outputs
	if len(outputs) != 3 {
		t.Fatalf("flow enrich has %d outputs, want 3", len(outputs))
	}
	for i, want := range []struct{ name, typ string }{
		{"ok", "EnrichResult"},
		{"retryable", "Order"},
		{"failed", "error"},
	} {
		if outputs[i].Name.Name != want.name || outputs[i].Type.Text != want.typ {
			t.Errorf("output %d is %q %q, want %q %q",
				i, outputs[i].Name.Name, outputs[i].Type.Text, want.name, want.typ)
		}
	}

	// A flow with no signature is legal and must not invent one.
	plain := mustParse(t, "flow plain\nsource in Poll\n")
	if flowAt(t, plain, 0).Signature != nil {
		t.Errorf("a flow with no signature parsed one")
	}
}
