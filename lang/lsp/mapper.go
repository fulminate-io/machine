// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lsp

import (
	"math"
	"unicode/utf8"

	"github.com/whitaker-io/machine/lang/ast"
	"go.lsp.dev/protocol"
)

// surrogateFloor is the lowest code point that does not fit one UTF-16 code
// unit. A rune above it is encoded as a surrogate PAIR and therefore counts
// two, which is the entire difference between a byte column and a protocol one
// for text outside the basic multilingual plane.
const surrogateFloor = 0xFFFF

// Mapper converts between the parser's byte-addressed positions and the
// protocol's UTF-16 ones, over one document version's bytes.
//
// THE CONVERSION LIVES HERE RATHER THAN IN lang/ast, and that placement is the
// answer to a seam question rather than an accident. ast.Position.Col counts
// BYTES on purpose — position.go says so, and a byte column is what maps onto
// an editor's own byte offsets without a re-scan. UTF-16 is a PROTOCOL
// encoding, so the module that speaks the protocol owns the conversion, and
// lang/analysis already retains Source.Src for exactly this consumer.
//
// The line index is built ONCE per document version, alongside the reparse,
// because both are per-change costs. A conversion is then a bounded walk of one
// line rather than a scan from the top of the file.
type Mapper struct {
	src    []byte
	starts []int
}

// NewMapper indexes src's line starts so later conversions cost one lookup.
//
// The index is built the way lang/ast's own position-fidelity helper builds it
// — seed with offset zero, append the byte after every '\n' — so the two
// modules agree on where a line begins rather than drifting.
func NewMapper(src []byte) *Mapper {
	starts := []int{0}
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return &Mapper{src: src, starts: starts}
}

// ToLSP converts a parser position to a protocol one: 1-based line and byte
// column become 0-based line and UTF-16 code-unit character.
//
// A position naming a line the source does not have, or a column before the
// first, yields the zero Position rather than an arithmetic result about a
// place that does not exist.
func (m *Mapper) ToLSP(pos ast.Position) protocol.Position {
	start, end, ok := m.lineSpan(pos.Line - 1)
	if !ok || pos.Col < 1 {
		return protocol.Position{}
	}
	stop := start + pos.Col - 1
	if stop > end {
		stop = end
	}
	return newPosition(pos.Line-1, utf16Len(m.src[start:stop]))
}

// ToOffset converts a protocol position back to a byte offset into the source,
// reporting whether the position names a place that exists.
//
// IT REFUSES RATHER THAN CLAMPS. A character index past the line's last code
// unit is a request from a client whose document version has moved on, and
// answering it against the nearest legal byte would produce a confident
// completion list for a place the user's cursor is not. A caller returns an
// empty result on false.
func (m *Mapper) ToOffset(pos protocol.Position) (int, bool) {
	line, lok := toInt(pos.Line)
	character, cok := toInt(pos.Character)
	if !lok || !cok {
		return 0, false
	}
	start, end, ok := m.lineSpan(line)
	if !ok {
		return 0, false
	}
	return advance(m.src[start:end], character, start)
}

// lineSpan returns the byte range of a 0-based line, excluding its terminator,
// and whether the source has that line.
func (m *Mapper) lineSpan(line int) (start, end int, ok bool) {
	if line < 0 || line >= len(m.starts) {
		return 0, 0, false
	}
	start = m.starts[line]
	end = len(m.src)
	if line+1 < len(m.starts) {
		end = m.starts[line+1] - 1
	}
	return start, end, true
}

// advance walks line's runes until it has passed character code units,
// returning the byte offset it reached measured from base.
//
// It reports false when the walk runs out of line first, which is the refusal
// ToOffset's contract promises. A character index landing INSIDE a surrogate
// pair also reports false: there is no byte at that place.
func advance(line []byte, character, base int) (int, bool) {
	units, at := 0, 0
	for at < len(line) {
		if units == character {
			return base + at, true
		}
		r, size := utf8.DecodeRune(line[at:])
		units += unitsFor(r)
		at += size
	}
	if units == character {
		return base + at, true
	}
	return 0, false
}

// utf16Len is the number of UTF-16 code units the bytes encode.
func utf16Len(b []byte) int {
	units := 0
	for _, r := range string(b) {
		units += unitsFor(r)
	}
	return units
}

// unitsFor is how many UTF-16 code units one rune occupies.
//
// BYTE LENGTH IS A SEPARATE AXIS and the two must not be conflated: U+00E9 is
// two UTF-8 bytes and one code unit, U+20AC and U+4E16 are three bytes and one
// code unit, and only a rune outside the basic multilingual plane such as
// U+1F600 — four bytes — occupies two.
func unitsFor(r rune) int {
	if r > surrogateFloor {
		return 2
	}
	return 1
}

// newPosition builds a protocol.Position, refusing a coordinate that does not
// fit the protocol's uint32 fields rather than converting it anyway.
func newPosition(line, character int) protocol.Position {
	l, lok := toUint32(line)
	c, cok := toUint32(character)
	if !lok || !cok {
		return protocol.Position{}
	}
	return protocol.Position{Line: l, Character: c}
}

// toUint32 bounds v on BOTH sides before converting.
//
// A one-sided guard rejecting only negatives does not clear gosec's G115,
// which the repo's shared lint config does not exclude, and more importantly it
// does not actually bound the conversion: an int above math.MaxUint32 wraps
// silently into a small, plausible-looking column.
func toUint32(v int) (uint32, bool) {
	if v < 0 || uint64(v) > math.MaxUint32 {
		return 0, false
	}
	return uint32(v), true
}

// toInt bounds a protocol coordinate before converting it to the signed int
// this mapper's arithmetic uses. uint32 exceeds int on a 32-bit build, so the
// bound is real rather than ceremonial.
func toInt(v uint32) (int, bool) {
	if uint64(v) > uint64(math.MaxInt) {
		return 0, false
	}
	return int(v), true
}
