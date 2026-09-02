// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ast

// The stdlib source-walking packages are aliased because every one of them
// collides with a name this package already owns.
import (
	goast "go/ast"
	goparser "go/parser"
	gotoken "go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// recognizerSourceFile is where the recognizer is declared. The structural check
// below reads it rather than trusting a comment.
const recognizerSourceFile = "grammar_test.go"

// mentionsIdent reports whether an expression names the given identifier.
func mentionsIdent(e goast.Expr, name string) bool {
	found := false
	goast.Inspect(e, func(n goast.Node) bool {
		if id, ok := n.(*goast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// findFunc returns a top-level function declaration by name.
func findFunc(t *testing.T, path, name string) *goast.FuncDecl {
	t.Helper()
	file, err := goparser.ParseFile(gotoken.NewFileSet(), path, nil, goparser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*goast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("CONTROL FAILED: %s declares no function named %s", path, name)
	return nil
}

// TestRecognizerOwnsItsLexer asserts the recognizer reads the source through its
// OWN scanner rather than the parser's.
//
// This is what makes the agreement gate evidence at all: two independent readers
// of the same bytes reaching the same verdict is evidence, and one reader
// consulted twice is not. It is checked structurally AND at runtime, because the
// structural leg alone would pass against a constructor that built a lexer and
// then threw it away.
func TestRecognizerOwnsItsLexer(t *testing.T) {
	ctor := findFunc(t, recognizerSourceFile, "newRecognizer")

	for _, param := range ctor.Type.Params.List {
		if mentionsIdent(param.Type, "parser") {
			t.Errorf("newRecognizer takes a parser; the recognizer must never be handed the parser's lexer")
		}
	}
	buildsOwn := false
	goast.Inspect(ctor.Body, func(n goast.Node) bool {
		call, ok := n.(*goast.CallExpr)
		if !ok {
			return true
		}
		if id, isIdent := call.Fun.(*goast.Ident); isIdent && id.Name == "newLexer" {
			buildsOwn = true
		}
		return true
	})
	if !buildsOwn {
		t.Errorf("newRecognizer does not construct its own lexer")
	}

	// The runtime leg: two readers, two scanners.
	src := []byte("flow orders\nsource ingest Poll\n")
	p := &parser{src: src}
	p.lex = newLexer(p.src)
	r := newRecognizer(computeSets(loadGrammar(t)), src)

	if r.lex == nil || p.lex == nil {
		t.Fatalf("CONTROL FAILED: one of the two readers has no lexer at all")
	}
	if r.lex == p.lex {
		t.Fatalf("the recognizer and the parser share one lexer, so their agreement proves nothing")
	}
}

// collectNodeTypes records the concrete type name of every AST node reachable
// from a value.
//
// Reflection rather than a hand-written walk: a walk would have to be extended
// for each new node type, and the whole point of this census is to notice a node
// type nothing in the corpus exercises.
func collectNodeTypes(v reflect.Value, into map[string]bool) {
	if !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return
		}
		collectNodeTypes(v.Elem(), into)
	case reflect.Slice:
		for i := range v.Len() {
			collectNodeTypes(v.Index(i), into)
		}
	case reflect.Struct:
		if v.CanInterface() {
			if _, isNode := v.Interface().(Node); isNode {
				into[v.Type().Name()] = true
			}
		}
		for i := range v.NumField() {
			collectNodeTypes(v.Field(i), into)
		}
	default:
	}
}

// censusCorpora are the directories whose trees the node-type census walks.
var censusCorpora = []string{validCorpusDir, analysisRejectDir, strawmanDir}

// TestEveryNodeTypeAppearsInTheValidCorpus asserts the corpus exercises every
// node type the package declares.
//
// BadStmt is exempt by name — it stands in for source the parser could not read,
// and the broken corpus is what covers it. FuncDecl is NOT exempt, so this
// census requires a corpus file carrying a func.
func TestEveryNodeTypeAppearsInTheValidCorpus(t *testing.T) {
	declared := declaredNodeTypes(t)
	if len(declared) < 10 {
		t.Fatalf("CONTROL FAILED: the source walk found %d node types, which is not this package's AST", len(declared))
	}

	seen := map[string]bool{}
	files := 0
	for _, dir := range censusCorpora {
		for _, path := range corpusFiles(t, dir) {
			file, err := parseCorpusFile(t, path)
			if err != nil {
				t.Fatalf("%s: a census corpus file must parse clean: %v", path, err)
			}
			collectNodeTypes(reflect.ValueOf(file), seen)
			files++
		}
	}
	if files == 0 {
		t.Fatalf("CONTROL FAILED: the census walked no files at all")
	}

	var missing []string
	for _, name := range declared {
		if name == "BadStmt" || seen[name] {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		t.Errorf("no corpus file exercises these node types: %v", missing)
	}
	t.Logf("%d files exercise %d of the %d declared node types", files, len(seen), len(declared))
}

// declaredNodeTypes enumerates every type declaring an isNode method.
func declaredNodeTypes(t *testing.T) []string {
	t.Helper()
	var out []string
	for typeName, decls := range packageMethods(t) {
		if slices.Contains(methodNames(decls), "isNode") {
			out = append(out, typeName)
		}
	}
	slices.Sort(out)
	return out
}

// TestEveryKeywordIsExercisedByTheCorpus asserts every ruled keyword appears as
// a keyword TOKEN in at least one successfully parsed corpus file.
//
// The scope is the corpus rather than the canonical examples because the three
// strawmen exercise only 21 of the 26 keywords — const, else, param, switch and
// use are round-6 additions that postdate every drawing, and demanding all 26
// from ratified artifacts would be satisfiable only by editing them.
func TestEveryKeywordIsExercisedByTheCorpus(t *testing.T) {
	seen := map[tokenKind]string{}
	files := 0
	for _, dir := range []string{validCorpusDir, strawmanDir} {
		for _, path := range corpusFiles(t, dir) {
			src := readFixture(t, path)
			if _, err := Parse(src); err != nil {
				t.Fatalf("%s: a keyword-census file must parse clean: %v", path, err)
			}
			files++
			lex := newLexer(src)
			for tok := lex.next(); tok.kind != tokEOF; tok = lex.next() {
				if _, isKeyword := keywords[tok.text]; isKeyword {
					seen[tok.kind] = filepath.Base(path)
				}
			}
		}
	}
	if files == 0 {
		t.Fatalf("CONTROL FAILED: the census read no files at all")
	}
	if _, sawFlow := seen[kwFlow]; !sawFlow {
		t.Fatalf("CONTROL FAILED: the scan did not even find the flow keyword")
	}

	var missing []string
	for spelling, kind := range keywords {
		if _, ok := seen[kind]; !ok {
			missing = append(missing, spelling)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		t.Errorf("no corpus file exercises these ruled keywords: %v", missing)
	}
	t.Logf("%d files exercise %d of the %d ruled keywords", files, len(seen), len(keywords))
}

// TestExemptionSetMatchesItsAnnotations guards the derivation itself: an
// annotation naming a fixture that does not exist would silently widen the
// exemption and quietly excuse a real divergence.
func TestExemptionSetMatchesItsAnnotations(t *testing.T) {
	exempt := parserOnlyExemptions(t)
	if len(exempt) == 0 {
		t.Fatalf("CONTROL FAILED: the annotations derived no exemptions at all")
	}
	present := fixtureNames(corpusFiles(t, invalidCorpusDir))
	for _, name := range exempt {
		if !slices.Contains(present, name) {
			t.Errorf("an annotation names testdata/invalid/%s.flow, which does not exist", name)
		}
	}
	if strings.Join(exempt, ",") == "" {
		t.Fatalf("CONTROL FAILED: the derived set rendered empty")
	}
}
