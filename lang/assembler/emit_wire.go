// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"slices"
	"strings"
	"unicode"
)

// wire writes one flow's handle declarations, its ingest struct and its wiring
// function.
func (e *emitter) wire(plan *Plan, s *synthesis) {
	e.flowErrorHandler(plan)
	e.handles(plan, s)
	e.ingestStruct(plan)

	name := exported(plan.Flow)
	e.wireDoc(plan, name)
	e.writeln("func Wire" + name + "(m *machine.Machine) (" + name + "Ingests, error) {")
	e.journalCheck(plan)
	for _, op := range plan.Ops {
		e.op(op, s)
	}
	e.writeln("")
	e.ingestReturn(plan)
	e.writeln("}")
	e.writeln("")
}

// wireDoc writes Wire's doc comment.
//
// It is split from wire above only because the module's linter caps a function at
// twenty statements; the two together are one emission.
func (e *emitter) wireDoc(plan *Plan, name string) {
	e.writeln("// Wire" + name + " declares the " + plan.Flow + " flow on m and returns its ingests.")
	e.writeln("//")
	e.writeln("// IT RETURNS " + name + "Ingests AND error UNCONDITIONALLY. A flow with no")
	e.writeln("// checkpoint returns a nil error, and a flow whose sources are all driven by")
	e.writeln("// their transports returns a struct the host is free to ignore. The signature is")
	e.writeln("// one contract rather than two, so a caller never has to know whether the .flow")
	e.writeln("// source happens to checkpoint or to be fed programmatically, and a regeneration")
	e.writeln("// cannot silently change how this is called.")
	e.writeln("//")
	e.writeln("// THE ZERO STRUCT IS NOT USABLE. On a non-nil error the returned struct is the")
	e.writeln("// zero value and every ingest field is a nil func, so a caller that ignores the")
	e.writeln("// error and pushes panics rather than dropping the value silently.")
	if planCheckpoints(plan) {
		e.writeln("//")
		e.writeln("// THIS FLOW CHECKPOINTS, so it needs a journal. The check below runs BEFORE the")
		e.writeln("// first node is declared and leaves the machine untouched when it fires.")
	}
}

// ingestStruct writes the per-flow ingest struct.
//
// IT IS DECLARED FOR EVERY FLOW, including one with no source statement, because
// Wire's signature names it unconditionally. An empty struct is the honest
// spelling of "this flow exposes no programmatic entry".
//
// WHY A RETURNED STRUCT RATHER THAN PACKAGE-LEVEL VARS: package-level values
// would be process-global, so two Machines wiring the same flow would overwrite
// each other's ingests — the same hazard the state-handle qualifier exists to
// avoid. A returned value is per-call and cannot collide.
func (e *emitter) ingestStruct(plan *Plan) {
	name := exported(plan.Flow) + "Ingests"
	e.writeln("// " + name + " carries one typed ingest per source statement in the " +
		plan.Flow + " flow.")
	e.writeln("//")
	e.writeln("// Each field pushes a value into its source as if the source's transport had")
	e.writeln("// delivered it, so a host can feed the flow programmatically. A field is a nil")
	e.writeln("// func on the zero struct, which is what Wire returns beside a non-nil error.")
	if len(plan.Ingests) == 0 {
		e.writeln("//")
		e.writeln("// THIS FLOW DECLARES NO SOURCE, so the struct is empty. It exists anyway")
		e.writeln("// because Wire's signature names it whether or not the flow has one.")
		e.writeln("type " + name + " struct{}")
		e.writeln("")

		return
	}
	e.writeln("type " + name + " struct {")
	for _, in := range plan.Ingests {
		e.writeln("\t// " + in.Field + " feeds the " + in.Node + " source.")
		e.writeln("\t" + in.Field + " machine.Ingest[" + in.Type + "]")
	}
	e.writeln("}")
	e.writeln("")
}

// ingestReturn writes Wire's success return.
func (e *emitter) ingestReturn(plan *Plan) {
	name := exported(plan.Flow) + "Ingests"
	if len(plan.Ingests) == 0 {
		e.writeln("\treturn " + name + "{}, nil")

		return
	}
	e.writeln("\treturn " + name + "{")
	for _, in := range plan.Ingests {
		e.writeln("\t\t" + in.Field + ": " + in.Var + ",")
	}
	e.writeln("\t}, nil")
}

// flowErrorHandler exposes a flow-level `on error` handler for the HOST to
// install.
//
// WIRE CANNOT INSTALL IT, and that is the runtime's shape rather than a
// limitation of this generator: a machine's error handler is a machine.New
// option, and Wire receives an already-built Machine. So the handler is exported
// as a value the host passes, on exactly the terms the journal is — generated
// code states what it needs and the host supplies it.
//
// A NODE-level `on error` is different and IS installed by Wire, because that one
// is a NodeOption.
func (e *emitter) flowErrorHandler(plan *Plan) {
	if plan.OnError == "" {
		return
	}
	name := exported(plan.Flow) + "OnError"
	e.writeln("// " + name + " is the flow-level error handler " + plan.Flow + " declares.")
	e.writeln("//")
	e.writeln("// WIRE CANNOT INSTALL IT. A machine's error handler is a machine.New option and")
	e.writeln("// Wire is handed a machine that is already built, so the host passes")
	e.writeln("// machine.OptionErrorHandler(" + name + ") when constructing it. Without that,")
	e.writeln("// the flow runs under the runtime's default handler and this declaration has no")
	e.writeln("// effect.")
	e.writeln("var " + name + " = machine.ErrorHandler[any](" + plan.OnError + ")")
	e.writeln("")
}

// journalCheck writes the Wire-time refusal for a flow that checkpoints.
//
// IT IS THE FIRST STATEMENT, deliberately: no node is declared and nothing is
// registered on the machine, so a host that forgot the option gets a clean
// refusal rather than a half-built machine.
func (e *emitter) journalCheck(plan *Plan) {
	if !planCheckpoints(plan) {
		return
	}
	e.writeln("\tif !m.HasJournal() {")
	e.writeln("\t\treturn " + exported(plan.Flow) + "Ingests{}, fmt.Errorf(\"" + plan.Flow +
		": this flow checkpoints and needs a recovery journal; \" +")
	e.writeln("\t\t\t\"build the machine with machine.OptionJournal\")")
	e.writeln("\t}")
	e.writeln("")
}

// planCheckpoints reports whether any op in a plan declares a checkpoint.
func planCheckpoints(plan *Plan) bool {
	for _, op := range plan.Ops {
		for _, option := range op.Options {
			if strings.Contains(option, "machine.WithCheckpoint(") {
				return true
			}
		}
	}

	return false
}

// handles writes the process-global key and cell declarations.
//
// THE NAMES CARRY THE QUALIFIER AND THE FLOW because the runtime keeps keys and
// cells in ONE namespace for the whole process and panics on a duplicate. Two
// generated packages in one binary, or two flows in one package, would otherwise
// collide on any shared var name.
func (e *emitter) handles(plan *Plan, s *synthesis) {
	if len(plan.Handles) == 0 {
		return
	}
	e.writeln("// State handles for the " + plan.Flow + " flow. The names are qualified because the")
	e.writeln("// runtime holds every key and cell in one process-global namespace.")
	e.writeln("var (")
	for _, h := range plan.Handles {
		if h.Kind == HandleCell {
			e.writeln("\t" + h.Var + " = machine.NewCell[" + h.Type + "](\"" + h.Name + "\")")

			continue
		}
		// machine.NewKey DEMANDS a cloner and panics without one, because Tee
		// deep-copies the frame and the frame's clone walks the cloner map. A
		// trivially-copyable type gets the identity; anything else gets the
		// derived deep copy.
		e.writeln("\t" + h.Var + " = machine.NewKey[" + h.Type + "](\"" + h.Name + "\", " + keyCloner(s, h.Type) + ")")
	}
	e.writeln(")")
	e.writeln("")
}

// teeDuplicator names the Duplicator a tee call takes.
func teeDuplicator(s *synthesis, spelling string) string {
	if s != nil {
		if derived := s.DuplicatorFor(spelling); derived != "" {
			return derived
		}
	}

	return "func(d " + spelling + ") (" + spelling + ", " + spelling + ") { return d, d }"
}

// keyCloner renders the clone function a key's NewKey takes.
func keyCloner(s *synthesis, spelling string) string {
	if s != nil {
		if derived := s.ClonerFor(spelling); derived != "" {
			return derived
		}
	}

	return "func(v " + spelling + ") " + spelling + " { return v }"
}

// op writes one builder call.
func (e *emitter) op(op Op, s *synthesis) {
	call := e.callText(op, s)
	if call == "" {
		return
	}
	switch len(op.Results) {
	case 0:
		e.writeAt(op.Start, "\t"+call)
	case 1:
		e.writeAt(op.Start, "\t"+op.Results[0]+" := "+call)
	default:
		e.writeAt(op.Start, "\t"+strings.Join(op.Results, ", ")+" := "+call)
	}
}

// callText renders one op's Go call expression.
//
// IT REFUSES A METHOD OUTSIDE THE RUNTIME'S CLOSED VOCABULARY rather than writing
// it. The lowering is what chooses methods and a cross-check gates that choice,
// but emitting an unknown one would produce Go that does not compile against the
// runtime, and saying so here names the op rather than leaving a build error
// against a generated line.
func (e *emitter) callText(op Op, s *synthesis) string {
	if !slices.Contains(builderMethods, op.Method) {
		e.diags = append(e.diags, diagnosticAt(op.Start, op.Stop,
			"the op for %q calls %q, which is not one of the runtime's builder methods %v",
			op.Node, op.Method, builderMethods))

		return ""
	}
	args := e.argsOf(op, s)
	if op.Method == MethodSource {
		return "m.Source[" + op.TypeArg + "](\"" + op.Node + "\"" + args + ")"
	}

	return op.Receiver + "." + op.Method + "(" + strings.TrimPrefix(args, ", ") + ")"
}

// argsOf renders an op's argument list, leading comma included.
func (*emitter) argsOf(op Op, s *synthesis) string {
	var parts []string
	switch op.Method {
	case MethodSend:
		return ", " + op.Ref
	case MethodSource:
		if op.Ref != "" {
			parts = append(parts, "machine.WithEdge("+op.Ref+")")
		}
	case MethodTee:
		// A TEE CARRIES NO GO REFERENCE. The language says a tee is route-only and
		// applies no function, so its Duplicator is entirely the generator's and
		// comes from the derivation rather than from the source.
		parts = append(parts, "\""+op.Node+"\"", teeDuplicator(s, op.TypeArg))
	default:
		parts = append(parts, "\""+op.Node+"\"")
		if op.Ref != "" {
			parts = append(parts, op.Ref)
		}
	}
	parts = append(parts, op.Options...)
	if len(parts) == 0 {
		return ""
	}

	return ", " + strings.Join(parts, ", ")
}

// exported upper-cases a flow name's first rune for the wiring function's name.
//
// IT IS A NAMING TRANSFORM AND SAYS NOTHING ABOUT EXPORT. Whether the FLOW is
// exported is decided by the author's own first rune and read elsewhere; this
// only makes a valid exported Go identifier out of a flow name.
func exported(name string) string {
	if name == "" {
		return ""
	}
	runes := []rune(name)
	runes[0] = unicode.ToUpper(runes[0])

	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}

		return '_'
	}, string(runes))
}
