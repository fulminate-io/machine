// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"slices"
	"strings"

	"github.com/whitaker-io/machine/lang/ast"
)

// nameSep separates the parts of a namespaced node or handle name.
const nameSep = "."

// lowerFile lowers every flow in a file, inlining subflows and ordering the
// result.
//
// IT TAKES THE BOUNDARY FACTS AND THE DEPENDENCY SET because neither is
// derivable from one flow. The boundary is lang/analysis's export, supplied by
// the driver; an ABSENT entry means no fact was exported, which is refused rather
// than read as an empty output set.
func lowerFile(programs []*Program, boundary map[string]Boundary, cfg Config) ([]*Plan, []Diagnostic) {
	deps := make(map[string]*Program, len(programs))
	for _, p := range programs {
		deps[p.Name] = p
	}

	var (
		plans []*Plan
		diags []Diagnostic
	)
	for _, p := range programs {
		plan, flowDiags := lowerProgram(p, deps, boundary, cfg)
		plans = append(plans, plan)
		diags = append(diags, flowDiags...)
	}

	return plans, diags
}

// lowerProgram lowers one flow with its dependencies available.
func lowerProgram(p *Program, deps map[string]*Program, boundary map[string]Boundary, cfg Config) (*Plan, []Diagnostic) {
	l := newLowering(p, cfg)
	l.deps = deps
	l.boundary = boundary
	l.collectHandles(p, "")
	if p.OnError != nil {
		l.plan.OnError = p.OnError.Handler.Text
	}
	l.carryCheckpointCodecs(p)
	for _, n := range p.Nodes {
		l.node(n)
	}
	l.plan.Ops = l.ordered()

	return l.plan, l.diags
}

// collectHandles records the state handles a flow declares.
//
// THE NAME IS WHERE THE INSTANCE LIVES. A flow's own handles are
// <qualifier>.<flow>.<var>; an inlined subflow's carry the instance too, which is
// the whole reason two instances of one subflow do not collide in the runtime's
// single process-global namespace.
func (l *lowering) collectHandles(p *Program, instance string) {
	for _, v := range p.Vars {
		l.addHandle(HandleKey, instance, v.Name.Name, v.Type.Text)
	}
	for _, f := range p.State {
		l.addHandle(HandleCell, instance, f.Name.Name, f.Type.Text)
	}
}

// addHandle records one handle under the caller's namespace.
func (l *lowering) addHandle(kind HandleKind, instance, name, spelling string) {
	parts := []string{l.cfg.Qualifier, l.program.Name}
	if instance != "" {
		parts = append(parts, instance)
	}
	parts = append(parts, name)

	variable := varOf(name)
	if instance != "" {
		variable = varOf(instance) + "_" + variable
	}
	l.plan.Handles = append(l.plan.Handles, Handle{
		Var: variable, Name: strings.Join(parts, nameSep), Kind: kind, Type: spelling,
	})
}

// use inlines a referenced flow's statements into this plan.
//
// INLINING RATHER THAN A GENERATED FUNCTION PER SUBFLOW, because a function
// returning a multi-output subflow's flows would have to SPELL each output's Go
// type in its signature, which this package does not know. Inlining needs no type
// at all: every emitted call goes on inferring its types from the user-written Go
// references. It also gives two instances independent state handles for free.
func (l *lowering) use(n Node) {
	s, ok := n.Stmt.(ast.UseStmt)
	if !ok || len(s.Flow) != 1 {
		return
	}
	dep, resolved := l.deps[s.Flow[0].Name]
	if !resolved {
		l.diagf(n.Start, n.Stop, "the use of %q names a flow this file does not declare", s.Flow[0].Name)

		return
	}
	if !l.bindingsAgree(n, s, dep) {
		return
	}
	l.inline(n, s, dep)
}

// bindingsAgree resolves the use statement's identifiers against the boundary
// lang/analysis exported, and reconciles that boundary with this module's own
// graph.
//
// THE BOUNDARY IS THE AUTHORITY AND THIS MODULE IS NEVER A SECOND OPINION. Where
// the two views differ the answer is a loud refusal naming both, never a quiet
// preference for either: preferring the local view is the drift the single-owner
// ruling exists to prevent, and silently dropping a name the boundary exports
// would generate a program missing an output its consumer was told it could bind.
func (l *lowering) bindingsAgree(n Node, s ast.UseStmt, dep *Program) bool {
	fact, exported := l.boundary[dep.Name]
	if !exported {
		l.diagf(n.Start, n.Stop,
			"no output boundary was exported for %q, so its bindable outputs are unknown; "+
				"an absent fact is not an empty one", dep.Name)

		return false
	}
	if !l.reconcile(n, dep, fact) {
		return false
	}

	return l.resolveBindings(s, fact)
}

// reconcile refuses a disagreement between the exported boundary and this
// module's own graph, in BOTH directions.
func (l *lowering) reconcile(n Node, dep *Program, fact Boundary) bool {
	produced := producedNames(dep)
	agreed := true
	for _, name := range produced {
		if !slices.Contains(fact.Outputs, name) && !declaredOutput(dep, name) {
			continue
		}
		if !slices.Contains(fact.Outputs, name) {
			l.diagf(n.Start, n.Stop, boundaryDisagreement, dep.Name, name, "this generator's graph", fact.Outputs)
			agreed = false
		}
	}
	for _, name := range fact.Outputs {
		if !slices.Contains(produced, name) {
			l.diagf(n.Start, n.Stop, boundaryDisagreement, dep.Name, name, "the exported boundary", produced)
			agreed = false
		}
	}

	return agreed
}

// boundaryDisagreement is the one message both reconciliation directions use, so
// neither can drift into being quieter than the other.
const boundaryDisagreement = "the output boundary for %q disagrees with this generator: %q is known to %s only. " +
	"Refusing rather than lowering on either view; the other view holds %v"

// resolveBindings checks each identifier names an exported output, once.
//
// THE IDENTIFIERS ARE A SET. Order is irrelevant, a SUBSET is legal, and there is
// NO count check — a count is meaningful only under an ordering the source never
// states. Positional binding is the retired semantic, and it is retired because
// it cross-binds silently: it wires `-> bad, ok` so bad receives ok's value with
// no diagnostic and no type error when the two share a type.
func (l *lowering) resolveBindings(s ast.UseStmt, fact Boundary) bool {
	seen := make(map[string]bool, len(s.Bindings))
	ok := true
	for _, binding := range s.Bindings {
		if seen[binding.Name] {
			l.diagf(binding.Pos(), binding.End(), "%q is bound twice by one use statement", binding.Name)
			ok = false

			continue
		}
		seen[binding.Name] = true
		if !slices.Contains(fact.Outputs, binding.Name) {
			l.diagf(binding.Pos(), binding.End(),
				"%q names no output of that flow; its outputs are %v", binding.Name, fact.Outputs)
			ok = false
		}
	}

	return ok
}

// inline re-enters the lowering over the dependency's nodes under the instance
// namespace.
func (l *lowering) inline(n Node, s ast.UseStmt, dep *Program) {
	instance := s.Instance.Name
	l.collectHandles(dep, instance)

	// THE SUBFLOW'S IMPLICIT INPUT BINDS TO THE USE STATEMENT'S DECLARING INPUT,
	// which is what connects the inlined body to the caller's graph at all.
	if len(n.Inputs) > 0 {
		if variable, bound := l.flowVar[n.Inputs[0]]; bound {
			l.flowVar[instance+nameSep+implicitInput] = variable
		}
	}
	for _, inner := range dep.Nodes {
		l.node(l.rename(inner, instance))
	}
	// The caller binds the dependency's outputs BY NAME, so each identifier takes
	// the flow the inlined body bound under the instance namespace.
	for _, binding := range s.Bindings {
		if variable, bound := l.flowVar[instance+nameSep+binding.Name]; bound {
			l.flowVar[binding.Name] = variable
		}
	}
}

// rename namespaces one inlined node and every name it references.
func (l *lowering) rename(n Node, instance string) Node {
	renamed := n
	renamed.Name = instance + nameSep + n.Name
	renamed.Inputs = prefixAll(n.Inputs, instance)
	renamed.Outputs = prefixAll(n.Outputs, instance)

	return renamed
}

// prefixAll namespaces a name list under an instance.
func prefixAll(names []string, instance string) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, instance+nameSep+name)
	}

	return out
}

// producedNames lists every name a flow's body publishes.
func producedNames(p *Program) []string {
	var out []string
	for _, n := range p.Nodes {
		out = append(out, n.Outputs...)
	}

	return out
}

// declaredOutput reports whether a flow's signature declares a name.
func declaredOutput(p *Program, name string) bool {
	if p.Signature == nil {
		return true
	}
	for _, o := range p.Signature.Outputs {
		if o.Name.Name == name {
			return true
		}
	}

	return false
}
