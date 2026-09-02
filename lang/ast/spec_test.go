// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ast

import (
	"os"
	"regexp"
	"slices"
	"testing"
)

// specPath is the in-context syntax guide this package ships beside the grammar.
const specPath = "SPEC.md"

// specFence matches a fenced block opened by three backticks and the word flow,
// and captures its body.
//
// A ```text fence is deliberately NOT matched. Those blocks hold illustrative
// fragments rather than whole files, and a fragment cannot be held to the
// parser; a block that claims to be flow source is.
var specFence = regexp.MustCompile("(?s)```flow\n(.*?)```")

// specExamples returns every flow example the spec quotes, in document order.
func specExamples(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("reading %s: %v", specPath, err)
	}
	var out []string
	for _, match := range specFence.FindAllStringSubmatch(string(raw), -1) {
		out = append(out, match[1])
	}

	return out
}

// statementForms returns the statement production names the grammar admits.
//
// The set is DERIVED from the notation rather than written down here, so a
// statement form added to the language widens what the spec must demonstrate
// without anyone remembering to update a list.
func statementForms(t *testing.T) []string {
	t.Helper()
	doc := loadGrammar(t)
	production, ok := doc.productions["Statement"]
	if !ok {
		t.Fatalf("%s carries no Statement production, so the wanted set could not be derived", grammarPath)
	}
	forms := namesIn(production)
	slices.Sort(forms)

	return forms
}

// stmtForm maps a parsed statement to the grammar's own production name, and
// returns the empty string for anything the notation does not name — a recovered
// BadStmt above all.
func stmtForm(s Stmt) string {
	switch s.(type) {
	case SourceStmt:
		return "Source"
	case TransformStmt:
		return "Transform"
	case BranchStmt:
		return "Branch"
	case SwitchStmt:
		return "Switch"
	case TeeStmt:
		return "Tee"
	case SinkStmt:
		return "Sink"
	case DropStmt:
		return "Drop"
	case LoopStmt:
		return "Loop"
	case SendStmt:
		return "Send"
	case UseStmt:
		return "Use"
	default:
		return ""
	}
}

// TestSpecExamplesParse asserts every flow example the spec quotes is a complete
// file this package's own parser accepts.
//
// The spec is handed to a model as the language's whole in-context definition,
// so an example that does not parse teaches a form the language does not have.
func TestSpecExamplesParse(t *testing.T) {
	examples := specExamples(t)
	if len(examples) == 0 {
		t.Fatalf("CONTROL FAILED: %s quotes no flow example at all, so this test would pass without checking anything", specPath)
	}

	for i, src := range examples {
		if _, err := Parse([]byte(src)); err != nil {
			t.Errorf("example %d does not parse: %v\nthe offending source:\n%s", i+1, err, src)
		}
	}

	// The count LOGGED is the count CHECKED, never a claim that they passed: a
	// line saying they parsed clean prints just the same when one of them did not.
	t.Logf("flow examples checked: %d", len(examples))
}

// TestSpecExamplesCoverEveryStatementForm asserts the spec demonstrates every
// statement form the grammar admits.
//
// Both sides are derived at run time — the wanted set from the Statement
// production, the seen set by parsing the examples — so neither pins a count that
// would go stale the moment the language grows a form.
func TestSpecExamplesCoverEveryStatementForm(t *testing.T) {
	want := statementForms(t)
	if len(want) == 0 {
		t.Fatalf("CONTROL FAILED: no statement form was derived from %s, so nothing would be required", grammarPath)
	}

	seen := make(map[string]bool, len(want))
	for _, src := range specExamples(t) {
		file, _ := Parse([]byte(src))
		for _, decl := range file.Decls {
			flow, ok := decl.(FlowDecl)
			if !ok {
				continue
			}
			for _, stmt := range flow.Body {
				if form := stmtForm(stmt); form != "" {
					seen[form] = true
				}
			}
		}
	}

	var missing []string
	for _, form := range want {
		if !seen[form] {
			missing = append(missing, form)
		}
	}
	if len(missing) > 0 {
		t.Errorf("the spec's examples exercise no %v, against the %d forms the grammar admits: %v",
			missing, len(want), want)
	}
}
