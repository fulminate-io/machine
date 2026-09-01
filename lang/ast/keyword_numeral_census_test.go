// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// numeralWords is the spelled-out vocabulary this census can read. It covers the
// range the two reserved-spelling tables can plausibly reach; a table that grew past
// it would spell a numeral this map does not carry, and the no-numeral control below
// fails rather than passing in silence.
var numeralWords = map[string]int{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7,
	"eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12,
}

var numeralRe = regexp.MustCompile(
	`(?i)\b(one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve)\b`)

// docNumerals returns the spelled-out numbers in the doc comment block immediately
// above a declaration.
func docNumerals(t *testing.T, file, decl string) []int {
	t.Helper()

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	lines := strings.Split(string(raw), "\n")
	at := -1
	for i, line := range lines {
		if strings.HasPrefix(line, decl) {
			at = i

			break
		}
	}
	if at < 0 {
		t.Fatalf("%s declares no %q; this census names a declaration that does not exist", file, decl)
	}

	var out []int
	for i := at - 1; i >= 0 && strings.HasPrefix(lines[i], "//"); i-- {
		for _, m := range numeralRe.FindAllStringSubmatch(lines[i], -1) {
			out = append(out, numeralWords[strings.ToLower(m[1])])
		}
	}

	return out
}

// TestSpelledOutKeywordCountsAgreeWithTheirTables gates the ANCHOR SITES of the
// keyword numeral sweep: each reserved-spelling table's own doc comment must spell
// out the number of entries the table actually holds.
//
// NO OTHER CENSUS IN THIS PACKAGE READS PROSE. Adding a reserved spelling moves a
// table's length; the English numeral beside it does not move on its own, and every
// landed census compares code against code. That gap shipped once: the idempotent
// keyword moved BOTH tables and only three of eight prose counts, leaving token.go
// and lexer_span.go stating different numbers for one closed set.
//
// IT GATES THE ANCHORS, NOT EVERY SITE, and that boundary is deliberate. The derived
// statements in token.go, parser_decl.go, parser_clauses.go and grammar.ebnf stay
// covered by the sweep checklist recorded on the keyword step, because pinning the
// package's whole numeral-bearing comment population measures 126 lines dominated by
// ordinary English ("one mistake reported five times", "the three region scans") and
// would red on unrelated prose edits.
func TestSpelledOutKeywordCountsAgreeWithTheirTables(t *testing.T) {
	for _, tc := range []struct {
		file, decl string
		size       int
	}{
		{"lexer_span.go", "var spanStopKeywords", len(spanStopKeywords)},
		{"parser_helpers.go", "var clauseStarters", len(clauseStarters)},
	} {
		got := docNumerals(t, tc.file, tc.decl)

		// CONTROL: the doc spells out SOME number. Without it a doc comment carrying
		// no numeral at all would satisfy the agreement check vacuously.
		if len(got) == 0 {
			t.Fatalf("CONTROL FAILED: the doc above %s in %s spells out no number at all, so this census "+
				"cannot discriminate", tc.decl, tc.file)
		}

		found := false
		for _, n := range got {
			if n == tc.size {
				found = true
			}
		}
		if !found {
			t.Errorf("%s in %s holds %d entries, but its own doc comment spells out %v — a reserved spelling "+
				"was added or removed without moving the English numeral beside it", tc.decl, tc.file, tc.size, got)
		}
		t.Logf("%s: %d entries, doc spells out %v", tc.decl, tc.size, got)
	}
}
