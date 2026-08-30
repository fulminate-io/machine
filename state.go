// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import "sync"

// Store is the heap storage seam. It is machine-scoped state that outlives every
// traversal, and it is one of the two approved type-erasure points in the package:
// a single store holds values of many payload types, so the typing lives on the
// Cell handle rather than on the storage.
//
// A Store instance is held privately by a Machine and is never handed to a node
// function. Nodes reach it only through the capability-gated Frame methods Load,
// Save and Update; the host reaches it only through Machine.Host.
type Store interface {
	Load(path string) (any, bool)
	Save(path string, value any)
	Update(path string, fn func(any) any) any
}

type memStore struct {
	mutex  sync.Mutex
	values map[string]any
}

// NewMemStore returns the default in-memory Store. Update holds the lock across
// the whole read-modify-write, so a heap update through a Frame is atomic under
// concurrent nodes.
func NewMemStore() Store {
	return &memStore{values: map[string]any{}}
}

func (s *memStore) Load(path string) (any, bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	value, ok := s.values[path]
	return value, ok
}

func (s *memStore) Save(path string, value any) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.values[path] = value
}

func (s *memStore) Update(path string, fn func(any) any) any {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	updated := fn(s.values[path])
	s.values[path] = updated
	return updated
}
