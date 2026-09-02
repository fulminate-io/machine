// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeModule lays down a go.mod carrying body and returns its directory.
func writeModule(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}

	return dir
}

// TestDerivePackagePathAnswersTheImportPathTheToolchainWould is the property the
// whole compile path rests on.
//
// WITH NO PACKAGE PATH EVERY TYPE SPELLING RESOLVES IN AN EMPTY SCOPE, which is
// what made the shipped binary refuse every flow with a typed source. The
// derivation is the module path joined with the directory's path relative to the
// module root, which is how the Go toolchain derives one.
func TestDerivePackagePathAnswersTheImportPathTheToolchainWould(t *testing.T) {
	root := writeModule(t, "module example.com/app\n\ngo 1.27\n")

	t.Run("the module root answers the module path unchanged", func(t *testing.T) {
		got, err := DerivePackagePath(root)
		if err != nil {
			t.Fatalf("the module root did not derive: %v", err)
		}
		if got != "example.com/app" {
			t.Errorf("derived %q, want example.com/app", got)
		}
	})

	t.Run("a nested directory is joined onto the module path", func(t *testing.T) {
		nested := filepath.Join(root, "internal", "generated")
		if err := os.MkdirAll(nested, 0o750); err != nil {
			t.Fatalf("creating the nested directory: %v", err)
		}
		got, err := DerivePackagePath(nested)
		if err != nil {
			t.Fatalf("a nested directory did not derive: %v", err)
		}
		if got != "example.com/app/internal/generated" {
			t.Errorf("derived %q, want example.com/app/internal/generated", got)
		}
	})

	t.Run("the separator is the import path's, never the filesystem's", func(t *testing.T) {
		nested := filepath.Join(root, "a", "b")
		if err := os.MkdirAll(nested, 0o750); err != nil {
			t.Fatalf("creating the nested directory: %v", err)
		}
		got, err := DerivePackagePath(nested)
		if err != nil {
			t.Fatalf("a nested directory did not derive: %v", err)
		}
		if strings.ContainsRune(got, '\\') {
			t.Errorf("the derived path %q carries a filesystem separator", got)
		}
	})
}

// TestDerivePackagePathRefusesRatherThanAnsweringEmpty covers all three refusals.
//
// AN EMPTY STRING WOULD REINTRODUCE THE EMPTY SCOPE this function exists to
// prevent, and it would do it silently: generation would proceed and every typed
// source would fail with a message about payload types that names nothing about
// the real cause.
func TestDerivePackagePathRefusesRatherThanAnsweringEmpty(t *testing.T) {
	t.Run("a go.mod with no module line", func(t *testing.T) {
		dir := writeModule(t, "go 1.27\n")
		got, err := DerivePackagePath(dir)
		if err == nil {
			t.Fatalf("a go.mod declaring no module path derived %q", got)
		}
		if !strings.Contains(err.Error(), "declares no module path") {
			t.Errorf("the refusal is %q, want it to name the missing module line", err)
		}
	})

	t.Run("an unreadable go.mod", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("file mode permissions do not deny a read on this platform")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "go.mod")
		if err := os.WriteFile(path, []byte("module example.com/app\n"), 0o200); err != nil {
			t.Fatalf("writing go.mod: %v", err)
		}
		// THE CONTROL: the same path must be readable once the mode allows it, or
		// the refusal below could be about something other than the read.
		got, err := DerivePackagePath(dir)
		if err == nil {
			t.Fatalf("an unreadable go.mod derived %q", got)
		}
		if !strings.Contains(err.Error(), "reading ") {
			t.Errorf("the refusal is %q, want it to name the file it could not read", err)
		}
		if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
			t.Fatalf("restoring the mode: %v", chmodErr)
		}
		if _, ctlErr := DerivePackagePath(dir); ctlErr != nil {
			t.Fatalf("CONTROL FAILED: the same go.mod still does not derive once readable: %v", ctlErr)
		}
	})
}

// TestDerivePackagePathWalksUpwardToTheNearestModule pins which go.mod answers.
//
// THE NEAREST ENCLOSING ONE WINS, which is the toolchain's rule: a nested module
// is its own module and its parent's path must not leak into it.
func TestDerivePackagePathWalksUpwardToTheNearestModule(t *testing.T) {
	outer := writeModule(t, "module example.com/outer\n\ngo 1.27\n")
	inner := filepath.Join(outer, "sub")
	if err := os.MkdirAll(inner, 0o750); err != nil {
		t.Fatalf("creating the nested module directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inner, "go.mod"),
		[]byte("module example.com/inner\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatalf("writing the nested go.mod: %v", err)
	}

	got, err := DerivePackagePath(inner)
	if err != nil {
		t.Fatalf("the nested module did not derive: %v", err)
	}
	if got != "example.com/inner" {
		t.Errorf("derived %q, want example.com/inner — the NEAREST enclosing module", got)
	}

	// AND THE PARENT STILL ANSWERS ITS OWN, so the walk is not simply preferring
	// whichever module it saw last.
	if outerGot, outerErr := DerivePackagePath(outer); outerErr != nil || outerGot != "example.com/outer" {
		t.Errorf("the outer module derived (%q, %v), want example.com/outer", outerGot, outerErr)
	}
}

// TestDriverPackagePathPrefersTheCallersOverTheDerivation pins the override.
//
// A CALLER WHO SAYS WHICH PACKAGE IT IS GENERATING IS BELIEVED. The -pkgpath flag
// exists for the case the derivation cannot serve, and production sets nothing so
// the derivation answers.
func TestDriverPackagePathPrefersTheCallersOverTheDerivation(t *testing.T) {
	dir := writeModule(t, "module example.com/derived\n\ngo 1.27\n")

	stated := &Driver{PackagePath: "example.com/stated"}
	got, err := stated.packagePath(dir)
	if err != nil {
		t.Fatalf("a stated package path errored: %v", err)
	}
	if got != "example.com/stated" {
		t.Errorf("a stated package path answered %q, want it verbatim", got)
	}
	if stated.PackagePath != "example.com/stated" {
		t.Error("resolving the package path wrote back to the field")
	}

	derived := &Driver{}
	got, err = derived.packagePath(dir)
	if err != nil {
		t.Fatalf("an empty package path did not derive: %v", err)
	}
	if got != "example.com/derived" {
		t.Errorf("an empty package path answered %q, want the derivation", got)
	}
	if derived.PackagePath != "" {
		t.Errorf("the derivation wrote %q back to the field", derived.PackagePath)
	}
}
