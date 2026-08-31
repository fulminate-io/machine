// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package ast

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
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
	{"a switch else must be last", "an `else` arm must be LAST"},
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

// ---------------------------------------------------------------------------
// FIRST, FOLLOW and the position-addressed recognizer.
// ---------------------------------------------------------------------------

// kindSet is a set of token kinds — the currency of FIRST, FOLLOW and the Go
// span stop sets, which is why it is token kinds rather than grammar symbols.
type kindSet map[tokenKind]bool

// lexicalKinds maps the seven opaque lexical terminals onto their token kinds.
var lexicalKinds = map[string]tokenKind{
	"ident":      tokIdent,
	"string":     tokString,
	"number":     tokNumber,
	"newline":    tokNewline,
	"noteText":   tokNoteText,
	"goSpan":     tokGoSpan,
	"goFuncSpan": tokGoFuncSpan,
}

// punctuationKinds maps the quoted punctuation terminals onto their kinds.
var punctuationKinds = map[string]tokenKind{
	arrowOp: tokArrow,
	",":     tokComma,
	".":     tokDot,
	"=":     tokAssign,
	"{":     tokLBrace,
	"}":     tokRBrace,
	"(":     tokLParen,
	")":     tokRParen,
}

// terminalKind maps a quoted grammar terminal onto the token kind that can match
// it. A bare word that is not a keyword — `error` is the only one — arrives as
// an ordinary identifier token.
func terminalKind(text string) tokenKind {
	if kind, ok := keywords[text]; ok {
		return kind
	}
	if kind, ok := punctuationKinds[text]; ok {
		return kind
	}
	return tokIdent
}

// sortedKinds renders a kind set as a stable slice for scanGoSpan.
func sortedKinds(set kindSet) []tokenKind {
	out := make([]tokenKind, 0, len(set))
	for kind := range set {
		out = append(out, kind)
	}
	slices.Sort(out)
	return out
}

// grammarSets holds the nullable, FIRST and FOLLOW relations over a grammar.
type grammarSets struct {
	doc      *grammarDocument
	nullable map[string]bool
	first    map[string]kindSet
	follow   map[string]kindSet
}

// nullableOf reports whether an expression can derive the empty string.
func (g *grammarSets) nullableOf(e *expr) bool {
	switch e.kind {
	case exprTerminal:
		return false
	case exprName:
		if _, ok := g.doc.productions[e.text]; ok {
			return g.nullable[e.text]
		}
		return false
	case exprOption, exprRepeat:
		return true
	case exprAlt:
		return slices.ContainsFunc(e.items, g.nullableOf)
	case exprSeq:
		for _, item := range e.items {
			if !g.nullableOf(item) {
				return false
			}
		}
		return true
	}
	return false
}

// firstOf returns the terminals an expression can begin with.
func (g *grammarSets) firstOf(e *expr) kindSet {
	out := kindSet{}
	switch e.kind {
	case exprTerminal:
		out[terminalKind(e.text)] = true
	case exprName:
		if _, ok := g.doc.productions[e.text]; ok {
			maps.Copy(out, g.first[e.text])
		} else if kind, ok := lexicalKinds[e.text]; ok {
			out[kind] = true
		}
	case exprOption, exprRepeat:
		maps.Copy(out, g.firstOf(e.items[0]))
	case exprAlt:
		for _, item := range e.items {
			maps.Copy(out, g.firstOf(item))
		}
	case exprSeq:
		for _, item := range e.items {
			maps.Copy(out, g.firstOf(item))
			if !g.nullableOf(item) {
				break
			}
		}
	}
	return out
}

// afterOf returns the terminals that can follow a prefix of a sequence, given
// what follows the sequence as a whole.
//
// THIS IS THE COMPUTATION THE NAIVE STOP-SET RULE GETS WRONG. "The terminals
// that follow the goSpan in the production being matched" yields the EMPTY set
// wherever goSpan is last — Output, Over, NodeError — and nonterminals rather
// than terminals in five more places. On subflow-and-use.flow that makes
// Output's span swallow `, bad ErrResult` while the parser stops at the comma.
func (g *grammarSets) afterOf(rest []*expr, after kindSet) kindSet {
	out := kindSet{}
	for _, item := range rest {
		maps.Copy(out, g.firstOf(item))
		if !g.nullableOf(item) {
			return out
		}
	}
	maps.Copy(out, after)
	return out
}

// followInto propagates a follow set into the nonterminals of an expression,
// reporting whether anything changed.
func (g *grammarSets) followInto(e *expr, after kindSet) bool {
	switch e.kind {
	case exprName:
		return g.addFollow(e.text, after)
	case exprOption:
		return g.followInto(e.items[0], after)
	case exprRepeat:
		// What follows the inner expression is either another iteration of it
		// or whatever follows the repetition.
		inner := kindSet{}
		maps.Copy(inner, g.firstOf(e.items[0]))
		maps.Copy(inner, after)
		return g.followInto(e.items[0], inner)
	case exprAlt:
		changed := false
		for _, item := range e.items {
			changed = g.followInto(item, after) || changed
		}
		return changed
	case exprSeq:
		changed := false
		for i, item := range e.items {
			changed = g.followInto(item, g.afterOf(e.items[i+1:], after)) || changed
		}
		return changed
	}
	return false
}

// addFollow adds terminals to a nonterminal's follow set.
func (g *grammarSets) addFollow(name string, after kindSet) bool {
	if _, ok := g.doc.productions[name]; !ok {
		return false
	}
	changed := false
	for kind := range after {
		if !g.follow[name][kind] {
			g.follow[name][kind] = true
			changed = true
		}
	}
	return changed
}

// computeSets runs the nullable/FIRST fixpoint and then the FOLLOW fixpoint.
func computeSets(doc *grammarDocument) *grammarSets {
	g := &grammarSets{
		doc:      doc,
		nullable: map[string]bool{},
		first:    map[string]kindSet{},
		follow:   map[string]kindSet{},
	}
	for name := range doc.productions {
		g.first[name] = kindSet{}
		g.follow[name] = kindSet{}
	}

	for changed := true; changed; {
		changed = false
		for name, body := range doc.productions {
			if nullable := g.nullableOf(body); nullable != g.nullable[name] {
				g.nullable[name] = nullable
				changed = true
			}
			for kind := range g.firstOf(body) {
				if !g.first[name][kind] {
					g.first[name][kind] = true
					changed = true
				}
			}
		}
	}

	g.follow["File"][tokEOF] = true
	for changed := true; changed; {
		changed = false
		for name, body := range doc.productions {
			changed = g.followInto(body, g.follow[name]) || changed
		}
	}
	return g
}

// recognizer decides membership in the grammar directly from the notation.
//
// IT IS POSITION-ADDRESSED AND LAZY. Its whole parse state is a BYTE OFFSET; it
// materializes no token slice, and backtracking restores the saved offset and
// re-scans, which the lexer supports because next() advances from wherever the
// cursor sits and caches nothing.
//
// IT CONSTRUCTS ITS OWN LEXER and never borrows the parser's. Two independent
// readers of the same bytes reaching the same verdict is evidence; one reader
// consulted twice is not.
type recognizer struct {
	sets *grammarSets
	lex  *lexer
}

// newRecognizer builds a recognizer over its own lexer.
func newRecognizer(sets *grammarSets, src []byte) *recognizer {
	return &recognizer{sets: sets, lex: newLexer(src)}
}

// seek repositions the cursor. Line and column are reset rather than tracked
// because the recognizer decides membership and never reports a position.
func (r *recognizer) seek(off int) {
	r.lex.off = off
	r.lex.line = 1
	r.lex.col = 1
}

// tokenAt scans the token beginning at off and returns the offset just past it.
func (r *recognizer) tokenAt(off int) (token, int) {
	r.seek(off)
	t := r.lex.next()
	return t, r.lex.off
}

// accepts reports whether the source derives from the start symbol.
func (r *recognizer) accepts() bool {
	off, ok := r.match(r.sets.doc.productions["File"], 0, kindSet{tokEOF: true})
	if !ok {
		return false
	}
	t, _ := r.tokenAt(off)
	return t.kind == tokEOF
}

// match attempts one expression at an offset, returning where it ended.
func (r *recognizer) match(e *expr, off int, follow kindSet) (int, bool) {
	switch e.kind {
	case exprTerminal:
		return r.matchTerminal(e.text, off)
	case exprName:
		return r.matchName(e.text, off, follow)
	case exprOption:
		if next, ok := r.match(e.items[0], off, follow); ok {
			return next, true
		}
		return off, true
	case exprRepeat:
		return r.matchRepeat(e.items[0], off, follow)
	case exprAlt:
		for _, alt := range e.items {
			if next, ok := r.match(alt, off, follow); ok {
				return next, true
			}
		}
		return off, false
	case exprSeq:
		return r.matchSeq(e.items, off, follow)
	}
	return off, false
}

// matchSeq threads the per-position follow set down the sequence.
func (r *recognizer) matchSeq(items []*expr, off int, follow kindSet) (int, bool) {
	for i, item := range items {
		next, ok := r.match(item, off, r.sets.afterOf(items[i+1:], follow))
		if !ok {
			return off, false
		}
		off = next
	}
	return off, true
}

// matchRepeat iterates until the inner expression fails or stops consuming.
func (r *recognizer) matchRepeat(inner *expr, off int, follow kindSet) (int, bool) {
	innerFollow := kindSet{}
	maps.Copy(innerFollow, r.sets.firstOf(inner))
	maps.Copy(innerFollow, follow)
	for {
		next, ok := r.match(inner, off, innerFollow)
		if !ok || next == off {
			return off, true
		}
		off = next
	}
}

// matchTerminal matches a token whose TEXT equals the quoted terminal, whatever
// kind the lexer gave it. That is what lets `"error"` match without `error`
// being a keyword.
func (r *recognizer) matchTerminal(text string, off int) (int, bool) {
	t, next := r.tokenAt(off)
	if t.text != text {
		return off, false
	}
	return next, true
}

// matchName matches a nonterminal or one of the seven lexical terminals.
func (r *recognizer) matchName(name string, off int, follow kindSet) (int, bool) {
	if body, ok := r.sets.doc.productions[name]; ok {
		return r.match(body, off, follow)
	}
	switch name {
	case "goSpan":
		return r.matchGoSpan(off, follow)
	case "goFuncSpan":
		return r.matchGoFuncSpan(off)
	}
	kind, ok := lexicalKinds[name]
	if !ok {
		return off, false
	}
	t, next := r.tokenAt(off)
	if t.kind != kind {
		return off, false
	}
	return next, true
}

// matchGoSpan matches the opaque Go terminal, stopping at the follow set
// computed for THIS occurrence across productions.
func (r *recognizer) matchGoSpan(off int, follow kindSet) (int, bool) {
	r.seek(off)
	if span := r.lex.scanGoSpan(sortedKinds(follow)...); span.text == "" {
		return off, false
	}
	return r.lex.off, true
}

// matchGoFuncSpan calls the SAME scanGoFuncSpan helper the parser uses.
//
// A func body SELF-TERMINATES at brace balance, so this terminal needs no follow
// set — and a second implementation of it would disagree with the parser's on
// boundaries and report drift that does not exist.
func (r *recognizer) matchGoFuncSpan(off int) (int, bool) {
	r.seek(off)
	if span := r.lex.scanGoFuncSpan(); span.text == "" {
		return off, false
	}
	return r.lex.off, true
}

// readFixture reads one corpus file.
func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return src
}

// parserOnlyExemptions DERIVES the recognizer/parser exemption set from the
// grammar header's annotations rather than from a hand-kept list.
//
// Each PARSER-ONLY RULE annotation ends by naming its gate, and this reads those
// lines for testdata/invalid references. Deriving rather than listing is what
// makes a rule added without its fixture RED: the annotation check requires
// every rule to name a testdata path, and an unnamed one cannot silently widen
// the exemption here. Rule 2's gate is a VALID fixture, so it contributes
// nothing and the set is three files.
func parserOnlyExemptions(t *testing.T) []string {
	doc := loadGrammar(t)
	blocks := strings.Split(doc.header, "PARSER-ONLY RULE:")
	if len(blocks) < 2 {
		t.Fatalf("CONTROL FAILED: %s carries no parser-only rule annotation to derive from", grammarPath)
	}

	pattern := regexp.MustCompile(`testdata/invalid/([A-Za-z0-9._-]+)\.flow`)
	var out []string
	for _, block := range blocks[1:] {
		for _, match := range pattern.FindAllStringSubmatch(block, -1) {
			if !slices.Contains(out, match[1]) {
				out = append(out, match[1])
			}
		}
	}
	slices.Sort(out)
	return out
}

// TestGrammarFirstAndFollowSets pins the two sets the goSpan stop-set derivation
// turns on, both computed by fixpoint rather than read off a production.
//
// FIRST(Clauses) IS SEVEN MEMBERS, NOT SIX. The six clause keywords are the
// obvious part; `newline` is contributed by the `[ newline ]` prefix that every
// Clauses alternative carries, which is what makes a clause able to continue
// onto the next line. Counting only the keywords is the natural mistake.
func TestGrammarFirstAndFollowSets(t *testing.T) {
	sets := computeSets(loadGrammar(t))

	wantFirst := kindSet{
		kwReads: true, kwWrites: true, kwOver: true,
		kwCheckpoint: true, kwOn: true, kwNote: true,
		tokNewline: true,
	}
	gotFirst := sets.first["Clauses"]
	if len(gotFirst) != len(wantFirst) {
		t.Errorf("FIRST(Clauses) has %d members, want %d: %v", len(gotFirst), len(wantFirst), sortedKinds(gotFirst))
	}
	for kind := range wantFirst {
		if !gotFirst[kind] {
			t.Errorf("FIRST(Clauses) is missing kind %d", kind)
		}
	}

	wantFollow := kindSet{tokNewline: true, tokLBrace: true}
	gotFollow := sets.follow["Clauses"]
	if len(gotFollow) != len(wantFollow) {
		t.Errorf("FOLLOW(Clauses) has %d members, want %d: %v", len(gotFollow), len(wantFollow), sortedKinds(gotFollow))
	}
	for kind := range wantFollow {
		if !gotFollow[kind] {
			t.Errorf("FOLLOW(Clauses) is missing kind %d", kind)
		}
	}

	// THE ARROW IS IN NEITHER, which is exactly why the prohibition on
	// `on error -> handler` had to become a parser-only rule: no stop set
	// derived from this grammar contains an arrow, so the span scans clean.
	if gotFirst[tokArrow] || gotFollow[tokArrow] {
		t.Errorf("an arrow appears in FIRST or FOLLOW of Clauses; the fourth parser-only rule rests on it being in neither")
	}
}

// TestEBNFRecognizerAgreesWithTheParser drives a recognizer straight off the
// notation and requires it to reach the parser's verdict in BOTH directions
// across the grammar-decidable corpora.
func TestEBNFRecognizerAgreesWithTheParser(t *testing.T) {
	sets := computeSets(loadGrammar(t))
	exempt := parserOnlyExemptions(t)

	accepted := 0
	for _, dir := range []string{validCorpusDir, analysisRejectDir, strawmanDir} {
		for _, path := range corpusFiles(t, dir) {
			src := readFixture(t, path)
			if _, err := Parse(src); err != nil {
				t.Errorf("%s: the parser reported %v on a source the grammar should accept", path, err)
				continue
			}
			if !newRecognizer(sets, src).accepts() {
				t.Errorf("%s: the parser accepts this and the recognizer does not", path)
				continue
			}
			accepted++
		}
	}
	if accepted == 0 {
		t.Fatalf("CONTROL FAILED: the accepting leg read no files at all")
	}

	rejected := 0
	for _, path := range corpusFiles(t, invalidCorpusDir) {
		name := strings.TrimSuffix(filepath.Base(path), ".flow")
		if slices.Contains(exempt, name) {
			continue
		}
		src := readFixture(t, path)
		if _, err := Parse(src); err == nil {
			t.Errorf("%s: the parser accepted a rejected form", path)
			continue
		}
		if newRecognizer(sets, src).accepts() {
			t.Errorf("%s: the parser rejects this and the recognizer accepts it", path)
			continue
		}
		rejected++
	}
	if rejected == 0 {
		t.Fatalf("CONTROL FAILED: the rejecting leg read no files at all")
	}
	t.Logf("recognizer and parser agree on %d accepted and %d rejected sources", accepted, rejected)
}

// TestParserOnlyFixturesAcceptedByGrammarRejectedByParser proves the split the
// annotations claim.
//
// These fixtures exist precisely because the notation ACCEPTS the form and only
// the parser refuses it, so a recognizer faithful to the grammar must accept
// them. Demanding otherwise would leave one route to green — teaching the
// recognizer to enforce parser rules — which destroys the independence that
// makes the agreement test evidence at all.
//
// This is what upgrades "the notation cannot express this" from an assertion in
// a comment to a demonstrated fact.
func TestParserOnlyFixturesAcceptedByGrammarRejectedByParser(t *testing.T) {
	sets := computeSets(loadGrammar(t))
	exempt := parserOnlyExemptions(t)
	if len(exempt) == 0 {
		t.Fatalf("CONTROL FAILED: the exemption set derived from the annotations is empty")
	}

	for _, name := range exempt {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(invalidCorpusDir, name+".flow")
			src := readFixture(t, path)

			if !newRecognizer(sets, src).accepts() {
				t.Errorf("the grammar must ACCEPT %s: the rule is annotated as inexpressible in the notation", name)
			}
			if _, err := Parse(src); err == nil {
				t.Errorf("the parser must REJECT %s: that is the whole content of the annotated rule", name)
			}
		})
	}
	t.Logf("the annotations derive an exemption set of %d fixtures: %v", len(exempt), exempt)
}
