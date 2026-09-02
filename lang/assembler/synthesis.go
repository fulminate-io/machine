// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"strings"

	"github.com/whitaker-io/machine/lang/ast"
)

// synthesis holds the clone and duplicator functions one generated file needs.
//
// IT REPLACES THE PHASE-3 REFUSALS RATHER THAN RELAXING THEM. A tee and a var of
// a non-trivially-copyable type were refused because no duplicator or cloner
// could be derived without type structure; with the loader supplying that
// structure the derivation runs and the refusals are DELETED, message and all.
// Nothing is left as a dead branch.
type synthesis struct {
	deriver *cloneDeriver
	// duplicators maps a payload type spelling to the name of its Duplicator.
	duplicators map[string]string
	// cloners maps a var's type spelling to the name of its clone function.
	cloners map[string]string
	// order preserves emission order so generated output stays byte-stable.
	order []string
	diags []Diagnostic
}

// newSynthesis derives every clone and duplicator a program needs.
//
// WITHOUT A LOADED PACKAGE SET IT DERIVES NOTHING AND SAYS SO. That is not a
// silent degradation: a tee or a non-trivial var reaching this path with no types
// available is refused by name, because the alternative — a shallow copy — leaves
// two branches sharing backing memory with no diagnostic at all.
func newSynthesis(p *Program, typed *Types) *synthesis {
	local := ""
	if typed != nil {
		local = typed.pkgPath
	}
	s := &synthesis{
		deriver:     newCloneDeriver(local),
		duplicators: map[string]string{},
		cloners:     map[string]string{},
	}
	s.deriveVars(p, typed)
	s.deriveTees(p, typed)
	s.diags = append(s.diags, s.deriver.Diagnostics()...)

	return s
}

// deriveVars synthesizes the clone function every var's NewKey requires.
//
// machine.NewKey demands a cloner and PANICS without one, because Tee deep-copies
// the frame into both branches and the frame's clone walks the cloner map calling
// each. A trivially-copyable type still gets one — the identity function — so
// every key is constructed the same way rather than two ways.
//
// AN EXPLICIT `clone` OVERRIDE SHORT-CIRCUITS DERIVATION ENTIRELY. The override is
// used verbatim and no derivation is attempted, so an author is never blocked by a
// gap in the derivation.
func (s *synthesis) deriveVars(p *Program, typed *Types) {
	for _, v := range p.Vars {
		spelling := strings.TrimSpace(v.Type.Text)
		if v.Clone != nil {
			s.cloners[spelling] = strings.TrimSpace(v.Clone.Text)

			continue
		}
		if isTriviallyCopyableSpelling(spelling) {
			// A VALUE ASSIGNMENT ALREADY COPIES IT COMPLETELY, so the identity is
			// the correct deep copy and no type structure is needed to know that.
			// Deriving here anyway would make an int var depend on a loaded
			// package set for no gain.
			s.cloners[spelling] = ""

			continue
		}
		if typed == nil {
			s.diags = append(s.diags, diagnosticAt(v.Start, v.Stop,
				"the var %q is declared %q, which a value assignment does not fully copy, and no type "+
					"information is available to derive its clone from; a shallow copy would leave two tee "+
					"branches sharing the same memory", v.Name.Name, spelling))

			continue
		}
		name, ok := s.derive(spelling, typed, span(v.Start, v.Stop))
		if ok {
			s.cloners[spelling] = name
		}
	}
}

// triviallyCopyableSpellings are the Go type names a value assignment copies
// completely.
//
// THE JUDGEMENT IS SYNTACTIC AND DELIBERATELY NARROW, because it runs where no
// type structure is available. A spelling this set does not name might be an
// alias for one that is, and treating it as non-trivial is the conservative
// direction: the other error admits something that is not actually copyable,
// which is the silent-sharing defect.
var triviallyCopyableSpellings = map[string]bool{
	"bool": true, "string": true, "byte": true, "rune": true, "error": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"float32": true, "float64": true, "complex64": true, "complex128": true,
}

// isTriviallyCopyableSpelling reports whether a written type name is one a value
// assignment fully copies.
func isTriviallyCopyableSpelling(spelling string) bool {
	return triviallyCopyableSpellings[strings.TrimSpace(spelling)]
}

// deriveTees synthesizes the Duplicator every tee needs.
//
// A TEE CARRIES NO GO REFERENCE AT ALL — the language says a tee is route-only and
// applies no function — so the duplicator is entirely the generator's, which is
// why it could not exist before type resolution did.
func (s *synthesis) deriveTees(p *Program, typed *Types) {
	for _, n := range p.Nodes {
		if n.Kind != KindTee {
			continue
		}
		spelling, ok := p.InputTypes[n.Name]
		if !ok || strings.TrimSpace(spelling) == "" {
			s.diags = append(s.diags, diagnosticAt(n.Start, n.Stop,
				"the tee %q needs its payload type to synthesize a duplicator, "+
					"which no type information supplies", n.Name))

			continue
		}
		if _, done := s.duplicators[spelling]; done {
			continue
		}
		if typed == nil {
			s.diags = append(s.diags, diagnosticAt(n.Start, n.Stop,
				"the tee %q needs a duplicator derived from %q's structure, and no type information is "+
					"available; a shallow copy would leave both branches sharing the same memory",
				n.Name, spelling))

			continue
		}
		clone, derived := s.derive(spelling, typed, span(n.Start, n.Stop))
		if !derived {
			continue
		}
		s.duplicators[spelling] = duplicatorName(spelling)
		s.order = append(s.order, spelling)
		s.emitDuplicator(spelling, clone)
	}
}

// derive resolves a spelling and derives its clone function.
func (s *synthesis) derive(spelling string, typed *Types, at posRange) (string, bool) {
	resolved, diag := typed.PayloadOf(spelling, at.start)
	if diag != nil {
		s.diags = append(s.diags, *diag)

		return "", false
	}

	return s.deriver.derive(resolved, at, 0)
}

// emitDuplicator records the Duplicator wrapping a derived clone.
//
// A DUPLICATOR IS THE DERIVED CLONE CALLED TWICE. Returning the input as one half
// would leave that branch aliasing the original, which is the shallow-copy defect
// in its least visible form: one branch is isolated and the other is not.
func (s *synthesis) emitDuplicator(spelling, clone string) {
	body := "\treturn " + cloneCall(clone, "d") + ", " + cloneCall(clone, "d")
	s.deriver.bodies[duplicatorKey(spelling)] =
		"func " + duplicatorName(spelling) + "(d " + spelling + ") (" + spelling + ", " + spelling + ") {\n" +
			body + "\n}\n"
	s.deriver.order = append(s.deriver.order, duplicatorKey(spelling))
}

// cloneCall renders a call to a derived clone, or a plain copy when the type
// needed none.
func cloneCall(clone, arg string) string {
	if clone == "" {
		return arg
	}

	return clone + "(" + arg + ")"
}

// duplicatorName renders the Duplicator's function name for a payload type.
func duplicatorName(spelling string) string {
	return "duplicateFlow_" + cloneFuncName(spelling)[len("cloneFlow_"):]
}

// duplicatorKey is the emission-order key for a duplicator.
func duplicatorKey(spelling string) string { return "duplicator:" + spelling }

// Functions renders every synthesized function.
func (s *synthesis) Functions() string { return s.deriver.Functions() }

// ClonerFor names the clone function a var's NewKey takes, or the identity.
func (s *synthesis) ClonerFor(spelling string) string {
	return s.cloners[strings.TrimSpace(spelling)]
}

// DuplicatorFor names the Duplicator a tee of this payload type takes.
func (s *synthesis) DuplicatorFor(spelling string) string {
	return s.duplicators[strings.TrimSpace(spelling)]
}

// Diagnostics returns the refusals.
func (s *synthesis) Diagnostics() []Diagnostic { return s.diags }

// span builds a posRange from two positions.
func span(start, stop ast.Position) posRange {
	return posRange{start: start, stop: stop}
}
