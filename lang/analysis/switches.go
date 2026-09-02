// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package analysis

import (
	"reflect"
	"strings"
	"unicode"

	"github.com/whitaker-io/machine/lang/ast"
)

// The classifications an arm value can carry.
//
// The parser deliberately does not make this call: SwitchArm's documentation
// says it records values as "positioned verbatim spans" and that telling a
// literal from a Go predicate "would need an imported Go expression grammar with
// precedence backtracking. Classification belongs to the analysis engine."
const (
	armLiteral   = "literal"
	armPredicate = "predicate"
	armPattern   = "pattern"
)

// SwitchesAnalyzer classifies switch arm values and requires an else.
var SwitchesAnalyzer = &Analyzer{
	Name: "switches",
	Doc: "switches classifies each arm value as a literal, a Go predicate or a destructuring " +
		"pattern, and requires every switch to carry an else. A pattern is REJECTED: an arm value " +
		"is matched, not destructured, and binding a pattern is not a form this language has — " +
		"pattern matching was rejected because there are no Go sum types to back it. The else is " +
		"required because V1 CANNOT PROVE COVERAGE: exhaustiveness was ruled required with else " +
		"mandatory unless the analysis engine can prove coverage, and proving it needs go/types, " +
		"which the structural-first ruling puts out of scope. So the mandate is a v1 limitation " +
		"rather than a language rule, and whoever lands go/types should read this as the signal " +
		"that a coverage proof is now available and the mandate can relax.",
	Requires:   []*Analyzer{SymbolsAnalyzer},
	Run:        runSwitches,
	ResultType: reflect.TypeOf((*ArmClassification)(nil)),
}

// ArmValue is one classified arm value.
type ArmValue struct {
	Flow  string
	Text  string
	Class string
	Pos   ast.Position
}

// ArmClassification is the switches analyzer's result: every arm value it read,
// in source order, with the class it assigned.
type ArmClassification struct {
	Values []ArmValue
}

// runSwitches classifies and checks every switch in every source.
func runSwitches(p *Pass) (any, error) {
	table, ok := p.ResultOf[SymbolsAnalyzer].(*SymbolTable)
	if !ok {
		return nil, errNoSymbols
	}

	out := &ArmClassification{}
	for f := range table.Files {
		for i := range table.Files[f].Flows {
			checkFlowSwitches(p, table.Files[f].Src, &table.Files[f].Flows[i], out)
		}
	}
	return out, nil
}

// checkFlowSwitches walks one flow's switch statements.
func checkFlowSwitches(p *Pass, src Source, flow *FlowSymbols, out *ArmClassification) {
	for _, stmt := range flow.Body {
		sw, ok := stmt.(ast.SwitchStmt)
		if !ok {
			continue
		}
		classifyArms(p, src, flow, sw, out)
		requireElse(p, src, flow, sw)
	}
}

// classifyArms records every arm value's class and reports the pattern-shaped
// ones.
func classifyArms(p *Pass, src Source, flow *FlowSymbols, sw ast.SwitchStmt, out *ArmClassification) {
	for _, arm := range sw.Arms {
		for _, value := range arm.Values {
			class := classifyArmValue(value.Text)
			out.Values = append(out.Values, ArmValue{Flow: flow.Name, Text: value.Text, Class: class, Pos: value.Start})
			if class != armPattern {
				continue
			}
			p.Report(src, Diagnostic{
				Pos: value.Start,
				End: value.Stop,
				Message: "the switch arm value " + strings.TrimSpace(value.Text) + " is a destructuring pattern; " +
					"an arm value is matched, not destructured, and binding a pattern is not a form this language has",
				Severity: SeverityError,
			})
		}
	}
}

// requireElse reports a switch with no else.
func requireElse(p *Pass, src Source, flow *FlowSymbols, sw ast.SwitchStmt) {
	if sw.Else != nil {
		return
	}
	p.Report(src, Diagnostic{
		Pos: sw.Name.NamePos,
		End: sw.Name.End(),
		Message: "the switch " + sw.Name.Name + " in flow " + flow.Name + " has no else; routing is first-match " +
			"and this engine cannot prove the arms cover the subject, so an else is required",
		Severity: SeverityError,
	})
}

// classifyArmValue decides what one verbatim arm span is.
//
// A PATTERN is an identifier or qualified name immediately followed by a brace —
// the composite-literal shape, which is how a destructuring arm is written. A
// LITERAL is a quoted string, a number, or a bare name with nothing applied to
// it. Everything else is a Go predicate expression.
func classifyArmValue(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return armPredicate
	}
	if head := leadingName(trimmed); head != "" && strings.HasPrefix(strings.TrimSpace(trimmed[len(head):]), "{") {
		return armPattern
	}
	if isLiteralSpan(trimmed) {
		return armLiteral
	}
	return armPredicate
}

// leadingName returns the identifier or qualified name a span starts with, or an
// empty string when it starts with something else.
func leadingName(text string) string {
	end := 0
	for end < len(text) {
		r := rune(text[end])
		isFirst := end == 0
		switch {
		case unicode.IsLetter(r) || r == '_':
		case !isFirst && (unicode.IsDigit(r) || r == '.'):
		default:
			return text[:end]
		}
		end++
	}
	return text
}

// isLiteralSpan reports whether a span is a quoted string, a number, or a bare
// name that nothing is applied to.
func isLiteralSpan(text string) bool {
	if strings.HasPrefix(text, `"`) || strings.HasPrefix(text, "`") {
		return true
	}
	if unicode.IsDigit(rune(text[0])) || (text[0] == '-' && len(text) > 1 && unicode.IsDigit(rune(text[1]))) {
		return true
	}
	return leadingName(text) == text
}
