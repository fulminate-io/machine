// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import (
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"testing"
)

// allCorpora is every directory holding .flow sources.
//
// The invalid and broken corpora are included DELIBERATELY: a recovered tree
// carrying BadStmt nodes is where a span is most likely to be wrong, and it is
// the tree an editor spends most of its time holding.
var allCorpora = []string{
	validCorpusDir,
	analysisRejectDir,
	invalidCorpusDir,
	brokenCorpusDir,
	strawmanDir,
}

// lineStarts returns the byte offset of each line's first byte.
func lineStarts(src []byte) []int {
	starts := []int{0}
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// lineColAt recomputes a line and byte-column from an offset by counting
// newlines in the RAW SOURCE.
//
// Computing it from the bytes rather than reading it back from the lexer is the
// whole point: it is what catches a cursor that updated one counter and not the
// other.
func lineColAt(starts []int, off int) (int, int) {
	line := sort.SearchInts(starts, off+1) - 1
	if line < 0 {
		line = 0
	}
	return line + 1, off - starts[line] + 1
}

// nodeVisit is one node together with its nearest enclosing node.
type nodeVisit struct {
	node   Node
	parent Node
	field  string
}

// walkNodes collects every AST node reachable from a value, each paired with the
// node that encloses it.
func walkNodes(v reflect.Value, parent Node, field string, into *[]nodeVisit) {
	if !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return
		}
		walkNodes(v.Elem(), parent, field, into)
	case reflect.Slice:
		for i := range v.Len() {
			walkNodes(v.Index(i), parent, field, into)
		}
	case reflect.Struct:
		walkStruct(v, parent, field, into)
	default:
	}
}

// walkStruct records a struct if it is a node, then descends into its fields.
func walkStruct(v reflect.Value, parent Node, field string, into *[]nodeVisit) {
	inner := parent
	if v.CanInterface() {
		if node, isNode := v.Interface().(Node); isNode {
			*into = append(*into, nodeVisit{node: node, parent: parent, field: field})
			inner = node
		}
	}
	for i := range v.NumField() {
		walkNodes(v.Field(i), inner, v.Type().Field(i).Name, into)
	}
}

// TestPositionsAreInternallyConsistent asserts the four span invariants across
// every corpus, each computed independently of the parser rather than read back
// from it.
func TestPositionsAreInternallyConsistent(t *testing.T) {
	visited := 0
	for _, dir := range allCorpora {
		for _, path := range corpusFiles(t, dir) {
			src := readFixture(t, path)
			file, _ := Parse(src)
			if file == nil {
				t.Fatalf("%s: Parse returned a nil File", path)
			}

			var nodes []nodeVisit
			walkNodes(reflect.ValueOf(file), nil, "File", &nodes)
			visited += len(nodes)

			t.Run(filepath.Base(path), func(t *testing.T) {
				checkSpans(t, src, nodes)
				checkSiblingOrder(t, file)
			})
		}
	}

	// The control: a walk that visited almost nothing would satisfy every
	// invariant vacuously.
	const obviouslyPresent = 200
	if visited < obviouslyPresent {
		t.Fatalf("CONTROL FAILED: the walk visited only %d nodes across %d corpora", visited, len(allCorpora))
	}
	t.Logf("checked %d node spans across %d corpora", visited, len(allCorpora))
}

// checkSpans asserts bounds, recomputed line and column, and containment.
func checkSpans(t *testing.T, src []byte, nodes []nodeVisit) {
	t.Helper()
	starts := lineStarts(src)

	for _, v := range nodes {
		start, end := v.node.Pos(), v.node.End()
		name := reflect.TypeOf(v.node).Name()

		if start.Offset < 0 || start.Offset > len(src) || end.Offset < 0 || end.Offset > len(src) {
			t.Errorf("%s span %d..%d falls outside the %d-byte source", name, start.Offset, end.Offset, len(src))
			continue
		}
		if end.Offset < start.Offset {
			t.Errorf("%s ends at %d before it starts at %d", name, end.Offset, start.Offset)
		}

		wantLine, wantCol := lineColAt(starts, start.Offset)
		if start.Line != wantLine || start.Col != wantCol {
			t.Errorf("%s starts at recorded %d:%d but offset %d is at %d:%d",
				name, start.Line, start.Col, start.Offset, wantLine, wantCol)
		}

		if v.parent == nil {
			continue
		}
		// FuncDecl's opaque body is the largest single span in any file and the
		// one most likely to be off by a byte at either end.
		outer := reflect.TypeOf(v.parent).Name()
		if start.Offset < v.parent.Pos().Offset || end.Offset > v.parent.End().Offset {
			t.Errorf("%s.%s (%s) span %d..%d escapes its parent's %d..%d",
				outer, v.field, name, start.Offset, end.Offset,
				v.parent.Pos().Offset, v.parent.End().Offset)
		}
	}
}

// checkSiblingOrder asserts statements are in non-decreasing offset order, which
// is what lets a consumer binary-search the tree by offset — what an editor does
// on every keystroke.
func checkSiblingOrder(t *testing.T, file *File) {
	t.Helper()
	assertNonDecreasing(t, "file declarations", declOffsets(file.Decls))
	for _, decl := range file.Decls {
		flow, ok := decl.(FlowDecl)
		if !ok {
			continue
		}
		offsets := make([]int, 0, len(flow.Body))
		for _, stmt := range flow.Body {
			offsets = append(offsets, stmt.Pos().Offset)
		}
		assertNonDecreasing(t, "statements of flow "+flow.Name.Name, offsets)
	}
}

// declOffsets projects declarations down to their start offsets.
func declOffsets(decls []Decl) []int {
	out := make([]int, 0, len(decls))
	for _, decl := range decls {
		out = append(out, decl.Pos().Offset)
	}
	return out
}

// assertNonDecreasing checks an offset sequence is sorted.
func assertNonDecreasing(t *testing.T, what string, offsets []int) {
	t.Helper()
	if !slices.IsSorted(offsets) {
		t.Errorf("%s are not in non-decreasing offset order: %v", what, offsets)
	}
}
