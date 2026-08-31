// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import "sync"

// registry holds every analyzer this package has declared, in registration
// order.
//
// The mutex is not decoration: registration runs from package init functions
// today, but a consumer building an analyzer set at runtime is a shape the LSP
// ticket will plausibly want, and a slice appended from two goroutines is a data
// race whose symptom is a lost analyzer rather than a crash.
var (
	registryMu sync.Mutex
	registry   []*Analyzer
	registered = map[string]bool{}
)

// Register adds an analyzer to the package-level set All returns.
//
// A duplicate name PANICS rather than replacing or ignoring the earlier
// registration. Two analyzers under one name would make a diagnostic's Code
// ambiguous, and there is no return value through which to report it — bad input
// errors at the point of the mistake instead of being absorbed.
func Register(a *Analyzer) {
	registryMu.Lock()
	defer registryMu.Unlock()

	if a == nil {
		panic("analysis: Register was given a nil analyzer")
	}
	if a.Name == "" {
		panic("analysis: Register was given an analyzer with no Name")
	}
	if registered[a.Name] {
		panic("analysis: an analyzer named " + a.Name + " is already registered")
	}
	registered[a.Name] = true
	registry = append(registry, a)
}

// All returns every registered analyzer, in registration order.
//
// The slice is a copy, so a caller sorting or filtering it cannot reorder the
// registry itself. Ordering is not a promise about execution order: the driver
// computes that from the Requires edges.
func All() []*Analyzer {
	registryMu.Lock()
	defer registryMu.Unlock()

	out := make([]*Analyzer, len(registry))
	copy(out, registry)
	return out
}
