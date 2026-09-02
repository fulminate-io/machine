// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package lsp

import (
	"os"
	"path/filepath"
	"testing"

	"go.lsp.dev/uri"
)

const (
	flowAlpha = `flow alpha
source ingest Poll
sink done Store from ingest
`
	flowAlphaEdited = `flow alpha
source ingest Poll
transform step Step from ingest
sink done Store from step
`
	flowBeta = `flow beta
source seed Poll
sink out Store from seed
`
	// flowDamaged is lang/ast/testdata/broken/missing-arrow-target.flow: a
	// branch whose arrow names no target. It parses to a tree AND an error,
	// which is the pair every damaged-document assertion in this module needs.
	flowDamaged = `flow payments
source ingest Poll
branch split Valid from ingest ->
sink out Write from ingest
`
)

// writeFlow puts one source on disk and returns its path.
func writeFlow(t *testing.T, dir, name, src string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// docFor finds the document a store holds for a path, failing if it has none.
func docFor(t *testing.T, s *Store, path string) *Document {
	t.Helper()
	for _, doc := range s.Documents() {
		if doc.Path == path {
			return doc
		}
	}
	t.Fatalf("the store holds no document for %s", path)
	return nil
}

func TestTheWorkspaceIsScannedOncePerInitializeAndNotPerChange(t *testing.T) {
	dir := t.TempDir()
	alpha := writeFlow(t, dir, "alpha.flow", flowAlpha)

	s := NewStore()
	if err := s.Scan(dir); err != nil {
		t.Fatalf("the initial scan failed: %v", err)
	}
	if got := len(s.Documents()); got != 1 {
		t.Fatalf("the initial scan found %d documents, want 1", got)
	}

	// A SECOND FILE APPEARS ON DISK AFTER THE SCAN. Nothing done per change is
	// allowed to notice it: that is the whole assertion.
	writeFlow(t, dir, "beta.flow", flowBeta)

	u := uri.File(alpha)
	if u.FsPath() != alpha {
		t.Fatalf("the URI round trip lost the path: %q became %q", alpha, u.FsPath())
	}
	for range 3 {
		s.Change(u, []byte(flowAlphaEdited))
	}

	if got := len(s.Documents()); got != 1 {
		t.Fatalf("after three Change calls the store holds %d documents, want 1; "+
			"Change rescanned the workspace and picked up beta.flow", got)
	}

	// Change must still have DONE something, or a Store whose Change is a no-op
	// would satisfy the count above for the wrong reason.
	if got := string(docFor(t, s, alpha).Src); got != flowAlphaEdited {
		t.Fatalf("Change left the document's bytes at %q", got)
	}

	// KNOWN POSITIVE, same run and same instrument: an explicit Scan DOES find
	// beta.flow. Without it, a store that could never see a new file would pass
	// the assertion above while proving nothing.
	if err := s.Scan(dir); err != nil {
		t.Fatalf("the second scan failed: %v", err)
	}
	if got := len(s.Documents()); got != 2 {
		t.Fatalf("the control failed: an explicit rescan found %d documents, want 2 — "+
			"the count above was not evidence that Change skipped the walk", got)
	}

	// The rescan must not have clobbered the open buffer with the disk bytes.
	if got := string(docFor(t, s, alpha).Src); got != flowAlphaEdited {
		t.Fatalf("the rescan overwrote the open buffer with disk bytes: %q", got)
	}
}

func TestAnOpenDocumentOverridesItsFileOnDisk(t *testing.T) {
	// The fixture must separate the two byte sequences, or the overlay logic
	// could be absent entirely and this test would still pass.
	if flowAlpha == flowAlphaEdited {
		t.Fatal("the fixture's disk and overlay bytes are equal, so it cannot separate " +
			"an overlay that is consulted from one that is merely stored")
	}

	dir := t.TempDir()
	path := writeFlow(t, dir, "alpha.flow", flowAlpha)

	s := NewStore()
	if err := s.Scan(dir); err != nil {
		t.Fatalf("the scan failed: %v", err)
	}
	if got := string(docFor(t, s, path).Src); got != flowAlpha {
		t.Fatalf("the scan loaded %q, want the disk bytes", got)
	}

	u := uri.File(path)
	s.Open(u, []byte(flowAlphaEdited))

	// THE ASSERTION IS AGAINST Documents(), because that is the accessor the
	// analysis adapter reads. An overlay honored by Get but not by Documents
	// would leave every diagnostic computed against the file on disk.
	if got := string(docFor(t, s, path).Src); got != flowAlphaEdited {
		t.Fatalf("Documents() reports %q for an open document; the overlay was stored but never consulted", got)
	}
	got, ok := s.Get(u)
	if !ok {
		t.Fatal("Get does not know a document that was just opened")
	}
	if string(got.Src) != flowAlphaEdited {
		t.Fatalf("Get reports %q for an open document, want the overlay bytes", string(got.Src))
	}

	// A scan while the buffer is open must not undo the overlay.
	if err := s.Scan(dir); err != nil {
		t.Fatalf("the rescan failed: %v", err)
	}
	if got := string(docFor(t, s, path).Src); got != flowAlphaEdited {
		t.Fatalf("a rescan replaced the open buffer with the disk bytes: %q", got)
	}

	// Closing returns the document to disk, which is the other half of the
	// precedence rule and proves the overlay was a layer rather than a
	// permanent overwrite of the store's only copy.
	s.Close(u)
	if got := string(docFor(t, s, path).Src); got != flowAlpha {
		t.Fatalf("after Close the document reports %q, want the bytes on disk", got)
	}
}

func TestClosingABufferWithNothingOnDiskForgetsIt(t *testing.T) {
	dir := t.TempDir()
	u := uri.File(filepath.Join(dir, "scratch.flow"))

	s := NewStore()
	s.Open(u, []byte(flowAlpha))
	if _, ok := s.Get(u); !ok {
		t.Fatal("an opened buffer is not in the store")
	}

	s.Close(u)
	if _, ok := s.Get(u); ok {
		t.Fatal("a closed buffer with no file behind it is still in the store, holding bytes no file has")
	}
}

func TestADamagedDocumentKeepsItsTreeAndCarriesItsParseError(t *testing.T) {
	dir := t.TempDir()
	u := uri.File(filepath.Join(dir, "damaged.flow"))

	s := NewStore()
	// A branch whose arrow names no target, taken from lang/ast's own broken
	// corpus so the fixture is a shape the parser really rejects rather than
	// one this test assumes it does.
	s.Open(u, []byte(flowDamaged))

	doc, ok := s.Get(u)
	if !ok {
		t.Fatal("a damaged document is not in the store")
	}
	if doc.File == nil {
		t.Fatal("a damaged document carries no tree; lang/analysis refuses a nil-File source")
	}
	if doc.ParseErr == nil {
		t.Fatal("a document the parser rejected carries no ParseErr, so its diagnostics have no route out")
	}

	// KNOWN POSITIVE: a clean document through the same field reports no error,
	// so the non-nil above is a property of THIS source rather than of the field.
	clean := uri.File(filepath.Join(dir, "clean.flow"))
	s.Open(clean, []byte(flowAlpha))
	doc, _ = s.Get(clean)
	if doc.ParseErr != nil {
		t.Fatalf("the control failed: a clean document reports ParseErr %v", doc.ParseErr)
	}
}
