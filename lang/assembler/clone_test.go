// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"go/types"
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
	"github.com/whitaker-io/machine/lang/loader"
)

// somewhere is the .flow span refusals are reported against in these tests.
var somewhere = posRange{start: ast.Position{Line: 5, Col: 3}, stop: ast.Position{Line: 5, Col: 20}}

// deriveOne runs a derivation over one type.
func deriveOne(t *testing.T, typ types.Type) (*cloneDeriver, string, bool) {
	t.Helper()
	d := newCloneDeriver("")
	name, ok := d.derive(typ, somewhere, 0)

	return d, name, ok
}

// namedStruct builds a named struct type with the given fields.
func namedStruct(name string, fields ...*types.Var) *types.Named {
	obj := types.NewTypeName(0, nil, name, nil)
	named := types.NewNamed(obj, nil, nil)
	named.SetUnderlying(types.NewStruct(fields, nil))

	return named
}

// field builds an exported field.
func field(name string, typ types.Type) *types.Var {
	return types.NewField(0, nil, name, typ, false)
}

var (
	basicInt    = types.Typ[types.Int]
	basicString = types.Typ[types.String]
)

// TestCloneSynthesisCoversAndRefuses walks every derivable shape and every
// underivable one.
//
// THE DERIVABLE HALF ASSERTS THE SYNTHESIZED BODY ISOLATES THE COPY, which is
// what a shallow clone would fail: a slice clone that returned its input, or a
// struct clone that assigned rather than walked, would still produce a function
// and still compile.
func TestCloneSynthesisCoversAndRefuses(t *testing.T) {
	t.Run("a value type is copied by assignment", func(t *testing.T) {
		d, name, ok := deriveOne(t, basicInt)
		if !ok {
			t.Fatalf("int was refused: %v", messagesOf(d.Diagnostics()))
		}
		if !strings.Contains(d.bodies[basicInt.String()], "return v") {
			t.Errorf("a basic type's clone is %q, want a plain assignment", d.bodies[basicInt.String()])
		}
		_ = name
	})

	t.Run("a slice is walked element by element and keeps nil as nil", func(t *testing.T) {
		sliceOfSlices := types.NewSlice(types.NewSlice(basicInt))
		d, _, ok := deriveOne(t, sliceOfSlices)
		if !ok {
			t.Fatalf("a slice of slices was refused: %v", messagesOf(d.Diagnostics()))
		}
		body := d.bodies[sliceOfSlices.String()]
		if !strings.Contains(body, "make(") || !strings.Contains(body, "for i := range v") {
			t.Errorf("the slice clone does not walk its elements:\n%s", body)
		}
		// THE ISOLATION ASSERTION: the inner slice is CLONED, not assigned. A
		// body that wrote `out[i] = v[i]` would compile and share backing arrays.
		if strings.Contains(body, "out[i] = v[i]") {
			t.Errorf("the slice clone assigns its elements, so both copies share backing memory:\n%s", body)
		}
		if !strings.Contains(body, "if v == nil") {
			t.Errorf("the slice clone turns a nil slice into a non-nil one:\n%s", body)
		}
	})

	t.Run("a map is walked and a pointer is followed", func(t *testing.T) {
		mapType := types.NewMap(basicString, types.NewSlice(basicInt))
		d, _, ok := deriveOne(t, mapType)
		if !ok {
			t.Fatalf("a map was refused: %v", messagesOf(d.Diagnostics()))
		}
		if body := d.bodies[mapType.String()]; !strings.Contains(body, "for k, value := range v") {
			t.Errorf("the map clone does not walk its entries:\n%s", body)
		}

		pointer := types.NewPointer(types.NewSlice(basicInt))
		d, _, ok = deriveOne(t, pointer)
		if !ok {
			t.Fatalf("a pointer was refused: %v", messagesOf(d.Diagnostics()))
		}
		body := d.bodies[pointer.String()]
		if !strings.Contains(body, "return &copied") {
			t.Errorf("the pointer clone does not allocate a fresh target:\n%s", body)
		}
		if !strings.Contains(body, "if v == nil") {
			t.Errorf("the pointer clone turns a nil pointer into a non-nil one:\n%s", body)
		}
	})

	t.Run("a struct is walked field by field", func(t *testing.T) {
		payload := namedStruct("Payload", field("ID", basicString), field("Tags", types.NewSlice(basicString)))
		d, _, ok := deriveOne(t, payload)
		if !ok {
			t.Fatalf("a struct was refused: %v", messagesOf(d.Diagnostics()))
		}
		body := d.bodies[payload.String()]
		// THE TRIVIAL FIELD IS COPIED BY THE STRUCT ASSIGNMENT and needs no call;
		// the slice field MUST be cloned or the two copies share it.
		if !strings.Contains(body, "out.Tags = ") {
			t.Errorf("the struct clone does not clone its slice field:\n%s", body)
		}
		if strings.Contains(body, "out.ID = ") {
			t.Errorf("the struct clone emits a redundant call for a trivially copied field:\n%s", body)
		}
	})

	t.Run("a type carrying its own Clone uses it", func(t *testing.T) {
		// The type's own method is PREFERRED over any derivation, and it is asked
		// through loader.Carries so the value and pointer method sets are answered
		// separately.
		self := types.NewTypeName(0, nil, "SelfCloning", nil)
		named := types.NewNamed(self, types.NewStruct(nil, nil), nil)
		signature := types.NewSignatureType(types.NewVar(0, nil, "", named), nil, nil, nil,
			types.NewTuple(types.NewVar(0, nil, "", named)), false)
		named.AddMethod(types.NewFunc(0, nil, "Clone", signature))

		d, _, ok := deriveOne(t, named)
		if !ok {
			t.Fatalf("a self-cloning type was refused: %v", messagesOf(d.Diagnostics()))
		}
		if body := d.bodies[named.String()]; !strings.Contains(body, "v.Clone()") {
			t.Errorf("the derivation ignored the type's own Clone:\n%s", body)
		}
	})

	// THE UNDERIVABLE SHAPES, each a positioned refusal naming the type.
	for name, typ := range map[string]types.Type{
		"an interface": types.NewInterfaceType(nil, nil),
		"a func":       types.NewSignatureType(nil, nil, nil, nil, nil, false),
		"a channel":    types.NewChan(types.SendRecv, basicInt),
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			d, _, ok := deriveOne(t, typ)
			if ok {
				t.Fatalf("%s was derived", name)
			}
			diags := d.Diagnostics()
			if len(diags) == 0 {
				t.Fatalf("%s was refused with no diagnostic", name)
			}
			if !strings.Contains(diags[0].Message, "clone") {
				t.Errorf("the refusal %q does not tell the author how to proceed", diags[0].Message)
			}
			if diags[0].Pos.Line != 5 {
				t.Errorf("the refusal is at line %d, want the declaration's line 5", diags[0].Pos.Line)
			}
		})
	}

	t.Run("refuses a foreign struct with unexported fields", func(t *testing.T) {
		foreign := types.NewPackage("example.com/other", "other")
		hidden := types.NewField(0, foreign, "hidden", basicInt, false)
		payload := namedStruct("Foreign", field("ID", basicString), hidden)

		d, _, ok := deriveOne(t, payload)
		if ok {
			t.Fatal("a foreign struct with unexported fields was derived")
		}
		if !strings.Contains(strings.Join(messagesOf(d.Diagnostics()), "\n"), "hidden") {
			t.Errorf("the refusal does not name the field: %v", messagesOf(d.Diagnostics()))
		}
	})
}

// TestCloneWalkTerminatesAndRefusesPastItsCeiling holds the derivation to the
// design the loader's exhaustion incident produced.
//
// THE INCIDENT: the same memoized recursion over the same types.Type structure,
// run with its memo seed removed, recursed into Go's 1GB goroutine-stack ceiling
// — a 512MB stack, a 43,561-byte goroutine dump and a host memory alert. The two
// bounding mechanisms here are INDEPENDENT, and the third leg is what proves the
// distinction: with the seed broken the ceiling still terminates the walk, so a
// termination-only assertion would pass over a defective memo.
//
// NO LEG HERE TERMINATES BY RESOURCE EXHAUSTION. Each is a bounded assertion that
// completes in milliseconds; a go test -timeout is a wall-clock bound and would
// not discharge that requirement.
func TestCloneWalkTerminatesAndRefusesPastItsCeiling(t *testing.T) {
	t.Run("a deep but finite type still derives", func(t *testing.T) {
		// THE CEILING BOUNDS A RUNAWAY; IT DOES NOT POLICE LEGITIMATE DEPTH. A
		// gate that only ever refused would be satisfied by a derivation that
		// refuses everything.
		deep := types.Type(basicInt)
		for range 32 {
			deep = types.NewSlice(deep)
		}
		d, _, ok := deriveOne(t, deep)
		if !ok {
			t.Fatalf("a 32-deep finite type was refused: %v", messagesOf(d.Diagnostics()))
		}
	})

	t.Run("a self-referential type terminates through the memo, not the ceiling", func(t *testing.T) {
		// A node holding a slice of itself. Without the seed this recursion does
		// not run slowly — it exhausts the stack.
		obj := types.NewTypeName(0, nil, "SelfRef", nil)
		named := types.NewNamed(obj, nil, nil)
		named.SetUnderlying(types.NewStruct([]*types.Var{
			field("ID", basicString),
			field("Children", types.NewSlice(named)),
		}, nil))

		d, _, ok := deriveOne(t, named)
		if !ok {
			t.Fatalf("a self-referential type was refused: %v", messagesOf(d.Diagnostics()))
		}
		// TERMINATION ALONE IS NOT THE ASSERTION. The ceiling would terminate this
		// walk too, so the leg that separates the two mechanisms is the ABSENCE of
		// a ceiling refusal.
		for _, d := range d.Diagnostics() {
			if strings.Contains(d.Message, "ceiling") {
				t.Errorf("the self-referential type terminated through the CEILING rather than the memo: %q",
					d.Message)
			}
		}
	})

	t.Run("past the ceiling the refusal names the bound", func(t *testing.T) {
		d := newCloneDeriver("")
		// Enter one frame past the bound directly, so the assertion is bounded
		// rather than reached by building a pathological type.
		_, ok := d.derive(types.NewSlice(basicInt), somewhere, cloneCeiling+1)
		if ok {
			t.Fatal("a walk past the ceiling was allowed to continue")
		}
		message := strings.Join(messagesOf(d.Diagnostics()), "\n")
		if !strings.Contains(message, "ceiling") {
			t.Errorf("the refusal %q does not name the bound", message)
		}
		// THE BOUND IS THE LOADER'S, so the two derivations over the same
		// structure refuse at the same depth rather than drifting apart.
		if cloneCeiling != loader.MaxDepth {
			t.Errorf("the clone ceiling is %d and the loader's is %d; they have drifted",
				cloneCeiling, loader.MaxDepth)
		}
	})
}
