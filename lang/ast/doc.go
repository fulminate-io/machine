// Package ast - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
//
// Package ast holds the grammar, lexer, parser and syntax tree for the flow
// language: the .flow source form in which a machine pipeline is written.
//
// The grammar lives beside this package in grammar.ebnf and is the source of
// truth for the language's shape. The parser here implements it by recursive
// descent, and the test suite drives a recognizer straight off the EBNF so the
// two cannot silently disagree.
//
// Parse is the entire entry point. It is error tolerant by design: a source
// with mistakes in it still yields a usable, positioned partial tree together
// with the diagnostics describing what went wrong, because an editor asking
// for structure while the author is mid-keystroke is the common case rather
// than the exception.
package ast
