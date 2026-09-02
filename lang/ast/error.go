// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ast

import "fmt"

// Error carries every problem found in one source file, together with the
// partial tree the parser built in spite of them.
//
// It is the ONLY exported error type in this package, and that is deliberate. A
// lexical problem and a syntactic problem both arrive here as Diagnostics on the
// same value, so a caller never type-switches between a scanning failure and a
// parsing one — an unterminated note and a missing arrow reach an editor through
// exactly the same channel.
type Error struct {
	Diagnostics []Diagnostic
	File        *File
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
