// Package loader - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package loader

import (
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
)

// TestPositionParsingRefusesAsAUnit covers the shapes a packages.Error position
// can take that are NOT `file:line:col`.
//
// The refusal is deliberately all-or-nothing: a string that does not end in two
// parseable numbers yields the whole string as the path and a zero position,
// rather than a path silently truncated at whichever colon happened to be last.
// A half-parsed position is worse than an unparsed one, because it names a file
// that does not exist.
func TestPositionParsingRefusesAsAUnit(t *testing.T) {
	for _, probe := range []struct {
		in       string
		wantPath string
		wantLine int
		wantCol  int
	}{
		{"/tmp/a.go:4:17", "/tmp/a.go", 4, 17},
		{"C:/win/a.go:4:17", "C:/win/a.go", 4, 17},
		{"/tmp/a.go:4", "/tmp/a.go:4", 0, 0},
		{"/tmp/a.go", "/tmp/a.go", 0, 0},
		{"", "", 0, 0},
		{"/tmp/a.go:x:17", "/tmp/a.go:x:17", 0, 0},
		{"/tmp/a.go:4:x", "/tmp/a.go:4:x", 0, 0},
	} {
		path, at := splitPos(probe.in)
		if path != probe.wantPath || at.Line != probe.wantLine || at.Col != probe.wantCol {
			t.Errorf("splitPos(%q) = %q %d:%d, want %q %d:%d",
				probe.in, path, at.Line, at.Col, probe.wantPath, probe.wantLine, probe.wantCol)
		}
	}
}

// TestTheQueriesRefuseAPackageTheyNeverLoaded covers the not-loaded arm of every
// query that has one, so a caller can always tell "never loaded" from an answer.
func TestTheQueriesRefuseAPackageTheyNeverLoaded(t *testing.T) {
	loaded, err := Load(subjectDir, []string{"./..."})
	if err != nil {
		t.Fatalf("the subject fixture module did not load: %v", err)
	}

	const absent = "example.com/never-loaded"

	// CONTROL: the same queries must SUCCEED for a package that was loaded, or
	// a refusal proves nothing about the package name and only that the queries
	// always refuse.
	if _, ok := loaded.Scope(subjectPath); !ok {
		t.Fatal("CONTROL FAILED: Scope refused a package that was loaded")
	}

	if _, ok := loaded.Module(subjectPath); !ok {
		t.Fatal("CONTROL FAILED: Module refused a package that was loaded")
	}

	if _, ok := loaded.Scope(absent); ok {
		t.Error("Scope answered for a package that was never loaded")
	}

	if _, ok := loaded.Module(absent); ok {
		t.Error("Module answered for a package that was never loaded")
	}

	sources, err := loaded.Sources(absent)
	if err == nil {
		t.Errorf("Sources answered for a package that was never loaded: %v", sources)
	} else if !strings.Contains(err.Error(), absent) {
		t.Errorf("the Sources refusal does not name the package: %v", err)
	}

	if _, err := loaded.Flows(absent); err == nil {
		t.Error("Flows answered for a package that was never loaded")
	}

	// A reference INTO a package that was never loaded is still a diagnostic
	// rather than a panic or a silent empty answer.
	if _, bad := loaded.ResolveFlow(absent, "Anything", ast.Position{}, "consumer.flow"); bad == nil {
		t.Error("ResolveFlow resolved a reference into a package that was never loaded")
	}
}

// TestLoadRefusesADirectoryWithNoPackages covers the arm where the toolchain
// resolves nothing to load, which must be a refusal rather than an empty set.
func TestLoadRefusesADirectoryWithNoPackages(t *testing.T) {
	if loaded, err := Load(decoyDir, []string{"./..."}); err == nil {
		t.Errorf("Load accepted a directory carrying no Go packages: %v", loaded)
	}
}
