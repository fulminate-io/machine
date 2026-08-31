// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import "strconv"

// Position is a byte-addressed point in a source file.
//
// Offset is the 0-based byte index. Line and Col are 1-based, and Col counts
// BYTES rather than runes so that a mapping onto an editor's own byte offsets
// stays exact without a re-scan.
type Position struct {
	Offset int
	Line   int
	Col    int
}

// String renders the position as `line:col`, the form an editor and a command
// line both read without further formatting.
func (p Position) String() string {
	return strconv.Itoa(p.Line) + ":" + strconv.Itoa(p.Col)
}

// Diagnostic is one positioned problem found in a source file.
//
// It is a value the caller renders, not an error: a parse that produced
// diagnostics still produced a tree, and both are handed back together.
type Diagnostic struct {
	Pos     Position
	End     Position
	Message string
}
