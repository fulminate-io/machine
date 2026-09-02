// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import "strings"

// THE TWO REFUSALS IN THIS FILE ARE TEMPORARY BY DESIGN AND LOUD BY NECESSITY.
//
// A tee needs a machine.Duplicator and a var needs a clone function, and both
// have to be SYNTHESIZED from the payload's Go type structure. Until type
// resolution lands there is nothing to synthesize them from, and the only
// alternative to refusing is a SHALLOW COPY — which for a payload carrying a
// slice, a map or a pointer means two branches sharing the same backing memory,
// silently, with no diagnostic and no crash. That is the worst possible
// behaviour: it is correct-looking and wrong, and the damage shows up as data
// corruption somewhere else entirely.
//
// So they are positioned refusals naming the construct, and the type-resolution
// phase lifts them by supplying the synthesis rather than by relaxing the check.

// triviallyCopyable is the set of Go type spellings a value assignment copies
// completely.
//
// THE JUDGEMENT IS SYNTACTIC and deliberately narrow, because this file runs
// BEFORE any type resolution: a spelling this set does not name might be a type
// alias for one that is, and refusing it is the conservative direction. The
// alternative error — admitting something that is not actually copyable — is the
// silent-sharing defect above.
var triviallyCopyable = map[string]bool{
	"bool": true, "string": true, "byte": true, "rune": true, "error": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"float32": true, "float64": true, "complex64": true, "complex128": true,
}

// isTriviallyCopyable reports whether a value of this type spelling is fully
// copied by assignment.
func isTriviallyCopyable(spelling string) bool {
	return triviallyCopyable[strings.TrimSpace(spelling)]
}

// refuseUnsynthesizable reports the constructs EMISSION cannot complete.
//
// IT BELONGS TO EMISSION RATHER THAN TO LOWERING, deliberately. The lowering can
// produce a tee's chain perfectly well; what it cannot produce is the DUPLICATOR
// ARGUMENT that chain's calls need, and that is a property of writing the file
// rather than of planning it. Keeping the refusal here also keeps the lowering's
// own gates meaningful: the chain shape stays testable while the file that would
// use it is still refused.
//
// It runs over the whole program rather than per node so one flow reports every
// such construct at once, which lets an author fix them in one pass.
func refuseUnsynthesizable(p *Program) []Diagnostic {
	var diags []Diagnostic
	for _, n := range p.Nodes {
		if n.Kind != KindTee {
			continue
		}
		diags = append(diags, diagnosticAt(n.Start, n.Stop,
			"the tee %q needs a duplicator synthesized from its payload's type, which this generator "+
				"cannot derive yet; a shallow copy would leave both branches sharing the same memory", n.Name))
	}
	for _, v := range p.Vars {
		if isTriviallyCopyable(v.Type.Text) {
			continue
		}
		diags = append(diags, diagnosticAt(v.Start, v.Stop,
			"the var %q is declared %q, which a value assignment does not fully copy; its clone function must be "+
				"synthesized from the type's structure and this generator cannot derive one yet",
			v.Name.Name, strings.TrimSpace(v.Type.Text)))
	}

	return diags
}
