// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lsp

import (
	"context"
	"errors"

	"github.com/whitaker-io/machine/lang/analysis"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

var (
	// errNoClient reports a handler that cannot reach the client it would have
	// published to. Returning nil here would mean computing diagnostics nobody
	// will ever see and reporting success for it.
	errNoClient = errors.New("lsp: no LSP client in this request's context, so diagnostics would reach nobody")
	// errNoContentChange reports a change notification carrying no content.
	errNoContentChange = errors.New("lsp: a didChange notification carried no content change")
	// errRangeChange reports a ranged change against a server that declared
	// full-document sync, which is a client protocol violation rather than
	// something to guess at.
	errRangeChange = errors.New("lsp: a ranged content change arrived, but this server declared full-document sync")
	// errShutDown reports document work arriving after shutdown.
	errShutDown = errors.New("lsp: the server has been shut down and no longer processes document notifications")
)

// DidOpen records an editor's buffer and republishes.
func (s *Server) DidOpen(ctx context.Context, params *protocol.DidOpenTextDocumentParams) error {
	client, err := s.clientFor(ctx)
	if err != nil {
		return err
	}
	s.store.Open(params.TextDocument.URI, []byte(params.TextDocument.Text))
	return s.refresh(ctx, client, "")
}

// DidChange replaces an open document's bytes and republishes.
func (s *Server) DidChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) error {
	client, err := s.clientFor(ctx)
	if err != nil {
		return err
	}
	text, err := wholeDocument(params.ContentChanges)
	if err != nil {
		return err
	}
	s.store.Change(params.TextDocument.URI, []byte(text))
	return s.refresh(ctx, client, "")
}

// DidClose clears the closed document's diagnostics and republishes the rest.
//
// THE EMPTY ARRAY IS THE POINT. LSP treats a publish as replacing a file's
// whole set, so silence leaves the editor rendering stale squiggles for a file
// nobody has open. The closed document is then held OUT of the republish that
// follows, because clearing it and immediately refilling it in the same
// notification would satisfy the letter of the clear and invert its purpose.
func (s *Server) DidClose(ctx context.Context, params *protocol.DidCloseTextDocumentParams) error {
	client, err := s.clientFor(ctx)
	if err != nil {
		return err
	}
	closed := params.TextDocument.URI
	if err := publish(ctx, client, closed, nil); err != nil {
		return err
	}
	s.store.Close(closed)
	return s.refresh(ctx, client, closed.FsPath())
}

// clientFor returns the client this request must publish to, or refuses.
func (s *Server) clientFor(ctx context.Context) (protocol.Client, error) {
	if s.down {
		return nil, errShutDown
	}
	client, ok := protocol.ClientFromContext(ctx)
	if !ok {
		return nil, errNoClient
	}
	return client, nil
}

// refresh re-runs the analysis over every known document and publishes one
// diagnostic set per document, skipping the path named by skip.
//
// ON FAILURE IT PUBLISHES NOTHING AT ALL and returns the error. An empty-array
// publish here would tell the editor every file is clean at exactly the moment
// the server has lost its ability to say, and the previously published state is
// a better thing to leave standing than a confident lie.
func (s *Server) refresh(ctx context.Context, client protocol.Client, skip string) error {
	docs := s.store.Documents()
	snap, err := analyze(docs)
	if err != nil {
		return err
	}
	s.snap = snap

	byPath := groupByPath(snap.Diagnostics)
	for _, doc := range docs {
		if doc.Path == skip {
			continue
		}
		if err := publish(ctx, client, doc.URI, convert(doc, byPath[doc.Path])); err != nil {
			return err
		}
	}
	return nil
}

// publish sends one document's whole diagnostic set.
//
// A nil slice becomes an EMPTY ARRAY rather than a JSON null, because an empty
// array is what clears a file's diagnostics and null is not.
func publish(ctx context.Context, client protocol.Client, u uri.URI, diags []protocol.Diagnostic) error {
	if diags == nil {
		diags = []protocol.Diagnostic{}
	}
	return client.PublishDiagnostics(ctx, &protocol.PublishDiagnosticsParams{URI: u, Diagnostics: diags})
}

// groupByPath buckets a run's diagnostics by the file each is about.
//
// Path rather than position: the driver stamps it from the reporting source
// precisely because every parsed tree starts at offset zero, so a position
// alone cannot name a file.
func groupByPath(diags []analysis.Diagnostic) map[string][]analysis.Diagnostic {
	out := make(map[string][]analysis.Diagnostic, len(diags))
	for _, d := range diags {
		out[d.Path] = append(out[d.Path], d)
	}
	return out
}

// convert renders one document's findings as protocol diagnostics, mapping
// byte positions through that document's own Mapper.
func convert(doc *Document, diags []analysis.Diagnostic) []protocol.Diagnostic {
	out := make([]protocol.Diagnostic, 0, len(diags))
	for _, d := range diags {
		out = append(out, protocol.Diagnostic{
			Range:    protocol.Range{Start: doc.Mapper.ToLSP(d.Pos), End: doc.Mapper.ToLSP(d.End)},
			Severity: lspSeverity(d.Severity),
			Code:     protocol.String(d.Code),
			Source:   protocol.NewOptional(serverName),
			Message:  protocol.String(d.Message),
		})
	}
	return out
}

// lspSeverity maps the analysis vocabulary onto the protocol's, one distinct
// level each.
//
// The default arm renders an unrecognized severity as Information rather than
// Error. A severity this server has not been taught is not evidence that the
// finding is serious, and promoting it would turn a future hint into a red
// squiggle nobody asked for.
func lspSeverity(s analysis.Severity) protocol.DiagnosticSeverity {
	switch s {
	case analysis.SeverityError:
		return protocol.DiagnosticSeverityError
	case analysis.SeverityWarning:
		return protocol.DiagnosticSeverityWarning
	case analysis.SeverityHint:
		return protocol.DiagnosticSeverityHint
	default:
		return protocol.DiagnosticSeverityInformation
	}
}

// wholeDocument is the full text carried by a full-sync change notification.
//
// A ranged change is REFUSED rather than merged: this server declared
// full-document sync, so a range arriving means the client and the server
// disagree about the contract, and guessing at the result would silently
// corrupt the buffer the analysis then runs over.
func wholeDocument(changes []protocol.TextDocumentContentChangeEvent) (string, error) {
	if len(changes) == 0 {
		return "", errNoContentChange
	}
	whole, ok := changes[len(changes)-1].(*protocol.TextDocumentContentChangeWholeDocument)
	if !ok {
		return "", errRangeChange
	}
	return whole.Text, nil
}
