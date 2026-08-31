// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"text/scanner"
)

// grammarPath is the notation this package implements.
const grammarPath = "grammar.ebnf"

// lexicalTerminals are the seven names the grammar refers to but never defines,
// because their shape is a scanning rule rather than a production. The header
// documents each one.
var lexicalTerminals = []string{
	"ident", "string", "number", "newline", "noteText", "goSpan", "goFuncSpan",
}

// exprKind distinguishes the four structural forms of an EBNF expression from
// its two leaf forms.
type exprKind int

const (
	exprSeq exprKind = iota
	exprAlt
	exprOption
	exprRepeat
	exprTerminal
	exprName
)

// expr is one node of a production's expression tree.
//
// The tree is STRUCTURED rather than a flat list of the names a production
// mentions, and that matters downstream: the conformance recognizer computes
// FIRST and FOLLOW from it to know where a goSpan stops, and FOLLOW cannot be
// derived from a mention list. It needs to know that in
// `Transform = "transform" ident goSpan From Clauses newline .` the goSpan is
// followed by the nonterminal From.
type expr struct {
	kind  exprKind
	text  string
	items []*expr
}

// walk visits e and every node beneath it.
func (e *expr) walk(fn func(*expr)) {
	if e == nil {
		return
	}
	fn(e)
	for _, item := range e.items {
		item.walk(fn)
	}
}

// grammarDocument is a parsed grammar.ebnf: its header comment, its productions
// in declaration order, and each production's expression tree.
type grammarDocument struct {
	header      string
	order       []string
	productions map[string]*expr
}

// namesIn returns every nonterminal or lexical-terminal reference in e.
func namesIn(e *expr) []string {
	var out []string
	e.walk(func(n *expr) {
		if n.kind == exprName {
			out = append(out, n.text)
		}
	})
	return out
}

// terminalsIn returns every quoted terminal in e.
func terminalsIn(e *expr) []string {
	var out []string
	e.walk(func(n *expr) {
		if n.kind == exprTerminal {
			out = append(out, n.text)
		}
	})
	return out
}

// lexeme is one token of the notation itself.
type lexeme struct {
	kind rune
	text string
	pos  scanner.Position
}

// scanEBNF tokenizes the notation with the standard library's scanner.
// SkipComments is why the header needs no special handling here: the `//` form
// is exactly what text/scanner skips by default.
func scanEBNF(src string) ([]lexeme, error) {
	var s scanner.Scanner
	var errs []string
	s.Init(strings.NewReader(src))
	s.Mode = scanner.ScanIdents | scanner.ScanStrings | scanner.ScanComments | scanner.SkipComments
	s.Error = func(_ *scanner.Scanner, msg string) { errs = append(errs, msg) }

	var out []lexeme
	for tok := s.Scan(); tok != scanner.EOF; tok = s.Scan() {
		out = append(out, lexeme{kind: tok, text: s.TokenText(), pos: s.Position})
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("scanning the notation: %s", strings.Join(errs, "; "))
	}
	return out, nil
}

// ebnfParser walks a lexeme slice with one token of lookahead.
type ebnfParser struct {
	lex []lexeme
	i   int
}

func (p *ebnfParser) at(i int) lexeme {
	if i < 0 || i >= len(p.lex) {
		return lexeme{kind: scanner.EOF}
	}
	return p.lex[i]
}

func (p *ebnfParser) peek() lexeme { return p.at(p.i) }

func (p *ebnfParser) next() lexeme {
	l := p.at(p.i)
	p.i++
	return l
}

// parseEBNF parses a whole grammar into productions and their expression trees.
func parseEBNF(src string) (*grammarDocument, error) {
	lex, err := scanEBNF(src)
	if err != nil {
		return nil, err
	}
	p := &ebnfParser{lex: lex}
	doc := &grammarDocument{productions: map[string]*expr{}}
	for p.peek().kind != scanner.EOF {
		name, body, perr := p.parseProduction()
		if perr != nil {
			return nil, perr
		}
		if _, dup := doc.productions[name]; dup {
			return nil, fmt.Errorf("production %s is declared twice", name)
		}
		doc.order = append(doc.order, name)
		doc.productions[name] = body
	}
	if len(doc.order) == 0 {
		return nil, fmt.Errorf("the notation holds no productions at all")
	}
	return doc, nil
}

// parseProduction parses `Name = Expression .`.
func (p *ebnfParser) parseProduction() (string, *expr, error) {
	head := p.next()
	if head.kind != scanner.Ident {
		return "", nil, fmt.Errorf("%s: expected a production name, got %q", head.pos, head.text)
	}
	if eq := p.next(); eq.text != "=" {
		return "", nil, fmt.Errorf("%s: production %s is not followed by '='", eq.pos, head.text)
	}
	body, err := p.parseExpression()
	if err != nil {
		return "", nil, err
	}
	if end := p.peek(); end.text != "." {
		return "", nil, fmt.Errorf("%s: production %s is not terminated by a period", end.pos, head.text)
	}
	p.next()
	return head.text, body, nil
}

// parseExpression parses alternatives separated by `|`.
func (p *ebnfParser) parseExpression() (*expr, error) {
	first, err := p.parseAlternative()
	if err != nil {
		return nil, err
	}
	alts := []*expr{first}
	for p.peek().text == "|" {
		p.next()
		alt, aerr := p.parseAlternative()
		if aerr != nil {
			return nil, aerr
		}
		alts = append(alts, alt)
	}
	if len(alts) == 1 {
		return first, nil
	}
	return &expr{kind: exprAlt, items: alts}, nil
}

// parseAlternative parses a sequence of terms.
func (p *ebnfParser) parseAlternative() (*expr, error) {
	var items []*expr
	for !p.alternativeEnds() {
		term, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		items = append(items, term)
	}
	if len(items) == 1 {
		return items[0], nil
	}
	return &expr{kind: exprSeq, items: items}, nil
}

// alternativeEnds reports whether the cursor sits past the end of a sequence.
//
// THE LOOKAHEAD IS THE WHOLE POINT of this function. An identifier followed by
// `=` opens the NEXT production, which means the current one was never
// terminated; without that check a grammar with a missing period is silently
// accepted, swallowing the following production as part of this one.
func (p *ebnfParser) alternativeEnds() bool {
	l := p.peek()
	if l.kind == scanner.EOF {
		return true
	}
	if l.kind == scanner.Ident {
		return p.at(p.i+1).text == "="
	}
	return slices.Contains([]string{"|", ".", ")", "]", "}"}, l.text)
}

// parseTerm parses one term: a name, a quoted terminal, or a bracketed group.
func (p *ebnfParser) parseTerm() (*expr, error) {
	l := p.next()
	switch {
	case l.kind == scanner.Ident:
		return &expr{kind: exprName, text: l.text}, nil
	case l.kind == scanner.String:
		unquoted, err := strconv.Unquote(l.text)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", l.pos, err)
		}
		return &expr{kind: exprTerminal, text: unquoted}, nil
	case l.text == "(":
		return p.parseGroup(exprSeq, ")")
	case l.text == "[":
		return p.parseGroup(exprOption, "]")
	case l.text == "{":
		return p.parseGroup(exprRepeat, "}")
	}
	return nil, fmt.Errorf("%s: unexpected %q in an expression", l.pos, l.text)
}

// parseGroup parses a bracketed expression. Plain parentheses are precedence
// only and add no node; option and repetition brackets each add their own.
func (p *ebnfParser) parseGroup(kind exprKind, closing string) (*expr, error) {
	inner, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if got := p.next(); got.text != closing {
		return nil, fmt.Errorf("%s: expected %q, got %q", got.pos, closing, got.text)
	}
	if kind == exprSeq {
		return inner, nil
	}
	return &expr{kind: kind, items: []*expr{inner}}, nil
}

// headerOf returns the file's comment lines with their markers stripped and
// their wrapping removed, so an annotation that spans several lines matches as
// one string.
func headerOf(src string) string {
	var parts []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, "//"); ok {
			parts = append(parts, strings.TrimSpace(after))
		}
	}
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

var (
	grammarOnce sync.Once
	grammarDoc  *grammarDocument
	grammarErr  error
)

// loadGrammar parses grammar.ebnf once per test binary and shares the result.
// The expression tree is built once and reused by the conformance recognizer
// rather than rebuilt.
func loadGrammar(t *testing.T) *grammarDocument {
	t.Helper()
	grammarOnce.Do(func() {
		raw, err := os.ReadFile(grammarPath)
		if err != nil {
			grammarErr = err
			return
		}
		grammarDoc, grammarErr = parseEBNF(string(raw))
		if grammarDoc != nil {
			grammarDoc.header = headerOf(string(raw))
		}
	})
	if grammarErr != nil {
		t.Fatalf("parsing %s: %v", grammarPath, grammarErr)
	}
	return grammarDoc
}

// TestGrammarEBNFParses asserts the notation parses, starts at File, and really
// produced structure rather than a flattened token list.
func TestGrammarEBNFParses(t *testing.T) {
	doc := loadGrammar(t)

	if len(doc.order) < 10 {
		t.Fatalf("CONTROL FAILED: %s parsed to %d productions, which is not a real grammar", grammarPath, len(doc.order))
	}
	t.Logf("%s parsed to %d productions", grammarPath, len(doc.order))

	if doc.order[0] != "File" {
		t.Fatalf("the first production is %s, want File as the start symbol", doc.order[0])
	}

	// A walker that silently flattened every group would still satisfy the
	// dangling and unreachable checks, so structure is asserted directly.
	structured := 0
	for _, name := range doc.order {
		doc.productions[name].walk(func(n *expr) {
			if n.kind == exprOption || n.kind == exprRepeat {
				structured++
			}
		})
	}
	if structured == 0 {
		t.Fatalf("no production carries an option or repetition node; the walker flattened the grammar")
	}
	t.Logf("the expression trees carry %d option and repetition nodes", structured)
}

// TestGrammarEBNFRejectsMalformed asserts the walker refuses a grammar whose
// production is never terminated, and — as the control on that claim — accepts
// the same grammar once the period is restored.
func TestGrammarEBNFRejectsMalformed(t *testing.T) {
	const wellFormed = "A = \"x\" [ B ] .\nB = \"y\" .\n"
	if _, err := parseEBNF(wellFormed); err != nil {
		t.Fatalf("CONTROL FAILED: a well-formed grammar was rejected: %v", err)
	}

	for _, tc := range []struct {
		name string
		src  string
	}{
		{"missing terminating period", "A = \"x\"\nB = \"y\" .\n"},
		{"missing equals", "A \"x\" .\n"},
		{"unclosed option bracket", "A = [ \"x\" .\nB = \"y\" .\n"},
		{"empty", "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseEBNF(tc.src); err == nil {
				t.Fatalf("malformed grammar was accepted")
			}
		})
	}
}

// TestGrammarEBNFHasNoDanglingOrOrphanProductions asserts both directions of
// closure: nothing referenced is undefined, and nothing defined is unreachable.
func TestGrammarEBNFHasNoDanglingOrOrphanProductions(t *testing.T) {
	doc := loadGrammar(t)

	referenced := map[string]bool{}
	for _, name := range doc.order {
		for _, ref := range namesIn(doc.productions[name]) {
			referenced[ref] = true
			if _, ok := doc.productions[ref]; ok {
				continue
			}
			if slices.Contains(lexicalTerminals, ref) {
				continue
			}
			t.Errorf("production %s references undefined nonterminal %s", name, ref)
		}
	}
	if len(referenced) == 0 {
		t.Fatalf("CONTROL FAILED: no production references anything at all")
	}

	var unreachable []string
	for _, name := range doc.order {
		if name != "File" && !referenced[name] {
			unreachable = append(unreachable, name)
		}
	}
	if len(unreachable) > 0 {
		t.Errorf("productions unreachable from the start symbol: %v", unreachable)
	}
}

// TestEveryKeywordAppearsAsAGrammarTerminal asserts the keyword table is fully
// covered by the notation.
//
// NOTE THE DIRECTION: table to terminals, never the reverse. `error` is a quoted
// terminal in OnError and NodeError and is NOT a keyword, and `number` is a
// lexical terminal no production references. A reverse check would demand both
// be reconciled into the table, which the keyword census exists to refuse.
func TestEveryKeywordAppearsAsAGrammarTerminal(t *testing.T) {
	doc := loadGrammar(t)

	found := map[string]bool{}
	for _, name := range doc.order {
		for _, term := range terminalsIn(doc.productions[name]) {
			found[term] = true
		}
	}
	if !found["flow"] {
		t.Fatalf("CONTROL FAILED: the terminal scan did not even find the flow keyword")
	}

	for keyword := range keywords {
		if !found[keyword] {
			t.Errorf("keyword %q has no production in %s", keyword, grammarPath)
		}
	}
}

// parserOnlyRules is the locked list: the four rules the parser enforces that
// the notation cannot express, each matched on a stable substring.
//
// The list, the grammar header and both consuming gates are kept in exact sync.
var parserOnlyRules = []struct {
	name     string
	fragment string
}{
	{"clauses appear at most once", "each alternative may appear AT MOST ONCE"},
	{"flow-level on error and note come first", "must appear BEFORE the first statement of the flow body"},
	{"a statement must follow a flow declaration", "a Statement must follow a FlowDecl"},
	{"on error takes no arrow", "must be NON-EMPTY and must NOT BEGIN WITH"},
}

// TestGrammarAnnotatesEveryParserOnlyRule asserts the header carries all four
// annotations and that each one NAMES ITS GATE.
//
// The gate naming is not cosmetic: the conformance suite derives its
// recognizer-versus-parser exemption set by reading these lines for testdata
// paths, so an annotation without one would silently contribute nothing to that
// derivation.
func TestGrammarAnnotatesEveryParserOnlyRule(t *testing.T) {
	doc := loadGrammar(t)

	blocks := strings.Split(doc.header, "PARSER-ONLY RULE:")
	if len(blocks) < 2 {
		t.Fatalf("CONTROL FAILED: %s carries no parser-only rule annotation at all", grammarPath)
	}
	annotations := blocks[1:]
	if len(annotations) != len(parserOnlyRules) {
		t.Errorf("the header carries %d annotations, want %d", len(annotations), len(parserOnlyRules))
	}

	for _, rule := range parserOnlyRules {
		if !strings.Contains(doc.header, rule.fragment) {
			t.Errorf("the header does not annotate the parser-only rule %q (looking for %q)", rule.name, rule.fragment)
		}
	}

	for i, annotation := range annotations {
		if !strings.Contains(annotation, "testdata/") {
			t.Errorf("parser-only rule annotation %d names no testdata gate: %.120s", i+1, annotation)
		}
	}
}
