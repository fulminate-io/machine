// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"
)

// dataflow is one statement's contribution to the graph: the names it consumes
// and the names it produces.
//
// A BRANCH, TEE, SWITCH or USE produces its TARGET names rather than its own
// name — the node's name identifies the node, and the names later statements
// consume `from` are the ones it routes to.
type dataflow struct {
	inputs  []string
	outputs []string
	isLoop  bool
	name    string
}

// namesOf projects identifiers down to their text.
func namesOf(idents []Ident) []string {
	out := make([]string, 0, len(idents))
	for _, id := range idents {
		out = append(out, id.Name)
	}
	return out
}

// flowOf reads one statement's inputs and outputs.
//
// A LOOP is the one shape with NO inputs: it carries no from-clause at all, so
// it is reachable only when a send targets it. That is the whole reason an
// orphaned loop chain is possible.
func flowOf(stmt Stmt) (dataflow, bool) {
	switch s := stmt.(type) {
	case SourceStmt:
		return dataflow{outputs: []string{s.Name.Name}}, true
	case TransformStmt:
		return dataflow{inputs: namesOf(s.From), outputs: []string{s.Name.Name}}, true
	case SinkStmt:
		return dataflow{inputs: namesOf(s.From)}, true
	case BranchStmt:
		return dataflow{inputs: namesOf(s.From), outputs: []string{s.TrueTarget.Name, s.FalseTarget.Name}}, true
	case TeeStmt:
		return dataflow{inputs: namesOf(s.From), outputs: namesOf(s.Targets)}, true
	case UseStmt:
		return dataflow{inputs: namesOf(s.From), outputs: namesOf(s.Bindings)}, true
	case SwitchStmt:
		return dataflow{inputs: namesOf(s.From), outputs: switchTargets(s)}, true
	case DropStmt:
		return dataflow{inputs: []string{s.Input.Name}}, true
	case SendStmt:
		return dataflow{inputs: []string{s.Source.Name}, outputs: []string{s.Target.Name}}, true
	case LoopStmt:
		return dataflow{outputs: []string{s.Name.Name}, isLoop: true, name: s.Name.Name}, true
	}
	return dataflow{}, false
}

// switchTargets collects an arm target per arm plus the else target.
func switchTargets(s SwitchStmt) []string {
	out := make([]string, 0, len(s.Arms)+1)
	for _, arm := range s.Arms {
		out = append(out, arm.Target.Name)
	}
	if s.Else != nil {
		out = append(out, s.Else.Name)
	}
	return out
}

// orphanedStatements returns the statements no data can ever reach, under the
// RULED reading: the roots are every source, plus any loop label targeted by a
// REACHABLE send.
//
// The loop clause is what makes this a fixpoint rather than a single pass. A
// loop becomes a root only once the send feeding it is itself shown reachable,
// and that send may sit downstream of the loop's own consumer.
func orphanedStatements(t *testing.T, flow FlowDecl) []string {
	t.Helper()

	available := map[string]bool{}
	reached := make([]bool, len(flow.Body))

	for changed := true; changed; {
		changed = false
		for i, stmt := range flow.Body {
			if reached[i] {
				continue
			}
			data, ok := flowOf(stmt)
			if !ok {
				t.Fatalf("statement %d is %T, which this probe cannot read", i, stmt)
			}
			if !isReachable(data, available) {
				continue
			}
			reached[i] = true
			changed = true
			for _, name := range data.outputs {
				available[name] = true
			}
		}
	}

	var orphans []string
	for i, stmt := range flow.Body {
		if !reached[i] {
			orphans = append(orphans, fmt.Sprintf("%T at %s", stmt, stmt.Pos()))
		}
	}
	sort.Strings(orphans)
	return orphans
}

// isReachable applies the root rule to one statement.
func isReachable(data dataflow, available map[string]bool) bool {
	if data.isLoop {
		return available[data.name]
	}
	if len(data.inputs) == 0 {
		return true
	}
	for _, in := range data.inputs {
		if available[in] {
			return true
		}
	}
	return false
}

// preAmendmentToy is toy.flow as it stood before decision a675e321, kept as the
// KNOWN POSITIVE for the probe below.
//
// Without it, "zero orphans across the strawmen" is indistinguishable from a
// probe that cannot find an orphan at all — and this text is the one input
// proven to contain exactly one.
const preAmendmentToy = `flow orders
import "github.com/acme/billing"
import "acme.dev/flows/audit"
source ingest http.Listen[Order](":8080")
branch validate billing.Validate from ingest -> billable, rejected
transform charge billing.Charge from billable
tee split from charge -> ledger, mirror
transform archive audit.Store from rejected, mirror
drop ledger
loop retry
transform redo billing.Backoff from retry
send redo -> charge
`

// TestStrawmenHaveNoOrphanedStatements asserts every canonical example is fully
// reachable, and proves the probe can see an orphan by running it against the
// text that had one.
func TestStrawmenHaveNoOrphanedStatements(t *testing.T) {
	// THE DISCRIMINATING DIRECTION FIRST. A loop label carries no from-clause,
	// so nothing routed into `retry` and the chain below it could never execute.
	// This is the defect the analysis lane's dead-node rule caught in our own
	// canonical fixture.
	before, err := Parse([]byte(preAmendmentToy))
	if err != nil {
		t.Fatalf("the known-positive fixture does not parse: %v", err)
	}
	orphans := orphanedStatements(t, flowAt(t, before, 0))
	if len(orphans) != 3 {
		t.Fatalf("the pre-amendment toy yields %d orphans, want exactly the 3-statement chain: %v",
			len(orphans), orphans)
	}
	t.Logf("pre-amendment toy orphans, as ruled: %v", orphans)

	for _, path := range corpusFiles(t, strawmanDir) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			file, perr := parseCorpusFile(t, path)
			if perr != nil {
				t.Fatalf("a canonical example produced diagnostics: %v", perr)
			}
			for _, decl := range file.Decls {
				flow, isFlow := decl.(FlowDecl)
				if !isFlow {
					continue
				}
				if found := orphanedStatements(t, flow); len(found) > 0 {
					t.Errorf("flow %s carries statements nothing can reach: %v", flow.Name.Name, found)
				}
			}
		})
	}
}

// TestToyStrawmanRetryChainIsReachable pins the amendment itself, so a later
// edit that re-orphans the chain names the reason rather than only the count.
func TestToyStrawmanRetryChainIsReachable(t *testing.T) {
	file, err := parseCorpusFile(t, filepath.Join(strawmanDir, "toy.flow"))
	if err != nil {
		t.Fatalf("toy.flow produced diagnostics: %v", err)
	}
	flow := flowAt(t, file, 0)

	var loops, sendsToLoop, sendsToNode int
	loopNames := map[string]bool{}
	for _, stmt := range flow.Body {
		if loop, ok := stmt.(LoopStmt); ok {
			loops++
			loopNames[loop.Name.Name] = true
		}
	}
	for _, stmt := range flow.Body {
		send, ok := stmt.(SendStmt)
		if !ok {
			continue
		}
		if loopNames[send.Target.Name] {
			sendsToLoop++
			continue
		}
		sendsToNode++
	}

	if loops != 1 {
		t.Fatalf("toy declares %d loop labels, want 1", loops)
	}
	if sendsToLoop != 1 {
		t.Errorf("toy has %d sends targeting a loop label, want 1 — the loop is reachable only through one", sendsToLoop)
	}
	// BOTH SEND FORMS ARE THIS FILE'S CONTRIBUTION. The simpler repair for the
	// orphan — retargeting the existing send at the loop — would have fixed the
	// reachability and silently cost the corpus its only strawman-level example
	// of the node-targeted form.
	if sendsToNode != 1 {
		t.Errorf("toy has %d sends targeting a node, want 1", sendsToNode)
	}
}
