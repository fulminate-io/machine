// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"strings"

	"github.com/whitaker-io/machine/lang/ast"
)

// clauseDisposition says what the lowering does with one clause.
//
// THERE ARE EXACTLY TWO, AND NO IMPLICIT THIRD. A clause is LOWERED into
// something emitted, or HELD with a positioned not-yet-lowered diagnostic. A
// clause the partition does not classify at all reaches the dispatch's mandatory
// default arm and is refused by name. Silence is not a disposition — a clause
// that parses, reaches the graph and is dropped without a word is how
// `idempotent` went missing from an entire plan, and for that clause the cost is
// a SEMANTIC INVERSION rather than a missing feature: an unmarked node anchors on
// completion, a marked one on arrival, so dropping the marker reverses what the
// author wrote and the failure surfaces only during recovery.
type clauseDisposition int

const (
	clauseLowered clauseDisposition = iota
	clauseHeld
)

// clausePartition classifies every field of ast.Clauses.
//
// ITS PARITY WITH ast.Clauses IS GATED at run time by deriving the field names
// from ../ast/node.go, so a clause added to the AST without a row here reds
// rather than passing unnoticed.
//
// THE HELD SET IS EMPTY. Both members that were once held — Checkpoint and
// Idempotent — are lowered as of the ruling that gave the checkpoint clause its
// codec operand. The disposition is KEPT rather than deleted because a clause
// added to the grammar ahead of its lowering needs somewhere honest to sit, and
// the held code path is kept under test with a synthetic member so it cannot rot
// into a silent drop while unused.
var clausePartition = map[string]clauseDisposition{
	clauseFrom:       clauseLowered,
	clauseReads:      clauseLowered,
	clauseWrites:     clauseLowered,
	clauseOver:       clauseLowered,
	clauseOnError:    clauseLowered,
	clauseNote:       clauseLowered,
	clauseCheckpoint: clauseLowered,
	clauseIdempotent: clauseLowered,
}

// The clause names, spelled once. They are the FIELD NAMES of ast.Clauses, and
// the partition's parity with that struct is derived from the AST's own source at
// run time rather than trusted to these constants.
const (
	clauseFrom       = "From"
	clauseReads      = "Reads"
	clauseWrites     = "Writes"
	clauseOver       = "Over"
	clauseOnError    = "OnError"
	clauseNote       = "Note"
	clauseCheckpoint = "Checkpoint"
	clauseIdempotent = "Idempotent"
)

// lowering carries one flow's lowering state.
type lowering struct {
	program *Program
	plan    *Plan
	diags   []Diagnostic
	// flowVar maps an output name to the Go variable holding the Flow that
	// produces it.
	flowVar map[string]string
	// held is the clause set treated as HELD for this lowering. It is the
	// partition's held members, and a test may add one to keep the held path
	// exercised while the live set is empty.
	held map[string]bool
	// carried holds options a node receives from ANOTHER node's clause: the
	// codec-only option a completion-anchored checkpoint puts on its successor.
	carried map[string][]string
	// cfg carries the caller's decisions: the generated package and the handle
	// qualifier.
	cfg Config
	// deps are the other flows in the same file, available to inline.
	deps map[string]*Program
	// boundary holds the bindable-output fact lang/analysis exported per flow.
	// An ABSENT entry means no fact, which is refused rather than read as empty.
	boundary map[string]Boundary
}

// newLowering builds one flow's lowering state.
func newLowering(p *Program, cfg Config) *lowering {
	return &lowering{
		program:  p,
		cfg:      cfg,
		plan:     &Plan{Flow: p.Name, Ops: make([]Op, 0, len(p.Nodes))},
		flowVar:  make(map[string]string, len(p.Nodes)),
		held:     heldClauses(),
		carried:  map[string][]string{},
		deps:     map[string]*Program{},
		boundary: map[string]Boundary{},
	}
}

// carryCheckpointCodecs runs the cross-node pass.
//
// IT RUNS BEFORE ANY NODE IS LOWERED. A completion-anchored checkpoint puts a
// codec option on its SUCCESSOR, and a successor can be lowered before or after
// the node that requires it — a loop makes the second case ordinary. Collecting
// the carried options up front removes that ordering dependence entirely.
func (l *lowering) carryCheckpointCodecs(p *Program) {
	for _, n := range p.Nodes {
		for successor, options := range l.successorCodecOptions(n) {
			l.carried[successor] = append(l.carried[successor], options...)
		}
	}
}

// heldClauses lists the partition's held members.
func heldClauses() map[string]bool {
	held := map[string]bool{}
	for name, disposition := range clausePartition {
		if disposition == clauseHeld {
			held[name] = true
		}
	}

	return held
}

// diagf records a positioned refusal.
func (l *lowering) diagf(start, end ast.Position, format string, args ...any) {
	l.diags = append(l.diags, diagnosticAt(start, end, format, args...))
}

// node lowers one graph node onto the builder set.
func (l *lowering) node(n Node) {
	options := l.options(n)
	switch n.Kind {
	case KindSource:
		l.source(n, options)
	case KindTransform:
		l.emit(n, MethodMap, l.receiver(n), []string{varOf(n.Name)}, options)
	case KindBranch:
		l.emit(n, MethodIf, l.receiver(n), l.branchResults(n), options)
	case KindSwitch:
		l.switchChain(n, options)
	case KindTee:
		l.teeChain(n, options)
	default:
		l.flowControl(n, options)
	}
}

// flowControl lowers the shapes that terminate, route or declare nothing.
//
// It is split from node only because the module's linter caps a dispatch's
// cyclomatic complexity; the two together are one switch, and the default arm
// below is its mandatory refusal.
func (l *lowering) flowControl(n Node, options []string) {
	switch n.Kind {
	case KindSink:
		l.sink(n, options)
	case KindDrop:
		l.emit(n, MethodDrop, l.receiver(n), nil, options)
	case KindSend:
		l.send(n)
	case KindUse:
		l.use(n)
	case KindLoop, KindInvalid:
		// A loop is a label and declares nothing; invalid never reaches a built
		// graph.
		return
	default:
		l.diagf(n.Start, n.Stop, "the %s statement has no lowering onto the runtime's builder set", n.Kind)
	}
}

// source lowers a source: the one call that begins a chain, on the machine
// rather than on a flow.
func (l *lowering) source(n Node, options []string) {
	// THE INGEST CLOSURE IS KEPT AND EXPORTED. Machine.Source returns a Flow and
	// an Ingest[T]. Wire returns a per-flow struct carrying one typed field per
	// source, so a host can feed the flow programmatically rather than only
	// through the transport an `over` clause names.
	payload := l.payloadType(n)
	ingestVar := varOf(n.Name + derivedSep + "ingest")
	results := []string{varOf(n.Name), ingestVar}
	l.append(Op{
		Method: MethodSource, Receiver: machineVar, Results: results, TypeArg: payload,
		Node: n.Name, Ref: refOf(n), Options: options, Start: n.Start, Stop: n.Stop,
	})
	l.ingest(n, ingestVar, payload)
	l.bind(n.Outputs, results[0])
}

// ingest records one source's exported ingest, refusing a field-name collision.
//
// THE COLLISION THE REFUSAL SET DOES NOT ALREADY COVER: `source a` and `source A`
// are DISTINCT node names, so the duplicate-node-name member never fires, yet both
// upper-case to the field `A`. Silently dropping one would give the host a struct
// whose field feeds a flow it did not name. Refusing names both sources.
func (l *lowering) ingest(n Node, ingestVar, payload string) {
	field := exported(n.Name)
	for _, prior := range l.plan.Ingests {
		if prior.Field == field {
			l.diagf(n.Start, n.Stop,
				"sources %q and %q both export the ingest field %q, so one would shadow the "+
					"other; rename one so the names differ by more than the case of their first letter",
				prior.Node, n.Name, field)

			return
		}
	}
	l.plan.Ingests = append(l.plan.Ingests, Ingest{
		Node: n.Name, Field: field, Var: ingestVar, Type: payload,
	})
}

// sink lowers a sink to a Map followed by a Drop of its drain.
//
// THERE IS NO Sink METHOD. The runtime's flow method set is closed at Map, If,
// Tee, Send, Drop and Output, so a terminal node is a Map whose result is
// dropped; the drain carries the unforgeable separator so it cannot collide with
// a name the author wrote.
func (l *lowering) sink(n Node, options []string) {
	result := varOf(n.Name)
	l.emit(n, MethodMap, l.receiver(n), []string{result}, options)
	l.append(Op{
		Method: MethodDrop, Receiver: result, Node: n.Name + derivedSep + "drain",
		Start: n.Start, Stop: n.Stop,
	})
}

// send lowers a send: merging an existing flow into the consumer another flow
// already feeds, binding nothing.
//
// THE ARGUMENT IS THE FLOW THAT PRECEDES THE NODE BEING RE-ENTERED, not the flow
// that node produces — the runtime's own doc says so, because Send merges into
// the SAME downstream consumer the target already feeds. Passing the re-entered
// node's own output would route the loop one node too far along.
func (l *lowering) send(n Node) {
	target, ok := sendTarget(n.Stmt)
	if !ok {
		l.diagf(n.Start, n.Stop, "a send statement names no target")

		return
	}
	reentered, precedes := l.reentryPoint(target)
	if reentered == "" {
		l.diagf(n.Start, n.Stop, "the send to %q re-enters no declared node", target)

		return
	}
	l.append(Op{
		Method: MethodSend, Receiver: l.receiver(n), Ref: precedes,
		Node: n.Name, After: reentered, Start: n.Start, Stop: n.Stop,
	})
}

// reentryPoint resolves a send target to the node being re-entered and to the
// Go variable holding the flow that PRECEDES it.
//
// A target names either a loop label or a node. Either way the node re-entered is
// the one that CONSUMES that name, and the flow to pass is that node's own
// receiver.
func (l *lowering) reentryPoint(target string) (node, precedes string) {
	for _, candidate := range l.program.Nodes {
		if candidate.Kind == KindSend {
			continue
		}
		for _, in := range candidate.Inputs {
			if in == target {
				return candidate.Name, l.receiver(candidate)
			}
		}
	}

	return "", ""
}

// switchChain lowers an N-arm switch to a chain of If calls, one per arm, in
// source order so first-match-wins is preserved.
//
// The k-th If takes the FALSE branch of the previous one, so an earlier arm
// always wins. The else target is the last If's false branch; a switch with no
// else leaves that branch unconsumed, which the unconsumed-output refusal
// catches rather than this lowering.
func (l *lowering) switchChain(n Node, options []string) {
	s, ok := n.Stmt.(ast.SwitchStmt)
	if !ok {
		l.diagf(n.Start, n.Stop, "the switch statement carries no switch shape")

		return
	}
	receiver := l.receiver(n)
	for k, arm := range s.Arms {
		name := derivedName(n.Name, k)
		trueVar, falseVar := varOf(arm.Target.Name), varOf(name)+"Rest"
		if k == len(s.Arms)-1 && s.Else != nil {
			falseVar = varOf(s.Else.Name)
		}
		l.append(Op{
			Method: MethodIf, Receiver: receiver, Results: []string{trueVar, falseVar},
			Node: name, Ref: armPredicate(l, n, s, arm), Options: armOptions(options, k),
			Start: n.Start, Stop: n.Stop,
		})
		l.flowVar[arm.Target.Name] = trueVar
		receiver = falseVar
	}
	if s.Else != nil {
		l.flowVar[s.Else.Name] = varOf(s.Else.Name)
	}
}

// teeChain lowers an N-target tee to N-1 chained Tee calls.
func (l *lowering) teeChain(n Node, options []string) {
	receiver := l.receiver(n)
	for k := 0; k+1 < len(n.Outputs); k++ {
		name := derivedName(n.Name, k)
		trueVar, falseVar := varOf(n.Outputs[k]), varOf(name)+"Rest"
		if k+2 == len(n.Outputs) {
			falseVar = varOf(n.Outputs[k+1])
		}
		l.append(Op{
			Method: MethodTee, Receiver: receiver, Results: []string{trueVar, falseVar},
			Node: name, TypeArg: l.program.InputTypes[n.Name],
			Options: armOptions(options, k), Start: n.Start, Stop: n.Stop,
		})
		l.flowVar[n.Outputs[k]] = trueVar
		receiver = falseVar
	}
	if len(n.Outputs) > 0 {
		l.flowVar[n.Outputs[len(n.Outputs)-1]] = receiver
	}
}

// emit appends one op and binds the node's outputs to its first result.
func (l *lowering) emit(n Node, method, receiver string, results, options []string) {
	l.append(Op{
		Method: method, Receiver: receiver, Results: results, Node: n.Name,
		Ref: refOf(n), Options: options, Start: n.Start, Stop: n.Stop,
	})
	// BIND ONLY A SINGLE-RESULT CALL HERE. A branch and a tee bind one flow PER
	// OUTLET, and their own helpers have already recorded that mapping; binding
	// every output to results[0] would overwrite it and point both outlets at the
	// left one. That is not a cosmetic error: a later `drop lost` would then drop
	// the KEPT flow, leaving the real one declared and never consumed.
	if len(results) == 1 {
		l.bind(n.Outputs, results[0])
	}
	l.mergeInputs(n, receiver)
}

// mergeInputs emits the Send that routes each NON-declaring input into this node.
//
// THE FIRST NAME BUILDS THE NODE AND EVERY LATER ONE IS ROUTED IN. `transform t
// Foo from a, b` constructs t from a's flow and then merges b into the same
// consumer, which is a Send because the runtime has no fan-in constructor. A
// lowering that ignored the later names would emit a node reading only its first
// input and leave the extra flows declared and never consumed — Go rejects that
// file, which is how this gap surfaced rather than shipping.
func (l *lowering) mergeInputs(n Node, receiver string) {
	if len(n.Inputs) < 2 {
		return
	}
	for _, extra := range n.Inputs[1:] {
		source, bound := l.flowVar[extra]
		if !bound {
			// A loop label carries no flow of its own: the send that TARGETS the
			// label is what closes that path, and it is lowered from its own
			// statement.
			continue
		}
		l.append(Op{
			Method: MethodSend, Receiver: source, Ref: receiver,
			Node: extra + derivedSep + "merge", After: n.Name, Start: n.Start, Stop: n.Stop,
		})
	}
}

// branchResults names the two flows a branch binds, one per declared target.
func (l *lowering) branchResults(n Node) []string {
	if len(n.Outputs) != 2 {
		l.diagf(n.Start, n.Stop, "a branch declares %d targets, want two", len(n.Outputs))

		return nil
	}
	left, right := varOf(n.Outputs[0]), varOf(n.Outputs[1])
	l.flowVar[n.Outputs[0]] = left
	l.flowVar[n.Outputs[1]] = right

	return []string{left, right}
}

// append records one op.
func (l *lowering) append(op Op) { l.plan.Ops = append(l.plan.Ops, op) }

// bind records which Go variable holds the flow producing each name.
func (l *lowering) bind(outputs []string, variable string) {
	for _, out := range outputs {
		l.flowVar[out] = variable
	}
}

// receiver names the Go variable this node's first call is made on: the flow
// bound by whatever produces its DECLARING input.
func (l *lowering) receiver(n Node) string {
	if len(n.Inputs) == 0 {
		return machineVar
	}
	if variable, ok := l.flowVar[n.Inputs[0]]; ok {
		return variable
	}

	return varOf(n.Inputs[0])
}

// machineVar is the Go variable holding the machine a wiring function declares
// against.
const machineVar = "m"

// derivedName renders the k-th call of a chained lowering.
//
// The zeroth keeps the source-written name so a single-arm switch or two-target
// tee reads as the author wrote it; later ones carry the unforgeable separator.
func derivedName(base string, k int) string {
	if k == 0 {
		return base
	}

	return base + derivedSep + itoa(k)
}

// varOf renders a flow name as a Go variable, replacing the derived separator,
// which is legal in a flow name and not in a Go identifier.
func varOf(name string) string {
	return strings.ReplaceAll(strings.ReplaceAll(name, derivedSep, "_"), ".", "_")
}

// refOf reads the Go reference a statement named, verbatim.
func refOf(n Node) string {
	switch s := n.Stmt.(type) {
	case ast.SourceStmt:
		return s.Ref.Text
	case ast.TransformStmt:
		return s.Ref.Text
	case ast.BranchStmt:
		return s.Ref.Text
	case ast.SinkStmt:
		return s.Ref.Text
	default:
		return ""
	}
}

// payloadType reads a node's declared input type spelling, reporting when the
// driver supplied none.
//
// Machine.Source is the one builder call that cannot infer its payload type, so
// an absent spelling is a refusal rather than a guess: emitting `any` there
// would produce a machine whose whole flow is erased, which compiles.
func (l *lowering) payloadType(n Node) string {
	spelling, ok := l.program.InputTypes[n.Name]
	if !ok || strings.TrimSpace(spelling) == "" {
		l.diagf(n.Start, n.Stop,
			"the source %q needs its payload type, which no type information supplies", n.Name)

		return ""
	}

	return spelling
}

// armOptions carries the statement's options onto the FIRST call of a chain
// only. Repeating them on every link would declare the same capability and the
// same checkpoint several times over for one source statement.
func armOptions(options []string, k int) []string {
	if k == 0 {
		return options
	}

	return nil
}
