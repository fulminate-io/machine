// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// dottedToken is what the fixture's own sink prints once the imported flow has
// passed a datum through. The COUNT is deliberately not asserted: the transport
// pushes on its own while the host pushes in process, so whichever datum arrives
// first decides the number. What is deterministic is that every datum reaches the
// sink only THROUGH the imported flow's branch.
const dottedToken = "flow-e2e: processed="

// TestACrossModuleImportSurvivesEveryAliasAndPathSpelling is the gate on a
// substring-collision defect, and the four cases are the defect's own census.
//
// THE DEFECT. The flow language's `import` serves two purposes — qualifying Go
// names, and naming the module a dotted `use` reaches — and only the first
// belongs in the emitted Go, so an import that served ONLY a flow reference is
// left out. Deciding that by counting the qualifier followed by a dot in the
// source text reads the import declaration's OWN path literal as a use of the
// thing it declares: `import other "other.example/screening"` counts two
// occurrences of `other.` where there is one reference, judges the import needed,
// and emits Go that does not compile.
//
// WHY FOUR CASES AND NOT ONE. Alias and module path are the two variables, and
// only their COMBINATION separates the defect from the feature. Two of these
// spellings collide and two do not, and a fix that simply stopped emitting every
// flow-referenced import would pass the two colliding cases while breaking
// nothing visible — so the two non-colliding ones are the control that keeps the
// gate honest, and a fix that emitted them all would fail the other two.
//
// MEASURED BEFORE THE FIX, with one binary and one fixture per case: `alias other
// / path other.example/screening` gave `"other.example/screening" imported as
// other and not used`, `unnamed / path screening.example/screening` gave
// `"screening.example/screening" imported and not used`, and the other two ran.
func TestACrossModuleImportSurvivesEveryAliasAndPathSpelling(t *testing.T) {
	flowc := flowcBinary(t)

	cases := []struct {
		name string
		// alias is the import's alias, empty for the unnamed form.
		alias string
		// modulePath is the upstream module's path, and the second variable.
		modulePath string
	}{
		{"the alias is the path's first label", "other", "other.example/screening"},
		{"the alias collides with nothing", "scr", "other.example/screening"},
		{"unnamed, and the last segment appears once", "", "other.example/screening"},
		{"unnamed, and the last segment is also the first label", "", "screening.example/screening"},
	}

	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			dir := stageDottedFixture(t, one.alias, one.modulePath)

			cmd := exec.Command(flowc, "-in", ".", "-out", ".", "-package", "main", "-qualifier", "flowdotted")
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("the shipped flowc refused the fixture (%v):\n%s", err, out)
			}

			generated, err := os.ReadFile(filepath.Join(dir, "dotted.flow.go"))
			if err != nil {
				t.Fatalf("the binary wrote no generated file: %v", err)
			}

			run := exec.Command("go", "run", "-buildvcs=false", ".")
			run.Dir = dir
			output, runErr := run.CombinedOutput()
			if runErr != nil {
				t.Fatalf("the generated program did not build and run: %v\n%s\n--- generated ---\n%s",
					runErr, output, generated)
			}
			if !strings.Contains(string(output), dottedToken) {
				t.Fatalf("the program ran but the imported flow passed no datum through: "+
					"its stdout does not carry %q.\n%s\n--- generated ---\n%s",
					dottedToken, output, generated)
			}
		})
	}
}

// stageDottedFixture writes the two-module workspace one case needs and answers
// the consumer's directory.
//
// THE TWO MODULES ARE REAL and the consumer reaches the upstream one by a go.mod
// replace, so the fixture needs no network and no module cache. The upstream
// module carries a .go file because a directory with no package clause is not a
// loaded Go package at all, and the reference would then refuse for that reason
// rather than for anything this test is about.
func stageDottedFixture(t *testing.T, alias, modulePath string) string {
	t.Helper()
	root := machineRoot(t)
	base := t.TempDir()

	up := filepath.Join(base, "up")
	app := filepath.Join(base, "app")
	for _, dir := range []string{up, app} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}

	write := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	write(up, "go.mod", "module "+modulePath+"\n\ngo 1.27\n")
	write(up, "doc.go", "package screening\n")
	write(up, "screen.flow", dottedUpstreamFlow)

	write(app, "go.mod", "module flowdotted\n\ngo 1.27\n\nrequire "+modulePath+
		" v0.0.0\n\nreplace "+modulePath+" => ../up\n")
	write(app, "go.work", "go 1.27\n\nuse (\n\t.\n\t"+root+"\n)\n")
	if sum, err := os.ReadFile(filepath.Join(root, "go.sum")); err == nil {
		write(app, "go.work.sum", string(sum))
	}
	write(app, "feed.go", readFixtureFile(t, filepath.Join("testdata", "e2e", "feed.go.txt")))
	write(app, "main.go", dottedMain)

	qualifier := alias
	statement := "import " + alias + " \"" + modulePath + "\""
	if alias == "" {
		statement = "import \"" + modulePath + "\""
		qualifier = modulePath[strings.LastIndex(modulePath, "/")+1:]
	}
	write(app, "dotted.flow", statement+dottedConsumerFlow(qualifier))

	return app
}

// dottedUpstreamFlow is the exported, signature-carrying flow the consumer
// reaches only through its import path.
const dottedUpstreamFlow = `func Screenable(f machine.Frame[Order]) bool { return f.Value().ID != "" }

flow Screen (Order) -> ok Order, bad Order
branch check Screenable from in -> ok, bad
`

// dottedConsumerFlow is the consumer, whose import serves ONLY the flow
// reference: no Go name in it is qualified with the import, so the emitted file
// must not carry that import.
func dottedConsumerFlow(qualifier string) string {
	return `

func Report(f machine.Frame[Order]) Order {
  v := f.Value()
  Emit(v.Seen)
  return v
}

flow main
source ingest Feed()
use s ` + qualifier + `.Screen from ingest -> ok, bad
sink emit Report from ok
sink audit Audit from bad
`
}

// dottedMain wires the generated flow and waits on the datum rather than on a
// timer.
const dottedMain = `package main

import (
	"context"
	"fmt"
	"os"
	"time"

	machine "github.com/whitaker-io/machine/v4"
)

func main() {
	m := machine.New("crossmodule")
	ingests, err := WireMain(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wiring:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}

	if err := ingests.Ingest(ctx, Order{ID: "pushed", Seen: 2, Tags: []string{"a"}}); err != nil {
		fmt.Fprintln(os.Stderr, "ingest:", err)
		os.Exit(1)
	}

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		fmt.Fprintln(os.Stderr, "the flow drove no datum through")
		os.Exit(1)
	}
}
`
