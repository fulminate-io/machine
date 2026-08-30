// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

// NodeError carries one node failure to a registered handler. Panic distinguishes a
// recovered panic from a returned error, and Payload is the datum the node was
// processing. Core retains nothing for redelivery: retry and dead-lettering are
// composed by the user inside a handler.
type NodeError[T any] struct {
	Node    string
	Err     error
	Payload T
	Panic   bool
}

// ErrorHandler receives every error and recovered panic the node supervisor
// observes. With no handler registered the behavior is a no-op drop, which is the
// default and preserves the prior semantics: no retry, no redelivery, no restart.
// A per-node handler registered with WithErrorHandler wins over the global one
// registered with OptionErrorHandler.
type ErrorHandler[T any] func(NodeError[T])
