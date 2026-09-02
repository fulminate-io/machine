// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	goast "go/ast"
	"go/parser"
	"strings"

	"github.com/whitaker-io/machine/lang/ast"
)

// armPredicate synthesizes the machine.Filter closure one switch arm lowers to.
//
// A SWITCH HAS NO RUNTIME METHOD, so each arm becomes an If, and an If takes a
// predicate. The .flow source writes a SUBJECT and a list of arm VALUES; this
// turns the pair into a Go closure over the datum.
//
// THE DATUM IS BOUND TO THE INPUT NAME the source wrote. A subject like
// `ingest.Kind`, and a predicate arm like `isRefund(ingest)`, both name the input
// node; binding that name to the frame's value is what makes either spelling
// mean what the author meant.
//
// HOW AN ARM VALUE IS CLASSIFIED, and this is the one judgement here. lang/ast
// deliberately does NOT classify an arm's values into literals versus Go
// predicates, and says classification belongs to the analysis engine. Analysis
// does not supply that classification to this generator, so the rule used here is
// SYNTACTIC and stated plainly rather than hidden: a value that parses as a Go
// basic literal is COMPARED against the subject, and anything else is EVALUATED
// as a boolean expression. That reading is what makes payments.flow's
// `isRefund(ingest)` arm mean what it plainly means beside a `"card"` arm.
//
// ITS LIMIT IS REAL AND LOUD. A non-literal arm value that is not a boolean
// expression produces Go that does not compile, reported against the generated
// line and mapped back to the .flow line — a build failure, never a wrong
// routing. A semantic classification belongs to analysis, and when analysis
// exports one this function should consume it rather than re-deriving it.
func armPredicate(l *lowering, n Node, s ast.SwitchStmt, arm ast.SwitchArm) string {
	spelling, ok := l.program.InputTypes[n.Name]
	if !ok || strings.TrimSpace(spelling) == "" {
		l.diagf(n.Start, n.Stop,
			"the switch %q needs its input type to synthesize an arm predicate, "+
				"which no type information supplies", n.Name)

		return ""
	}
	datum := datumName(n)
	conditions := armConditions(s.Subject.Text, arm)
	if len(conditions) == 0 {
		l.diagf(arm.Start, arm.Stop, "the switch arm names no values to route on")

		return ""
	}

	var b strings.Builder
	_, _ = b.WriteString("func(f machine.Frame[" + spelling + "]) bool {\n")
	_, _ = b.WriteString("\t\t" + datum + " := f.Value()\n")
	_, _ = b.WriteString("\t\t_ = " + datum + "\n")
	_, _ = b.WriteString("\t\treturn " + strings.Join(conditions, " || ") + "\n")
	_, _ = b.WriteString("\t}")

	return b.String()
}

// datumName is the Go variable the arm predicate binds the frame's value to.
//
// It is the switch's DECLARING INPUT name, because that is the name the subject
// and the predicate arms were written against.
func datumName(n Node) string {
	if len(n.Inputs) == 0 {
		return "datum"
	}

	return varOf(n.Inputs[0])
}

// armConditions renders one arm's values as boolean conditions over the subject.
func armConditions(subject string, arm ast.SwitchArm) []string {
	out := make([]string, 0, len(arm.Values))
	for _, value := range arm.Values {
		text := strings.TrimSpace(value.Text)
		if text == "" {
			continue
		}
		if isBasicLiteral(text) {
			out = append(out, subject+" == "+text)

			continue
		}
		out = append(out, "("+text+")")
	}

	return out
}

// isBasicLiteral reports whether an arm value is a Go basic literal.
//
// A literal is COMPARED to the subject; anything else is EVALUATED as a boolean.
// The distinction is syntactic on purpose — see armPredicate's note on where a
// semantic classification belongs.
func isBasicLiteral(text string) bool {
	expr, err := parser.ParseExpr(text)
	if err != nil {
		return false
	}
	switch typed := expr.(type) {
	case *goast.BasicLit:
		return true
	case *goast.UnaryExpr:
		// A negative numeric literal parses as a unary minus over one.
		_, isLit := typed.X.(*goast.BasicLit)

		return isLit
	default:
		return false
	}
}
