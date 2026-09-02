// Package loader - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package loader

import (
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	subjectDir  = "testdata/subject"
	subjectPath = "example.com/subject"
	brokenDir   = "testdata/broken"
	decoyDir    = "testdata/decoy"
	decoyFile   = "testdata/decoy/notmine.flow"
)

// TestTheLoadingSurfaceRefusesLoudlyAndSplitsReceivers exercises the four
// behaviors the assembler and lane F build on, against real modules on disk.
//
// It is one test rather than four because the legs share an expensive load and
// because leg 3's control — the CLEAN module reporting zero problems in the same
// run as the broken one reports some — is only a control if both are measured
// together. A run where the clean module were absent could not distinguish a
// working diagnostic surface from one that always answers.
func TestTheLoadingSurfaceRefusesLoudlyAndSplitsReceivers(t *testing.T) {
	t.Run("an empty pattern set is refused rather than loaded as nothing", func(t *testing.T) {
		if _, err := Load(subjectDir, nil); err == nil {
			t.Fatal("Load accepted an empty pattern set instead of refusing it")
		}

		if _, err := Load(subjectDir, []string{}); err == nil {
			t.Fatal("Load accepted an empty pattern slice instead of refusing it")
		}
	})

	loaded, err := Load(subjectDir, []string{"./..."})
	if err != nil {
		t.Fatalf("the clean fixture module did not load: %v", err)
	}

	t.Run("what cannot be resolved is refused by name", func(t *testing.T) {
		unloaded, err := loaded.Resolve("example.com/never-loaded", "Payload")
		if err == nil {
			t.Fatal("Resolve invented an answer for a package that was never loaded")
		}

		if unloaded != nil {
			t.Fatalf("Resolve refused but still handed back a type: %v", unloaded)
		}

		if !strings.Contains(err.Error(), "example.com/never-loaded") {
			t.Fatalf("the refusal does not name the package it could not find: %v", err)
		}

		absent, err := loaded.Resolve(subjectPath, "NoSuchTypeHere")
		if err == nil {
			t.Fatal("Resolve invented an answer for a spelling that denotes no type")
		}

		if absent != nil {
			t.Fatalf("Resolve refused but still handed back a type: %v", absent)
		}

		if !strings.Contains(err.Error(), "NoSuchTypeHere") {
			t.Fatalf("the refusal does not name the spelling it could not resolve: %v", err)
		}
	})

	t.Run("a package that does not type-check yields positioned diagnostics", func(t *testing.T) {
		if clean := loaded.Errors(); len(clean) != 0 {
			t.Fatalf("the clean fixture module reported %d type-check problems: %v", len(clean), clean)
		}

		damaged, err := Load(brokenDir, []string{"./..."})
		if err != nil {
			t.Fatalf("the broken fixture module failed to LOAD, which is a different failure: %v", err)
		}

		found := damaged.Errors()
		if len(found) == 0 {
			t.Fatal("a package that does not type-check reported no diagnostics at all")
		}

		first := found[0]
		t.Logf("type-check diagnostic: %s:%d:%d: %s", first.Path, first.Pos.Line, first.Pos.Col, first.Message)

		if !strings.HasSuffix(first.Path, "broken.go") {
			t.Fatalf("the diagnostic does not name the offending file: %q", first.Path)
		}

		if first.Pos.Line == 0 || first.Pos.Col == 0 {
			t.Fatalf("the diagnostic carries no position: line=%d col=%d", first.Pos.Line, first.Pos.Col)
		}
	})

	t.Run("the value and pointer method sets are two separate answers", func(t *testing.T) {
		scope, ok := loaded.Scope(subjectPath)
		if !ok {
			t.Fatalf("the subject package %q was not loaded, so an answer about it would prove nothing", subjectPath)
		}

		if scope.Lookup("Payload") == nil {
			t.Fatal("the subject package does not declare Payload, so a false answer could come from an empty scope")
		}

		payload, err := loaded.Resolve(subjectPath, "Payload")
		if err != nil {
			t.Fatalf("the subject type did not resolve: %v", err)
		}

		encoder := iface(t, loaded, "gob.GobEncoder")
		decoder := iface(t, loaded, "gob.GobDecoder")

		if value, pointer := Carries(payload, encoder); !value || !pointer {
			t.Fatalf("GobEncode has a VALUE receiver so both sets carry it: value=%v pointer=%v", value, pointer)
		}

		if value, pointer := Carries(payload, decoder); value || !pointer {
			t.Fatalf("GobDecode has a POINTER receiver so only the pointer set carries it: value=%v pointer=%v", value, pointer)
		}
	})
}

// iface resolves an interface by its spelling in the subject package's own
// scope, so the test measures the loader's own resolution rather than an
// interface it hand-built and then agreed with.
func iface(t *testing.T, loaded *Packages, spelling string) *types.Interface {
	t.Helper()

	resolved, err := loaded.Resolve(subjectPath, spelling)
	if err != nil {
		t.Fatalf("%s did not resolve: %v", spelling, err)
	}

	under, ok := resolved.Underlying().(*types.Interface)
	if !ok {
		t.Fatalf("%s resolved to %v, which is not an interface", spelling, resolved)
	}

	return under
}

// TestSourcesFindsFlowFilesThroughTheResolvedModuleDirectory proves discovery
// walks the directory the Go toolchain RESOLVED for the module rather than the
// process working directory.
//
// The two implementations are indistinguishable without help: the subject module
// sits under this test's own working directory, so a walk rooted at "." returns
// the same files as a walk rooted at the module. THE DECOY IS WHAT SEPARATES
// THEM — a .flow file under testdata but outside the subject module, which only
// a wrongly-rooted walk can collect.
//
// The assertions are on the PROPERTY, never on a count: every path absolute and
// under the resolved module directory, the decoy absent, both fixture depths
// reached, the result sorted. A pinned count is a scheduled false failure
// against correct work, because the next fixture anyone adds breaks it.
func TestSourcesFindsFlowFilesThroughTheResolvedModuleDirectory(t *testing.T) {
	loaded, err := Load(subjectDir, []string{"./..."})
	if err != nil {
		t.Fatalf("the subject fixture module did not load: %v", err)
	}

	mod, ok := loaded.Module(subjectPath)
	if !ok {
		t.Fatalf("no module was resolved for %q, so a walk proves nothing", subjectPath)
	}

	if mod.Dir == "" {
		t.Fatal("the resolved module directory is empty, so a walk rooted at it would walk nothing")
	}

	// CONTROL: the decoy must actually be on disk, or "the walk excluded it" is
	// indistinguishable from "there was nothing to exclude".
	if _, err := os.Stat(decoyFile); err != nil {
		t.Fatalf("the decoy fixture is missing, so this test cannot separate the two walks: %v", err)
	}

	decoyRoot, err := filepath.Abs(decoyDir)
	if err != nil {
		t.Fatalf("the decoy directory did not resolve: %v", err)
	}

	sources, err := loaded.Sources(subjectPath)
	if err != nil {
		t.Fatalf("Sources refused for a module that resolved: %v", err)
	}

	if len(sources) == 0 {
		t.Fatal("Sources found no flow files at all in a module that carries them")
	}

	atRoot, deeper := false, false

	for _, path := range sources {
		if !filepath.IsAbs(path) {
			t.Errorf("%q is not absolute, so it cannot have come from the resolved module directory", path)

			continue
		}

		if !underDir(path, mod.Dir) {
			t.Errorf("%q is not rooted at the resolved module directory %q, so the walk did not start there", path, mod.Dir)
		}

		if underDir(path, decoyRoot) {
			t.Errorf("the walk collected %q, which sits outside the subject module", path)
		}

		if filepath.Dir(path) == mod.Dir {
			atRoot = true
		} else if underDir(path, mod.Dir) {
			deeper = true
		}
	}

	if !atRoot {
		t.Error("no source was found at the module's own directory")
	}

	if !deeper {
		t.Error("no source was found below the module's own directory, so the walk is not recursive")
	}

	if !sort.StringsAreSorted(sources) {
		t.Errorf("Sources returned an unsorted result, so its output depends on directory order: %v", sources)
	}

	t.Logf("resolved module dir %s, %d flow sources", mod.Dir, len(sources))
}

// underDir reports whether path sits at or beneath dir, comparing at path
// separator boundaries so a sibling directory sharing a name prefix is not
// mistaken for a child.
func underDir(path, dir string) bool {
	trimmed := strings.TrimSuffix(dir, string(filepath.Separator)) + string(filepath.Separator)

	return path == dir || strings.HasPrefix(path, trimmed)
}
