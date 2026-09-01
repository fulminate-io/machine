// Package loader - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package loader

import "go/types"

// Carries reports whether a type satisfies an interface, answering separately
// for the type's VALUE method set and its POINTER method set.
//
// TWO ANSWERS RATHER THAN ONE, AND THE SPLIT IS THE WHOLE POINT. Go's method
// sets are not the same set: a method declared on a value receiver belongs to
// both, while one declared on a pointer receiver belongs only to the pointer's.
// The escape hatches callers ask about are routinely split across exactly that
// line — gob's own is conventionally written with GobEncode on the value and
// GobDecode on the pointer — so a single boolean collapses the two and answers
// the wrong question for whichever half sits on the pointer.
//
// THE INTERFACE IS A PARAMETER SO NO PROPERTY IS HARD-CODED HERE. The
// serialization derivation asks about the gob codec pair, a clone derivation
// asks about a Clone or Copy method, and a type-flow asks about whatever a
// signature named. All three are the same question about a different interface.
func Carries(typ types.Type, iface *types.Interface) (value, pointer bool) {
	return types.Implements(typ, iface), types.Implements(types.NewPointer(typ), iface)
}
