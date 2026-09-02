// Package loader - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package loader

import (
	"go/token"
	"go/types"
	"strconv"
	"sync"
)

// Site is WHERE a value is being serialized, and it is a parameter rather than a
// property of the type because the same Go type is serializable in one surface
// and not in the other.
//
// The question this package answers is always "is T serializable AT THIS SITE",
// never "is T serializable". It is spelled Site rather than Position to avoid
// colliding with ast.Position, which this package also uses.
type Site int

const (
	// SiteConcrete is a value stored under its own static type.
	SiteConcrete Site = iota
	// SiteInterface is a value stored in an interface-typed slot, where the
	// decoder must reconstruct the concrete type from a name on the wire.
	SiteInterface
)

// Reason is why a type is not cleanly serializable.
//
// THE THREE ARE NOT INTERCHANGEABLE, and two of them are not even the same KIND
// of statement. ReasonNeedsRegistration is a REQUIREMENT that emitted code
// satisfies with a gob.Register call. ReasonSilentDrop is a REFUSAL: no
// generated code repairs it, and registration in particular does not.
const (
	// ReasonSilentDrop is a field the codec discards while reporting success —
	// the class with no runtime signal at all, which is what makes it the
	// generation-time refusal.
	ReasonSilentDrop Reason = iota
	// ReasonNoExportedFields is a struct the codec cannot encode at all,
	// because it carries nothing the codec is allowed to see.
	ReasonNoExportedFields
	// ReasonNeedsRegistration is a named type in an interface slot, which the
	// decoder cannot reconstruct until its name is registered.
	ReasonNeedsRegistration
	// ReasonDepthExceeded is the walk refusing to descend further, because it
	// passed MaxDepth frames without finishing. It is a statement about the
	// WALK rather than about the type: a type that provokes it is not
	// necessarily unserializable, it is one this derivation declined to decide.
	ReasonDepthExceeded
)

// MaxDepth is the walk's frame ceiling.
//
// IT IS EXPORTED BECAUSE A CONSUMER TURNING A ReasonDepthExceeded FINDING INTO A
// DIAGNOSTIC MUST BE ABLE TO NAME THE BOUND IT WAS REFUSED BY. A ceiling that
// constrains a caller and that the caller cannot read is half a contract.
//
// THE DIMENSION IS WALK FRAMES — the recursion depth of the walk itself — because
// that is the quantity which grows without bound. It is NOT struct-nesting
// depth, and the ratio between the two is a property of this file's recursion
// structure rather than of the language, so it is deliberately not stated here:
// if the recursion changes, the margin is RE-MEASURED in frames rather than
// re-derived from an assumed ratio.
//
// The value is roughly 64x the deepest walk any real type in this repository's
// corpus produces. Permissive is the correct direction: the ceiling exists to
// bound a RUNAWAY, not to police legitimate depth, so a generous bound costs
// microseconds while a tight one would refuse a type somebody legitimately
// wrote.
const MaxDepth = 256

// Reason is why a type is not cleanly serializable at the site it was asked
// about.
type Reason int

// The two halves of the gob escape hatch, named once because they are referred
// to more than once and a repeated literal is a divergence waiting to happen.
const (
	gobEncodeMethod = "GobEncode"
	gobDecodeMethod = "GobDecode"
)

// memoSep separates a memo key's two components. It is a byte no type string can
// contain, so no pair of distinct (type, site) keys can collide by concatenation.
const memoSep = "\x00"

// Finding is one reason a type is not cleanly serializable, and where inside the
// type it sits.
//
// IT CARRIES A FIELD PATH AND NOT A SOURCE POSITION, which is the seam with the
// caller. This walk knows where inside the type the problem is — `.Inner.C` —
// and cannot know which declaration named that type. The caller knows that, and
// stamps the source position itself.
//
// DO NOT CONFUSE Finding.Path WITH Diagnostic.Path. They share a name and mean
// different things: this one is a FIELD CHAIN inside a type, while
// Diagnostic.Path is a FILESYSTEM PATH naming a source file. A consumer
// rendering one of these against a source location composes the two.
type Finding struct {
	Path   string
	Type   string
	Reason Reason
}

// Deriver answers serializability questions and remembers what it answered.
//
// A CONSUMER HOLDS ONE FOR A WHOLE GENERATION RUN, which is what makes the memo
// key's shape load-bearing rather than an implementation detail.
type Deriver struct {
	memo map[string][]Finding

	// ceiling is the frame bound this Deriver enforces, MaxDepth in every
	// production path. It is a field rather than a bare constant reference so
	// that the module's own tests can measure a fixture's NATURAL depth with the
	// bound lifted — which is what proves an over-ceiling fixture genuinely
	// exceeds the bound rather than merely reaching it. It is unexported, so no
	// consumer can weaken the ceiling.
	ceiling int
}

// NewDeriver returns a Deriver with an empty memo.
func NewDeriver() *Deriver {
	return &Deriver{memo: map[string][]Finding{}, ceiling: MaxDepth}
}

// Serializable reports every reason typ is not cleanly serializable at site,
// with a path to each problem inside the type. An empty result means the type is
// clean AT THAT SITE.
func (d *Deriver) Serializable(typ types.Type, site Site) []Finding {
	return d.walk(typ, site, 1)
}

// walk is the memoized entry to the family split.
//
// THE MEMO IS KEYED BY TYPE STRING AND SITE TOGETHER, and dropping the site is
// not a micro-optimization — it is a defect with a runtime consequence. Because
// one Deriver serves a whole run, a site-blind key returns the concrete answer
// to a later interface question, ReasonNeedsRegistration never reaches the
// generator, the gob.Register call is not emitted, and the program fails in
// production with "type not registered for interface" — the exact class this
// derivation exists to prevent.
//
// THE KEY IS SEEDED WITH A NIL ENTRY BEFORE RECURSING, and that is what makes a
// self-referential type terminate at all: the recursion re-enters the same key,
// hits the seed, and unwinds. Without it the walk does not merely run slowly, it
// exhausts the stack.
//
// Paths in a memoized result are relative to the type that was walked. Callers
// that descend into a field prefix them, which is what keeps one cached answer
// correct at every place the type appears.
func (d *Deriver) walk(typ types.Type, site Site, depth int) []Finding {
	// THE CEILING IS CHECKED BEFORE THE MEMO KEY IS STORED, deliberately. A
	// depth-limited answer is a property of the WALK, not of the type, so
	// memoizing it would poison the memo for a later query about the same type
	// reached at a shallower depth.
	if depth > d.ceiling {
		return []Finding{{Type: typ.String(), Reason: ReasonDepthExceeded}}
	}

	key := typ.String() + memoSep + strconv.Itoa(int(site))
	if found, ok := d.memo[key]; ok {
		return found
	}

	d.memo[key] = nil

	var found []Finding

	switch shape := typ.(type) {
	case *types.Alias:
		found = d.walk(types.Unalias(shape), site, depth+1)
	case *types.Pointer:
		found = d.walk(shape.Elem(), site, depth+1)
	case *types.Interface:
		found = []Finding{{Type: typ.String(), Reason: ReasonNeedsRegistration}}
	case *types.Named:
		found = d.named(shape, site, depth)
	default:
		// THE COMPOSITE'S OWN QUESTION, asked before descending. A map, an
		// array or a slice that is ITSELF the value in an interface slot needs
		// registration in its own right; descending into its elements answers a
		// different question and leaves this one unasked, which is how a
		// map[string]int state entry came back clean while gob refused it.
		if site == SiteInterface && needsRegistration(typ) {
			found = append(found, Finding{Type: typ.String(), Reason: ReasonNeedsRegistration})
		}

		found = append(found, d.concrete(typ, depth)...)
	}

	d.memo[key] = found

	return found
}

// named applies the two INDEPENDENT axes to a named type, in an order that is
// the whole of the step.
//
// REGISTRATION IS DECIDED FIRST, AND THE HATCH ONLY AFTERWARDS. The tempting
// shape — an early `if hasGobCodec(t) { return nil }` before the site check —
// compiles, reads well, and ships the runtime failure this derivation exists to
// prevent: a hatch-carrying type fails in interface position exactly as an
// ordinary named type does until it is registered, because registration names
// the concrete type on the wire and is orthogonal to who produced the bytes.
//
// WHAT THE HATCH DOES SUPPRESS is the STRUCTURAL walk of the type it sits on: a
// type that supplies its own GobEncode owns its bytes, so a chan field beneath
// it is no longer a drop. That is the only thing it suppresses.
func (d *Deriver) named(typ *types.Named, site Site, depth int) []Finding {
	var found []Finding

	if site == SiteInterface {
		found = append(found, Finding{Type: typ.String(), Reason: ReasonNeedsRegistration})
	}

	if hasGobCodec(typ) {
		return found
	}

	return append(found, d.concrete(typ.Underlying(), depth)...)
}

// concrete handles the shapes whose answer does not depend on the site.
//
// It takes no Site because none of these arms consults one: a struct's FIELD is
// not in an interface slot merely because the struct it belongs to is, and a
// field that IS interface-typed reaches the Interface arm of walk on its own
// account. Descending at SiteConcrete is therefore the accurate statement, not a
// simplification.
func (d *Deriver) concrete(typ types.Type, depth int) []Finding {
	switch shape := typ.(type) {
	case *types.Struct:
		return d.structFields(shape, depth)
	case *types.Slice:
		return prefixPath(d.walk(shape.Elem(), SiteConcrete, depth+1), "[]")
	case *types.Array:
		return prefixPath(d.walk(shape.Elem(), SiteConcrete, depth+1), "[]")
	case *types.Map:
		keys := prefixPath(d.walk(shape.Key(), SiteConcrete, depth+1), "[key]")

		return append(keys, prefixPath(d.walk(shape.Elem(), SiteConcrete, depth+1), "[value]")...)
	case *types.Chan, *types.Signature:
		return []Finding{{Type: typ.String(), Reason: ReasonSilentDrop}}
	default:
		return nil
	}
}

// needsRegistration reports whether a value of this type, placed DIRECTLY in an
// interface slot, must be registered with the codec before it can cross.
//
// THE SET IS CENSUSED, NOT REASONED. gob carries exactly three things through an
// interface slot with no registration: a basic type, a pointer to one, and a
// slice whose element is basic. It refuses everything else — every map, every
// array EVEN OF A BASIC ELEMENT, nested slices, slices of named elements, and
// unnamed structs. The array is the row a reasoned rule gets wrong: []int is
// carried and [3]int is not, so "the element is basic" is the wrong predicate.
//
// A POINTER NEEDS NO ARM HERE because walk resolves *T by recursing into T at the
// same site, so a pointer never reaches this function.
//
// CHAN AND FUNC ARE EXEMPT DELIBERATELY. concrete already refuses them as a
// silent drop, which is the stronger statement; a second reason to refuse the
// same type is noise for whoever composes the diagnostic.
//
// It takes no receiver because it reads nothing from the Deriver.
func needsRegistration(typ types.Type) bool {
	switch shape := typ.(type) {
	case *types.Basic:
		return false
	case *types.Chan, *types.Signature:
		return false
	case *types.Slice:
		_, basic := shape.Elem().(*types.Basic)

		return !basic
	default:
		return true
	}
}

// structFields walks only the EXPORTED fields, because those are the only ones
// the codec carries, and reports a struct that has none — such a struct is not
// partially encodable, it is refused outright by the codec.
func (d *Deriver) structFields(typ *types.Struct, depth int) []Finding {
	var (
		found    []Finding
		exported int
	)

	for i := range typ.NumFields() {
		field := typ.Field(i)
		if !field.Exported() {
			continue
		}

		exported++

		found = append(found, prefixPath(d.walk(field.Type(), SiteConcrete, depth+1), "."+field.Name())...)
	}

	if exported == 0 {
		return append(found, Finding{Type: typ.String(), Reason: ReasonNoExportedFields})
	}

	return found
}

// prefixPath returns a COPY of found with prefix prepended to every path.
//
// The copy is not incidental: the slice handed in may be a memoized answer
// shared by every place the type appears, and prefixing it in place would
// rewrite that shared answer with one caller's path.
func prefixPath(found []Finding, prefix string) []Finding {
	if len(found) == 0 {
		return nil
	}

	out := make([]Finding, len(found))
	for i, one := range found {
		out[i] = one
		out[i].Path = prefix + one.Path
	}

	return out
}

// hasGobCodec reports whether a type supplies BOTH halves of the gob escape
// hatch.
//
// BOTH HALVES ARE REQUIRED. A type carrying GobEncode alone can be written and
// never read back, so treating it as hatched would suppress the structural walk
// on a type nothing can decode.
//
// THE POINTER'S METHOD SET IS THE ONE THAT ANSWERS, because the conventional
// split puts GobEncode on the value and GobDecode on the pointer: the pointer
// set contains both, the value set only one. The question is asked through
// Carries rather than re-derived here, so there is one implementation of the
// receiver split in this module.
func hasGobCodec(typ types.Type) bool {
	encoder, decoder := gobCodecs()

	_, encodes := Carries(typ, encoder)
	_, decodes := Carries(typ, decoder)

	return encodes && decodes
}

// gobCodecs builds the two codec interfaces once.
//
// They are constructed rather than resolved through a loaded package because the
// derivation must answer for any types.Type a caller hands it, including one
// from a package set that never imported encoding/gob.
var gobCodecs = sync.OnceValues(func() (*types.Interface, *types.Interface) {
	bytes := types.NewSlice(types.Typ[types.Byte])
	errorType := types.Universe.Lookup("error").Type()

	encode := types.NewSignatureType(nil, nil, nil, nil, types.NewTuple(
		types.NewVar(token.NoPos, nil, "", bytes),
		types.NewVar(token.NoPos, nil, "", errorType),
	), false)

	decode := types.NewSignatureType(nil, nil, nil, types.NewTuple(
		types.NewVar(token.NoPos, nil, "", bytes),
	), types.NewTuple(
		types.NewVar(token.NoPos, nil, "", errorType),
	), false)

	encoder := types.NewInterfaceType([]*types.Func{
		types.NewFunc(token.NoPos, nil, gobEncodeMethod, encode),
	}, nil)
	encoder.Complete()

	decoder := types.NewInterfaceType([]*types.Func{
		types.NewFunc(token.NoPos, nil, gobDecodeMethod, decode),
	}, nil)
	decoder.Complete()

	return encoder, decoder
})
