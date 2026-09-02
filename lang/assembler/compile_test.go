// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// machineRoot is the local checkout of the runtime module, reached relatively.
func machineRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the machine checkout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("CONTROL FAILED: %s is not a Go module, so nothing here compiles against the real runtime", root)
	}

	return root
}

// buildInWorkspace writes files into a temp module and builds them against the
// LOCAL machine checkout, returning the toolchain's combined output.
//
// IT USES A GO WORKSPACE RATHER THAN A replace DIRECTIVE, and that is not a
// style choice: a replace still needs a `require` naming a version, and the local
// checkout is not a published one, so the toolchain tries to resolve v4.0.0 from
// the network and fails. A workspace resolves the module from the directory
// itself. The root's go.sum is copied in as the workspace sum so the runtime's
// own dependencies resolve from the module cache without a network fetch.
//
// THIS IS THE INSTRUMENT THAT PROVES THE GENERATOR EMITS REAL GO. Text assertions
// over emitted source prove the text is stable; only compiling it proves it
// builds against the runtime it targets.
func buildInWorkspace(t *testing.T, files map[string]string) (string, error) {
	t.Helper()
	root := machineRoot(t)
	dir := t.TempDir()

	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	write("go.mod", "module probe\n\ngo 1.27\n")
	write("go.work", "go 1.27\n\nuse (\n\t.\n\t"+root+"\n)\n")

	if sum, err := os.ReadFile(filepath.Join(root, "go.sum")); err == nil {
		write("go.work.sum", string(sum))
	}
	for name, content := range files {
		write(name, content)
	}

	cmd := exec.Command("go", "build", "-buildvcs=false", "./...")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()

	return string(out), err
}
