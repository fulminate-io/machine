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
			name:    entry.Name(),
			dir:     dir,
			source:  readGoldenFile(t, filepath.Join(dir, "pipeline.flow")),
			types:   readTypes(t, filepath.Join(dir, "types.txt")),
			support: readGoldenFile(t, filepath.Join(dir, "support.go.txt")),
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

// generateCase runs the whole pipeline over one fixture.
func generateCase(t *testing.T, c goldenCase) Generated {
	t.Helper()
	file, err := ast.Parse([]byte(c.source))
	if err != nil {
		t.Fatalf("%s: the fixture must parse clean: %v", c.name, err)
	}
	programs, buildDiags := buildFile(file)
	if len(buildDiags) != 0 {
		t.Fatalf("%s: the fixture must build clean:\n%s", c.name, strings.Join(messagesOf(buildDiags), "\n"))
	}
	boundary := map[string]Boundary{}
	for _, p := range programs {
		p.InputTypes = c.types
		boundary[p.Name] = Boundary{}
	}
	cfg := Config{Package: "generated", Qualifier: "acme"}
	plans, lowerDiags := lowerFile(programs, boundary, cfg)
	if len(lowerDiags) != 0 {
		t.Fatalf("%s: the fixture must lower clean:\n%s", c.name, strings.Join(messagesOf(lowerDiags), "\n"))
	}
	out, emitDiags := Generate(file, programs, plans, cfg, "pipeline.flow")
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
