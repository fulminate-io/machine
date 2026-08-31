// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
)

// idBytes is the number of random bytes behind a frame identifier.
const idBytes = 16

const (
	accessRead  = "read"
	accessWrite = "write"
	unboundNode = "<unbound>"
)

// The declaration registry is PROCESS-wide: NewKey and NewCell write to it once
// per handle at declaration time, and its only reader on the data plane side is
// RebuildPacket, which restores cloners for a datum arriving from a remote
// transport. No per-datum frame path touches it.
var registryMu sync.Mutex
var registry = map[string]func(any) any{}
var declared = map[string]struct{}{}

// KeyRef is the untyped view of a state handle. One capability declaration names
// handles of many payload types, and Go forbids type parameters on interface
// methods, so the declaration options take KeyRef rather than a typed handle.
type KeyRef interface{ Name() string }

// Key is the STACK handle: per-traversal state that travels inside the frame and
// is reclaimed when the traversal ends.
type Key[V any] struct {
	name  string
	clone func(V) V
}

// NewKey declares a stack key. The cloner is mandatory: Tee deep-copies the frame
// into both branches, and only the caller knows how to copy V without leaving the
// two branches sharing a pointer, slice or map. It panics on an empty name, a nil
// cloner, or a name already declared as a key or a cell.
func NewKey[V any](name string, clone func(V) V) Key[V] {
	if clone == nil {
		panic("machine: stack key " + name + " declared without a cloner")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	reserve(name)
	registry[name] = erase(clone)
	return Key[V]{name: name, clone: clone}
}

// Name returns the key's declared name.
func (k Key[V]) Name() string { return k.name }

// Cell is the HEAP handle: machine-scoped state that outlives every traversal. It
// is inert on its own, holding only a name, and reaches storage exclusively through
// the capability-gated Frame and HostState accessors.
type Cell[V any] struct {
	name string
}

// NewCell declares a heap cell. It takes no cloner because heap values are
// machine-scoped and are never copied by Tee. It panics on an empty name or a name
// already declared as a key or a cell.
func NewCell[V any](name string) Cell[V] {
	registryMu.Lock()
	defer registryMu.Unlock()
	reserve(name)
	return Cell[V]{name: name}
}

// Name returns the cell's declared name.
func (c Cell[V]) Name() string { return c.name }

// reserve claims a name in the ONE declaration namespace shared by keys and cells.
// A capability set is keyed by name alone, so separate namespaces would let a read
// declared on a stack key silently grant access to an unrelated heap cell of the
// same name. Callers hold registryMu.
func reserve(name string) {
	if name == "" {
		panic("machine: state handle declared with an empty name")
	}
	if _, ok := declared[name]; ok {
		panic("machine: state handle already declared: " + name)
	}
	declared[name] = struct{}{}
}

// erase adapts a typed cloner to the erased form the frame carries beside each
// value. The assertion cannot fail for a value written through the key it was
// declared with, and panics loudly if it ever does.
func erase[V any](clone func(V) V) func(any) any {
	return func(value any) any { return clone(value.(V)) }
}

// CapabilityError reports a state access a node did not declare. It is raised as a
// panic, because a node function returns a bare payload and has no error slot; the
// supervisor's panic boundary routes it to the registered handler as a NodeError,
// where errors.As recovers this type intact.
type CapabilityError struct {
	Node   string
	Key    string
	Access string
}

// Error implements the error interface.
func (e *CapabilityError) Error() string {
	return fmt.Sprintf("machine: node %q has no %s capability for %q", e.Node, e.Access, e.Key)
}

// capabilities is one node's declared view over both namespaces. A write
// capability does NOT imply a read capability.
type capabilities struct {
	node   string
	reads  map[string]struct{}
	writes map[string]struct{}
}

func (c *capabilities) check(name, access string) {
	if c == nil {
		panic(&CapabilityError{Node: unboundNode, Key: name, Access: access})
	}
	set := c.reads
	if access == accessWrite {
		set = c.writes
	}
	if _, ok := set[name]; !ok {
		panic(&CapabilityError{Node: c.node, Key: name, Access: access})
	}
}

// frameState is the per-traversal execution state. It carries NO lock, and neither
// does any per-datum frame path: a datum visits one node at a time, and the only
// fan-out that could share state is Tee, which deep-copies. The cloners travel with
// the values so clone walks only its own maps.
type frameState struct {
	id      string
	parent  string
	source  string
	node    string
	values  map[string]any
	cloners map[string]func(any) any
	store   Store
}

func (s *frameState) clone() *frameState {
	next := &frameState{
		id:      newID(),
		parent:  s.id,
		source:  s.source,
		node:    s.node,
		values:  make(map[string]any, len(s.values)),
		cloners: make(map[string]func(any) any, len(s.cloners)),
		store:   s.store,
	}
	for name, clone := range s.cloners {
		next.cloners[name] = clone
		next.values[name] = clone(s.values[name])
	}
	return next
}

func (s *frameState) release() {
	clear(s.values)
	clear(s.cloners)
}

// Frame is the envelope. It wraps the payload rather than traveling beside it, so
// one type serves both the wire and the node call site: a node function takes
// Frame[T] and returns a bare payload, and the runtime re-wraps that payload onto
// the same state. A node therefore cannot drop the frame by ignoring it, and cannot
// forge one, because there is no exported constructor.
type Frame[T any] struct {
	payload T
	state   *frameState
	caps    *capabilities
	ctx     context.Context
}

// Value returns the payload. It takes no capability, so a node declaring no state
// handles can still read its datum; the accessors that DO take one are Has, Get, Set,
// Load, Save and Update.
func (f Frame[T]) Value() T { return f.payload }

// ID returns the frame's identifier.
func (f Frame[T]) ID() string { return f.state.id }

// Parent returns the identifier of the frame this one was cloned from, empty for a
// frame born at a Source.
func (f Frame[T]) Parent() string { return f.state.parent }

// Source returns the name of the node the frame was ingested at.
func (f Frame[T]) Source() string { return f.state.source }

// Node returns the name of the last node that PROCESSED the frame. A terminal that
// only drains does not advance it.
func (f Frame[T]) Node() string { return f.state.node }

// Context returns the context THIS node execution runs under: the span context the
// worker's guard opened for this datum at this node, descended from the context passed
// to Start. An outbound call made with it is CANCELED when the machine's context ends,
// and its span is parented to this node's span. Composing a per-call timeout on top of
// it is the node's own business.
//
// The context is EXECUTION-SCOPED, never part of the envelope. A context does not
// serialize, so no packet carries one: a datum arriving over a remote transport, or
// rebuilt through RebuildPacket, runs under the RESUMING worker's context rather than
// the one its originating process used.
//
// It takes no capability, joining Value, ID, Parent, Source and Node in the ungated
// identity set, because the capability model gates STATE — Has, Get, Set, Load, Save
// and Update.
func (f Frame[T]) Context() context.Context { return f.ctx }

// Has reports whether a declared handle currently holds a value: a stack key in
// this frame, or a heap cell in the machine's store. It takes the read capability.
// It is what distinguishes a handle that was never written from one holding a zero
// value, which is the state an untaken If branch leaves behind. The store lookup is
// safe because a frame carrying a capability view has always been bound to a node,
// and binding attaches the store.
//
// A stack key answers from the frame itself and cannot fail, so its error is always
// nil. A heap cell reaches the store, which can, and a non-nil error means the store
// did not answer rather than that it answered absent.
func (f Frame[T]) Has(ref KeyRef) (bool, error) {
	name := ref.Name()
	f.caps.check(name, accessRead)
	if _, ok := f.state.values[name]; ok {
		return true, nil
	}
	_, ok, err := f.state.store.Load(f.ctx, name)
	return ok, err
}

// Get returns the value held under a declared stack key, or the zero value of V if
// the key was never written. It takes the read capability.
func (f Frame[T]) Get[V any](k Key[V]) V {
	f.caps.check(k.name, accessRead)
	value, ok := f.state.values[k.name]
	if !ok {
		var zero V
		return zero
	}
	return value.(V)
}

// Set stores a value under a declared stack key, recording the key's cloner beside
// it so a later Tee can deep-copy without consulting the process registry. It takes
// the write capability.
func (f Frame[T]) Set[V any](k Key[V], value V) {
	f.caps.check(k.name, accessWrite)
	f.state.values[k.name] = value
	f.state.cloners[k.name] = erase(k.clone)
}

// Load returns the value held in a declared heap cell and whether it was present.
// It takes the read capability. The heap rides the same capability gate as the
// stack; there is no other way for a node to reach the machine's store.
//
// On a non-nil error the value is the zero value of V and present is false, because
// the store DID NOT ANSWER — which is not the same as answering absent. Reporting
// present beside a failure would hand the caller a value the store never confirmed.
func (f Frame[T]) Load[V any](c Cell[V]) (V, bool, error) {
	f.caps.check(c.name, accessRead)
	value, ok, err := f.state.store.Load(f.ctx, c.name)
	if !ok || err != nil {
		var zero V
		return zero, false, err
	}
	return value.(V), true, nil
}

// Save writes a value into a declared heap cell. It takes the write capability.
//
// A non-nil error means the write may or may not have landed: what a failure implies
// about the stored value is the store's own contract to state, not this method's.
func (f Frame[T]) Save[V any](c Cell[V], value V) error {
	f.caps.check(c.name, accessWrite)
	return f.state.store.Save(f.ctx, c.name, value)
}

// Update applies fn to a declared heap cell and returns the new value. It takes BOTH
// the read and the write capability, because it does both.
//
// WHETHER THE READ-MODIFY-WRITE IS ATOMIC IS THE STORE'S GUARANTEE AND NOT THIS
// METHOD'S. NewMemStore holds its lock across the whole of it, so an update through a
// frame is atomic under concurrent nodes there; a store that computes fn at the caller
// and replicates the result gives a weaker guarantee, and this method does not add one.
//
// On a non-nil error the value is the zero value of V.
func (f Frame[T]) Update[V any](c Cell[V], fn func(V) V) (V, error) {
	f.caps.check(c.name, accessRead)
	f.caps.check(c.name, accessWrite)
	updated, err := f.state.store.Update(f.ctx, c.name, func(current any) any {
		existing, _ := current.(V)
		return fn(existing)
	})
	if err != nil {
		var zero V
		return zero, err
	}
	return updated.(V), nil
}

// FrameData is the serializable projection of a frame's stack state, produced by
// Packet.Data and consumed by RebuildPacket. A remote transport marshals it alongside
// the payload.
type FrameData struct {
	ID     string         `json:"id"`
	Parent string         `json:"parent,omitempty"`
	Source string         `json:"source"`
	Node   string         `json:"node"`
	Values map[string]any `json:"values,omitempty"`
}

// newFrame is the only place a frame is born, called by the Ingest closure a Source
// hands back.
func newFrame[T any](source string, payload T, store Store) Frame[T] {
	return Frame[T]{
		payload: payload,
		state: &frameState{
			id:      newID(),
			source:  source,
			values:  map[string]any{},
			cloners: map[string]func(any) any{},
			store:   store,
		},
	}
}

// rewrap carries one frame's state, capability view and execution context onto a
// payload of a different type. It is how the runtime re-wraps a node's bare return
// value, so the frame leaving a node is the frame the node held on a new payload.
func rewrap[T, U any](f Frame[T], payload U) Frame[U] {
	return Frame[U]{payload: payload, state: f.state, caps: f.caps, ctx: f.ctx}
}

func newID() string {
	buf := make([]byte, idBytes)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
