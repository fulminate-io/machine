// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import (
	"slices"
	"testing"
)

// TestKeywordInventoryIsExactlyTheRuledSet states the ruled inventory a second
// time, independently of the keywords table, and asserts set equality in both
// directions. A keyword added to the table without being ruled fails here, and
// so does a ruled keyword dropped from the table.
//
// The literal list's own length is asserted too, so deleting a spelling from
// BOTH sides cannot pass silently.
func TestKeywordInventoryIsExactlyTheRuledSet(t *testing.T) {
	ruledInventory := []string{
		"flow", "note", "import", "state", "var", "const", "param",
		"on", "source", "transform", "branch", "switch", "tee", "sink",
		"drop", "loop", "send", "use", "from", "over", "reads", "writes",
		"clone", "else", "checkpoint", "func",
	}

	if len(ruledInventory) != 26 {
		t.Fatalf("the ruled inventory is 26 spellings; this test states %d", len(ruledInventory))
	}

	table := make([]string, 0, len(keywords))
	for k := range keywords {
		table = append(table, k)
	}

	slices.Sort(ruledInventory)
	slices.Sort(table)

	if !slices.Equal(ruledInventory, table) {
		for _, want := range ruledInventory {
			if !slices.Contains(table, want) {
				t.Errorf("ruled keyword %q is missing from the keywords table", want)
			}
		}
		for _, got := range table {
			if !slices.Contains(ruledInventory, got) {
				t.Errorf("keywords table carries %q, which is not a ruled keyword", got)
			}
		}
		t.Fatalf("keyword table (%d) diverges from the ruled inventory (%d)", len(table), len(ruledInventory))
	}
}

// TestKeywordKindsAreDistinct guards the const block itself: two spellings
// mapped to the same kind would make the parser silently accept one for the
// other, and set equality on the spellings alone cannot see it.
func TestKeywordKindsAreDistinct(t *testing.T) {
	seen := map[tokenKind]string{}
	for spelling, kind := range keywords {
		if prior, ok := seen[kind]; ok {
			t.Errorf("keywords %q and %q share token kind %d", prior, spelling, kind)
		}
		seen[kind] = spelling
	}
	if len(seen) != len(keywords) {
		t.Fatalf("got %d distinct kinds for %d keywords", len(seen), len(keywords))
	}
}
