// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"bytes"
	"flag"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
	"github.com/whitaker-io/machine/lang/loader"
)

// updateGolden rewrites the checked-in expectations instead of comparing them.
//
// It exists so a deliberate emitter change is one command rather than hand-edited
// expectations, which is how a golden set stops being an expectation and becomes
// a transcript of whatever the code last did.
var updateGolden = flag.Bool("update-golden", false, "rewrite the golden .flow.go expectations")

// goldenDir holds one directory per case.
const goldenDir = "testdata/golden"

// goldenCase is one fixture directory, loaded.
type goldenCase struct {
	name    string
	dir     string
	source  string
	types   map[string]string
	support string
	// boundary is the per-flow bindable-output fact the driver would supply,
	// read from an OPTIONAL boundary.txt. A fixture carrying no `use` needs
	// none, which is why the file is optional rather than empty-but-required.
	boundary map[string]Boundary
	// upstream is an OPTIONAL second module directory a cross-module `use`
	// reaches, holding its own go.mod and .flow files.
	upstream string
}

// goldenCases loads every fixture directory, refusing an empty read.
func goldenCases(t *testing.T) []goldenCase {
	t.Helper()
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatalf("reading %s: %v", goldenDir, err)
	}

	var cases []goldenCase
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(goldenDir, entry.Name())
		cases = append(cases, goldenCase{
			name:     entry.Name(),
			dir:      dir,
			source:   readGoldenFile(t, filepath.Join(dir, "pipeline.flow")),
			types:    readTypes(t, filepath.Join(dir, "types.txt")),
			support:  readGoldenFile(t, filepath.Join(dir, "support.go.txt")),
			boundary: readBoundary(t, filepath.Join(dir, "boundary.txt")),
			upstream: optionalDir(filepath.Join(dir, "upstream")),
		})
	}
	if len(cases) < 3 {
		t.Fatalf("CONTROL FAILED: %s holds %d cases, want at least 3; a golden sweep over fewer is not evidence",
			goldenDir, len(cases))
	}

	return cases
}

// readGoldenFile reads a fixture file, refusing an empty one.
func readGoldenFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		t.Fatalf("CONTROL FAILED: %s is empty, so any expectation over it is vacuous", path)
	}

	return string(body)
}

// readTypes parses the driver-supplied per-node input type table.
func readTypes(t *testing.T, path string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(readGoldenFile(t, path), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		node, spelling, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("%s: %q is not a node=Type line", path, line)
		}
		out[strings.TrimSpace(node)] = strings.TrimSpace(spelling)
	}
	if len(out) == 0 {
		t.Fatalf("CONTROL FAILED: %s declares no types at all", path)
	}

	return out
}

// readBoundary parses the OPTIONAL per-flow bindable-output fact.
//
// AN ABSENT FILE IS NO FACT AT ALL, which is a different thing from a fact with
// no names — the distinction lang/analysis's Boundaries preserves and this package
// refuses on. A fixture carrying no `use` needs no file; one carrying a `use`
// declares what analysis would have exported for the flow it embeds.
//
// Each line is `flow=out1,out2`, and a flow with no bindable names is written
// `flow=` so a case can state the empty fact deliberately.
func readBoundary(t *testing.T, path string) map[string]Boundary {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading %s: %v", path, err)
	}

	out := map[string]Boundary{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		flow, names, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("%s: %q is not a flow=out1,out2 line", path, line)
		}
		out[strings.TrimSpace(flow)] = Boundary{Outputs: splitNames(names)}
	}
	if len(out) == 0 {
		t.Fatalf("CONTROL FAILED: %s exists and declares no boundary at all", path)
	}

	return out
}

// splitNames splits a comma-separated binding list, answering nil for an empty
// one rather than a slice holding one empty name.
func splitNames(names string) []string {
	var out []string
	for _, name := range strings.Split(names, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}

	return out
}

// optionalDir answers the path when it names a directory, and the empty string
// otherwise.
func optionalDir(path string) string {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return path
	}

	return ""
}

// fixtureResolver answers a dotted reference out of a fixture's own upstream
// directory, standing in for the LOADER and for nothing else.
//
// THE DOUBLE REPLACES A DEPENDENCY, NOT THE CODE UNDER TEST. What a golden pins
// is this package's half of a cross-module `use` — reading the resolved file,
// renaming the declaration, building its graph, splicing its funcs and dropping
// the flow-only import — and every one of those runs through the production
// resolveImportsWith below. Only the import-path-to-module lookup, which is
// lang/loader's and is proven end to end by its own criterion, is supplied here,
// so a golden case does not have to stand up a package load.
func fixtureResolver(dir string) flowResolver {
	return func(_, name string, at ast.Position, from string) (loader.Flow, *loader.Diagnostic) {
		paths, err := filepath.Glob(filepath.Join(dir, "*.flow"))
		if err != nil {
			return loader.Flow{}, &loader.Diagnostic{Path: from, Pos: at, End: at, Message: err.Error()}
		}
		for _, path := range paths {
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				continue
			}
			if declaresFlow(body, name) {
				return loader.Flow{Name: name, File: path, Pos: at}, nil
			}
		}

		return loader.Flow{}, &loader.Diagnostic{Path: from, Pos: at, End: at,
			Message: "no flow named " + name + " in " + dir}
	}
}

// declaresFlow reports whether a .flow source declares a flow by name.
func declaresFlow(body []byte, name string) bool {
	file, err := ast.Parse(body)
	if err != nil || file == nil {
		return false
	}
	for _, decl := range file.Decls {
		if flow, ok := decl.(ast.FlowDecl); ok && flow.Name.Name == name {
			return true
		}
	}

	return false
}

// resolveGoldenImports resolves a case's cross-module references, or answers
// nothing when the case declares no upstream module.
func resolveGoldenImports(t *testing.T, c goldenCase, source Source) map[string]*Program {
	t.Helper()
	if c.upstream == "" {
		return nil
	}

	imported, diags := resolveImportsWith([]Source{source}, fixtureResolver(c.upstream))
	if len(diags) != 0 {
		t.Fatalf("%s: the cross-module references must resolve clean:\n%s",
			c.name, strings.Join(messagesOf(diags), "\n"))
	}
	if len(imported) == 0 {
		t.Fatalf("CONTROL FAILED: %s declares an upstream module but resolved no cross-module reference", c.name)
	}

	out := make(map[string]*Program, len(imported))
	for _, one := range imported {
		out[one.Ref] = one.Program
	}

	return out
}

// generateCase runs the whole pipeline over one fixture.
func generateCase(t *testing.T, c goldenCase) Generated {
	t.Helper()
	file, err := ast.Parse([]byte(c.source))
	if err != nil {
		t.Fatalf("%s: the fixture must parse clean: %v", c.name, err)
	}
	// THE IMPORTS RESOLVE BEFORE THE GRAPH IS BUILT, because resolving splices
	// the dependency's funcs into this file's declarations and drops an import
	// that served only a flow reference — both of which the build and the
	// emission then see.
	imported := resolveGoldenImports(t, c, Source{Path: "pipeline.flow", Src: []byte(c.source), File: file})
	programs, buildDiags := buildFile(file)
	if len(buildDiags) != 0 {
		t.Fatalf("%s: the fixture must build clean:\n%s", c.name, strings.Join(messagesOf(buildDiags), "\n"))
	}
	// EVERY DECLARED FACT IS SEEDED FIRST, including one keyed by a cross-module
	// REFERENCE. An imported flow is not among this file's programs, so a map
	// built only from those would leave its boundary absent — which the lowering
	// correctly refuses, since an absent fact is not an empty one.
	boundary := map[string]Boundary{}
	for flow, declared := range c.boundary {
		boundary[flow] = declared
	}
	for _, p := range programs {
		p.InputTypes = c.types
		if _, declared := boundary[p.Name]; !declared {
			boundary[p.Name] = Boundary{}
		}
	}
	cfg := Config{Package: "generated", Qualifier: "acme"}
	plans, lowerDiags := lowerFile(programs, imported, boundary, cfg)
	if len(lowerDiags) != 0 {
		t.Fatalf("%s: the fixture must lower clean:\n%s", c.name, strings.Join(messagesOf(lowerDiags), "\n"))
	}
	out, emitDiags := Generate(Request{File: file, Programs: programs, Plans: plans, Config: cfg, Source: "pipeline.flow"})
	if len(emitDiags) != 0 {
		t.Fatalf("%s: emission reported:\n%s", c.name, strings.Join(messagesOf(emitDiags), "\n"))
	}

	return out
}

// TestGoldenFilesAreByteStableAndGofmtClean is the DRIFT instrument, and nothing
// more.
//
// A byte diff answers exactly one question — did the output change — so on its
// own it says nothing about whether the output is correct or even compiles. It is
// counted as the drift check it is, and the compile test beside it is what makes
// the golden set evidence.
func TestGoldenFilesAreByteStableAndGofmtClean(t *testing.T) {
	for _, c := range goldenCases(t) {
		t.Run(c.name, func(t *testing.T) {
			out := generateCase(t, c)
			path := filepath.Join(c.dir, "pipeline.flow.go")

			if *updateGolden {
				if err := os.WriteFile(path, out.Source, 0o600); err != nil {
					t.Fatalf("writing %s: %v", path, err)
				}
				t.Logf("rewrote %s", path)

				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s (run with -update-golden to create it): %v", path, err)
			}
			if !bytes.Equal(want, out.Source) {
				t.Errorf("%s drifted from its expectation.\n--- want ---\n%s\n--- got ---\n%s",
					path, want, out.Source)
			}

			// GOFMT CLEANLINESS is asserted on the emitted bytes rather than on
			// the file, so it holds even when the expectation has drifted.
			formatted, err := format.Source(out.Source)
			if err != nil {
				t.Fatalf("the emitted file is not valid Go: %v", err)
			}
			if !bytes.Equal(formatted, out.Source) {
				t.Error("the emitted file is not gofmt-clean")
			}
		})
	}
}

// TestTheEmitterIsDeterministic runs the same fixture twice and requires byte
// equality.
//
// The drift test above compares against a checked-in file, which a
// non-deterministic emitter could still satisfy on the run that produced it. This
// compares two runs in one process, which map iteration order alone would break.
func TestTheEmitterIsDeterministic(t *testing.T) {
	for _, c := range goldenCases(t) {
		t.Run(c.name, func(t *testing.T) {
			first := generateCase(t, c)
			for range 4 {
				again := generateCase(t, c)
				if !bytes.Equal(first.Source, again.Source) {
					t.Fatalf("%s emitted different bytes on a second run", c.name)
				}
			}
		})
	}
}

// TestGoldenModulesCompile is the instrument that makes the golden set EVIDENCE
// rather than a transcript.
//
// It writes each golden file into a temp module beside the Go an author would
// have written and builds it against the LOCAL machine checkout. Because it
// builds against the working tree rather than a published version, it also
// catches the next runtime change automatically.
//
// EXPECT IT TO BE THE SLOWEST TEST IN THIS MODULE: it runs a full `go build` per
// fixture. On a cold module cache the first one dominates.
func TestGoldenModulesCompile(t *testing.T) {
	for _, c := range goldenCases(t) {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(c.dir, "pipeline.flow.go")
			generated, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}

			output, err := buildInWorkspace(t, map[string]string{
				"support.go":       c.support,
				"pipeline.flow.go": string(generated),
			})
			if err != nil {
				t.Fatalf("the checked-in golden does not compile:\n%s\n--- generated ---\n%s", output, generated)
			}
		})
	}
}
