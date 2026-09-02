// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package lsp

import (
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
	"go.lsp.dev/protocol"
)

// mixedWidthSource carries one rune of every width class that separates a byte
// column from a UTF-16 one, so a single fixture exercises both discriminating
// cases in one run.
//
// Line 1 is "a€b": U+20AC is THREE UTF-8 bytes and ONE UTF-16 code unit, so the
// byte column of 'b' is 5 while its character index is 2.
// Line 2 is "x😀y": U+1F600 is FOUR UTF-8 bytes and TWO code units, so the byte
// column of 'y' is 6 while its character index is 3.
//
// U+00E9 is deliberately absent: it is only two bytes, so it separates the two
// counts by a narrower gap than either case here.
const mixedWidthSource = "a€b\nx\U0001F600y"

// byte offsets into mixedWidthSource, spelled out so a test asserting one is
// asserting against a hand-computed constant rather than against the Mapper's
// own arithmetic.
const (
	offsetEuro     = 1  // the euro sign begins here
	offsetB        = 4  // 'b', after the euro's three bytes
	offsetLineOne  = 5  // one past 'b': the newline
	offsetX        = 6  // 'x' opens line two
	offsetEmoji    = 7  // the emoji begins here
	offsetY        = 11 // 'y', after the emoji's four bytes
	offsetLineTwo  = 12 // one past 'y': end of source
	lineOneUnits   = 3  // "a€b" is three UTF-16 code units
	lineTwoUnits   = 4  // "x😀y" is four UTF-16 code units
	lineOneByteLen = 5  // ...but five bytes
	lineTwoByteLen = 6  // ...and six bytes
)

func TestByteColumnsBecomeUTF16CodeUnits(t *testing.T) {
	m := NewMapper([]byte(mixedWidthSource))

	cases := []struct {
		name string
		in   ast.Position
		want protocol.Position
	}{
		{"the first column of the first line", ast.Position{Line: 1, Col: 1}, protocol.Position{Line: 0, Character: 0}},
		{"the byte column of a three-byte rune", ast.Position{Line: 1, Col: 2}, protocol.Position{Line: 0, Character: 1}},
		// THE THREE-BYTE CASE: byte column 5 is character 2, not 4.
		{"the column after a three-byte rune", ast.Position{Line: 1, Col: 5}, protocol.Position{Line: 0, Character: 2}},
		{"the end of the first line", ast.Position{Line: 1, Col: 6}, protocol.Position{Line: 0, Character: lineOneUnits}},
		{"the first column of the second line", ast.Position{Line: 2, Col: 1}, protocol.Position{Line: 1, Character: 0}},
		// THE FOUR-BYTE CASE: byte column 6 is character 3, not 5, because the
		// emoji costs two code units where it cost four bytes.
		{"the column after a four-byte rune", ast.Position{Line: 2, Col: 6}, protocol.Position{Line: 1, Character: 3}},
		{"the end of the second line", ast.Position{Line: 2, Col: 7}, protocol.Position{Line: 1, Character: lineTwoUnits}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := m.ToLSP(c.in); got != c.want {
				t.Fatalf("ToLSP(%+v) = %+v, want %+v", c.in, got, c.want)
			}
		})
	}

	// The mutant this test exists to reject is "return the byte column
	// unchanged". Assert it explicitly, so a reader can see the two columns
	// where the byte answer and the code-unit answer differ at all.
	if lineOneByteLen == lineOneUnits || lineTwoByteLen == lineTwoUnits {
		t.Fatal("the fixture no longer separates byte length from code-unit length; " +
			"a mapper returning the byte column would pass this test")
	}
}

func TestUTF16CodeUnitsBecomeByteOffsets(t *testing.T) {
	m := NewMapper([]byte(mixedWidthSource))

	cases := []struct {
		name string
		in   protocol.Position
		want int
	}{
		{"the first character of the first line", protocol.Position{Line: 0, Character: 0}, 0},
		{"the character index of a three-byte rune", protocol.Position{Line: 0, Character: 1}, offsetEuro},
		// THE THREE-BYTE CASE: character 2 is byte 4, not byte 2 — byte 2 sits
		// inside the euro sign's encoding.
		{"the character after a three-byte rune", protocol.Position{Line: 0, Character: 2}, offsetB},
		{"the end of the first line", protocol.Position{Line: 0, Character: lineOneUnits}, offsetLineOne},
		{"the first character of the second line", protocol.Position{Line: 1, Character: 0}, offsetX},
		{"the character index of a four-byte rune", protocol.Position{Line: 1, Character: 1}, offsetEmoji},
		// THE FOUR-BYTE CASE: character 3 is byte 11, not byte 9.
		{"the character after a four-byte rune", protocol.Position{Line: 1, Character: 3}, offsetY},
		{"the end of the second line", protocol.Position{Line: 1, Character: lineTwoUnits}, offsetLineTwo},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := m.ToOffset(c.in)
			if !ok {
				t.Fatalf("ToOffset(%+v) refused a position that is inside the source", c.in)
			}
			if got != c.want {
				t.Fatalf("ToOffset(%+v) = %d, want %d", c.in, got, c.want)
			}
		})
	}

	// A character index landing INSIDE a surrogate pair names no byte, and is
	// refused rather than rounded to one of the two runes it sits between.
	if got, ok := m.ToOffset(protocol.Position{Line: 1, Character: 2}); ok {
		t.Fatalf("ToOffset inside a surrogate pair returned byte %d; it names no byte and must be refused", got)
	}

	// ROUND TRIP, which is what proves the two directions agree rather than
	// each being separately self-consistent.
	for _, pos := range []ast.Position{
		{Line: 1, Col: 1}, {Line: 1, Col: 2}, {Line: 1, Col: 5},
		{Line: 2, Col: 1}, {Line: 2, Col: 6},
	} {
		off, ok := m.ToOffset(m.ToLSP(pos))
		if !ok {
			t.Fatalf("the round trip refused %+v", pos)
		}
		if want := lineStart(pos.Line) + pos.Col - 1; off != want {
			t.Fatalf("the round trip of %+v reached byte %d, want %d", pos, off, want)
		}
	}
}

func TestAPositionPastTheLineEndIsRefusedRatherThanClamped(t *testing.T) {
	m := NewMapper([]byte(mixedWidthSource))

	// KNOWN POSITIVE, in the same run and through the same call: the last
	// legal character index on each line resolves. Without it a Mapper that
	// refused EVERY position would pass the refusals below.
	for _, legal := range []protocol.Position{
		{Line: 0, Character: lineOneUnits},
		{Line: 1, Character: lineTwoUnits},
	} {
		if _, ok := m.ToOffset(legal); !ok {
			t.Fatalf("the control failed: %+v is the last legal position on its line and was refused", legal)
		}
	}

	refusals := []struct {
		name string
		in   protocol.Position
	}{
		{"one character past the first line's last code unit", protocol.Position{Line: 0, Character: lineOneUnits + 1}},
		{"one character past the second line's last code unit", protocol.Position{Line: 1, Character: lineTwoUnits + 1}},
		{"a character index far past the line", protocol.Position{Line: 0, Character: 4096}},
		{"a line the source does not have", protocol.Position{Line: 9, Character: 0}},
	}

	for _, c := range refusals {
		t.Run(c.name, func(t *testing.T) {
			got, ok := m.ToOffset(c.in)
			// The mutant is CLAMPING: it would return the line's last byte
			// with ok true, and every in-range assertion elsewhere would still
			// pass. Both halves are asserted so a clamp cannot hide in the
			// offset while ok looks right.
			if ok {
				t.Fatalf("ToOffset(%+v) returned byte %d rather than refusing; a stale position was clamped", c.in, got)
			}
			if got != 0 {
				t.Fatalf("ToOffset(%+v) refused but still reported byte %d", c.in, got)
			}
		})
	}
}

// lineStart is the byte offset of a 1-based line in mixedWidthSource, written
// out rather than read back from the Mapper so the round trip above is checked
// against an independent expectation.
func lineStart(line int) int {
	if line == 1 {
		return 0
	}
	return offsetX
}
