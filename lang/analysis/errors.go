// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import "errors"

// The failures an analyzer returns when a prerequisite's result is absent or not
// the type that prerequisite declares.
//
// It is a stop rather than a skip. A missing or mistyped prerequisite result
// means the driver ran the wrong analyzer, or an analyzer changed its ResultType
// without its dependants following; an analysis that silently produced no
// findings under either condition would report a clean program.
// errNoPackages and errFileMismatch are refusals of a different kind: the first
// is a caller handing the inference no package set, and the second is the symbol
// tables and the derived graphs disagreeing about how many files the run held.
// Neither is recoverable by guessing, and pairing a flow with another file's
// graph would type it against the wrong imports.
var (
	errNoSymbols       = errors.New("the symbols analyzer produced no table")
	errNoGraph         = errors.New("the flowgraph analyzer produced no graph")
	errNoGraphs        = errors.New("the flowgraph analyzer produced no graph set")
	errNoInferredTypes = errors.New("the type inference analyzer produced no table")
	errNoPackages      = errors.New("type inference needs a loaded package set, and none was supplied")
	errFileMismatch    = errors.New("the symbol tables and the derived graphs cover different numbers of files")
)
