// Package main - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// wantFlags is the WHOLE accepted flag surface, as a POSITIVE QUANTITY.
//
// It is deliberately not a denylist of forbidden names. The next suppression
// lever will not be called "exclude", and a list of names not to accept blesses
// every spelling nobody thought of.
var wantFlags = []string{"fail-on", "format", "rules"}

// astTestdata is the lang/ast corpus, three directories up from this command.
var astTestdata = filepath.Join("..", "..", "..", "ast", "testdata")

// invoke runs the command in process and returns its status and both streams.
func invoke(args ...string) (int, string, string) {
	var out, errOut strings.Builder
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestExitStatusSeparatesCleanFoundAndCannotRun(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		want  int
		names string
	}{
		{name: "the canonical corpus is clean", args: []string{filepath.Join(astTestdata, "strawman")}, want: 0},
		{name: "the reject corpus is found", args: []string{filepath.Join(astTestdata, "analysis-rejects")}, want: 1},
		{name: "no paths cannot run", args: nil, want: 2, names: "no paths named"},
		{name: "an unknown fail-on level cannot run", args: []string{"-fail-on=nope", astTestdata}, want: 2,
			names: "nope"},
		{name: "an unknown format cannot run", args: []string{"-format=sarif", astTestdata}, want: 2,
			names: "sarif"},
		{name: "an unknown flag cannot run", args: []string{"-exclude=state", astTestdata}, want: 2,
			names: "exclude"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := invoke(tc.args...)
			if code != tc.want {
				t.Errorf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.want, out, errOut)
			}
			if tc.names != "" && !strings.Contains(errOut, tc.names) {
				t.Errorf("the refusal does not name %q; stderr reads:\n%s", tc.names, errOut)
			}
		})
	}
}

func TestAnEmptyPathSetIsRefusedRatherThanReportedClean(t *testing.T) {
	empty := t.TempDir()

	code, out, errOut := invoke(empty)
	if code != 2 {
		t.Fatalf("an empty directory exited %d, want 2 — a run that could not be performed is not a clean "+
			"run\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, empty) {
		t.Errorf("the refusal does not name the directory it could not fill; stderr reads:\n%s", errOut)
	}
	if !strings.Contains(errOut, "no .flow sources under") {
		t.Errorf("the refusal does not say what was missing; stderr reads:\n%s", errOut)
	}
}

func TestRulesPrintsEveryRuleAndExitsClean(t *testing.T) {
	code, out, errOut := invoke("-rules")
	if code != 0 {
		t.Fatalf("-rules exited %d, want 0\nstderr:\n%s", code, errOut)
	}
	// A verbatim substring of typeflow's own Doc, so a paraphrased listing fails.
	if !strings.Contains(out, "IS NOT TYPE CHECKING") {
		t.Errorf("the listing does not carry typeflow's own stated limit verbatim; it reads:\n%s", out)
	}
}

func TestJSONFormatIsSelectableAndStillFails(t *testing.T) {
	code, out, errOut := invoke("-format=json", filepath.Join(astTestdata, "analysis-rejects"))
	if code != 1 {
		t.Fatalf("the reject corpus in json exited %d, want 1 — a format cannot change a verdict\nstderr:\n%s",
			code, errOut)
	}
	for _, want := range []string{`"threshold"`, `"failing"`, `"diagnostics"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the document does not carry %s; stdout reads:\n%s", want, out)
		}
	}
}

func TestFlagSurfaceIsExactlyTheThreeAccepted(t *testing.T) {
	flags, _ := newFlags(io.Discard)

	var registered []string
	flags.VisitAll(func(f *flag.Flag) {
		registered = append(registered, f.Name)
	})
	if len(registered) == 0 {
		t.Fatal("CONTROL FAILED: the flag set registers nothing at all, so a set-equality assertion would be vacuous")
	}
	sort.Strings(registered)

	if strings.Join(registered, " ") != strings.Join(wantFlags, " ") {
		t.Errorf("the flag surface is %v, want exactly %v; a fourth flag is a way to make a finding disappear "+
			"without fixing it, and the no-suppression ruling forbids one", registered, wantFlags)
	}
}
