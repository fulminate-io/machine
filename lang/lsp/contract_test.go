// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package lsp

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/whitaker-io/machine/lang/analysis"
	"github.com/whitaker-io/machine/lang/ast"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// These pin behaviors the plan mandates in prose and the feature tests reach
// only incidentally: the refusal arms, the bounds, and the fall-through. Each
// one is a sentence in a step description that would otherwise have no test
// going red when it is violated.

func TestExitReturnsNil(t *testing.T) {
	s, _, ctx := newSession(t)
	// Exit is a NOTIFICATION. The dispatcher treats a non-nil error from one as
	// a connection-level failure and tears the connection down, so returning an
	// error here would kill the session on the way out of it.
	if err := s.Exit(ctx); err != nil {
		t.Fatalf("Exit returned %v; a notification handler must not report an error", err)
	}
}

func TestAnUnknownFlowMethodIsRefusedAsMethodNotFound(t *testing.T) {
	s, _, ctx := newSession(t)

	got, err := s.Request(ctx, "flow/doesNotExist", protocol.LSPAny("{}"))
	if err == nil {
		t.Fatalf("an unknown method answered (%v, nil) instead of being refused", got)
	}
	if got != nil {
		t.Fatalf("an unknown method returned a non-nil result %v alongside its error", got)
	}

	// KNOWN POSITIVE: a method this server DOES answer still answers, so the
	// refusal above is about the method name and not about Request itself.
	if _, ok := s.Request(ctx, flowAnalyzersMethod, nil); ok != nil {
		t.Fatalf("the control failed: flow/analyzers returned %v", ok)
	}
}

func TestFlowGuidanceRefusesParamsThatAreNotJSON(t *testing.T) {
	s, _, ctx := newSession(t)

	// The dispatcher hands a non-standard method's params through as opaque
	// JSON. Anything else is a malformed request and errors rather than being
	// coerced into an empty position.
	if _, err := s.Request(ctx, flowGuidanceMethod, 42); err == nil {
		t.Fatal("flow/guidance accepted params that are not JSON")
	}
	if _, err := s.Request(ctx, flowGuidanceMethod, protocol.LSPAny("not json at all")); err == nil {
		t.Fatal("flow/guidance accepted params that do not parse as JSON")
	}
}

func TestAnUnknownSeverityRendersAsInformationRatherThanError(t *testing.T) {
	// A severity this server has not been taught is not evidence the finding is
	// serious. Promoting it would turn a future hint into a red squiggle.
	got := lspSeverity(analysis.Severity(99))
	if got == protocol.DiagnosticSeverityError {
		t.Fatal("an unrecognized severity was promoted to Error")
	}
	if got != protocol.DiagnosticSeverityInformation {
		t.Fatalf("an unrecognized severity rendered as %v, want Information", got)
	}
}

func TestProtocolCoordinatesAreBoundedOnBothSides(t *testing.T) {
	if _, ok := toUint32(-1); ok {
		t.Fatal("toUint32 accepted a negative value")
	}
	if _, ok := toUint32(math.MaxUint32 + 1); ok {
		t.Fatal("toUint32 accepted a value above the protocol's uint32 field")
	}
	if got, ok := toUint32(7); !ok || got != 7 {
		t.Fatalf("the control failed: toUint32(7) = (%d, %v)", got, ok)
	}

	// toInt's bound is only reachable where int is narrower than uint32, so on
	// a 64-bit build the in-range leg is the whole assertion. Stated rather than
	// dressed up as a case this platform can exercise.
	if got, ok := toInt(9); !ok || got != 9 {
		t.Fatalf("toInt(9) = (%d, %v)", got, ok)
	}
}

func TestToLSPRefusesAPositionTheSourceDoesNotHave(t *testing.T) {
	m := NewMapper([]byte(mixedWidthSource))

	for _, bad := range []ast.Position{
		{Line: 0, Col: 1},   // lines are 1-based
		{Line: 99, Col: 1},  // past the end
		{Line: 1, Col: 0},   // columns are 1-based
		{Line: -3, Col: -3}, // negative on both axes
	} {
		if got := m.ToLSP(bad); got != (protocol.Position{}) {
			t.Fatalf("ToLSP(%+v) = %+v, want the zero Position for a place that does not exist", bad, got)
		}
	}

	// KNOWN POSITIVE: a real position still converts, so the zeros above are
	// about the inputs rather than a mapper that answers nothing.
	if got := m.ToLSP(ast.Position{Line: 1, Col: 5}); got == (protocol.Position{}) {
		t.Fatal("the control failed: a real position also converted to the zero Position")
	}
}

func TestInitializeSurfacesAWorkspaceScanFailure(t *testing.T) {
	s, _, ctx := newSession(t)
	missing := filepath.Join(t.TempDir(), "no-such-directory")

	_, err := s.Initialize(ctx, &protocol.InitializeParams{
		WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
			WorkspaceFolders: protocol.NewNullable([]protocol.WorkspaceFolder{
				{URI: uri.File(missing), Name: "missing"},
			}),
		},
	})
	// A folder the client named and the server cannot read is reported, not
	// silently treated as an empty workspace — that would leave every cross-file
	// answer quietly incomplete with no signal anywhere.
	if err == nil {
		t.Fatal("Initialize reported success for a workspace folder it could not scan")
	}
}

func TestDefinitionAndCompletionAnswerNothingBeforeAnyAnalysis(t *testing.T) {
	s := NewServer(NewStore())
	u := uri.File(filepath.Join(t.TempDir(), "alpha.flow"))

	// No document has been opened, so there is no snapshot. Both handlers must
	// answer emptily rather than dereferencing a nil table.
	got, err := s.Completion(t.Context(), &protocol.CompletionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: u},
		},
	})
	if err != nil {
		t.Fatalf("Completion before any analysis returned %v", err)
	}
	if items, ok := got.(protocol.CompletionItemSlice); !ok || len(items) != 0 {
		t.Fatalf("Completion before any analysis answered %v", got)
	}

	loc, err := s.Definition(t.Context(), &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: u},
		},
	})
	if err != nil {
		t.Fatalf("Definition before any analysis returned %v", err)
	}
	if loc != nil {
		t.Fatalf("Definition before any analysis answered %v", loc)
	}
}
