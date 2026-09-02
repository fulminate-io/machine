// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"go/types"
	"strings"
)

// structBody clones a struct field by field.
//
// A FOREIGN STRUCT WITH UNEXPORTED FIELDS IS REFUSED, because generated code
// cannot name those fields at all: copying only the exported half would produce a
// value that looks copied and silently shares or loses the rest.
func (d *cloneDeriver) structBody(shape *types.Struct, name, spelling string, at posRange, depth int) (string, bool) {
	var b strings.Builder
	b.WriteString("\tout := v\n")
	for i := range shape.NumFields() {
		field := shape.Field(i)
		if !field.Exported() && !sameGeneratedPackage(field) {
			d.refuse(at, "the type %q carries the unexported field %q from another package, "+
				"which generated code cannot copy; supply an explicit clone function",
				spelling, field.Name())

			return "", false
		}
		if isTriviallyCopyableType(field.Type()) {
			continue
		}
		fieldClone, ok := d.derive(field.Type(), at, depth+1)
		if !ok {
			return "", false
		}
		b.WriteString("\tout." + field.Name() + " = " + fieldClone + "(v." + field.Name() + ")\n")
	}
	b.WriteString("\n\treturn out")

	return d.wrap(name, spelling, b.String()), true
}

// sliceBody clones a slice element by element.
//
// A NIL SLICE STAYS NIL. Returning an empty non-nil slice for a nil one is a
// different value, and a consumer distinguishing the two would see the copy
// disagree with the original.
func (d *cloneDeriver) sliceBody(shape *types.Slice, name, spelling string, at posRange, depth int) (string, bool) {
	elem, ok := d.elementClone(shape.Elem(), at, depth)
	if !ok {
		return "", false
	}
	var b strings.Builder
	b.WriteString("\tif v == nil {\n\t\treturn nil\n\t}\n")
	b.WriteString("\tout := make(" + spelling + ", len(v))\n")
	b.WriteString("\tfor i := range v {\n\t\tout[i] = " + elem + "\n\t}\n")
	b.WriteString("\n\treturn out")

	return d.wrap(name, spelling, b.String()), true
}

// arrayBody clones a fixed-size array element by element.
func (d *cloneDeriver) arrayBody(shape *types.Array, name, spelling string, at posRange, depth int) (string, bool) {
	elem, ok := d.elementClone(shape.Elem(), at, depth)
	if !ok {
		return "", false
	}
	var b strings.Builder
	b.WriteString("\tout := v\n")
	b.WriteString("\tfor i := range v {\n\t\tout[i] = " + elem + "\n\t}\n")
	b.WriteString("\n\treturn out")

	return d.wrap(name, spelling, b.String()), true
}

// mapBody clones a map key and value at a time.
func (d *cloneDeriver) mapBody(shape *types.Map, name, spelling string, at posRange, depth int) (string, bool) {
	keyClone, ok := d.scalarOrDerived(shape.Key(), "k", at, depth)
	if !ok {
		return "", false
	}
	valueClone, ok := d.scalarOrDerived(shape.Elem(), "value", at, depth)
	if !ok {
		return "", false
	}
	var b strings.Builder
	b.WriteString("\tif v == nil {\n\t\treturn nil\n\t}\n")
	b.WriteString("\tout := make(" + spelling + ", len(v))\n")
	b.WriteString("\tfor k, value := range v {\n\t\tout[" + keyClone + "] = " + valueClone + "\n\t}\n")
	b.WriteString("\n\treturn out")

	return d.wrap(name, spelling, b.String()), true
}

// pointerBody follows a pointer, allocating a fresh target.
//
// A NIL POINTER STAYS NIL, for the reason a nil slice does.
func (d *cloneDeriver) pointerBody(shape *types.Pointer, name, spelling string, at posRange, depth int) (string, bool) {
	target, ok := d.scalarOrDerived(shape.Elem(), "*v", at, depth)
	if !ok {
		return "", false
	}
	var b strings.Builder
	b.WriteString("\tif v == nil {\n\t\treturn nil\n\t}\n")
	b.WriteString("\tcopied := " + target + "\n")
	b.WriteString("\n\treturn &copied")

	return d.wrap(name, spelling, b.String()), true
}

// elementClone renders the expression cloning one element of a container.
func (d *cloneDeriver) elementClone(elem types.Type, at posRange, depth int) (string, bool) {
	return d.scalarOrDerived(elem, "v[i]", at, depth)
}

// scalarOrDerived renders either a direct copy or a call to a derived clone.
func (d *cloneDeriver) scalarOrDerived(typ types.Type, expr string, at posRange, depth int) (string, bool) {
	if isTriviallyCopyableType(typ) {
		return expr, true
	}
	name, ok := d.derive(typ, at, depth+1)
	if !ok {
		return "", false
	}

	return name + "(" + expr + ")", true
}

// isTriviallyCopyableType reports whether assignment fully copies a value of this
// type.
//
// UNLIKE THE PHASE-3 SPELLING CHECK, this one reads the resolved TYPE rather than
// its written name, so it is exact rather than conservative: a named type whose
// underlying type is basic is copied by assignment whatever it is called.
func isTriviallyCopyableType(typ types.Type) bool {
	switch shape := typ.Underlying().(type) {
	case *types.Basic:
		return true
	case *types.Struct:
		for i := range shape.NumFields() {
			if !isTriviallyCopyableType(shape.Field(i).Type()) {
				return false
			}
		}

		return true
	case *types.Array:
		return isTriviallyCopyableType(shape.Elem())
	default:
		return false
	}
}

// sameGeneratedPackage reports whether an unexported field is one the generated
// code could name.
//
// A field unexported in the package the generated file itself belongs to IS
// nameable; one from another package is not, whatever it is called.
func sameGeneratedPackage(field *types.Var) bool {
	return field.Pkg() == nil
}
