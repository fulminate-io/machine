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
	if len(rows) != 26 {
		t.Fatalf("the record carries %d rows, want 26", len(rows))
	}

	recorded := make([]string, 0, len(rows))
	for _, row := range rows {
		recorded = append(recorded, row.keyword)
		if len(row.counts) == 0 {
			t.Errorf("keyword %q records no encoding at all", row.keyword)
		}
		for encoding, n := range row.counts {
			if n != 1 {
				t.Errorf("keyword %q tokenizes to %d tokens under %s; every keyword must be atomic",
					row.keyword, n, encoding)
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
