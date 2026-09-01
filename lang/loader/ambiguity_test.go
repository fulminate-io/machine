// Package loader - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package loader

import (
	"strings"
	"testing"
)

const (
	ambiguousDir  = "testdata/ambiguous"
	ambiguousPath = "example.com/ambiguous"
)

// TestResolveRefusesAnAmbiguousSpellingAcrossFileScopes pins what resolution
// does when a package's files disagree about what a spelling means.
//
// A package's imports are per-FILE, so one alias can name two packages in two
// files of the same package. Resolving such a spelling by answering with the
// first file that resolves it produces a stable answer, and a stable answer to
// an undecidable question is still a guess: the caller is handed one candidate
// and never learns the other existed. Under truthful inability the answer is a
// refusal that names both.
//
// The control leg is what stops this from being a rule that fires on any
// multi-file resolution: two files resolving a spelling to the SAME type is not
// a disagreement and must still answer.
func TestResolveRefusesAnAmbiguousSpellingAcrossFileScopes(t *testing.T) {
	disputed, err := Load(ambiguousDir, []string{"./..."})
	if err != nil {
		t.Fatalf("the ambiguous fixture module did not load: %v", err)
	}

	if problems := disputed.Errors(); len(problems) != 0 {
		t.Fatalf("the ambiguous fixture must itself be valid Go, but it reports %v", problems)
	}

	t.Run("two files resolving one spelling to two types is refused, naming both", func(t *testing.T) {
		resolved, err := disputed.Resolve(ambiguousPath, "codec.Encoder")
		if err == nil {
			t.Fatalf("Resolve picked a winner for a spelling two files disagree about: %v", resolved)
		}

		if resolved != nil {
			t.Fatalf("Resolve refused but still handed back a type: %v", resolved)
		}

		message := err.Error()
		for _, want := range []string{"encoding/gob.Encoder", "encoding/json.Encoder", "a_first.go", "b_second.go"} {
			if !strings.Contains(message, want) {
				t.Fatalf("the refusal does not name %q, so the caller cannot see what it is choosing between: %v", want, err)
			}
		}

		t.Logf("ambiguity refusal: %v", err)
	})

	t.Run("two files resolving one spelling to the SAME type still answers", func(t *testing.T) {
		agreed, err := disputed.Resolve(ambiguousPath, "buf.Buffer")
		if err != nil {
			t.Fatalf("both files resolve buf.Buffer to bytes.Buffer, which is agreement, not ambiguity: %v", err)
		}

		if got := agreed.String(); got != "bytes.Buffer" {
			t.Fatalf("buf.Buffer resolved to %q rather than bytes.Buffer", got)
		}
	})

	t.Run("a spelling only the LAST file can resolve is still found", func(t *testing.T) {
		loaded, err := Load(subjectDir, []string{"./..."})
		if err != nil {
			t.Fatalf("the subject fixture module did not load: %v", err)
		}

		// views.go sorts after subject.go and is the only file binding this
		// alias, so a walk that stops at the first file cannot answer this.
		resolved, err := loaded.Resolve(subjectPath, "codec.Marshaler")
		if err != nil {
			t.Fatalf("a spelling qualified by the last file's import did not resolve: %v", err)
		}

		if got := resolved.String(); got != "encoding/json.Marshaler" {
			t.Fatalf("codec.Marshaler resolved to %q rather than encoding/json.Marshaler", got)
		}
	})
}
