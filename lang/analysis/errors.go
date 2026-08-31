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
var (
	errNoSymbols = errors.New("the symbols analyzer produced no table")
	errNoGraph   = errors.New("the flowgraph analyzer produced no graph")
)
