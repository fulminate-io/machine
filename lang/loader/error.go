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
// the file named by its own Path.
//
// It reuses lang/ast's positioned-diagnostic vocabulary rather than declaring a
// parallel one, and adds Path for the reason lang/analysis's own Diagnostic
// does: a run covers many files, ast.Position.Offset is per-file, and two
// problems in two files routinely carry identical positions.
//
// THERE ARE TWO PRODUCERS AND THEY REPORT IN DIFFERENT COORDINATE FRAMES. Read
// this before mapping any position, because applying the wrong frame's line map
// silently relocates a diagnostic:
//
//   - Errors() reports go/types problems in the coordinates of the file go/types
//     actually READ, which for a generation run is the GENERATED Go file. Mapping
//     one of those back to the .flow line the author wrote is the EMITTER's job,
//     because only the emitter holds the line map. This module does not attempt it.
//
//   - ResolveFlow() reports reference refusals in the coordinates of the .flow
//     source that wrote the reference — already the flow AUTHOR's frame, taken
//     from the caller's own `at` and `from` arguments. A line map must NOT be
//     applied to these; they are already where the author is looking.
//
// PATH IS THE FRAME. A consumer decides how to treat a position by looking at
// the file Path names, never by branching on a producer flag or a diagnostic
// kind — there is deliberately no such discriminator to get wrong, and adding
// one would create a second authority that can disagree with Path.
//
// End equals Pos for a diagnostic derived from a go/types error: go/types
// reports one point rather than a span, and inventing an end offset would be a
// claim about extent that nothing measured.
//
// DO NOT CONFUSE Diagnostic.Path WITH Finding.Path. They share a name and mean
// different things: this one is a FILESYSTEM PATH naming a source file, while
// Finding.Path is a FIELD CHAIN inside a type, such as `.Inner.C`. A consumer
// rendering a serializability finding against a source location composes the
// two — the field chain says what is wrong inside the type, and a Diagnostic
// says where the type was named.
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
