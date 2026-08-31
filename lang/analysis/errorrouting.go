// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"reflect"
	"strings"

	"github.com/whitaker-io/machine/lang/ast"
)

// Where a statement's failure goes.
const (
	handlerNode = "node"
	handlerFlow = "flow"
	handlerNone = "none"
)

// ErrorRoutingAnalyzer resolves where each statement's failure goes.
var ErrorRoutingAnalyzer = &Analyzer{
	Name: "errorrouting",
	Doc: "errorrouting resolves every node-bearing statement's failure destination to its own on " +
		"error clause, or to the flow-level on error declaration, or to neither — error handling " +
		"is a DECLARATION rather than an edge, at flow level or per node, which is what makes a " +
		"failure destination statically knowable. A statement whose failure reaches no handler is " +
		"reported at HINT severity, not error: the supervisor error contract's ruled default is a " +
		"no-op drop, so an unhandled failure is a deliberate and legal configuration rather than a " +
		"defect, and a canonical program declares no handler at all. The handler-well-formedness " +
		"check here is a cheap invariant assertion and is NOT THE ENFORCEMENT: the parser already " +
		"refuses an empty handler and one beginning with an arrow, so this can only fire on a tree " +
		"the parser accepted, and nobody should relax the parser on the strength of it.",
	Requires:   []*Analyzer{SymbolsAnalyzer},
	Run:        runErrorRouting,
	ResultType: reflect.TypeOf((*ErrorRoutes)(nil)),
}

// ErrorRoute is one statement's resolved failure destination.
//
// Handler is the referenced Go function as written. Kind names where the
// resolution came from: the statement's own clause, the flow's declaration, or
// neither.
type ErrorRoute struct {
	Flow    string
	Stmt    int
	Label   string
	Kind    string
	Handler string
	Pos     ast.Position
}

// ErrorRoutes is the errorrouting analyzer's result: one entry per node-bearing
// statement in the run.
type ErrorRoutes struct {
	Routes []ErrorRoute
}

// runErrorRouting resolves and checks every flow in every source.
func runErrorRouting(p *Pass) (any, error) {
	table, ok := p.ResultOf[SymbolsAnalyzer].(*SymbolTable)
	if !ok {
		return nil, errNoSymbols
	}

	out := &ErrorRoutes{}
	for f := range table.Files {
		for i := range table.Files[f].Flows {
			routeFlow(p, table.Files[f].Src, &table.Files[f].Flows[i], out)
		}
	}
	return out, nil
}

// routeFlow resolves one flow's statements against its own declaration.
func routeFlow(p *Pass, src Source, flow *FlowSymbols, out *ErrorRoutes) {
	for i, stmt := range flow.Body {
		clauses, name, ok := namedClauses(stmt)
		if !ok {
			continue
		}
		route := resolveRoute(flow, i, name, clauses)
		out.Routes = append(out.Routes, route)
		reportRoute(p, src, flow, route)
	}
}

// resolveRoute picks a statement's handler: its own clause first, then the
// flow's declaration.
func resolveRoute(flow *FlowSymbols, stmt int, name ast.Ident, clauses ast.Clauses) ErrorRoute {
	route := ErrorRoute{Flow: flow.Name, Stmt: stmt, Label: name.Name, Kind: handlerNone, Pos: name.NamePos}
	switch {
	case clauses.OnError != nil:
		route.Kind = handlerNode
		route.Handler = strings.TrimSpace(clauses.OnError.Text)
	case flow.OnError != nil:
		route.Kind = handlerFlow
		route.Handler = strings.TrimSpace(flow.OnError.Handler.Text)
	}
	return route
}

// reportRoute reports an unreachable handler, and asserts the invariant the
// parser already enforces.
func reportRoute(p *Pass, src Source, flow *FlowSymbols, route ErrorRoute) {
	if route.Kind == handlerNone {
		p.Report(src, Diagnostic{
			Pos: route.Pos,
			End: endOfName(route.Pos, route.Label),
			Message: "the failure of " + route.Label + " in flow " + flow.Name + " reaches no handler; " +
				"the supervisor default is a no-op drop, so this is legal and may be deliberate",
			Severity: SeverityHint,
		})
		return
	}
	if route.Handler != "" && !strings.HasPrefix(route.Handler, "->") {
		return
	}
	p.Report(src, Diagnostic{
		Pos:      route.Pos,
		End:      endOfName(route.Pos, route.Label),
		Message:  "the handler for " + route.Label + " in flow " + flow.Name + " is not a function reference",
		Severity: SeverityError,
	})
}

// namedClauses reads the clause bundle and node name off the seven shapes that
// carry them, reporting whether the statement is one of them.
//
// The seven clause-bearing shapes can carry an on-error clause; drop, loop, send
// and bad cannot, and have no failure of their own to route — a drop discards, a
// loop is a label, and a send routes a datum some other statement produced.
func namedClauses(stmt ast.Stmt) (ast.Clauses, ast.Ident, bool) {
	switch s := stmt.(type) {
	case ast.SourceStmt:
		return s.Clauses, s.Name, true
	case ast.TransformStmt:
		return s.Clauses, s.Name, true
	case ast.BranchStmt:
		return s.Clauses, s.Name, true
	case ast.TeeStmt:
		return s.Clauses, s.Name, true
	case ast.SinkStmt:
		return s.Clauses, s.Name, true
	case ast.SwitchStmt:
		return s.Clauses, s.Name, true
	case ast.UseStmt:
		return s.Clauses, s.Instance, true
	default:
		return ast.Clauses{}, ast.Ident{}, false
	}
}
