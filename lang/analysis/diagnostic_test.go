// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
)

// TestSeverityRenders pins the words a command line and an editor display,
// including the fallback for a value outside the vocabulary.
func TestSeverityRenders(t *testing.T) {
	for _, tc := range []struct {
		in   Severity
		want string
	}{
		{in: SeverityError, want: "error"},
		{in: SeverityWarning, want: "warning"},
		{in: SeverityHint, want: "hint"},
		{in: Severity(9), want: "severity(9)"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Severity(%d).String() = %q, want %q", int(tc.in), got, tc.want)
		}
	}
}

// TestParseDiagnosticsConvertsAParseError pins the converter against a real
// broken fixture, so the shape it reads is the shape ast.Parse actually returns
// rather than one the test built.
func TestParseDiagnosticsConvertsAParseError(t *testing.T) {
	path := filepath.Join(astTestdata, "broken", "missing-arrow-target.flow")
	src, readErr := readFixture(t, path)
	if readErr != nil {
		t.Fatalf("cannot read %s: %v", path, readErr)
	}

	_, err := ast.Parse(src)
	if err == nil {
		t.Fatalf("%s parsed clean; it is the corpus's broken fixture", path)
	}

	diags := ParseDiagnostics(path, err)
	if len(diags) == 0 {
		t.Fatal("a parse error yielded no diagnostics")
	}
	for _, d := range diags {
		if d.Severity != SeverityError {
			t.Errorf("a parse diagnostic carries severity %s, want error", d.Severity)
		}
		if d.Code != "parse" {
			t.Errorf("a parse diagnostic carries code %q, want parse", d.Code)
		}
		if d.Path != path {
			t.Errorf("a parse diagnostic carries path %q, want %q", d.Path, path)
		}
		if d.Message == "" {
			t.Error("a parse diagnostic carries no message")
		}
	}
}

// TestParseDiagnosticsIgnoresOtherErrors pins that the converter reports nothing
// for an error it cannot read, rather than inventing a diagnostic at 0:0.
func TestParseDiagnosticsIgnoresOtherErrors(t *testing.T) {
	if got := ParseDiagnostics("nothing.flow", nil); got != nil {
		t.Errorf("a nil error yielded %d diagnostics", len(got))
	}
	if got := ParseDiagnostics("nothing.flow", errors.New("some unrelated failure")); got != nil {
		t.Errorf("an unrelated error yielded %d diagnostics", len(got))
	}
}
