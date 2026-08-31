// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import "context"

// Transformation is applied to a frame and returns the transformed payload. The
// frame WRAPS the datum, so a node reads it with Value and the runtime re-wraps the
// returned payload onto the same frame: there is no way to return a different
// frame, and ignoring the parameter still propagates the frame intact.
type Transformation[T, U any] func(f Frame[T]) U

// Monad is applied to a frame and returns a payload of the same type.
type Monad[T any] func(f Frame[T]) T

// Filter reports whether a frame takes the left branch of an If.
type Filter[T any] func(f Frame[T]) bool

// Duplicator splits a payload into two independent copies. Tee takes one because
// only the caller knows how to copy T without leaving the two branches sharing
// state; the frame itself is deep-copied by the runtime.
type Duplicator[T any] func(d T) (a, b T)

// Ingest submits a payload into the Source that returned it. Returning does not
// mean the datum has finished traversing the graph.
type Ingest[T any] func(ctx context.Context, payload T) error
