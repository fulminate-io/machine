// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"fmt"

	"github.com/whitaker-io/machine/lang/ast"
)

// Diagnostic is one positioned problem found while assembling.
//
// IT CARRIES THREE FIELDS AND NO MORE, matching ast.Diagnostic exactly: no
// severity and no rule code. Everything this package reports is a refusal, so a
// severity would have one value at every site and a code would be a second
// naming scheme for the message already there.
//
// The positions are ast.Position values taken from the offending statement, so a
// caller renders a graph refusal and a parse refusal through the same path. Col
// counts BYTES rather than runes, which is what lets an editor map an offset
// without re-scanning the line.
//
// PATH IS EMPTY FOR A REFUSAL THIS PACKAGE RAISED, and non-empty for one that
// crossed in from somewhere else. The earlier reading — that this type needs no
// Path because "this package is handed one flow's statements at a time and the
// driver knows the file" — was true of this package's own refusals and is not
// true of an analysis finding, which crosses a whole run of files. Two files'
// parsed trees both start at offset zero, so a position alone cannot name one.
type Diagnostic struct {
	Pos     ast.Position
	End     ast.Position
	Message string
	// Path is the file the diagnostic is about, EMPTY when this package raised
	// it and the caller's own file name is the right answer.
	Path string
}

// Error carries every problem found in one assembly run, together with whatever
// files were emitted in spite of them.
//
// IT IS THE ONLY EXPORTED ERROR TYPE IN THIS PACKAGE, and that is the same
// deliberate rule lang/ast follows. A graph problem, a lowering refusal and an
// emission failure all arrive here as Diagnostics on one value, so a caller
// never type-switches to find out which stage failed — an unresolvable from-name
// and a statement shape with no lowering reach an editor through exactly the
// same channel.
//
// PARTIAL IS NOT AN APOLOGY. A run over several flows that refuses one of them
// still generated the others, and handing them back lets a driver report every
// problem in the run rather than stopping at the first. It is the caller's
// decision whether a partial result is usable; this package's own driver
// declines to write any of it, so a refusal never leaves a half-regenerated
// output directory.
type Error struct {
	Diagnostics []Diagnostic
	Partial     []Generated
}

// Error renders the first diagnostic's position and message, plus how many
// problems were found in total.
func (e *Error) Error() string {
	if len(e.Diagnostics) == 0 {
		return "no diagnostics"
	}
	first := e.Diagnostics[0]
	if rest := len(e.Diagnostics) - 1; rest > 0 {
		return fmt.Sprintf("%s: %s (and %d more)", first.Pos, first.Message, rest)
	}
	return fmt.Sprintf("%s: %s", first.Pos, first.Message)
}

// diagnosticAt builds a positioned diagnostic spanning a statement.
//
// Every refusal in this package goes through here rather than constructing a
// Diagnostic inline, so no site can forget the End position and leave an editor
// with a zero-width span at the top of the file.
func diagnosticAt(start, end ast.Position, format string, args ...any) Diagnostic {
	return Diagnostic{Pos: start, End: end, Message: fmt.Sprintf(format, args...)}
}
