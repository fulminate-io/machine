// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"context"
	"sync"
)

// Store is the heap storage seam. It is machine-scoped state that outlives every
// traversal, and it is one of the two approved type-erasure points in the package:
// a single store holds values of many payload types, so the typing lives on the
// Cell handle rather than on the storage.
//
// A Store instance is held privately by a Machine and is never handed to a node
// function. Nodes reach it only through the capability-gated Frame methods Load,
// Save and Update; the host reaches it only through Machine.Host.
//
// Every method reports failure and takes a context, because an implementation may
// be remote or replicated: there a write can fail in a way that cannot be resolved
// locally, and a read can block on a quorum round trip. An implementation with
// nowhere to report that could only panic or swallow it, and a swallowed write is a
// silently degraded lane.
//
// Implementations are not interchangeable in their guarantees. Each implementation
// documents its own, and the interface promises nothing beyond reporting failure and
// honoring the context. In particular the interface does not guarantee that Update
// runs fn under a lock held across the whole read-modify-write: NewMemStore
// guarantees that, and an implementation that computes fn at the caller and
// replicates the result does not.
type Store interface {
	Load(ctx context.Context, path string) (any, bool, error)
	Save(ctx context.Context, path string, value any) error
	Update(ctx context.Context, path string, fn func(any) any) (any, error)
}

type memStore struct {
	mutex  sync.Mutex
	values map[string]any
}

// NewMemStore returns the default in-memory Store. Update holds the lock across
// the whole read-modify-write, so a heap update through a Frame is atomic under
// concurrent nodes.
//
// That atomicity is THIS implementation's guarantee and not the seam's: a store that
// computes fn at the caller and replicates the result gives a weaker one, so a caller
// depending on it depends on memStore rather than on Store.
//
// Nothing memStore does can fail, so every method returns a nil error, and nothing it
// does blocks, so every method ignores the context.
func NewMemStore() Store {
	return &memStore{values: map[string]any{}}
}

func (s *memStore) Load(_ context.Context, path string) (any, bool, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	value, ok := s.values[path]
	return value, ok, nil
}

func (s *memStore) Save(_ context.Context, path string, value any) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.values[path] = value
	return nil
}

func (s *memStore) Update(_ context.Context, path string, fn func(any) any) (any, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	updated := fn(s.values[path])
	s.values[path] = updated
	return updated, nil
}
