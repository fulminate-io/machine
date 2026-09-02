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

	"github.com/whitaker-io/machine/lang/ast"
	"github.com/whitaker-io/machine/lang/loader"
)

// teeFixture splits a payload CARRYING A SLICE, which is the only shape that can
// tell a deep copy from a shallow one: two branches holding the same backing
// array look identical until one of them writes.
const teeFixture = "flow split\n" +
	"var attempt []string\n" +
	"source ingest Poll()\n" +
	"tee fan from ingest -> left, right\n" +
	"sink kept Store from left\n" +
	"sink lost Store from right\n"

// teeSupport is the Go an author writes beside teeFixture.
const teeSupport = `package generated

import machine "github.com/whitaker-io/machine/v4"

type Order struct {
	ID   string
	Tags []string
}

func Poll() machine.EdgeFactory[Order] { return machine.Channel[Order](0) }

func Store(f machine.Frame[Order]) Order { return f.Value() }
`

// generateWithTypes runs the whole pipeline with a package set loaded over the
// AUTHOR'S Go, which is where the payload types are declared.
//
// THE TYPES COME FROM THE AUTHOR'S PACKAGE, NOT THE GENERATED FILE, and they have
// to: the synthesis runs BEFORE emission, so it cannot read a file that does not
// exist yet. What it needs is the structure of the types the .flow names, and
// those live in the Go beside it.
func generateWithTypes(t *testing.T, src, support string, types map[string]string) (Generated, string) {
	t.Helper()
	dir := t.TempDir()
	root := machineRoot(t)

	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	write("go.mod", "module probe\n\ngo 1.27\n")
	write("go.work", "go 1.27\n\nuse (\n\t.\n\t"+root+"\n)\n")
	write("support.go", support)
	if sum, err := os.ReadFile(filepath.Join(root, "go.sum")); err == nil {
		write("go.work.sum", string(sum))
	}

	pkgs, err := loader.Load(dir, []string{"./..."})
	if err != nil {
		t.Fatalf("loading the author's package: %v", err)
	}
	typed := NewTypes(pkgs, "probe", map[int]ast.Position{})

	file, parseErr := ast.Parse([]byte(src))
	if parseErr != nil {
		t.Fatalf("the fixture must parse clean: %v", parseErr)
	}
	programs, buildDiags := buildFile(file)
	if len(buildDiags) != 0 {
		t.Fatalf("the fixture must build clean: %v", messagesOf(buildDiags))
	}
	boundary := map[string]Boundary{}
	for _, p := range programs {
		p.InputTypes = types
		boundary[p.Name] = Boundary{}
	}
	cfg := Config{Package: "generated", Qualifier: "acme"}
	plans, lowerDiags := lowerFile(programs, boundary, cfg)
	if len(lowerDiags) != 0 {
		t.Fatalf("the fixture must lower clean:\n%s", strings.Join(messagesOf(lowerDiags), "\n"))
	}
	out, emitDiags := Generate(file, programs, plans, cfg, "pipeline.flow", typed)
	if len(emitDiags) != 0 {
		t.Fatalf("emission reported:\n%s", strings.Join(messagesOf(emitDiags), "\n"))
	}

	return out, dir
}

// TestUnsynthesizableCloneIsRefusedBeforeTypeResolution asserts the POSITIVE, and
// its name is deliberately kept.
//
// IT SUPERSEDES A REFUSAL TEST. Before type resolution existed, a tee and a var
// of a non-trivially-copyable type were REFUSED, because no duplicator or cloner
// could be derived and the only alternative was a shallow copy. This test asserted
// those refusals fired. With the derivation landed the refusals are gone, so the
// old body would be permanently red against correct work; the name is kept so the
// plan's own criterion still resolves to a live test rather than being left
// pointing at nothing.
//
// WHAT IS STILL REFUSED, and the distinction is the whole content of the change:
// the shapes are refused when NO TYPE INFORMATION IS AVAILABLE, not because they
// are tees and slices. Given types, both generate.
func TestUnsynthesizableCloneIsRefusedBeforeTypeResolution(t *testing.T) {
	t.Run("with types, a tee and a non-trivial var generate", func(t *testing.T) {
		out, _ := generateWithTypes(t, teeFixture, teeSupport,
			map[string]string{"ingest": "Order", "fan": "Order", "kept": "Order", "lost": "Order"})

		body := string(out.Source)
		if !strings.Contains(body, "duplicateFlow_") {
			t.Errorf("the tee emitted no synthesized duplicator:\n%s", body)
		}
		if !strings.Contains(body, "cloneFlow_") {
			t.Errorf("the non-trivial var emitted no synthesized cloner:\n%s", body)
		}
		// THE SHALLOW FORM MUST NOT APPEAR. `return d, d` is the duplicator a
		// generator with no derivation would write, and it compiles.
		if strings.Contains(body, "return d, d") {
			t.Errorf("the emitted duplicator returns its input twice, which is a shallow copy:\n%s", body)
		}
	})

	t.Run("without types, both are refused rather than shallow-copied", func(t *testing.T) {
		// THE REFUSAL SURVIVES WHERE IT IS STILL TRUE. A shallow copy here would
		// leave two branches sharing backing memory with no diagnostic at all,
		// which is why the answer is a refusal rather than a best effort.
		file, err := ast.Parse([]byte(teeFixture))
		if err != nil {
			t.Fatalf("the fixture must parse clean: %v", err)
		}
		programs, _ := buildFile(file)
		for _, p := range programs {
			p.InputTypes = map[string]string{"ingest": "Order", "fan": "Order", "kept": "Order", "lost": "Order"}
		}
		cfg := Config{Package: "generated", Qualifier: "acme"}
		plans, _ := lowerFile(programs, map[string]Boundary{"split": {}}, cfg)

		_, diags := Generate(file, programs, plans, cfg, "pipeline.flow", nil)
		if len(diags) == 0 {
			t.Fatal("a tee and a slice var were emitted with no type information and no diagnostic")
		}
		joined := strings.Join(messagesOf(diags), "\n")
		for _, want := range []string{"sharing the same memory"} {
			if !strings.Contains(joined, want) {
				t.Errorf("the refusal %q does not say what a shallow copy would cost", joined)
			}
		}
	})
}

// TestTeeDuplicatorIsSynthesized proves the duplicator is a DEEP copy by BUILDING
// AND RUNNING the generated flow, not by inspecting its text.
//
// A text assertion cannot tell a deep duplicator from a shallow one that happens
// to call a function: the difference only appears when one branch mutates and the
// other is read. This mirrors the runtime's own TestTeeDeepCopyIsolatesBranches at
// the generated-code level.
func TestTeeDuplicatorIsSynthesized(t *testing.T) {
	out, _ := generateWithTypes(t, teeFixture, teeSupport,
		map[string]string{"ingest": "Order", "fan": "Order", "kept": "Order", "lost": "Order"})

	const driver = `package main

import (
	"fmt"

	"probe/generated"
)

func main() {
	original := generated.Order{ID: "one", Tags: []string{"a", "b"}}
	left, right := generated.DuplicateOrderForTest(original)

	// MUTATE ONE BRANCH. If the duplicator were shallow the two would share the
	// same backing array and this write would be visible on the right.
	left.Tags[0] = "MUTATED"

	fmt.Printf("left=%s right=%s isolated=%v\n", left.Tags[0], right.Tags[0], right.Tags[0] == "a")
}
`

	// The generated duplicator is unexported, so the fixture exposes it under a
	// stable name rather than the test reaching into the package.
	exposure := "package generated\n\n" +
		"// DuplicateOrderForTest exposes the synthesized duplicator so a driver can\n" +
		"// exercise it. The generator emits the duplicator unexported.\n" +
		"func DuplicateOrderForTest(o Order) (Order, Order) { return " +
		duplicatorName("Order") + "(o) }\n"

	dir := t.TempDir()
	root := machineRoot(t)
	write := func(path, body string) {
		t.Helper()
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	write("go.mod", "module probe\n\ngo 1.27\n")
	write("go.work", "go 1.27\n\nuse (\n\t.\n\t"+root+"\n)\n")
	if sum, err := os.ReadFile(filepath.Join(root, "go.sum")); err == nil {
		write("go.work.sum", string(sum))
	}
	write("generated/support.go", teeSupport)
	write("generated/pipeline.flow.go", string(out.Source))
	write("generated/expose.go", exposure)
	write("main.go", driver)

	cmd := exec.Command("go", "run", "-buildvcs=false", ".")
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the generated flow did not build and run:\n%s", output)
	}

	got := strings.TrimSpace(string(output))
	if !strings.Contains(got, "isolated=true") {
		t.Fatalf("the synthesized duplicator is a SHALLOW copy: mutating one branch changed the other.\n%s", got)
	}
	t.Logf("the synthesized duplicator isolates its branches at run time: %s", got)
}
