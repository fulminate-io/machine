// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"reflect"

	"github.com/whitaker-io/machine/lang/ast"
)

// implicitInput is the name a flow with a signature consumes without declaring.
//
// FlowSignature's doc states it: "When a signature is present the body consumes
// an implicit `in`". It is tabled as a producer so a reference to it resolves.
const implicitInput = "in"

// SymbolsAnalyzer tables the names each flow declares and the names it
// references. Every later analyzer reads this table rather than re-walking the
// tree for names.
var SymbolsAnalyzer = &Analyzer{
	Name: "symbols",
	Doc: "symbols tables every flow-level name: which statement DECLARES it and which " +
		"statements REFERENCE it, plus the flow's vars, state fields, declared outputs and " +
		"the file's funcs and imports. The `->` arrow means two different things by shape: on " +
		"branch, tee, switch and use it INTRODUCES output names and the statement's own node " +
		"name is not consumable, while on send it REFERENCES an existing name backward. A " +
		"send's Target is therefore tabled as a reference here, not as a declaration.",
	Run:        runSymbols,
	ResultType: reflect.TypeOf((*SymbolTable)(nil)),
}

// NameRef is one appearance of a flow-level name: which statement it appeared
// in, and where in the source.
//
// The statement index is what lets the flowgraph analyzer draw an edge between
// two statements without re-walking the tree.
type NameRef struct {
	Stmt int
	Pos  ast.Position
}

// ImportRef is one import a file declares. The path is the literal as written,
// quotes included, because the parser captures it and resolves nothing.
type ImportRef struct {
	Alias string
	Path  string
	Pos   ast.Position
}

// FlowSymbols is one flow's name model.
//
// Producers maps a name to the statements DECLARING it; a name normally has one,
// and more than one is a redeclaration a later analyzer may report. Consumers
// maps a name to the statements REFERENCING it. Reads and Writes are the same
// shape over the reads and writes clauses, which name vars and state fields
// rather than flow-graph names.
type FlowSymbols struct {
	Name         string
	Pos          ast.Position
	Body         []ast.Stmt
	Producers    map[string][]NameRef
	Consumers    map[string][]NameRef
	Reads        map[string][]NameRef
	Writes       map[string][]NameRef
	Loops        map[string]NameRef
	Routing      map[string]NameRef
	Vars         map[string]ast.VarDecl
	State        map[string]ast.StateField
	OnError      *ast.OnErrorDecl
	Outputs      []ast.FlowOutput
	HasSignature bool
	Bad          []ast.BadStmt
}

// FileSymbols is one source file's declarations.
//
// Src is the source itself rather than only its path, because every analyzer
// downstream reports against this table and Report takes a Source.
type FileSymbols struct {
	Src     Source
	Funcs   map[string]ast.Position
	Imports []ImportRef
	Flows   []FlowSymbols
}

// SymbolTable is the symbols analyzer's result over every source in the run.
type SymbolTable struct {
	Files []FileSymbols
}

// Flow finds a flow by name anywhere in the run, which is how a `use` statement
// referencing a flow in ANOTHER FILE is resolved.
func (t *SymbolTable) Flow(name string) (*FlowSymbols, bool) {
	for f := range t.Files {
		for i := range t.Files[f].Flows {
			if t.Files[f].Flows[i].Name == name {
				return &t.Files[f].Flows[i], true
			}
		}
	}
	return nil, false
}

// runSymbols tables every source in the run.
func runSymbols(p *Pass) (any, error) {
	table := &SymbolTable{Files: make([]FileSymbols, 0, len(p.Sources))}
	for _, src := range p.Sources {
		table.Files = append(table.Files, tableFile(p, src))
	}
	return table, nil
}

// tableFile reads one file's declarations.
//
// The declaration switch carries a default arm for the same reason every switch
// in this package does: ast.Decl is sealed against outside implementations but
// NOT against lang/ast growing a shape, and this module is versioned separately
// from that one.
func tableFile(p *Pass, src Source) FileSymbols {
	out := FileSymbols{Src: src, Funcs: map[string]ast.Position{}}
	for _, decl := range src.File.Decls {
		switch d := decl.(type) {
		case ast.ImportDecl:
			out.Imports = append(out.Imports, importRef(d))
		case ast.FuncDecl:
			out.Funcs[d.Name.Name] = d.Name.NamePos
		case ast.FlowDecl:
			out.Flows = append(out.Flows, tableFlow(p, src, d))
		default:
			reportUnknownDecl(p, src, decl)
		}
	}
	return out
}

// importRef reads one import declaration, defaulting the alias to absent.
func importRef(d ast.ImportDecl) ImportRef {
	ref := ImportRef{Path: d.Path, Pos: d.Start}
	if d.Alias != nil {
		ref.Alias = d.Alias.Name
	}
	return ref
}

// reportUnknownDecl names a file-level declaration shape this module does not
// know, rather than skipping it silently.
//
// A const, param, note, state, var or on-error declaration reaching file level
// is not a symbol this table holds, so those are not unknown — they are simply
// not tabled here, and are matched off before this point by the shapes above
// only when they are the ones this analyzer wants. Anything else means lang/ast
// grew a declaration shape and this module has not caught up.
func reportUnknownDecl(p *Pass, src Source, decl ast.Decl) {
	switch decl.(type) {
	case ast.ConstDecl, ast.ParamDecl, ast.NoteBlock, ast.StateDecl, ast.VarDecl, ast.OnErrorDecl:
		return
	default:
		p.Report(src, Diagnostic{
			Pos:      decl.Pos(),
			End:      decl.End(),
			Message:  "this analysis module does not know the declaration shape " + typeName(decl),
			Severity: SeverityError,
		})
	}
}

// tableFlow reads one flow's names.
func tableFlow(p *Pass, src Source, fd ast.FlowDecl) FlowSymbols {
	c := &symbolCollector{pass: p, src: src, flow: newFlowSymbols(fd)}
	c.signature(fd.Signature)
	for _, v := range fd.Vars {
		c.flow.Vars[v.Name.Name] = v
	}
	if fd.State != nil {
		for _, field := range fd.State.Fields {
			c.flow.State[field.Name.Name] = field
		}
	}
	for i, stmt := range fd.Body {
		c.visit(i, stmt)
	}
	return *c.flow
}

// newFlowSymbols pre-sizes the tables from the body length, per the plan's perf
// shape: one walk, maps sized once.
func newFlowSymbols(fd ast.FlowDecl) *FlowSymbols {
	n := len(fd.Body)
	return &FlowSymbols{
		Name:      fd.Name.Name,
		Pos:       fd.Name.NamePos,
		Body:      fd.Body,
		OnError:   fd.OnError,
		Producers: make(map[string][]NameRef, n),
		Consumers: make(map[string][]NameRef, n),
		Reads:     make(map[string][]NameRef, n),
		Writes:    make(map[string][]NameRef, n),
		Loops:     map[string]NameRef{},
		Routing:   map[string]NameRef{},
		Vars:      map[string]ast.VarDecl{},
		State:     map[string]ast.StateField{},
	}
}

// symbolCollector accumulates one flow's tables as the walk proceeds.
type symbolCollector struct {
	pass *Pass
	src  Source
	flow *FlowSymbols
}

// signatureStmt is the statement index a name declared by the flow SIGNATURE
// rather than by a statement carries.
//
// It is deliberately out of range for Body: the implicit `in` has a position but
// no statement, so a consumer indexing Body by a NameRef.Stmt must skip it. The
// flowgraph analyzer does exactly that when it builds nodes.
const signatureStmt = -1

// typeName renders a node's dynamic type for a message about a shape this module
// does not know.
func typeName(n ast.Node) string {
	return reflect.TypeOf(n).String()
}

// signature records a flow's declared outputs and the implicit input its body
// consumes when a signature is present.
func (c *symbolCollector) signature(sig *ast.FlowSignature) {
	if sig == nil {
		return
	}
	c.flow.HasSignature = true
	c.flow.Outputs = sig.Outputs
	c.produce(signatureStmt, ast.Ident{Name: implicitInput, NamePos: sig.Start})
}

// visit routes one statement to the collector for its shape.
//
// THE DISPATCH IS SPLIT ON THE LINE THE AST ITSELF DRAWS: seven shapes embed
// Clauses and four do not. One eleven-case switch measures past both complexity
// limits, and the parser reached the same answer for the same reason — its
// declParsers and stmtParsers are maps because a switch that wide "measures past
// the cyclomatic limit".
func (c *symbolCollector) visit(i int, stmt ast.Stmt) {
	if c.visitClauseBearing(i, stmt) {
		return
	}
	c.visitPlain(i, stmt)
}

// visitClauseBearing handles the seven shapes that embed Clauses, reporting
// whether it recognized the statement.
//
// EVERY CASE IS THE VALUE FORM. ast.Parse emits values, and every node declares
// its interface methods on a value receiver, so a pointer-form case compiles
// clean and is silently always-false — a walk written that way matches nothing,
// forever, while every "the clean corpus produces no diagnostics" test passes.
func (c *symbolCollector) visitClauseBearing(i int, stmt ast.Stmt) bool {
	switch s := stmt.(type) {
	case ast.SourceStmt:
		c.source(i, s)
	case ast.TransformStmt:
		c.transform(i, s)
	case ast.BranchStmt:
		c.branch(i, s)
	case ast.TeeStmt:
		c.tee(i, s)
	case ast.SinkStmt:
		c.sink(i, s)
	case ast.SwitchStmt:
		c.switchStmt(i, s)
	case ast.UseStmt:
		c.use(i, s)
	default:
		return false
	}
	return true
}

// visitPlain handles the four shapes that carry no clauses.
func (c *symbolCollector) visitPlain(i int, stmt ast.Stmt) {
	switch s := stmt.(type) {
	case ast.DropStmt:
		c.consume(i, s.Input)
	case ast.LoopStmt:
		c.loop(i, s)
	case ast.SendStmt:
		c.send(i, s)
	case ast.BadStmt:
		c.flow.Bad = append(c.flow.Bad, s)
	default:
		c.unknownStmt(stmt)
	}
}

// unknownStmt names a statement shape this module cannot read.
//
// The arm is reachable in two ways, neither of them impossible. lang/ast may
// grow an eleventh shape while this separately-versioned module still knows ten;
// and a POINTER to a known shape satisfies ast.Stmt without matching any value
// case above, which is the exact trap that would otherwise make this walk match
// nothing while every clean-corpus assertion stayed green. Both are reported
// rather than skipped.
func (c *symbolCollector) unknownStmt(stmt ast.Stmt) {
	c.pass.Report(c.src, Diagnostic{
		Pos:      stmt.Pos(),
		End:      stmt.End(),
		Message:  "this analysis module does not know the statement shape " + typeName(stmt),
		Severity: SeverityError,
	})
}

// source tables a source's output. A source has no inputs, so its Clauses.From
// is always empty; its other clauses still count.
func (c *symbolCollector) source(i int, s ast.SourceStmt) {
	c.clauses(i, s.Clauses)
	c.produce(i, s.Name)
}

// transform tables a transform's inputs and its single output, which is its own
// node name.
func (c *symbolCollector) transform(i int, s ast.TransformStmt) {
	c.clauses(i, s.Clauses)
	c.produce(i, s.Name)
}

// branch tables a branch's inputs and its two target names. The branch's OWN
// name is routing rather than a producer.
func (c *symbolCollector) branch(i int, s ast.BranchStmt) {
	c.clauses(i, s.Clauses)
	c.route(i, s.Name)
	c.produce(i, s.TrueTarget)
	c.produce(i, s.FalseTarget)
}

// tee tables a tee's inputs and every target it copies to.
func (c *symbolCollector) tee(i int, s ast.TeeStmt) {
	c.clauses(i, s.Clauses)
	c.route(i, s.Name)
	c.produceEach(i, s.Targets)
}

// sink tables a sink's inputs. A sink is terminal and produces nothing.
func (c *symbolCollector) sink(i int, s ast.SinkStmt) {
	c.clauses(i, s.Clauses)
}

// switchStmt tables a switch's inputs and every target its arms route to.
func (c *symbolCollector) switchStmt(i int, s ast.SwitchStmt) {
	c.clauses(i, s.Clauses)
	c.route(i, s.Name)
	c.armTargets(i, s)
}

// use tables an embedded flow's inputs and the caller's names for its outputs.
func (c *symbolCollector) use(i int, s ast.UseStmt) {
	c.clauses(i, s.Clauses)
	c.route(i, s.Instance)
	c.produceEach(i, s.Bindings)
}

// loop tables a loop label, which is both a producer and the one shape a send
// can target by name.
func (c *symbolCollector) loop(i int, s ast.LoopStmt) {
	c.produce(i, s.Name)
	c.flow.Loops[s.Name.Name] = NameRef{Stmt: i, Pos: s.Name.NamePos}
}

// send tables BOTH of a send's names as references.
//
// A send declares nothing: it routes an existing name to an existing target, so
// its Target is a reference here even though the flowgraph's DATAFLOW model
// treats the send as producing that name.
func (c *symbolCollector) send(i int, s ast.SendStmt) {
	c.consume(i, s.Source)
	c.consume(i, s.Target)
}

// armTargets tables one target per arm plus the else target when present.
func (c *symbolCollector) armTargets(i int, s ast.SwitchStmt) {
	for _, arm := range s.Arms {
		c.produce(i, arm.Target)
	}
	if s.Else != nil {
		c.produce(i, *s.Else)
	}
}

// clauses tables the three name-bearing clauses every clause-bearing shape
// carries. Over, Checkpoint, Idempotent, OnError and Note name no flow-level names.
func (c *symbolCollector) clauses(i int, cl ast.Clauses) {
	c.consumeEach(i, cl.From)
	refEach(c.flow.Reads, i, cl.Reads)
	refEach(c.flow.Writes, i, cl.Writes)
}

// produce records a name this statement declares.
func (c *symbolCollector) produce(i int, id ast.Ident) {
	c.flow.Producers[id.Name] = append(c.flow.Producers[id.Name], NameRef{Stmt: i, Pos: id.NamePos})
}

// produceEach records every name in a list as declared here.
func (c *symbolCollector) produceEach(i int, ids []ast.Ident) {
	for _, id := range ids {
		c.produce(i, id)
	}
}

// consume records a name this statement references.
func (c *symbolCollector) consume(i int, id ast.Ident) {
	c.flow.Consumers[id.Name] = append(c.flow.Consumers[id.Name], NameRef{Stmt: i, Pos: id.NamePos})
}

// consumeEach records every name in a list as referenced here.
func (c *symbolCollector) consumeEach(i int, ids []ast.Ident) {
	for _, id := range ids {
		c.consume(i, id)
	}
}

// route records a branch, tee, switch or use statement's OWN name, which
// identifies the node and is never consumable.
func (c *symbolCollector) route(i int, id ast.Ident) {
	c.flow.Routing[id.Name] = NameRef{Stmt: i, Pos: id.NamePos}
}

// refEach records a list of identifiers into one of the clause tables.
func refEach(into map[string][]NameRef, i int, ids []ast.Ident) {
	for _, id := range ids {
		into[id.Name] = append(into[id.Name], NameRef{Stmt: i, Pos: id.NamePos})
	}
}
