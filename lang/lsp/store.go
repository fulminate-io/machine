// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lsp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/whitaker-io/machine/lang/ast"
	"go.lsp.dev/uri"
)

// flowExt is the extension a workspace scan collects.
const flowExt = ".flow"

// errNoTree reports a document that somehow carries no parsed tree.
//
// ast.Parse promises a non-nil tree even for a file it reported problems on
// (parser.go:57-64 says so and says not to "fix" it), so this guards against
// that promise changing rather than a path a caller can reach today. Scan
// reports it as an error; a nil tree arriving through Open instead reaches
// lang/analysis's driver, which refuses a nil-File source BY NAME. Both routes
// are loud, and neither drops a document quietly.
var errNoTree = errors.New("lsp: the parser returned no tree")

// Document is one source file the server knows about: where it lives, its
// current bytes, the tree parsed from them, and the position index over them.
//
// ParseErr IS PART OF THE DOCUMENT, and that is why the Store hands documents
// to the analysis adapter rather than analysis.Source values. analysis.Source
// carries exactly Path, Src and File and has nowhere to put a parse error, so a
// store that handed those over would leave the adapter with no route to the
// parse diagnostics except re-parsing every source on every change — measured
// at nearly half the change-to-diagnostics path, for information the Store
// already computed.
//
// A damaged document still carries a File. ast.Parse always returns a tree and
// reports its problems alongside it, which is what lets a buffer being typed
// stay in the analysis run instead of dropping out of completion and
// navigation the moment it stops parsing.
type Document struct {
	URI      uri.URI
	Path     string
	Src      []byte
	File     *ast.File
	ParseErr error
	Mapper   *Mapper
}

// Store holds every document the server knows about, whether the editor has it
// open or it merely sits in the workspace on disk.
//
// THE WORKSPACE IS SCANNED, not just the open buffers, because lang/analysis
// answers cross-file questions: SymbolTable.Flow searches every source in the
// run so a `use` naming a flow declared elsewhere resolves. Analyzing only open
// documents would answer those from a partial world — go-to-definition into an
// unopened file finding nothing, and a binding-count error against a flow in an
// unopened file never firing at all.
//
// PER-CHANGE COST IS ONE REPARSE. Scan walks the filesystem once per initialize
// and never per keystroke; Change reparses only the document whose bytes moved
// and leaves every other cached tree alone.
//
// A STORE IS SAFE FOR CONCURRENT USE, and that is a requirement rather than a
// courtesy. go.lsp.dev/protocol wires every connection through
// jsonrpc2.AsyncHandler, which releases the read loop as soon as a handler
// starts, so two didChange notifications — or a didChange and a completion —
// genuinely run at the same time. Without the lock the map writes below are a
// hard "concurrent map writes" panic while an author is typing, which was
// observed under -race before this lock existed.
type Store struct {
	mu      sync.RWMutex
	docs    map[string]*Document
	overlay map[string]bool
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{docs: map[string]*Document{}, overlay: map[string]bool{}}
}

// Scan walks root and loads every .flow file it finds.
//
// A path the editor has open is left alone: an unsaved buffer is the truth
// about what its author is looking at, and re-reading the file underneath it
// would answer with bytes nobody is editing.
func (s *Store) Scan(root string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != flowExt {
			return nil
		}
		return s.loadFromDisk(path)
	})
}

// loadFromDisk reads and parses one file, unless an overlay outranks it. The
// caller holds the write lock.
func (s *Store) loadFromDisk(path string) error {
	if s.overlay[path] {
		return nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if doc := s.put(uri.File(path), path, src); doc.File == nil {
		return fmt.Errorf("%w for %s", errNoTree, path)
	}
	return nil
}

// Open installs an editor's buffer for a document and parses it.
//
// From here the overlay outranks the file on disk for this path until Close.
func (s *Store) Open(u uri.URI, src []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := u.FsPath()
	s.overlay[path] = true
	s.put(u, path, src)
}

// Change replaces an open document's bytes and reparses THAT DOCUMENT ONLY.
//
// Every other document keeps the tree it already has. A full-workspace reparse
// per keystroke would multiply a per-file parse by the workspace size and learn
// nothing about the files nobody touched.
func (s *Store) Change(u uri.URI, src []byte) {
	s.Open(u, src)
}

// Close drops the editor's overlay, returning the document to what is on disk.
//
// A document with nothing on disk behind it — an unsaved buffer the editor
// created and closed — is forgotten rather than left holding bytes no file has.
func (s *Store) Close(u uri.URI) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := u.FsPath()
	delete(s.overlay, path)
	if err := s.loadFromDisk(path); err != nil {
		delete(s.docs, path)
	}
}

// Get returns the document for a URI, and whether the store knows it.
func (s *Store) Get(u uri.URI) (*Document, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	doc, ok := s.docs[u.FsPath()]
	return doc, ok
}

// byPath returns the document at a filesystem path, which is the key the
// analysis tables record against — a diagnostic's Path and a FileSymbols'
// Src.Path are both this, never a URI.
func (s *Store) byPath(path string) (*Document, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	doc, ok := s.docs[path]
	return doc, ok
}

// Documents returns every known document — overlay bytes where one is open,
// disk bytes otherwise — in a stable path order so two runs over one workspace
// produce the same result.
//
// IT RETURNS DOCUMENTS RATHER THAN analysis.Source VALUES so the parse error
// travels with the tree; the analysis adapter builds its sources from these.
func (s *Store) Documents() []*Document {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*Document, 0, len(s.docs))
	for _, doc := range s.docs {
		out = append(out, doc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// put parses src and records the document under its path. The caller holds the
// write lock.
//
// A DAMAGED DOCUMENT IS STILL RECORDED, tree and error together. That is the
// suppression policy's foundation: the buffer being typed stays in the analysis
// run so its tables stay alive, and only the analyzer-attributed findings about
// it are held back later.
func (s *Store) put(u uri.URI, path string, src []byte) *Document {
	file, perr := ast.Parse(src)
	doc := &Document{URI: u, Path: path, Src: src, File: file, ParseErr: perr, Mapper: NewMapper(src)}
	s.docs[path] = doc
	return doc
}
