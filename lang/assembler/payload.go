// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"go/types"
	"strings"
)

// PayloadOfRef reads a node's INPUT payload type off the Go reference the
// statement named.
//
// THIS IS WHERE A NODE'S TYPE ACTUALLY COMES FROM, and it is a different question
// from the BOUNDARY types the three-way rule answers. A boundary type is what a
// flow exposes across a module edge; a node's payload is whatever its own Go
// function takes, and the function's signature already says so. Reading it here
// means the commonest shape in the language — an unexported flow with a source
// and a few transforms — needs no cross-module inference at all.
//
// THE TWO SHAPES IT READS, both from the runtime's own vocabulary:
//
//	func(machine.Frame[T]) U        a Transformation or a Filter: the payload is T
//	func() machine.EdgeFactory[T]   a source's factory: the payload is T
//
// ANYTHING ELSE RETURNS false RATHER THAN A GUESS. A wrong payload type produces
// generated code that either fails to compile or, worse, compiles at a type the
// author did not mean, so an unreadable reference is reported by the caller
// rather than approximated here.
func (t *Types) PayloadOfRef(ref string) (string, bool) {
	if t == nil || t.pkgs == nil {
		return "", false
	}
	scope, ok := t.Scope()
	if !ok || scope == nil {
		return "", false
	}
	obj := scope.Lookup(baseIdent(ref))
	if obj == nil {
		return "", false
	}
	signature, ok := obj.Type().Underlying().(*types.Signature)
	if !ok {
		return "", false
	}

	return t.payloadFromSignature(signature)
}

// payloadFromSignature reads the payload out of a node function's signature.
func (t *Types) payloadFromSignature(signature *types.Signature) (string, bool) {
	// A NODE FUNCTION: its first parameter is the frame, whose type argument is
	// the payload.
	if params := signature.Params(); params.Len() > 0 {
		if arg, ok := soleTypeArgument(params.At(0).Type()); ok {
			return t.spell(arg), true
		}
	}
	// A FACTORY: it takes nothing and returns the edge factory, whose type
	// argument is the payload.
	if results := signature.Results(); results.Len() > 0 {
		if arg, ok := soleTypeArgument(results.At(0).Type()); ok {
			return t.spell(arg), true
		}
	}

	return "", false
}

// spell renders a type as the generated file must write it, dropping the local
// package's own qualifier.
func (t *Types) spell(typ types.Type) string {
	return types.TypeString(typ, func(p *types.Package) string {
		if p == nil || p.Path() == t.pkgPath {
			return ""
		}

		return p.Name()
	})
}

// soleTypeArgument returns the single type argument of an instantiated generic
// type.
//
// machine.Frame[T] and machine.EdgeFactory[T] each carry exactly one, which is
// the payload. A type carrying none, or several, is not one of those shapes.
func soleTypeArgument(typ types.Type) (types.Type, bool) {
	named, ok := typ.(*types.Named)
	if !ok {
		return nil, false
	}
	args := named.TypeArgs()
	if args == nil || args.Len() != 1 {
		return nil, false
	}

	return args.At(0), true
}

// baseIdent reduces a Go reference to the identifier a package scope holds.
//
// A reference may be written as a bare name, a call, or a qualified selector; the
// scope lookup needs the last identifier before any call parentheses.
func baseIdent(ref string) string {
	name := strings.TrimSpace(ref)
	if at := strings.IndexByte(name, '('); at >= 0 {
		name = name[:at]
	}
	if at := strings.LastIndexByte(name, '.'); at >= 0 {
		name = name[at+1:]
	}

	return strings.TrimSpace(name)
}
