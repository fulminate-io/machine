// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	goast "go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
)

// codecFamily is a checkpoint operand decomposed into the two pieces needed to
// RE-INSTANTIATE it at another type: the generic head, and the written form.
//
// WHY RE-INSTANTIATION IS THE WHOLE PROBLEM. A completion-anchored checkpoint
// journals with its SUCCESSOR's codec, not its own, so the generator has to
// express "the same codec family, at the successor's input type". Doing that
// means naming the operand's generic head and swapping its written type
// argument. An implementation that instead reused the operand verbatim would
// emit a codec instantiated at the WRONG payload type — and that is the worst
// failure available here, because it TYPE-CHECKS ON BOTH SIDES. Nothing fails at
// build time, nothing fails at Start, and the damage appears only when a
// recovery unmarshals a journal record with a codec built for another type.
type codecFamily struct {
	// head is the generic type or function, qualified as written, without its
	// type argument: `machine.GobCodec`, `codecs.New`.
	head string
	// literal is true for a composite literal (`machine.GobCodec[Order]{}`) and
	// false for a call (`codecs.New[Order]()`).
	literal bool
}

// instantiate renders the family at a given type spelling.
func (f codecFamily) instantiate(typeSpelling string) string {
	if f.literal {
		return f.head + "[" + typeSpelling + "]{}"
	}

	return f.head + "[" + typeSpelling + "]()"
}

// admissibleCodecForms is what a refusal tells the author they CAN write. It is
// stated once so the message and the implementation cannot drift.
const admissibleCodecForms = "an explicitly instantiated generic composite literal such as " +
	"machine.GobCodec[Order]{}, or an explicitly instantiated generic call such as codecs.New[Order]()"

// codecFamilyOf decomposes a checkpoint operand, or reports why it cannot.
//
// EXACTLY TWO WRITTEN FORMS ARE ADMITTED, and the narrowness is the point rather
// than a limitation to apologize for. Re-instantiating means naming a generic
// head and swapping a WRITTEN type argument, and only these two forms carry one:
//
//	machine.GobCodec[Order]{}   a composite literal of an explicitly instantiated generic type
//	codecs.New[Order]()         a call of an explicitly instantiated generic function
//
// EVERYTHING ELSE IS REFUSED BY NAME, each for its own reason:
//
//	order                 a bare identifier names a VALUE, not a family
//	codecs.NewOrder()     a non-generic call has no type argument to swap
//	codecs.New(order)     an INFERRED-generic call has no WRITTEN type argument to read,
//	                      and it is the form an author most plausibly writes
//	""                    the empty operand of a bare clause
//	GobCodec[Order        an expression that does not parse
//	machine.GobCodec      a bare type name is neither literal nor call
//
// THE REFUSAL IS THE HONEST OUTPUT. Stating what cannot be lowered is strictly
// better than emitting a codec the generator guessed, because the guess is
// undetectable downstream.
func codecFamilyOf(operand string) (codecFamily, string) {
	trimmed := strings.TrimSpace(operand)
	if trimmed == "" {
		return codecFamily{}, "the checkpoint clause names no codec"
	}
	expr, err := parser.ParseExpr(trimmed)
	if err != nil {
		return codecFamily{}, "the codec operand is not a Go expression"
	}
	switch typed := expr.(type) {
	case *goast.CompositeLit:
		return familyFromType(typed.Type, true)
	case *goast.CallExpr:
		return familyFromType(typed.Fun, false)
	default:
		return codecFamily{}, "the codec operand names a value rather than a codec type or constructor"
	}
}

// familyFromType reads a generic head out of an instantiated type or function
// expression.
//
// An IndexExpr is a single type argument (`GobCodec[Order]`) and an
// IndexListExpr is several; either carries a written argument, which is what
// makes the head re-instantiable. Anything else — a bare identifier, a bare
// selector — carries none.
func familyFromType(expr goast.Expr, literal bool) (codecFamily, string) {
	switch typed := expr.(type) {
	case *goast.IndexExpr:
		return codecFamily{head: renderExpr(typed.X), literal: literal}, ""
	case *goast.IndexListExpr:
		return codecFamily{head: renderExpr(typed.X), literal: literal}, ""
	default:
		if literal {
			return codecFamily{}, "the codec literal names no type argument, so its family cannot be re-instantiated"
		}

		return codecFamily{}, "the codec call names no written type argument, so its family cannot be re-instantiated"
	}
}

// renderExpr prints a Go expression back to source text.
func renderExpr(expr goast.Expr) string {
	var out strings.Builder
	if err := printer.Fprint(&out, token.NewFileSet(), expr); err != nil {
		return ""
	}

	return out.String()
}
