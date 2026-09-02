// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"fmt"
	"go/types"
	"strings"

	"github.com/whitaker-io/machine/lang/ast"
	"github.com/whitaker-io/machine/lang/loader"
)

// cloneCeiling is the walk's frame bound.
//
// IT IS STATED AS loader.MaxDepth RATHER THAN A PRIVATE CONSTANT so the two
// recursive derivations over the same types.Type structure refuse at the SAME
// depth instead of drifting apart. The loader exports it for exactly this: a
// consumer turning a depth refusal into a diagnostic has to be able to name the
// bound it was refused by.
const cloneCeiling = loader.MaxDepth

// cloneDeriver synthesizes a deep-copy function for a payload type.
//
// WHY A CLONER IS MANDATORY AND CANNOT BE SHALLOW. machine.NewKey requires one
// and panics without it, because Tee deep-copies the frame into both branches and
// the frame's clone walks the cloner map calling each. A shallow cloner leaves
// two branches sharing a pointer, slice or map — which is the precise defect the
// mandatory-cloner rule exists to prevent, and it is silent: both branches look
// right until one mutates.
//
// STATE CELLS TAKE NO CLONER. machine.NewCell has no cloner parameter, because a
// heap value is machine-scoped and is never copied by Tee. None is synthesized.
//
// THE WALK IS BOUNDED TWO INDEPENDENT WAYS, and that is the direct absorption of
// a live incident rather than defensive padding. The loader's serialization walk
// — the same memoized recursion over the same structure — was run with its memo
// seed removed and recursed into Go's 1GB goroutine-stack ceiling: a 512MB stack,
// a 43,561-byte goroutine dump and a host memory alert. The repaired design is
// the one taken here, and the two mechanisms are deliberately independent so a
// defect in either is still caught by a gate that checks for both.
type cloneDeriver struct {
	// memo maps a type's string to the name of the function that clones it.
	//
	// THE KEY IS SEEDED BEFORE RECURSING, and that seed is what makes a
	// self-referential type terminate AT ALL: the recursion re-enters the same
	// key, hits the seed and unwinds. Without it the walk does not run slowly, it
	// exhausts the stack.
	memo map[string]string
	// order preserves the emission order of the synthesized functions, because
	// map iteration order would make generated output unstable.
	order []string
	// bodies holds each synthesized function's source.
	bodies map[string]string
	// diags collects the refusals.
	diags []Diagnostic
	// local is the import path of the package the generated file BELONGS TO.
	//
	// It is what makes a synthesized signature compile. types.Type.String()
	// qualifies every named type by its package path, so a type declared in the
	// generated package itself renders as `probe.Order` — a spelling that is
	// undefined INSIDE probe, because a package does not import itself. The
	// qualifier below drops the prefix for exactly that package and keeps it for
	// every other.
	local string
}

// spellingOf renders a type as the generated file must WRITE it.
func (d *cloneDeriver) spellingOf(typ types.Type) string {
	return types.TypeString(typ, func(p *types.Package) string {
		if p == nil || p.Path() == d.local {
			return ""
		}

		return p.Name()
	})
}

// newCloneDeriver builds an empty derivation.
func newCloneDeriver(local string) *cloneDeriver {
	return &cloneDeriver{memo: map[string]string{}, bodies: map[string]string{}, local: local}
}

// derive returns the name of a function that deep-copies the given type.
//
// A REFUSAL IS NEVER MEMOIZED AS AN ANSWER ABOUT THE TYPE. The depth check runs
// BEFORE the memo key is stored, so a walk that refused once because it was deep
// does not report that refusal to an unrelated shallower caller.
func (d *cloneDeriver) derive(typ types.Type, at posRange, depth int) (string, bool) {
	if depth > cloneCeiling {
		d.refuse(at, "the type %q is deeper than the %d-frame derivation ceiling; "+
			"supply an explicit clone function for it", typ.String(), cloneCeiling)

		return "", false
	}
	key := typ.String()
	if name, seen := d.memo[key]; seen {
		return name, true
	}

	name := cloneFuncName(key)
	// SEED FIRST. Everything below may recurse back into this same type.
	d.memo[key] = name
	d.order = append(d.order, key)

	body, ok := d.bodyFor(typ, name, at, depth)
	if !ok {
		return "", false
	}
	d.bodies[key] = body

	return name, true
}

// bodyFor synthesizes one type's clone body.
func (d *cloneDeriver) bodyFor(typ types.Type, name string, at posRange, depth int) (string, bool) {
	spelling := d.spellingOf(typ)
	if carriesCloneMethod(typ) {
		// THE TYPE'S OWN Clone IS PREFERRED over anything derived. It is asked
		// through loader.Carries, which answers separately for the VALUE and
		// POINTER method sets, because a method on a pointer receiver is not in
		// the value's set and a single boolean would answer the wrong question.
		return d.wrap(name, spelling, "\treturn v.Clone()"), true
	}
	switch shape := typ.Underlying().(type) {
	case *types.Basic:
		return d.wrap(name, spelling, "\treturn v"), true
	case *types.Struct:
		return d.structBody(shape, name, spelling, at, depth)
	case *types.Slice:
		return d.sliceBody(shape, name, spelling, at, depth)
	case *types.Array:
		return d.arrayBody(shape, name, spelling, at, depth)
	case *types.Map:
		return d.mapBody(shape, name, spelling, at, depth)
	case *types.Pointer:
		return d.pointerBody(shape, name, spelling, at, depth)
	default:
		d.refuseUnderivable(typ, at)

		return "", false
	}
}

// refuseUnderivable reports a shape no derivation can copy.
func (d *cloneDeriver) refuseUnderivable(typ types.Type, at posRange) {
	what := "this shape"
	switch typ.Underlying().(type) {
	case *types.Interface:
		what = "an interface, whose dynamic value is unknown until run time"
	case *types.Signature:
		what = "a func, which has no meaningful copy"
	case *types.Chan:
		what = "a channel, which is an identity rather than a value"
	}
	d.refuse(at, "the type %q cannot be deep-copied: it is %s. "+
		"Supply an explicit clone function with the var's `clone` clause", d.spellingOf(typ), what)
}

// refuse records a positioned refusal.
func (d *cloneDeriver) refuse(at posRange, format string, args ...any) {
	d.diags = append(d.diags, diagnosticAt(at.start, at.stop, format, args...))
}

// wrap renders a clone function around a body.
func (d *cloneDeriver) wrap(name, spelling, body string) string {
	return "func " + name + "(v " + spelling + ") " + spelling + " {\n" + body + "\n}\n"
}

// posRange is the .flow span a refusal is reported against.
type posRange struct {
	start, stop ast.Position
}

// carriesCloneMethod reports whether a type declares its own Clone.
//
// It asks through loader.Carries so the VALUE and POINTER method sets are
// answered separately; the derivation only accepts the VALUE side, because a
// clone function receives a value and a pointer-receiver method is not in its
// set.
func carriesCloneMethod(typ types.Type) bool {
	iface := cloneInterface(typ)
	if iface == nil {
		return false
	}
	value, _ := loader.Carries(typ, iface)

	return value
}

// cloneInterface builds the `interface{ Clone() T }` a type would have to
// satisfy to clone itself.
func cloneInterface(typ types.Type) *types.Interface {
	signature := types.NewSignatureType(nil, nil, nil, nil,
		types.NewTuple(types.NewVar(0, nil, "", typ)), false)
	method := types.NewFunc(0, nil, "Clone", signature)
	iface := types.NewInterfaceType([]*types.Func{method}, nil)
	iface.Complete()

	return iface
}

// cloneFuncName renders a stable, unique function name for a type.
//
// It carries the derived separator's role: the name is built from the type's own
// string with every character a Go identifier cannot hold replaced, so two
// distinct types cannot collide and no author-written name can be produced.
func cloneFuncName(key string) string {
	var b strings.Builder
	b.WriteString("cloneFlow_")
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}

	return b.String()
}

// Functions renders every synthesized clone function, in derivation order.
func (d *cloneDeriver) Functions() string {
	var b strings.Builder
	for _, key := range d.order {
		if body, ok := d.bodies[key]; ok {
			b.WriteString(body)
			b.WriteString("\n")
		}
	}

	return b.String()
}

// Diagnostics returns the refusals.
func (d *cloneDeriver) Diagnostics() []Diagnostic { return d.diags }

// derivationError renders a refusal for a caller that wants one error.
func (d *cloneDeriver) derivationError() error {
	if len(d.diags) == 0 {
		return nil
	}

	return fmt.Errorf("%s", d.diags[0].Message)
}
