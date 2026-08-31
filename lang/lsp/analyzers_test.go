// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lsp

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whitaker-io/machine/lang/analysis"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// flowTypeflowConflict is lang/analysis/testdata/typeflow/declared-type-disagreement.flow.
// Its sink joins two inputs whose signature-declared spellings differ, which is
// the one shape the typeflow analyzer can decide today.
const flowTypeflowConflict = `flow screening (Order) -> ok OkResult, bad ErrResult
import "github.com/acme/fraud"
import "acme.dev/flows/audit"
branch check fraud.Clean from in -> ok, bad
sink merge audit.Store from ok, bad
`

// roundTripJSON sends a value out through JSON and reads it back, which is what
// a client on the far end of the connection actually receives.
func roundTripJSON(t *testing.T, in any, out any) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshaling the response failed: %v", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshaling the response failed: %v", err)
	}
}

// requestAnalyzers calls flow/analyzers the way the dispatcher does and returns
// what a client would see after the wire.
func requestAnalyzers(t *testing.T, s *Server) AnalyzersResult {
	t.Helper()
	got, err := s.Request(t.Context(), flowAnalyzersMethod, nil)
	if err != nil {
		t.Fatalf("flow/analyzers failed: %v", err)
	}
	var out AnalyzersResult
	roundTripJSON(t, got, &out)
	return out
}

func TestFlowAnalyzersReturnsEveryRegisteredAnalyzerWithItsDoc(t *testing.T) {
	s, _, _ := newSession(t)
	registered := analysis.All()

	// THE CONTROL, and it is not optional: a registry-driven assertion is
	// exactly the shape that passes vacuously against an empty registry.
	if len(registered) == 0 {
		t.Fatal("the analysis registry is empty, so a one-entry-per-analyzer assertion would be vacuous")
	}

	got := requestAnalyzers(t, s)
	if len(got.Analyzers) != len(registered) {
		t.Fatalf("flow/analyzers reported %d analyzers, want the %d the registry holds",
			len(got.Analyzers), len(registered))
	}

	docs := map[string]string{}
	for _, a := range got.Analyzers {
		docs[a.Name] = a.Doc
	}
	for _, a := range registered {
		doc, ok := docs[a.Name]
		if !ok {
			t.Fatalf("the registered analyzer %q is missing from the response", a.Name)
		}
		if doc != a.Doc {
			t.Fatalf("the Doc for %q was altered on the way out:\n got  %q\n want %q", a.Name, doc, a.Doc)
		}
		if doc == "" {
			t.Fatalf("the registered analyzer %q reported an empty Doc", a.Name)
		}
	}
}

func TestTypeflowsDisclosureSurvivesTheWire(t *testing.T) {
	s, _, _ := newSession(t)

	// THE EXPECTATION IS READ FROM THE REGISTRY IN THE SAME RUN, not copied
	// into a literal here. A literal would be an identity check whose subject
	// supplies its own answer key, and it would false-red the moment
	// lang/analysis reworded the disclosure.
	var want string
	for _, a := range analysis.All() {
		if a.Name == "typeflow" {
			want = a.Doc
		}
	}
	if want == "" {
		t.Fatal("the analysis registry has no typeflow analyzer, so this test could assert nothing")
	}
	// The disclosure this endpoint exists to carry must actually be in there,
	// or "survived the wire" would be a claim about an empty payload.
	if !strings.Contains(strings.ToLower(want), "is not type checking") {
		t.Fatalf("typeflow's Doc no longer carries its not-type-checking disclosure: %q", want)
	}

	got := requestAnalyzers(t, s)
	for _, a := range got.Analyzers {
		if a.Name != "typeflow" {
			continue
		}
		if a.Doc != want {
			t.Fatalf("typeflow's Doc did not survive the wire intact:\n got  %q\n want %q", a.Doc, want)
		}
		if !strings.Contains(strings.ToLower(a.Doc), "silence is not agreement") {
			t.Fatalf("the disclosure was truncated on the way out: %q", a.Doc)
		}
		return
	}
	t.Fatal("the response carries no typeflow entry")
}

func TestFlowAnalyzersCarriesTheScalingDisclosure(t *testing.T) {
	s, _, _ := newSession(t)

	got := requestAnalyzers(t, s)
	// THE MUTANT IS LEAVING THE DISCLOSURE IN A COMMENT, where no consumer and
	// no gate can reach it. A disclosure a consumer cannot read is not one.
	if got.Scaling == "" {
		t.Fatal("flow/analyzers carries no scaling disclosure")
	}
	if got.Scaling != ScalingDisclosure {
		t.Fatalf("the scaling disclosure was altered on the way out:\n got  %q\n want %q",
			got.Scaling, ScalingDisclosure)
	}
	if !strings.Contains(strings.ToUpper(got.Scaling), "LINEAR IN TOTAL WORKSPACE BYTES") {
		t.Fatalf("the scaling disclosure no longer states what the cost is linear in: %q", got.Scaling)
	}
}

func TestTypeflowDiagnosticsKeepTheirOwnCode(t *testing.T) {
	s, client, ctx := newSession(t)
	u := uri.File(filepath.Join(t.TempDir(), "screening.flow"))

	if err := s.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: u, Text: flowTypeflowConflict},
	}); err != nil {
		t.Fatalf("DidOpen failed: %v", err)
	}

	published := client.forURI(u)
	if len(published) != 1 {
		t.Fatalf("opening the fixture produced %d publishes, want 1", len(published))
	}

	// A typeflow finding reaches the editor under the analyzer's OWN name. The
	// driver stamps Code from the reporting analyzer precisely so one cannot
	// emit under a foreign code; relabeling it here — as a "type error", or
	// dropping it into a friendlier category — would present a structural
	// spelling comparison as something it is not.
	var found bool
	for _, d := range published[0].Diagnostics {
		if d.Code == protocol.String("typeflow") {
			found = true
		}
		if d.Code == nil {
			t.Fatalf("a published diagnostic carries no Code at all: %v", d.Message)
		}
	}
	if !found {
		t.Fatalf("no published diagnostic carries Code \"typeflow\"; the codes present were %v",
			codesOf(published[0].Diagnostics))
	}

	// CONTROL: the analyzers really do report typeflow for this fixture, so the
	// assertion above is about the CONVERSION rather than about the fixture.
	docs := s.store.Documents()
	srcs, _ := sourceSet(docs)
	raw, err := analysis.Run(srcs, analysis.All())
	if err != nil {
		t.Fatalf("the control run failed: %v", err)
	}
	var rawTypeflow int
	for _, d := range raw {
		if d.Code == "typeflow" {
			rawTypeflow++
		}
	}
	if rawTypeflow == 0 {
		t.Fatal("the control failed: the analyzers report no typeflow finding for this fixture, " +
			"so the assertion above could not have distinguished a relabeling conversion")
	}
}

// codesOf renders the codes present on a published set, for a failure message.
func codesOf(diags []protocol.Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		if s, ok := d.Code.(protocol.String); ok {
			out = append(out, string(s))
		}
	}
	return out
}
