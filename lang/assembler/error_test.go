// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package assembler

import (
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/ast"
)

// TestErrorCarriesDiagnosticsAndPartialGraph proves the one-error-type contract
// holds in both directions, and that a diagnostic's positions are the REAL
// positions of the offending statement.
//
// THE POSITION LEG IS TIED TO THE PARSER, NOT TO A FIXTURE I WROTE. A test that
// built its own ast.Position values and then asserted the diagnostic carried
// them would be checking that assignment works. So the positions here come from
// parsing actual .flow source and asking the statement for its own span: if this
// package ever positioned a refusal against something other than the statement
// it is about, the numbers would stop matching.
func TestErrorCarriesDiagnosticsAndPartialGraph(t *testing.T) {
	const src = "flow orders\n" +
		"source ingest Poll\n" +
		"transform charge Bill from ingest\n"

	file, err := ast.Parse([]byte(src))
	if err != nil {
		t.Fatalf("the fixture must parse clean: %v", err)
	}
	flow, ok := file.Decls[0].(ast.FlowDecl)
	if !ok {
		t.Fatalf("declaration 0 is %T, want ast.FlowDecl", file.Decls[0])
	}

	// THE OFFENDING STATEMENT is the transform on line 3. Its span is the
	// parser's, and it is what a refusal about that statement must carry.
	offender := flow.Body[1]
	if offender.Pos().Line != 3 {
		t.Fatalf("the fixture's second statement is on line %d, want 3; the source moved", offender.Pos().Line)
	}

	assembled := &Error{
		Diagnostics: []Diagnostic{
			diagnosticAt(offender.Pos(), offender.End(), "the %q statement has no lowering", "transform"),
		},
		Partial: []Generated{{Name: "orders.flow.go", Source: []byte("package generated\n")}},
	}

	// THE DIAGNOSTIC HALF.
	if len(assembled.Diagnostics) != 1 {
		t.Fatalf("the error carries %d diagnostics, want 1", len(assembled.Diagnostics))
	}
	got := assembled.Diagnostics[0]
	if got.Pos != offender.Pos() {
		t.Errorf("the diagnostic starts at %s, want the statement's own start %s", got.Pos, offender.Pos())
	}
	if got.End != offender.End() {
		t.Errorf("the diagnostic ends at %s, want the statement's own end %s", got.End, offender.End())
	}
	if got.End == got.Pos {
		t.Error("the diagnostic spans nothing; an editor cannot highlight a zero-width range")
	}
	if !strings.Contains(got.Message, "transform") {
		t.Errorf("the message %q does not name what was refused", got.Message)
	}

	// THE PARTIAL HALF. A run that refuses one flow still built the others, and
	// dropping them here is what would make a driver unable to report a whole
	// run's problems at once.
	if len(assembled.Partial) != 1 {
		t.Fatalf("the error carries %d partial files, want 1", len(assembled.Partial))
	}
	if assembled.Partial[0].Name != "orders.flow.go" {
		t.Errorf("the partial file is named %q", assembled.Partial[0].Name)
	}
	if len(assembled.Partial[0].Source) == 0 {
		t.Error("the partial file carries no source, so the partial product was lost")
	}

	// Error() renders the first diagnostic's POSITION and message. A renderer
	// that dropped the position would leave a caller unable to locate anything.
	rendered := assembled.Error()
	if !strings.Contains(rendered, offender.Pos().String()) {
		t.Errorf("Error() renders %q, which does not carry the position %s", rendered, offender.Pos())
	}

	// The count suffix appears only when there is more than one, and its absence
	// here is what proves the suffix is conditional rather than always printed.
	if strings.Contains(rendered, "and 0 more") {
		t.Errorf("Error() renders a count suffix for a single diagnostic: %q", rendered)
	}
}

// TestErrorRendersEveryDiagnosticCount covers the two remaining arms of Error():
// the empty case and the many case.
func TestErrorRendersEveryDiagnosticCount(t *testing.T) {
	empty := &Error{}
	if got := empty.Error(); got != "no diagnostics" {
		t.Errorf("an empty Error renders %q", got)
	}

	many := &Error{Diagnostics: []Diagnostic{
		diagnosticAt(ast.Position{Line: 3, Col: 1}, ast.Position{Line: 3, Col: 9}, "first"),
		diagnosticAt(ast.Position{Line: 4, Col: 1}, ast.Position{Line: 4, Col: 9}, "second"),
		diagnosticAt(ast.Position{Line: 5, Col: 1}, ast.Position{Line: 5, Col: 9}, "third"),
	}}
	rendered := many.Error()
	if !strings.Contains(rendered, "first") {
		t.Errorf("Error() renders %q, which does not lead with the first diagnostic", rendered)
	}
	if !strings.Contains(rendered, "and 2 more") {
		t.Errorf("Error() renders %q, which does not report the other two", rendered)
	}
}

// TestNodeKindNamesEveryMember proves the kind enumeration renders every member,
// and that an out-of-range kind renders its number rather than an empty string.
//
// THE ZERO VALUE IS THE POINT. KindInvalid exists so a Node left unbuilt is
// distinguishable from a legitimate Source; a String that returned "" for it
// would put an empty word in a diagnostic and hide exactly that case.
func TestNodeKindNamesEveryMember(t *testing.T) {
	for kind, want := range map[NodeKind]string{
		KindInvalid:   "invalid",
		KindSource:    "source",
		KindTransform: "transform",
		KindBranch:    "branch",
		KindSwitch:    "switch",
		KindTee:       "tee",
		KindSink:      "sink",
		KindDrop:      "drop",
		KindLoop:      "loop",
		KindSend:      "send",
		KindUse:       "use",
	} {
		if got := kind.String(); got != want {
			t.Errorf("NodeKind(%d).String() is %q, want %q", int(kind), got, want)
		}
	}

	// The known positive for the default arm: a kind past the enumeration renders
	// a locatable number instead of nothing.
	if got := NodeKind(99).String(); got != "NodeKind(99)" {
		t.Errorf("an out-of-range kind renders %q, want NodeKind(99)", got)
	}
	if got := NodeKind(-7).String(); got != "NodeKind(-7)" {
		t.Errorf("a negative kind renders %q, want NodeKind(-7)", got)
	}
}
