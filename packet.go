// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import "fmt"

// Packet is the edge-facing envelope, and it has two faces. To a channel edge it is a
// REFERENCE: it wraps the same frameState pointer the frame carried, so the hop copies
// two words and allocates nothing. To a remote codec it is a PROJECTION, built only
// when one asks for it through Data.
//
// A packet carries no capability view and declares no gated accessor, which is what
// makes the node boundary structural rather than documentary: an edge holds a packet
// and therefore cannot reach state, and a node holds a frame and therefore cannot
// reach the wire.
type Packet[T any] struct {
	payload T
	state   *frameState
}

// packetOf is the only Frame-to-Packet transformation. It is unexported because a node
// that could call it would hold an ungated read of every stack value on its datum,
// which is exactly the door the frame's capability check exists to close.
func packetOf[T any](f Frame[T]) Packet[T] {
	return Packet[T]{payload: f.payload, state: f.state}
}

// Value returns the payload.
func (p Packet[T]) Value() T { return p.payload }

// ID returns the packet's identifier.
func (p Packet[T]) ID() string { return p.state.id }

// Parent returns the identifier of the frame this one was cloned from, empty for a
// datum born at a Source.
func (p Packet[T]) Parent() string { return p.state.parent }

// Source returns the name of the node the datum was ingested at.
func (p Packet[T]) Source() string { return p.state.source }

// Node returns the name of the last node that PROCESSED the datum. A terminal that
// only drains does not advance it.
func (p Packet[T]) Node() string { return p.state.node }

// Data projects the packet's STACK state for a Codec. Heap state is machine-scoped and
// is never projected. This is the only method that builds anything: the reference face
// costs nothing, and a channel edge never calls it.
func (p Packet[T]) Data() FrameData {
	data := FrameData{
		ID:     p.state.id,
		Parent: p.state.parent,
		Source: p.state.source,
		Node:   p.state.node,
	}
	if len(p.state.values) > 0 {
		data.Values = make(map[string]any, len(p.state.values))
		for name, value := range p.state.values {
			data.Values[name] = value
		}
	}
	return data
}

// RebuildPacket reconstructs a packet from its projection, restoring each value's
// cloner from the process declaration registry. It REFUSES a projection naming an
// undeclared key rather than handing back a packet whose later Tee would have no
// cloner for that value.
//
// It is the exported constructor of a packet, and it takes a wire projection rather
// than a Frame: there is no exported path from a frame to a packet. The one other
// exported declaration that yields a packet, GobCodec.Unmarshal, decodes bytes and
// delegates here, so this is the single place a packet is built from a projection.
func RebuildPacket[T any](data FrameData, payload T) (Packet[T], error) {
	state := &frameState{
		id:      data.ID,
		parent:  data.Parent,
		source:  data.Source,
		node:    data.Node,
		values:  make(map[string]any, len(data.Values)),
		cloners: make(map[string]func(any) any, len(data.Values)),
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	for name, value := range data.Values {
		clone, ok := registry[name]
		if !ok {
			return Packet[T]{}, fmt.Errorf("machine: frame names undeclared stack key %q", name)
		}
		state.values[name] = value
		state.cloners[name] = clone
	}
	return Packet[T]{payload: payload, state: state}, nil
}
