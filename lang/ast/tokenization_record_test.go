// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// tokenizationRecordPath is the checked-in measurement of the keyword
// inventory's tokenization.
const tokenizationRecordPath = "testdata/keyword_tokenization.txt"

// tokenizationRow is one measured keyword: its spelling and the token count it
// produced under each encoding.
type tokenizationRow struct {
	keyword string
	counts  map[string]int
}

// atomicityExemption is one keyword admitted despite fragmenting, with the counts
// it was admitted at and the ruling that admitted it.
type atomicityExemption struct {
	counts   map[string]int
	reason   string
	decision string
}

// atomicityExemptions is the CLOSED list of keywords exempt from the one-token
// invariant.
//
// IT IS A CLOSED, NAMED LIST for the reason the contract-docs sweep gives about its
// own survivors: an exception a sweep does not name is an exception nobody can
// review. Each entry carries its measured counts, why it was admitted and the
// ruling id, and the ruling id is required by the test rather than by convention —
// an exemption with no ruling behind it cannot pass.
var atomicityExemptions = map[string]atomicityExemption{
	textIdempotent: {
		counts: map[string]int{"o200k_base": 3, "cl100k_base": 3},
		reason: "the spelling was priced against its semantic precision and kept: it names the " +
			"property the runtime keys the checkpoint anchor on, and no atomic synonym says the same thing",
		decision: "9c4ff0672ab88db0f87e9b6863692831",
	},
}

// readTokenizationRecord parses the checked-in record, skipping comments and
// blank lines.
func readTokenizationRecord(t *testing.T) []tokenizationRow {
	t.Helper()
	raw, err := os.ReadFile(tokenizationRecordPath)
	if err != nil {
		t.Fatalf("reading %s: %v", tokenizationRecordPath, err)
	}
	var rows []tokenizationRow
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rows = append(rows, parseTokenizationRow(t, i+1, line))
	}
	return rows
}

// parseTokenizationRow parses one `<keyword> enc=<n> enc=<n>` line.
func parseTokenizationRow(t *testing.T, lineNo int, line string) tokenizationRow {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) < 2 {
		t.Fatalf("%s:%d: %q is not a `<keyword> <encoding>=<n>...` row", tokenizationRecordPath, lineNo, line)
	}
	row := tokenizationRow{keyword: fields[0], counts: map[string]int{}}
	for _, field := range fields[1:] {
		name, count, ok := strings.Cut(field, "=")
		if !ok {
			t.Fatalf("%s:%d: field %q is not <encoding>=<n>", tokenizationRecordPath, lineNo, field)
		}
		n, err := strconv.Atoi(count)
		if err != nil {
			t.Fatalf("%s:%d: field %q has a non-numeric count: %v", tokenizationRecordPath, lineNo, field, err)
		}
		row.counts[name] = n
	}
	return row
}

// TestEveryKeywordHasATokenizationRecord asserts set equality between the
// keyword table and the checked-in record, and that every recorded count is a
// single token.
//
// The point is not to re-measure — the measurement lives outside this module and
// this record is its durable form. The point is that a keyword added later
// without measuring it fails here, which is exactly the situation `checkpoint`
// and then `func` created.
func TestEveryKeywordHasATokenizationRecord(t *testing.T) {
	rows := readTokenizationRecord(t)
	if len(rows) == 0 {
		t.Fatalf("CONTROL FAILED: %s parsed to zero rows", tokenizationRecordPath)
	}
	if len(rows) != 27 {
		t.Fatalf("the record carries %d rows, want 27", len(rows))
	}

	recorded := make([]string, 0, len(rows))
	for _, row := range rows {
		recorded = append(recorded, row.keyword)
		if len(row.counts) == 0 {
			t.Errorf("keyword %q records no encoding at all", row.keyword)
		}
		exemption, exempt := atomicityExemptions[row.keyword]
		for encoding, n := range row.counts {
			if !exempt {
				if n != 1 {
					t.Errorf("keyword %q tokenizes to %d tokens under %s; every keyword must be atomic",
						row.keyword, n, encoding)
				}

				continue
			}
			// AN EXEMPTION PINS THE MEASUREMENT, IT DOES NOT STOP MEASURING. A count
			// that drifts from the pinned one fails here, so an exemption cannot
			// quietly absorb a fragmentation it was never admitted for.
			want, pinned := exemption.counts[encoding]
			if !pinned {
				t.Errorf("exempted keyword %q records encoding %s, which its exemption does not pin",
					row.keyword, encoding)
			} else if n != want {
				t.Errorf("exempted keyword %q now tokenizes to %d tokens under %s, but its exemption pins %d: "+
					"an exemption pins the measurement, it does not stop measuring",
					row.keyword, n, encoding, want)
			}
		}
	}

	table := make([]string, 0, len(keywords))
	for k := range keywords {
		table = append(table, k)
	}
	slices.Sort(recorded)
	slices.Sort(table)

	if !slices.Equal(recorded, table) {
		for _, k := range table {
			if !slices.Contains(recorded, k) {
				t.Errorf("keyword %q has no tokenization row; measure it before adding it", k)
			}
		}
		for _, r := range recorded {
			if !slices.Contains(table, r) {
				t.Errorf("the record carries a row for %q, which is not a keyword", r)
			}
		}
		t.Fatalf("record (%d rows) diverges from the keyword table (%d entries)", len(recorded), len(table))
	}
}

// TestTokenizationRecordNamesItsInstrument keeps the record self-describing: a
// bare table of counts with no tokenizer, version or encodings named is not a
// measurement anyone can reproduce or supersede.
func TestTokenizationRecordNamesItsInstrument(t *testing.T) {
	raw, err := os.ReadFile(tokenizationRecordPath)
	if err != nil {
		t.Fatalf("reading %s: %v", tokenizationRecordPath, err)
	}
	header := string(raw)
	for _, want := range []string{"tiktoken", "o200k_base", "cl100k_base"} {
		if !strings.Contains(header, want) {
			t.Errorf("%s does not name %q", tokenizationRecordPath, want)
		}
	}
}

// TestKeywordAtomicityExemptionsAreJustifiedAndPinned keeps the exemption list
// honest in BOTH directions.
//
// An exemption is a hole in a check that was otherwise passing, so it earns its
// place only by being reviewable: it must name a keyword that really is in the
// language, pin counts for the encodings the record measures, carry a reason, and
// cite the ruling that admitted it. AND IT MUST STILL BE NEEDED — an exempted
// keyword that turns out to be atomic under every encoding is silencing nothing
// and is removed, which is what the needless-exemption guard below catches.
func TestKeywordAtomicityExemptionsAreJustifiedAndPinned(t *testing.T) {
	// CONTROL: the list is non-empty. Every assertion below is a loop over the
	// exemptions, and an empty map satisfies all of them vacuously.
	if len(atomicityExemptions) == 0 {
		t.Fatal("CONTROL FAILED: the exemption list is empty, so every assertion in this test is vacuous")
	}

	t.Logf("exemptions reviewed: %d", len(atomicityExemptions))

	rows := readTokenizationRecord(t)
	measured := make(map[string]map[string]int, len(rows))
	for _, row := range rows {
		measured[row.keyword] = row.counts
	}

	for keyword, exemption := range atomicityExemptions {
		if _, ok := keywords[keyword]; !ok {
			t.Errorf("exempted keyword %q is not in the keyword table at all", keyword)
		}
		if exemption.reason == "" {
			t.Errorf("exempted keyword %q carries no reason", keyword)
		}
		if exemption.decision == "" {
			t.Errorf("exempted keyword %q cites no ruling; an exemption admitted with nothing behind it "+
				"is an exception nobody can review", keyword)
		}
		if len(exemption.counts) == 0 {
			t.Errorf("exempted keyword %q pins no counts, so its measurement is not pinned at all", keyword)

			continue
		}

		// THE NEEDLESS-EXEMPTION GUARD. If the keyword is atomic everywhere it was
		// measured, the exemption is buying nothing and the check it silences was
		// passing on its own.
		fragments := false
		for _, n := range exemption.counts {
			if n != 1 {
				fragments = true
			}
		}
		if !fragments {
			t.Errorf("exempted keyword %q is atomic under every encoding it pins; the exemption silences "+
				"a check that was passing and must be removed", keyword)
		}

		// The pinned counts must match what the record actually measured, or the
		// exemption is pinning a measurement nobody took.
		for encoding, want := range exemption.counts {
			got, ok := measured[keyword][encoding]
			if !ok {
				t.Errorf("exempted keyword %q pins encoding %s, which the record does not measure", keyword, encoding)

				continue
			}
			if got != want {
				t.Errorf("exempted keyword %q pins %d tokens under %s but the record measures %d",
					keyword, want, encoding, got)
			}
		}
	}
}
