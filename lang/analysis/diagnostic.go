// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"errors"
	"strconv"

	"github.com/whitaker-io/machine/lang/ast"
)

// parseCode is the Code carried by a diagnostic converted out of a parse error.
//
// It is not an analyzer name: no analyzer produced it, the parser did.
const parseCode = "parse"

// Severity is how loudly a Diagnostic asks to be heard.
//
// The vocabulary is the framework's own addition. ast.Diagnostic carries a
// position and a message and deliberately nothing else, because the parser has
// no opinion about how bad a thing is; a linter needs one and an editor needs
// one, so the pass framework supplies it.
type Severity int

const (
	// SeverityError marks a program that is wrong: an undefined reference, a
	// node nothing can reach, a state entry spelled in a retired form.
	SeverityError Severity = iota
	// SeverityWarning marks a program that is suspicious but not provably wrong.
	SeverityWarning
	// SeverityHint marks an observation an author may reasonably ignore, such as
	// a producer whose output nothing consumes.
	SeverityHint
)

// String renders the severity as the lowercase word a command line and an editor
// both display without further formatting.
func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	case SeverityHint:
		return "hint"
	default:
		return "severity(" + strconv.Itoa(int(s)) + ")"
	}
}

// Diagnostic is one positioned finding, with the severity and the rule identity
// ast.Diagnostic does not carry.
//
// Code is the emitting analyzer's Name, so a consumer can suppress or route one
// rule without matching on message text.
type Diagnostic struct {
	Pos      ast.Position
	End      ast.Position
	Message  string
	Severity Severity
	Code     string
}

// Source is one file under analysis: its path, its bytes and its parsed tree.
//
// The bytes are retained alongside the tree because two consumers need them. The
// Mermaid renderer reads them, and an LSP mapping byte columns onto UTF-16 code
// units will need them too — ast.Position.Col counts BYTES rather than runes,
// which position.go states explicitly and which no re-scan of the tree recovers.
type Source struct {
	Path string
	Src  []byte
	File *ast.File
}

// ParseDiagnostics converts the diagnostics carried by an ast.Parse error into
// framework diagnostics at SeverityError under the "parse" code.
//
// A parse that produced diagnostics still produced a tree, so a caller feeds the
// tree to the analyzers AND renders these alongside whatever the analyzers find.
// A nil error, or an error that is not an *ast.Error, yields no diagnostics.
func ParseDiagnostics(err error) []Diagnostic {
	var perr *ast.Error
	if !errors.As(err, &perr) {
		return nil
	}
	out := make([]Diagnostic, 0, len(perr.Diagnostics))
	for _, d := range perr.Diagnostics {
		out = append(out, Diagnostic{
			Pos:      d.Pos,
			End:      d.End,
			Message:  d.Message,
			Severity: SeverityError,
			Code:     parseCode,
		})
	}
	return out
}
