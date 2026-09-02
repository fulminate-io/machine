// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
	"github.com/whitaker-io/machine/lang/loader"
)

// crossmoduleDir is the two-module tree that replaced the retired dotted-use
// refusal fixture.
const crossmoduleDir = "testdata/crossmodule"

// upstreamFlow is the .flow the upstream module declares.
const upstreamFlow = crossmoduleDir + "/upstream/screen.flow"

// crossSource parses one .flow into a Source carrying its bytes.
func crossSource(t *testing.T, path string) Source {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	file, parseErr := ast.Parse(body)
	if parseErr != nil {
		t.Fatalf("%s does not parse: %v", path, parseErr)
	}

	return Source{Path: filepath.Base(path), Src: body, File: file}
}

// answering builds a resolver that answers with one declaration.
//
// IT STANDS IN FOR THE LOADER AND FOR NOTHING ELSE. Everything after the lookup
// — reading the resolved file, renaming its declaration, building its graph,
// splicing its funcs and dropping a flow-only import — is this package's own and
// runs for real below. The lookup itself is proven end to end by the shipped
// binary's own criterion.
func answering(name, file string) flowResolver {
	return func(_, _ string, at ast.Position, _ string) (loader.Flow, *loader.Diagnostic) {
		return loader.Flow{Name: name, File: file, Pos: at}, nil
	}
}

// refusing builds a resolver that hands back the upstream module's own refusal.
func refusing(message string) flowResolver {
	return func(_, _ string, at ast.Position, from string) (loader.Flow, *loader.Diagnostic) {
		return loader.Flow{}, &loader.Diagnostic{Path: from, Pos: at, End: at, Message: message}
	}
}

// TestADottedUseResolvesRenamesAndSplices is the resolution's whole product.
func TestADottedUseResolvesRenamesAndSplices(t *testing.T) {
	src := crossSource(t, crossmoduleDir+"/app/app.flow")

	imported, diags := resolveImportsWith([]Source{src}, answering("Screen", upstreamFlow))
	if len(diags) != 0 {
		t.Fatalf("the resolution reported:\n%s", strings.Join(messagesOf(diags), "\n"))
	}
	if len(imported) != 1 {
		t.Fatalf("the resolution produced %d imports, want 1", len(imported))
	}

	one := imported[0]
	if one.Ref != "upstream.Screen" {
		t.Errorf("the import is keyed %q, want the reference the author wrote", one.Ref)
	}
	// THE DECLARATION IS RENAMED TO THE REFERENCE, which is what makes every
	// downstream key — the dependency set, the boundary fact, the inlined node
	// names — the thing the author wrote rather than the name the dependency chose.
	if one.Program == nil || one.Program.Name != "upstream.Screen" {
		t.Errorf("the resolved graph is named %v, want upstream.Screen", one.Program)
	}

	// THE DEPENDENCY'S FUNCS ARE SPLICED IN, because an inlined body is pasted
	// into the consumer's generated package and its Go references resolve there.
	if !declaresFunc(src.File, "Screenable") {
		t.Error("the dependency's func was not spliced into the consuming file")
	}
	// AND ITS FLOW DECLARATIONS ARE NOT, because those would become plans of this
	// file and the dependency's own build already generates its wiring.
	for _, decl := range src.File.Decls {
		if flow, ok := decl.(ast.FlowDecl); ok && flow.Name.Name == "upstream.Screen" {
			t.Error("the dependency's flow declaration was spliced in and would become a second plan")
		}
	}
	// AND THE FLOW-ONLY IMPORT IS DROPPED. Nothing in this file qualifies a Go
	// name with it, so emitting it produces `imported and not used`.
	if declaresImport(src.File, `"example.com/upstream"`) {
		t.Error("the import that served only the flow reference survived into the emitted declarations")
	}
}

// TestAnImportStillNeededByAGoReferenceIsKept is the other side of the drop, and
// without it the drop is indistinguishable from dropping every import.
func TestAnImportStillNeededByAGoReferenceIsKept(t *testing.T) {
	const body = `import "example.com/upstream"

func Report(f machine.Frame[upstream.Order]) upstream.Order { return f.Value() }

flow main
source ingest Feed()
use s upstream.Screen from ingest -> ok, bad
sink emit Report from ok
drop bad
`
	file, err := ast.Parse([]byte(body))
	if err != nil {
		t.Fatalf("the fixture does not parse: %v", err)
	}
	src := Source{Path: "app.flow", Src: []byte(body), File: file}

	if _, diags := resolveImportsWith([]Source{src}, answering("Screen", upstreamFlow)); len(diags) != 0 {
		t.Fatalf("the resolution reported:\n%s", strings.Join(messagesOf(diags), "\n"))
	}
	if !declaresImport(src.File, `"example.com/upstream"`) {
		t.Error("an import a Go reference still qualifies was dropped; the generated file would not compile")
	}
}

// TestTheResolversOwnRefusalsAreCarriedBackVerbatim covers the two refusals this
// file owns and the one it relays.
func TestTheResolversOwnRefusalsAreCarriedBackVerbatim(t *testing.T) {
	refusals := []struct {
		name     string
		body     string
		resolve  flowResolver
		fragment string
	}{
		{
			name:     "a reference whose part count is not two",
			body:     "import \"example.com/upstream\"\n\nflow main\nsource ingest Feed()\nuse s upstream.deep.Screen from ingest -> ok\nsink emit Audit from ok\n",
			resolve:  answering("Screen", upstreamFlow),
			fragment: "a cross-module reference is written <import>.<Flow>",
		},
		{
			name:     "a qualifier no import declares",
			body:     "flow main\nsource ingest Feed()\nuse s nowhere.Screen from ingest -> ok\nsink emit Audit from ok\n",
			resolve:  answering("Screen", upstreamFlow),
			fragment: `names the import "nowhere", which this file does not declare`,
		},
		{
			name:     "the upstream module's own refusal",
			body:     "import \"example.com/upstream\"\n\nflow main\nsource ingest Feed()\nuse s upstream.hidden from ingest -> ok\nsink emit Audit from ok\n",
			resolve:  refusing("no flow named hidden in module example.com/upstream"),
			fragment: "no flow named hidden in module example.com/upstream",
		},
	}

	for _, refusal := range refusals {
		t.Run(refusal.name, func(t *testing.T) {
			file, err := ast.Parse([]byte(refusal.body))
			if err != nil {
				t.Fatalf("the fixture does not parse: %v", err)
			}
			src := Source{Path: "app.flow", Src: []byte(refusal.body), File: file}

			imported, diags := resolveImportsWith([]Source{src}, refusal.resolve)
			if len(imported) != 0 {
				t.Errorf("a refused reference still produced %d import(s)", len(imported))
			}
			joined := strings.Join(messagesOf(diags), "\n")
			if !strings.Contains(joined, refusal.fragment) {
				t.Errorf("the refusal does not contain %q.\ngot:\n%s", refusal.fragment, joined)
			}
		})
	}
}

// TestAResolvedFlowThatCannotBeBuiltCarriesTheDependencysPath pins the
// attribution.
//
// THE AUTHOR NEEDS TO KNOW THE PROBLEM IS IN THE MODULE THEY IMPORTED rather than
// in the file they are writing; a diagnostic positioned in their own file sends
// them looking in the wrong place.
func TestAResolvedFlowThatCannotBeBuiltCarriesTheDependencysPath(t *testing.T) {
	src := crossSource(t, crossmoduleDir+"/app/app.flow")

	cases := []struct {
		name     string
		resolve  flowResolver
		fragment string
	}{
		{
			name:     "a file that does not exist",
			resolve:  answering("Screen", crossmoduleDir+"/upstream/absent.flow"),
			fragment: "could not be read",
		},
		{
			name:     "a file that no longer declares the flow",
			resolve:  answering("NotDeclared", upstreamFlow),
			fragment: "is no longer declared in",
		},
	}

	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			imported, diags := resolveImportsWith([]Source{src}, one.resolve)
			if len(imported) != 0 {
				t.Errorf("an unbuildable reference still produced %d import(s)", len(imported))
			}
			if len(diags) == 0 {
				t.Fatal("an unbuildable reference reported nothing")
			}
			joined := strings.Join(messagesOf(diags), "\n")
			if !strings.Contains(joined, one.fragment) {
				t.Errorf("the refusal does not contain %q.\ngot:\n%s", one.fragment, joined)
			}
			for _, d := range diags {
				if !strings.Contains(d.Path, "upstream") {
					t.Errorf("the refusal is attributed to %q, want the DEPENDENCY's path", d.Path)
				}
			}
		})
	}
}

// TestADependencyTheBuilderRefusesIsReportedAgainstTheDependency covers the
// third attribution arm and the import splice in one run.
//
// THE DEPENDENCY'S OWN DIAGNOSTICS CARRY ITS PATH, not the consumer's, and its
// IMPORTS are spliced into the consuming file alongside its funcs — an inlined
// body's Go references have to resolve in the package they are pasted into.
func TestADependencyTheBuilderRefusesIsReportedAgainstTheDependency(t *testing.T) {
	src := crossSource(t, crossmoduleDir+"/app/app.flow")

	imported, diags := resolveImportsWith([]Source{src},
		answering("Broken", crossmoduleDir+"/upstream/broken.flow"))
	if len(imported) != 0 {
		t.Errorf("a dependency the builder refused still produced %d import(s)", len(imported))
	}
	if len(diags) == 0 {
		t.Fatal("a dependency the builder refused reported nothing")
	}
	for _, d := range diags {
		if !strings.Contains(d.Path, "broken.flow") {
			t.Errorf("the refusal is attributed to %q, want the dependency's own file", d.Path)
		}
	}
}

// TestADependencysImportsAreSplicedIntoTheConsumer pins the import half of the
// splice, which the funcs half alone does not cover.
func TestADependencysImportsAreSplicedIntoTheConsumer(t *testing.T) {
	const body = `import "example.com/upstream"

flow main
source ingest Feed()
use s upstream.Screen from ingest -> ok, bad
sink emit Audit from ok
drop bad
`
	file, err := ast.Parse([]byte(body))
	if err != nil {
		t.Fatalf("the fixture does not parse: %v", err)
	}
	src := Source{Path: "app.flow", Src: []byte(body), File: file}

	// The dependency carries its own import, which must arrive in the consumer.
	// It is resolved from broken.flow's PARSE rather than its build, so this is
	// the splice under test rather than the build refusal above.
	dep := crossSource(t, crossmoduleDir+"/upstream/broken.flow")
	spliceDependency(src.File, dep.File)

	if !declaresImport(src.File, `"example.com/nowhere"`) {
		t.Error("the dependency's import was not spliced into the consuming file")
	}
	if !declaresFunc(src.File, "Keep") {
		t.Error("the dependency's func was not spliced into the consuming file")
	}

	// A SECOND SPLICE OF THE SAME DEPENDENCY ADDS NOTHING. Two references to one
	// module would otherwise paste a duplicate declaration and the generated file
	// would not compile.
	before := len(src.File.Decls)
	spliceDependency(src.File, dep.File)
	if len(src.File.Decls) != before {
		t.Errorf("splicing the same dependency twice grew the declarations from %d to %d",
			before, len(src.File.Decls))
	}
}

// TestAnImportForADifferentQualifierIsNotDropped pins that the drop is keyed on
// the qualifier the resolved reference actually consumed.
func TestAnImportForADifferentQualifierIsNotDropped(t *testing.T) {
	const body = `import "example.com/upstream"
import "example.com/other"

flow main
source ingest Feed()
use s upstream.Screen from ingest -> ok, bad
sink emit Audit from ok
drop bad
`
	file, err := ast.Parse([]byte(body))
	if err != nil {
		t.Fatalf("the fixture does not parse: %v", err)
	}
	src := Source{Path: "app.flow", Src: []byte(body), File: file}

	if _, diags := resolveImportsWith([]Source{src}, answering("Screen", upstreamFlow)); len(diags) != 0 {
		t.Fatalf("the resolution reported:\n%s", strings.Join(messagesOf(diags), "\n"))
	}
	if declaresImport(src.File, `"example.com/upstream"`) {
		t.Error("the import that served only the flow reference survived")
	}
	if !declaresImport(src.File, `"example.com/other"`) {
		t.Error("an import no flow reference consumed was dropped anyway")
	}
}

// TestResolveImportsIsSilentWithoutAPackageSet pins the production guard: a
// driver with no loaded packages resolves nothing rather than refusing a file
// that carries no cross-module reference at all.
func TestResolveImportsIsSilentWithoutAPackageSet(t *testing.T) {
	src := crossSource(t, crossmoduleDir+"/app/app.flow")

	imported, diags := resolveImports([]Source{src}, nil)
	if len(imported) != 0 || len(diags) != 0 {
		t.Errorf("a nil package set produced %d import(s) and %d diagnostic(s), want none",
			len(imported), len(diags))
	}
}

// TestAFileWithNoDottedUseIsLeftEntirelyAlone is the known-positive's negative
// twin: the resolution must not touch a file it has no business in.
func TestAFileWithNoDottedUseIsLeftEntirelyAlone(t *testing.T) {
	const body = `import "example.com/upstream"

flow main
source ingest Feed()
sink emit Audit from ingest
`
	file, err := ast.Parse([]byte(body))
	if err != nil {
		t.Fatalf("the fixture does not parse: %v", err)
	}
	src := Source{Path: "app.flow", Src: []byte(body), File: file}
	before := len(src.File.Decls)

	imported, diags := resolveImportsWith([]Source{src}, answering("Screen", upstreamFlow))
	if len(imported) != 0 || len(diags) != 0 {
		t.Errorf("a file with no dotted use produced %d import(s) and %d diagnostic(s)",
			len(imported), len(diags))
	}
	if len(src.File.Decls) != before {
		t.Errorf("the file's declarations moved from %d to %d", before, len(src.File.Decls))
	}
	if !declaresImport(src.File, `"example.com/upstream"`) {
		t.Error("an import was dropped from a file carrying no flow reference at all")
	}
}

// TestQualifierOfFollowsGosOwnRule pins the alias-else-last-segment rule both the
// load patterns and the resolution depend on.
func TestQualifierOfFollowsGosOwnRule(t *testing.T) {
	cases := map[string]struct {
		decl ast.ImportDecl
		want string
	}{
		"the last path segment": {ast.ImportDecl{Path: `"example.com/upstream"`}, "upstream"},
		"an alias wins":         {ast.ImportDecl{Path: `"example.com/upstream"`, Alias: &ast.Ident{Name: "up"}}, "up"},
		"a single-segment path": {ast.ImportDecl{Path: `"upstream"`}, "upstream"},
	}

	for name, one := range cases {
		t.Run(name, func(t *testing.T) {
			if got := qualifierOf(one.decl); got != one.want {
				t.Errorf("qualifierOf answered %q, want %q", got, one.want)
			}
		})
	}
}
