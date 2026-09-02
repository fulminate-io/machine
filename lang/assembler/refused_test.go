// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
)

// refusedDir holds one .flow fixture per live member of the refusal set.
const refusedDir = "testdata/refused"

// refusalReason is one live member of the closed refusal set.
//
// MEMBER NUMBERS ARE THE PLAN'S AND THEY ARE STABLE. There are now TWO deliberate
// gaps, and neither slot is ever reused, so the sibling steps that cite members by
// number keep meaning what they said.
//
// MEMBER 1 was the `checkpoint` refusal, retired once the runtime gained a
// checkpoint mechanism.
//
// MEMBER 3 was the dotted-`use` refusal — "names a flow in another module;
// cross-module flow references are not resolved here" — retired by the ruling
// that has the assembler resolve a cross-module reference through the loader's
// Packages.ResolveFlow and feed the resolved flow's boundary into the same
// by-name binding a local `use` already has. What was a refusal fixture is now a
// valid one: testdata/crossmodule carries the two-module tree the resolution
// runs over.
//
// A retired member declares no reason and owes no fixture, which is why this
// table starts at 2 and skips 3.
type refusalReason struct {
	member   int
	fixture  string
	fragment string
}

// liveRefusals is the closed set. The test below asserts it BOTH ways: every
// member has a fixture that is actually refused for that reason, and every
// fixture in the directory belongs to a member.
var liveRefusals = []refusalReason{
	{2, "loop-label-with-no-sender", "is never sent to"},
	{4, "recursive-use-chain", "uses itself through"},
	{5, "output-never-consumed", "which nothing in this flow consumes"},
	{6, "duplicate-node-name", "node names are unique within a flow"},
	{7, "unknown-name-reference", "which no statement in this flow produces"},
	{8, "unparsable-statement", "could not be parsed"},
	{9, "reserved-helper-name", "collides with the helper every generated file declares"},
}

// assembleFixture parses and builds one refusal fixture, returning the
// assembler's own diagnostics.
//
// IT TOLERATES A PARSE ERROR ON PURPOSE. Member 8's fixture is source the parser
// cannot read, and the parser still returns a positioned tree with a BadStmt in
// it — that recovery is what gives this package something to refuse. What is NOT
// tolerated is a nil file, which would mean there was nothing to build.
func assembleFixture(t *testing.T, path string) []Diagnostic {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	file, _ := ast.Parse(src)
	if file == nil {
		t.Fatalf("%s: Parse returned no file at all, so there is nothing to refuse", path)
	}
	_, diags := buildFile(file)

	return diags
}

// TestRefusedConstructsAreClosedAndPositioned proves the refusal set is closed
// in BOTH directions and that every refusal is positioned.
//
// THE EMPTY-DIRECTORY CONTROL IS THE POINT. A test that walked the fixture
// directory and asserted "everything here is refused" passes perfectly over an
// empty directory, which is the vacuous shape this whole family of tests exists
// to avoid. So the walk refuses to proceed on an empty read, and the member
// table is compared against the directory rather than derived from it.
func TestRefusedConstructsAreClosedAndPositioned(t *testing.T) {
	present, err := filepath.Glob(filepath.Join(refusedDir, "*.flow"))
	if err != nil {
		t.Fatalf("reading %s: %v", refusedDir, err)
	}
	if len(present) == 0 {
		t.Fatalf("CONTROL FAILED: %s holds no .flow fixtures, so a clean sweep would prove nothing", refusedDir)
	}

	// DIRECTION ONE: every declared member has a fixture, and that fixture is
	// refused FOR THAT REASON rather than for some other problem it happens to
	// carry.
	for _, reason := range liveRefusals {
		t.Run(reason.fixture, func(t *testing.T) {
			path := filepath.Join(refusedDir, reason.fixture+".flow")
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("refusal member %d declares no fixture: %v", reason.member, err)
			}
			diags := assembleFixture(t, path)
			if len(diags) == 0 {
				t.Fatalf("member %d's fixture assembled clean", reason.member)
			}
			joined := strings.Join(messagesOf(diags), "\n")
			if !strings.Contains(joined, reason.fragment) {
				t.Errorf("member %d refused for the wrong reason.\ngot:\n%s\nwant a diagnostic containing %q",
					reason.member, joined, reason.fragment)
			}
			for _, d := range diags {
				if d.Pos.Line == 0 {
					t.Errorf("member %d raised an unpositioned diagnostic: %q", reason.member, d.Message)
				}
			}
		})
	}

	// DIRECTION TWO: every fixture in the directory belongs to a declared member.
	// Without this a fixture could be added, refused for an undeclared reason, and
	// the set would no longer be closed while every other assertion stayed green.
	declared := make(map[string]bool, len(liveRefusals))
	for _, reason := range liveRefusals {
		declared[reason.fixture] = true
	}
	for _, path := range present {
		name := strings.TrimSuffix(filepath.Base(path), ".flow")
		if !declared[name] {
			t.Errorf("%s is refused by no declared member; the set is no longer closed", path)
		}
	}
	if len(present) != len(liveRefusals) {
		t.Errorf("%s holds %d fixtures against %d declared members", refusedDir, len(present), len(liveRefusals))
	}
}

// TestRefusalMemberOneIsARetiredGap pins the numbering rule, for BOTH gaps.
//
// Member 1 was the `checkpoint` refusal and member 3 was the dotted-`use` one;
// both are RETIRED — each construct is handled now, not refused. Their slots are
// kept because sibling steps cite members by number, and renumbering would
// silently re-point them. This test is what makes that a checked fact rather than
// a comment: it fails if either slot is ever reclaimed, which is what a
// well-meaning tidy-up would do.
func TestRefusalMemberOneIsARetiredGap(t *testing.T) {
	retired := map[int]string{1: "the checkpoint refusal", 3: "the dotted-use refusal"}
	for _, reason := range liveRefusals {
		if was, gap := retired[reason.member]; gap {
			t.Fatalf("member %d is a retired slot (%s) and must stay empty; %q claims it",
				reason.member, was, reason.fixture)
		}
	}
	if liveRefusals[0].member != 2 {
		t.Errorf("the live set starts at member %d, want 2 with 1 left as the retired gap", liveRefusals[0].member)
	}
}

// TestTheValidCorpusIsNotRefused is the known-positive for the whole refusal
// set, and it is deliberately run against ANOTHER MODULE'S corpus.
//
// Every test above asserts that something IS refused. A builder that refused
// everything would pass all of them. This asserts the opposite direction over
// fixtures nobody wrote for this package: lang/ast's own valid corpus, which is
// the language's canonical set of things that must assemble.
func TestTheValidCorpusIsNotRefused(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "ast", "testdata", "valid", "*.flow"))
	if err != nil {
		t.Fatalf("reading the valid corpus: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("CONTROL FAILED: the valid corpus glob matched nothing, so this proves nothing")
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			file, parseErr := ast.Parse(src)
			if parseErr != nil {
				t.Fatalf("a valid-corpus fixture does not parse: %v", parseErr)
			}
			if _, diags := buildFile(file); len(diags) != 0 {
				t.Errorf("the graph builder refused a VALID fixture:\n%s", strings.Join(messagesOf(diags), "\n"))
			}
		})
	}
}
