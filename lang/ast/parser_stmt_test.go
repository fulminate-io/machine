// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import (
	"fmt"
	"strings"
	"testing"
)

// TestParseEveryStatementShape covers all NINE shapes this step parses,
// including the two bare ones, and both goRef forms.
//
// A goRef is either a BARE name or a QUALIFIED one and the parser treats them
// identically: it records the span and BINDS NOTHING. Whether a bare name
// resolves to a local func and a qualified one to an import is the analysis
// engine's question, so the parser must not special-case bare names and must not
// treat an unresolvable one as a syntax error.
func TestParseEveryStatementShape(t *testing.T) {
	file := mustParse(t, `flow all
source ingest Poll
transform enrich Lookup from ingest
transform backoff ratelimit.Wait from enrich
branch split Valid from backoff -> good, bad
tee fan from good -> a, b
sink out Write from a
drop bad
loop retry
send b -> retry
use sub other.Flow from a -> x, y
`)

	body := flowAt(t, file, 0).Body
	want := []string{
		"ast.SourceStmt", "ast.TransformStmt", "ast.TransformStmt", "ast.BranchStmt",
		"ast.TeeStmt", "ast.SinkStmt", "ast.DropStmt", "ast.LoopStmt", "ast.SendStmt",
		"ast.UseStmt",
	}
	if len(body) != len(want) {
		t.Fatalf("parsed %d statements, want %d", len(body), len(want))
	}
	for i, wantType := range want {
		if got := fmt.Sprintf("%T", body[i]); got != wantType {
			t.Errorf("statement %d is %s, want %s", i, got, wantType)
		}
	}

	bare := body[1].(TransformStmt)
	if bare.Ref.Text != "Lookup" {
		t.Errorf("bare goRef is %q, want Lookup", bare.Ref.Text)
	}
	qualified := body[2].(TransformStmt)
	if qualified.Ref.Text != "ratelimit.Wait" {
		t.Errorf("qualified goRef is %q, want ratelimit.Wait", qualified.Ref.Text)
	}

	branch := body[3].(BranchStmt)
	if branch.TrueTarget.Name != "good" || branch.FalseTarget.Name != "bad" {
		t.Errorf("branch targets are %q and %q, want good and bad", branch.TrueTarget.Name, branch.FalseTarget.Name)
	}
	tee := body[4].(TeeStmt)
	if len(tee.Targets) != 2 {
		t.Errorf("tee has %d targets, want 2", len(tee.Targets))
	}

	// THE TWO BARE SHAPES. A drop names the input it CONSUMES rather than an
	// output, because it is terminal.
	drop := body[6].(DropStmt)
	if drop.Input.Name != "bad" {
		t.Errorf("drop consumes %q, want bad", drop.Input.Name)
	}
	loop := body[7].(LoopStmt)
	if loop.Name.Name != "retry" {
		t.Errorf("loop is named %q, want retry", loop.Name.Name)
	}

	send := body[8].(SendStmt)
	if send.Source.Name != "b" || send.Target.Name != "retry" {
		t.Errorf("send is %q -> %q, want b -> retry", send.Source.Name, send.Target.Name)
	}
}

// TestParseGoSpansAreVerbatim asserts a Go fragment is captured byte for byte
// with an exact span, including fragments whose commas sit inside brackets and
// so must not terminate the span.
func TestParseGoSpansAreVerbatim(t *testing.T) {
	src := `flow g
source ingest Poll
transform mapped mapper[K, V] from ingest
transform wrapped wrap(func(a, b int) error) from mapped
var cb func(a, b int) error
sink out http.Listen[Order](":8080") from wrapped
`
	file := mustParse(t, src)
	flow := flowAt(t, file, 0)

	// A var declaration is a flow entry rather than a body statement, so the
	// body holds only the source, the two transforms and the sink.
	if len(flow.Body) != 4 {
		t.Fatalf("flow body has %d statements, want 4", len(flow.Body))
	}

	for _, tc := range []struct {
		index int
		want  string
	}{
		{1, "mapper[K, V]"},
		{2, "wrap(func(a, b int) error)"},
		{3, `http.Listen[Order](":8080")`},
	} {
		var got GoSpan
		switch stmt := flow.Body[tc.index].(type) {
		case TransformStmt:
			got = stmt.Ref
		case SinkStmt:
			got = stmt.Ref
		default:
			t.Fatalf("statement %d is %T, which carries no Go reference", tc.index, stmt)
		}
		if got.Text != tc.want {
			t.Errorf("span is %q, want %q", got.Text, tc.want)
		}
		// The span must be addressed exactly, not approximately: slicing the
		// source at its offsets has to reproduce the captured text.
		if sliced := src[got.Start.Offset:got.Stop.Offset]; sliced != tc.want {
			t.Errorf("span offsets cover %q, but the captured text is %q", sliced, tc.want)
		}
	}

	if got := flow.Vars[0].Type.Text; got != "func(a, b int) error" {
		t.Errorf("var type span is %q, want %q", got, "func(a, b int) error")
	}
}

// TestParseClauseContinuationAcrossNewlines covers the continuation rule with
// ALL SIX clause keywords on continuation lines.
//
// It also pins the shadowing rule: once a statement has been parsed, a line
// opening with `on` is that statement's clause and NOT a sibling flow-level
// declaration. A parser that gets this wrong builds a wrong tree from source
// that parses without complaint, which is why the flow's own OnError is asserted
// nil here.
func TestParseClauseContinuationAcrossNewlines(t *testing.T) {
	file := mustParse(t, `flow c
source ingest Poll
transform enrich Lookup from ingest
	reads seen
	writes seen, tally
	over ratelimit.New(10)
	checkpoint
	on error Handle
	note """continued across newlines"""
drop enrich
`)

	flow := flowAt(t, file, 0)
	if len(flow.Body) != 3 {
		t.Fatalf("parsed %d statements, want 3; a continuation line was read as a statement", len(flow.Body))
	}

	enrich, ok := flow.Body[1].(TransformStmt)
	if !ok {
		t.Fatalf("statement 1 is %T, want TransformStmt", flow.Body[1])
	}
	if len(enrich.Reads) != 1 || enrich.Reads[0].Name != "seen" {
		t.Errorf("reads clause is %v", enrich.Reads)
	}
	if len(enrich.Writes) != 2 {
		t.Errorf("writes clause has %d names, want 2", len(enrich.Writes))
	}
	if enrich.Over == nil || enrich.Over.Text != "ratelimit.New(10)" {
		t.Errorf("over clause is %v", enrich.Over)
	}
	if enrich.Checkpoint == nil {
		t.Errorf("checkpoint clause was not recorded")
	}
	if enrich.OnError == nil || enrich.OnError.Text != "Handle" {
		t.Fatalf("the post-statement `on error` did not land in Clauses.OnError: %v", enrich.OnError)
	}
	if enrich.Note == nil || enrich.Note.Text != "continued across newlines" {
		t.Errorf("note clause is %v", enrich.Note)
	}

	// THE SHADOWING ASSERTION. The `on error` line came AFTER a statement, so it
	// belongs to that statement and the flow must carry no error handler of its
	// own.
	if flow.OnError != nil {
		t.Errorf("the statement's `on error` was misrouted to the flow level: %v", flow.OnError)
	}

	// A following line opening with a NON-clause keyword terminates the
	// statement rather than continuing it.
	if _, isDrop := flow.Body[2].(DropStmt); !isDrop {
		t.Errorf("the line after the clauses is %T, want DropStmt", flow.Body[2])
	}
}

// TestParseCheckpointIsBare asserts both directions of the clause's zero arity.
//
// The negative case is the one that matters: a clause loop that simply continues
// after a bare keyword reads the operand as the start of the next clause and
// reports nothing at all.
func TestParseCheckpointIsBare(t *testing.T) {
	file := mustParse(t, "flow c\nsource ingest Poll\ntransform t Foo from ingest checkpoint\n")
	stmt := flowAt(t, file, 0).Body[1].(TransformStmt)
	if stmt.Checkpoint == nil {
		t.Fatalf("the bare checkpoint clause was not recorded")
	}
	if stmt.Checkpoint.Line != 3 {
		t.Errorf("checkpoint recorded at %s, want line 3", stmt.Checkpoint)
	}

	diags := diagnosticsFor(t, "flow c\nsource ingest Poll\ntransform t Foo from ingest checkpoint enriched\n")
	requireDiagnostic(t, diags, "takes no operand")
}

// TestParseIdempotentIsBare covers the marker that SELECTS THE CHECKPOINT ANCHOR,
// in the three positions that can each fail on their own.
//
// THE AFTER-A-GO-SPAN ARM IS THE ONE THAT EARNS ITS PLACE. Two tables state the
// reserved spellings — the keyword inventory and the Go span scanner's stop set —
// and a keyword present in the first but missing from the second is swallowed into
// the preceding span with NO diagnostic at all. The clause is dropped in silence
// and the node falls back to the unmarked completion default, which is the opposite
// of what the author declared. No landed census exercises that position, and
// whether the corpus fixture happens to is decided by where an implementer puts the
// token, so it is pinned here instead of left to chance.
func TestParseIdempotentIsBare(t *testing.T) {
	file := mustParse(t, "flow c\nsource ingest Poll\ntransform t Foo from ingest idempotent\n")
	stmt := flowAt(t, file, 0).Body[1].(TransformStmt)
	if stmt.Idempotent == nil {
		t.Fatalf("the bare idempotent clause was not recorded")
	}
	if stmt.Idempotent.Line != 3 {
		t.Errorf("idempotent recorded at %s, want line 3", stmt.Idempotent)
	}

	// AFTER A GO SPAN: the span must STOP at the marker rather than swallowing it.
	spanned := mustParse(t,
		"flow c\nsource ingest Poll\ntransform t Foo from ingest over ratelimit.New(10) idempotent\n")
	spannedStmt := flowAt(t, spanned, 0).Body[1].(TransformStmt)
	if spannedStmt.Idempotent == nil {
		t.Fatal("idempotent following a Go span was not recorded: the span scanner swallowed the marker, " +
			"so the clause is dropped in silence and the node falls back to the unmarked default")
	}
	// CONTROL: the span itself survives and ends at the expression, so the stop did
	// not eat the Go code it was scanning.
	if spannedStmt.Over == nil {
		t.Fatal("CONTROL FAILED: the over clause was lost entirely, so this arm is not measuring the stop set")
	}
	if !strings.Contains(spannedStmt.Over.Text, "ratelimit.New(10)") {
		t.Errorf("the Go span reads %q, want it to end at the expression", spannedStmt.Over.Text)
	}
	if strings.Contains(spannedStmt.Over.Text, textIdempotent) {
		t.Errorf("the Go span swallowed the marker: %q", spannedStmt.Over.Text)
	}
	t.Logf("the marker after a Go span is recorded and the span ends at %q", spannedStmt.Over.Text)

	// AN OPERAND IS A DIAGNOSTIC, on the same zero-arity rule checkpoint states.
	diags := diagnosticsFor(t, "flow c\nsource ingest Poll\ntransform t Foo from ingest idempotent twice\n")
	requireDiagnostic(t, diags, "takes no operand")
}

// TestParseUseBindsOutputsPositionally covers the use embedding.
//
// The bindings are caller-chosen names for the embedded flow's outputs, matched
// by POSITION, so more than one is required to tell a positional implementation
// from one that only ever handles the first.
func TestParseUseBindsOutputsPositionally(t *testing.T) {
	file := mustParse(t, `flow caller
source ingest Poll
use sub payments.Enrich from ingest -> ok, retryable, failed
sink out Write from ok
`)

	stmt, ok := flowAt(t, file, 0).Body[1].(UseStmt)
	if !ok {
		t.Fatalf("statement 1 is %T, want UseStmt", flowAt(t, file, 0).Body[1])
	}
	if stmt.Instance.Name != "sub" {
		t.Errorf("instance is %q, want sub", stmt.Instance.Name)
	}

	wantPath := []string{"payments", "Enrich"}
	if len(stmt.Flow) != len(wantPath) {
		t.Fatalf("flow reference has %d segments, want %d", len(stmt.Flow), len(wantPath))
	}
	for i, want := range wantPath {
		if stmt.Flow[i].Name != want {
			t.Errorf("flow reference segment %d is %q, want %q", i, stmt.Flow[i].Name, want)
		}
	}

	wantBindings := []string{"ok", "retryable", "failed"}
	if len(stmt.Bindings) != len(wantBindings) {
		t.Fatalf("got %d bindings, want %d", len(stmt.Bindings), len(wantBindings))
	}
	for i, want := range wantBindings {
		if stmt.Bindings[i].Name != want {
			t.Errorf("binding %d is %q, want %q — bindings are positional", i, stmt.Bindings[i].Name, want)
		}
	}
}

// TestParseDuplicateClauseIsDiagnosed covers the at-most-once rule, which the
// notation cannot express because the clauses are order-free.
func TestParseDuplicateClauseIsDiagnosed(t *testing.T) {
	diags := diagnosticsFor(t, "flow c\nsource ingest Poll\ntransform t Foo from ingest reads a reads b\n")
	requireDiagnostic(t, diags, "already given")

	// The control: the same clause set given once is clean, so the rule is not
	// simply rejecting every repeated keyword it sees.
	mustParse(t, "flow c\nsource ingest Poll\ntransform t Foo from ingest reads a writes b\n")
}

// TestParseOnErrorRejectsAnArrow covers the rule that error handling is a
// declaration and never an edge, at BOTH the flow level and the clause level.
//
// The notation cannot express this: an arrow appears in neither FIRST nor FOLLOW
// of the clause set, so the span's stop set holds none and `-> handler` scans as
// a perfectly good non-empty span terminated by the newline.
func TestParseOnErrorRejectsAnArrow(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"flow level", "flow c\non error -> Handle\nsource ingest Poll\n"},
		{"node clause", "flow c\nsource ingest Poll\ntransform t Foo from ingest on error -> Handle\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireDiagnostic(t, diagnosticsFor(t, tc.src), "not an edge")
		})
	}

	for _, tc := range []struct {
		name string
		src  string
	}{
		{"flow level", "flow c\non error\nsource ingest Poll\n"},
		{"node clause", "flow c\nsource ingest Poll\ntransform t Foo from ingest on error\n"},
	} {
		t.Run("empty "+tc.name, func(t *testing.T) {
			requireDiagnostic(t, diagnosticsFor(t, tc.src), "needs a handler reference")
		})
	}

	// The control: a handler reference with no arrow is clean at both levels.
	mustParse(t, "flow c\non error Handle\nsource ingest Poll\ntransform t Foo from ingest on error Other\n")
}
