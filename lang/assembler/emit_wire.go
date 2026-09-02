// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"strings"
	"unicode"
)

// wire writes one flow's handle declarations and its wiring function.
func (e *emitter) wire(plan *Plan) {
	e.handles(plan)
	e.writeln("// Wire" + exported(plan.Flow) + " declares the " + plan.Flow + " flow on m.")
	e.writeln("//")
	e.writeln("// IT RETURNS error UNCONDITIONALLY. A flow with no checkpoint returns nil; the")
	e.writeln("// signature is one contract rather than two, so a caller never has to know")
	e.writeln("// whether the .flow source happens to checkpoint, and a regeneration cannot")
	e.writeln("// silently change how this is called.")
	if planCheckpoints(plan) {
		e.writeln("//")
		e.writeln("// THIS FLOW CHECKPOINTS, so it needs a journal. The check below runs BEFORE the")
		e.writeln("// first node is declared and leaves the machine untouched when it fires.")
	}
	e.writeln("func Wire" + exported(plan.Flow) + "(m *machine.Machine) error {")
	e.journalCheck(plan)
	for _, op := range plan.Ops {
		e.op(op)
	}
	e.writeln("")
	e.writeln("\treturn nil")
	e.writeln("}")
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
	e.writeln("\t\treturn fmt.Errorf(\"" + plan.Flow + ": this flow checkpoints and needs a recovery journal; \" +")
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
func (e *emitter) handles(plan *Plan) {
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
		// A key's clone function is supplied by Phase 4's synthesis; until then a
		// trivially-copyable type is copied by assignment.
		e.writeln("\t" + h.Var + " = machine.NewKey[" + h.Type + "](\"" + h.Name + "\", func(v " + h.Type + ") " + h.Type + " { return v })")
	}
	e.writeln(")")
	e.writeln("")
}

// op writes one builder call.
func (e *emitter) op(op Op) {
	call := e.callText(op)
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
func (e *emitter) callText(op Op) string {
	args := e.argsOf(op)
	if op.Method == MethodSource {
		return "m.Source[" + op.TypeArg + "](\"" + op.Node + "\"" + args + ")"
	}

	return op.Receiver + "." + op.Method + "(" + strings.TrimPrefix(args, ", ") + ")"
}

// argsOf renders an op's argument list, leading comma included.
func (e *emitter) argsOf(op Op) string {
	var parts []string
	switch op.Method {
	case MethodSend:
		return ", " + op.Ref
	case MethodSource:
		if op.Ref != "" {
			parts = append(parts, "machine.WithEdge("+op.Ref+")")
		}
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
