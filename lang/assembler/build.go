// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import "github.com/whitaker-io/machine/lang/ast"

// derivedSep separates a source-written name from a tooling-derived suffix.
//
// It is `#`, which the grammar admits in no identifier, so a derived name can
// never collide with one an author wrote.
const derivedSep = "#"

// The preamble helper names the emitter reserves. A .flow file declaring a
// verbatim func under either name would collide with the helper block every
// generated file carries, so the collision is refused at generation time rather
// than discovered as a redeclaration error in generated Go.
const (
	readsHelper  = "flowReadsOf"
	writesHelper = "flowWritesOf"
)

// implicitInput is the name a SIGNATURED flow's body uses for the datum arriving
// at its boundary.
//
// A signature declares the input's TYPE and no name, so the name is the
// language's rather than the author's. lang/analysis holds the same constant
// (symbols.go) and tables the implicit input against the signature; this package
// resolves references to it the same way, which is what lets a signatured flow's
// first statement consume something no statement in its body produces.
//
// A flow with NO signature has no implicit input, and a body referencing `in`
// there is an ordinary unknown name.
const implicitInput = "in"

// buildFile builds every flow in a parsed file and applies the refusals that
// need to see MORE THAN ONE FLOW.
//
// Two of the refusal set's members cannot be decided from a single flow: a
// recursive `use` chain is a property of the call graph between flows, and a
// reserved-helper collision is a property of the file's func declarations. So
// they live here rather than in buildProgram, which sees one flow at a time.
func buildFile(file *ast.File) ([]*Program, []Diagnostic) {
	var (
		programs []*Program
		diags    []Diagnostic
	)
	for _, decl := range file.Decls {
		flow, ok := decl.(ast.FlowDecl)
		if !ok {
			continue
		}
		program, flowDiags := buildProgram(flow)
		programs = append(programs, program)
		diags = append(diags, flowDiags...)
	}
	diags = append(diags, refuseReservedHelperNames(file)...)
	diags = append(diags, refuseRecursiveUse(file, programs)...)

	return programs, diags
}

// refuseReservedHelperNames refuses a verbatim func declaring a preamble helper
// name.
//
// The emitter writes flowReadsOf and flowWritesOf into every generated file, so
// a source-declared func under either name produces Go that does not compile.
// Refusing here names the .flow line that wrote it; letting it through would
// surface as a redeclaration error against a generated line the author never
// wrote.
func refuseReservedHelperNames(file *ast.File) []Diagnostic {
	var diags []Diagnostic
	for _, decl := range file.Decls {
		fn, ok := decl.(ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name.Name == readsHelper || fn.Name.Name == writesHelper {
			diags = append(diags, diagnosticAt(fn.Start, fn.Stop,
				"a func named %q collides with the helper every generated file declares; rename it",
				fn.Name.Name))
		}
	}

	return diags
}

// refuseRecursiveUse refuses a `use` chain that would not terminate when
// inlined.
//
// Inlining substitutes a referenced flow's statements into its caller, so a
// cycle in the use graph is a generator that does not terminate. It is refused
// with the cycle named rather than discovered as a hang.
func refuseRecursiveUse(file *ast.File, programs []*Program) []Diagnostic {
	uses := useGraph(programs)
	var diags []Diagnostic
	for _, program := range programs {
		if path, cyclic := reachesItself(program.Name, uses); cyclic {
			diags = append(diags, diagnosticAt(program.Start, program.Stop,
				"the flow %q uses itself through %s; inlining would not terminate",
				program.Name, path))
		}
	}
	_ = file

	return diags
}

// useGraph maps each flow to the flows it embeds with a `use` statement.
func useGraph(programs []*Program) map[string][]string {
	uses := make(map[string][]string, len(programs))
	for _, program := range programs {
		for _, n := range program.Nodes {
			if n.Kind != KindUse {
				continue
			}
			if s, ok := n.Stmt.(ast.UseStmt); ok && len(s.Flow) == 1 {
				uses[program.Name] = append(uses[program.Name], s.Flow[0].Name)
			}
		}
	}

	return uses
}

// reachesItself walks the use graph from one flow, reporting the path back to it
// when there is one.
func reachesItself(start string, uses map[string][]string) (string, bool) {
	seen := map[string]bool{}
	var walk func(at string, path []string) (string, bool)
	walk = func(at string, path []string) (string, bool) {
		for _, next := range uses[at] {
			if next == start {
				return renderPath(append(path, next)), true
			}
			if seen[next] {
				continue
			}
			seen[next] = true
			if rendered, cyclic := walk(next, append(path, next)); cyclic {
				return rendered, true
			}
		}

		return "", false
	}

	return walk(start, nil)
}

// renderPath renders a use chain for a diagnostic.
func renderPath(path []string) string {
	out := ""
	for i, name := range path {
		if i > 0 {
			out += " -> "
		}
		out += name
	}

	return out
}

// builder carries one flow's build state.
//
// It exists because the walk needs four collections threaded through a dozen
// small functions, and the module's linter caps a function at five arguments. It
// is per-call and never shared.
type builder struct {
	program *Program
	diags   []Diagnostic
	// producer maps a published output name to the node that publishes it.
	producer map[string]string
	// labels holds the loop labels declared in this flow.
	labels map[string]ast.Position
	// declared holds every node name, to catch a redeclaration.
	declared map[string]ast.Position
}

// buildProgram turns one flow declaration into a validated node graph.
//
// TWO PASSES OVER ONE SLICE. The first collects nodes, the names they publish
// and the loop labels; the second resolves every consumed name against that
// collection. Two passes rather than one because a flow may consume a loop label
// declared LATER in the source — that is the language's one backward reference —
// so no single forward pass can resolve every name.
//
// It returns the graph it managed to build ALONGSIDE the diagnostics, never
// instead of them: a caller reporting problems wants to see the shape of what
// was understood, and the driver decides whether a partial graph is usable.
func buildProgram(flow ast.FlowDecl) (*Program, []Diagnostic) {
	b := &builder{
		program:  newProgram(flow),
		producer: make(map[string]string, len(flow.Body)),
		labels:   make(map[string]ast.Position, len(flow.Body)),
		declared: make(map[string]ast.Position, len(flow.Body)),
	}
	for _, stmt := range flow.Body {
		b.collect(stmt)
	}
	for i := range b.program.Nodes {
		b.resolve(&b.program.Nodes[i])
	}
	b.reportUnfedLabels()
	b.reportUnconsumedOutputs()

	return b.program, b.diags
}

// newProgram lifts a flow declaration's own fields into the IR.
func newProgram(flow ast.FlowDecl) *Program {
	program := &Program{
		Name:      flow.Name.Name,
		Note:      flow.Note,
		Signature: flow.Signature,
		Vars:      flow.Vars,
		OnError:   flow.OnError,
		Nodes:     make([]Node, 0, len(flow.Body)),
		Start:     flow.Start,
		Stop:      flow.Stop,
	}
	if flow.State != nil {
		program.State = flow.State.Fields
	}

	return program
}

// diagf records a positioned refusal.
func (b *builder) diagf(start, end ast.Position, format string, args ...any) {
	b.diags = append(b.diags, diagnosticAt(start, end, format, args...))
}

// collect walks one statement into the graph.
//
// THE SWITCH IS OVER VALUE TYPES, NOT POINTERS. lang/ast declares every node's
// interface methods on a VALUE receiver, so `case *ast.SourceStmt:` compiles
// without complaint and matches nothing, forever — an engine written that way
// walks every tree, emits no diagnostics and passes every clean-corpus test. A
// landed CRITICAL corpus check enforces this shape.
//
// THE DEFAULT ARM IS A DIAGNOSTIC AND NOT A SKIP. The statement set is closed
// today; a shape added to the grammar without a case here must be a loud refusal
// rather than a statement that silently leaves the graph.
func (b *builder) collect(stmt ast.Stmt) {
	switch s := stmt.(type) {
	case ast.SourceStmt:
		b.addNode(node(s, KindSource, s.Name.Name, nil, []string{s.Name.Name}, s.Clauses))
	case ast.TransformStmt:
		b.addNode(node(s, KindTransform, s.Name.Name, fromNames(s.Clauses), []string{s.Name.Name}, s.Clauses))
	case ast.BranchStmt:
		outs := []string{s.TrueTarget.Name, s.FalseTarget.Name}
		b.addNode(node(s, KindBranch, s.Name.Name, fromNames(s.Clauses), outs, s.Clauses))
	case ast.SwitchStmt:
		b.addNode(node(s, KindSwitch, s.Name.Name, fromNames(s.Clauses), switchOutputs(s), s.Clauses))
	case ast.TeeStmt:
		b.addNode(node(s, KindTee, s.Name.Name, fromNames(s.Clauses), identNames(s.Targets), s.Clauses))
	case ast.SinkStmt:
		b.addNode(node(s, KindSink, s.Name.Name, fromNames(s.Clauses), nil, s.Clauses))
	case ast.UseStmt:
		b.refuseDottedFlowRef(s)
		b.addNode(node(s, KindUse, s.Instance.Name, fromNames(s.Clauses), identNames(s.Bindings), s.Clauses))
	default:
		b.collectFlowControl(stmt)
	}
}

// collectFlowControl walks the shapes that carry no Clauses and no Name.
//
// It is split from collect only because the module's linter caps a function at
// twenty statements; the two together are one dispatch and the default arm below
// is the dispatch's mandatory refusal.
func (b *builder) collectFlowControl(stmt ast.Stmt) {
	switch s := stmt.(type) {
	case ast.DropStmt:
		name := s.Input.Name + derivedSep + "drop"
		b.addNode(node(s, KindDrop, name, []string{s.Input.Name}, nil, ast.Clauses{}))
	case ast.SendStmt:
		name := s.Source.Name + derivedSep + "send"
		n := node(s, KindSend, name, []string{s.Source.Name}, nil, ast.Clauses{})
		b.addNode(n)
	case ast.LoopStmt:
		// A LOOP IS A LABEL, NOT A NODE. It applies nothing and carries no
		// clauses; what it names is a destination a send can route back into.
		b.declareLabel(s.Name)
	case ast.BadStmt:
		b.diagf(s.Start, s.Stop, "this statement could not be parsed, so it cannot be assembled")
	default:
		b.diagf(stmt.Pos(), stmt.End(), "statement shape %T has no graph representation", stmt)
	}
}

// node builds one IR node from a statement and its resolved name lists.
func node(stmt ast.Stmt, kind NodeKind, name string, in, out []string, clauses ast.Clauses) Node {
	return Node{
		Name:    name,
		Kind:    kind,
		Stmt:    stmt,
		Inputs:  in,
		Outputs: out,
		Clauses: clauses,
		Start:   stmt.Pos(),
		Stop:    stmt.End(),
	}
}

// addNode records a node, its published names, and refuses a redeclaration.
func (b *builder) addNode(n Node) {
	if at, seen := b.declared[n.Name]; seen {
		b.diagf(n.Start, n.Stop, "%q is already declared at %s; node names are unique within a flow", n.Name, at)

		return
	}
	b.declared[n.Name] = n.Start
	for _, out := range n.Outputs {
		if by, taken := b.producer[out]; taken {
			b.diagf(n.Start, n.Stop, "%q is already produced by %q; two statements cannot publish one name", out, by)

			continue
		}
		b.producer[out] = n.Name
	}
	b.program.Nodes = append(b.program.Nodes, n)
}

// declareLabel records a loop label and refuses a duplicate.
func (b *builder) declareLabel(name ast.Ident) {
	if at, seen := b.labels[name.Name]; seen {
		b.diagf(name.Pos(), name.End(), "the loop label %q is already declared at %s", name.Name, at)

		return
	}
	b.labels[name.Name] = name.NamePos
}

// resolve turns one node's consumed names into edges.
//
// THE FIRST NAME IS THE DECLARING INPUT and the rest are merges, which is not
// cosmetic: the runtime constructs a node FROM its declaring input and every
// other inbound name arrives as a Send into it, and a Send's target must already
// have a consumer. So the order the author wrote decides what is constructed and
// what is routed.
func (b *builder) resolve(n *Node) {
	for i, in := range n.Inputs {
		from, ok := b.producer[in]
		if !ok {
			b.resolveNonProducer(n, in, i)

			continue
		}
		b.program.Edges = append(b.program.Edges, Edge{Output: in, From: from, To: n.Name})
	}
}

// resolveNonProducer handles a consumed name no statement publishes: a loop
// label, a node name used as a send target, or a genuine mistake.
func (b *builder) resolveNonProducer(n *Node, in string, position int) {
	if _, isLabel := b.labels[in]; isLabel {
		// A LABEL RESOLVES TO THE INBOUND EDGE of whatever consumes it, so the
		// edge is recorded against the label itself and the ordering pass reads
		// it as a merge rather than a construction.
		b.program.Edges = append(b.program.Edges, Edge{Output: in, From: in, To: n.Name})

		return
	}
	if _, isNode := b.declared[in]; isNode {
		b.program.Edges = append(b.program.Edges, Edge{Output: in, From: in, To: n.Name})

		return
	}
	if in == implicitInput && b.program.Signature != nil {
		// THE SIGNATURE PRODUCES IT. A signatured flow's boundary datum has a
		// type and no author-given name, so the language names it; the edge is
		// recorded against the name itself, as a loop label's is.
		b.program.Edges = append(b.program.Edges, Edge{Output: in, From: in, To: n.Name})

		return
	}
	if position == 0 {
		b.diagf(n.Start, n.Stop, "%q consumes %q, which no statement in this flow produces", n.Name, in)

		return
	}
	b.diagf(n.Start, n.Stop, "%q merges %q, which no statement in this flow produces", n.Name, in)
}

// refuseDottedFlowRef refuses a `use` naming a flow through a dotted path.
//
// A dotted reference names a flow in ANOTHER MODULE, and resolving one is
// lang/loader's surface rather than this package's. It is refused with the
// reference named rather than resolved through some path invented here, because
// a second resolution mechanism is how two answers to one question appear.
func (b *builder) refuseDottedFlowRef(s ast.UseStmt) {
	if len(s.Flow) <= 1 {
		return
	}
	ref := renderPath(identNames(s.Flow))
	b.diagf(s.Start, s.Stop,
		"the use of %q names a flow in another module; cross-module flow references are not resolved here", ref)
}

// reportUnconsumedOutputs refuses a published name nothing reads.
//
// THE RUNTIME ALREADY REFUSES THIS, at Start, with "the flow produced by %q is
// never consumed". Refusing at generation time reports the same fact against the
// .flow line that wrote it instead of against a node name at run time.
//
// A NAME THE FLOW'S SIGNATURE DECLARES IS EXEMPT: those are the flow's boundary,
// consumed by whatever embeds it rather than inside the body. Without that
// exemption every exported flow would be refused for doing exactly what its
// header says.
func (b *builder) reportUnconsumedOutputs() {
	consumed := make(map[string]bool, len(b.program.Edges))
	for _, e := range b.program.Edges {
		consumed[e.Output] = true
	}
	declared := b.signatureOutputs()
	for _, n := range b.program.Nodes {
		for _, out := range n.Outputs {
			if consumed[out] || declared[out] {
				continue
			}
			b.diagf(n.Start, n.Stop, "%q produces %q, which nothing in this flow consumes", n.Name, out)
		}
	}
}

// signatureOutputs lists the names the flow's header declares as its boundary.
func (b *builder) signatureOutputs() map[string]bool {
	out := map[string]bool{}
	if b.program.Signature == nil {
		return out
	}
	for _, o := range b.program.Signature.Outputs {
		out[o.Name.Name] = true
	}

	return out
}

// reportUnfedLabels refuses a loop label nothing ever sends into.
//
// A label with no sender is a destination that never receives, so the loop the
// author wrote does not exist. It is a diagnostic rather than a silent no-op
// because the flow reads as if it loops.
func (b *builder) reportUnfedLabels() {
	fed := make(map[string]bool, len(b.labels))
	for _, n := range b.program.Nodes {
		if n.Kind != KindSend {
			continue
		}
		if target, ok := sendTarget(n.Stmt); ok {
			fed[target] = true
		}
	}
	for label, at := range b.labels {
		if !fed[label] {
			b.diagf(at, at, "the loop label %q is never sent to, so nothing would ever feed it", label)
		}
	}
}

// sendTarget reads a send statement's target name.
func sendTarget(stmt ast.Stmt) (string, bool) {
	if s, ok := stmt.(ast.SendStmt); ok {
		return s.Target.Name, true
	}

	return "", false
}

// switchOutputs lists the names a switch publishes: one per arm target, plus the
// else target when the switch has one.
//
// THEY ARE THE ARM TARGETS AND NOT THE SWITCH'S OWN NAME. testdata's
// switch-with-else.flow routes to `billable`, `refundable` and `other` and the
// statements after it consume exactly those; nothing consumes the switch's name.
// A switch publishing its own name would leave every one of those from-references
// unresolvable.
func switchOutputs(s ast.SwitchStmt) []string {
	out := make([]string, 0, len(s.Arms)+1)
	for _, arm := range s.Arms {
		out = append(out, arm.Target.Name)
	}
	if s.Else != nil {
		out = append(out, s.Else.Name)
	}

	return out
}

// fromNames lists the outputs a statement's `from` clause consumes, in the order
// the source wrote them.
func fromNames(clauses ast.Clauses) []string {
	return identNames(clauses.From)
}

// identNames projects identifiers to their names, preserving order.
func identNames(idents []ast.Ident) []string {
	if len(idents) == 0 {
		return nil
	}
	out := make([]string, 0, len(idents))
	for _, ident := range idents {
		out = append(out, ident.Name)
	}

	return out
}
