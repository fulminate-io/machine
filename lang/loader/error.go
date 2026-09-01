// Package loader - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package loader

import (
	"strconv"
	"strings"

	"github.com/whitaker-io/machine/lang/ast"
)

// Diagnostic is one positioned problem this module found, in the coordinates of
// the file it actually READ.
//
// It reuses lang/ast's positioned-diagnostic vocabulary rather than declaring a
// parallel one, and adds Path for the reason lang/analysis's own Diagnostic
// does: a run covers many files, ast.Position.Offset is per-file, and two
// problems in two files routinely carry identical positions.
//
// THE COORDINATES ARE THE READ FILE'S, NEVER THE FLOW AUTHOR'S. A type-check
// problem in a generated package is reported at its line in the generated file.
// Mapping that back to the .flow line the author wrote is the emitter's job,
// because only the emitter holds the line map, and this module deliberately
// knows nothing about flow sources at this seam.
//
// End equals Pos for a diagnostic derived from a go/types error: go/types
// reports one point rather than a span, and inventing an end offset would be a
// claim about extent that nothing measured.
type Diagnostic struct {
	Path    string
	Pos     ast.Position
	End     ast.Position
	Message string
}

// splitPos splits a packages.Error position of the form `file:line:col` into the
// file it names and the point inside it.
//
// The split walks in from the RIGHT because a path may itself contain colons,
// and it refuses as a unit: a position that does not end in two parseable
// numbers yields the whole string as the path and a zero position, rather than a
// path silently truncated at whichever colon happened to be last.
func splitPos(pos string) (string, ast.Position) {
	rest, col, ok := trimNumber(pos)
	if !ok {
		return pos, ast.Position{}
	}

	rest, line, ok := trimNumber(rest)
	if !ok {
		return pos, ast.Position{}
	}

	return rest, ast.Position{Line: line, Col: col}
}

// trimNumber splits a trailing `:<number>` off text, reporting whether it found
// one.
func trimNumber(text string) (string, int, bool) {
	at := strings.LastIndex(text, ":")
	if at < 0 {
		return text, 0, false
	}

	value, err := strconv.Atoi(text[at+1:])
	if err != nil {
		return text, 0, false
	}

	return text[:at], value, true
}
