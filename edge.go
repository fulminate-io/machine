// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"context"
	"errors"
	"sync"
)

// ErrEdgeClosed is what an edge returns from Send once it has been closed. It is a
// refusal rather than a silent drop: a closed edge has nobody left to deliver to.
var ErrEdgeClosed = errors.New("machine: the edge is closed")

// Edge is the transport seam. Edges carry Packet values rather than bare payloads,
// because the execution state must travel with the datum; the in-memory channel
// edge passes the packet by reference, and a remote transport marshals it with a
// Codec. A node owns its INBOUND edge, selected with WithEdge.
//
// An edge never sees a Frame. The runtime converts on the way out, stripping the
// capability view as it goes, and on the way in it mints a fresh frame carrying the
// RECEIVING node's declared capability view. A transport therefore has no reach into
// state at all: it holds an identity view and a projection, and nothing else.
//
// Close must be idempotent. The runtime closes every constructed edge when the
// machine's context ends, and a caller may close one directly as well, so the
// behavior after the first call is defined here rather than left undefined the
// way io.Closer leaves it.
type Edge[T any] interface {
	Start(ctx context.Context) error
	Send(ctx context.Context, packet Packet[T]) error
	Receive() <-chan Packet[T]
	Close() error
}

// Report is how an edge hands a failure back to the supervisor. An edge calls it for a
// failure that has NO datum to attribute — a refused inbound message, a broken
// connection — and the supervisor routes it exactly as it routes a node failure, to the
// node the edge delivers into: the typed per-node handler if one is registered,
// otherwise the machine's global handler. The NodeError carries the ZERO payload,
// because there is no datum, and Panic is false, because nothing panicked. The runtime
// always supplies a non-nil Report, so an edge never needs to check.
type Report func(ctx context.Context, err error)

// EdgeFactory constructs the edge that delivers into the named node, and is handed the
// report path that node's failures travel. It takes no context because the graph is
// declared before it runs: factories are invoked at declaration time, and Machine.Start
// brings the returned edge up via Edge.Start.
//
// It is a function type rather than an interface because Go forbids type parameters
// on interface methods, so no interface can express a generic constructor.
type EdgeFactory[T any] func(node string, report Report) (Edge[T], error)

// Codec marshals and unmarshals a Packet for a remote transport. It is stated in
// terms of Packet rather than a bare payload because the execution state must cross
// the wire with the datum: an implementation projects the state with Packet.Data and
// restores it with RebuildPacket.
type Codec[T any] interface {
	Marshal(packet Packet[T]) ([]byte, error)
	Unmarshal(data []byte) (Packet[T], error)
}

type channelEdge[T any] struct {
	ch   chan Packet[T]
	stop sync.Once
	done chan struct{}
}

// Channel returns the default in-memory transport. The buffer size is a property of
// the edge rather than of the machine, because a buffer's meaning is transport-specific:
// each transport that buffers sets its own depth where the edge is constructed.
func Channel[T any](buffer int) EdgeFactory[T] {
	return func(_ string, _ Report) (Edge[T], error) {
		return &channelEdge[T]{ch: make(chan Packet[T], buffer), done: make(chan struct{})}, nil
	}
}

func (*channelEdge[T]) Start(_ context.Context) error { return nil }

// Send blocks until the packet is accepted, the context is done or the edge closes. The
// leading non-blocking select is what makes a closed edge REFUSE rather than probably
// refuse: with the done arm only in the main select, a closed edge that still has spare
// buffer offers two ready arms and Go chooses uniformly, so it silently accepts about
// half of everything offered into a channel nobody will read. The done arm stays in the
// main select too, so a producer already parked there is released when the edge closes.
func (c *channelEdge[T]) Send(ctx context.Context, packet Packet[T]) error {
	select {
	case <-c.done:
		return ErrEdgeClosed
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrEdgeClosed
	case c.ch <- packet:
		return nil
	}
}

func (c *channelEdge[T]) Receive() <-chan Packet[T] { return c.ch }

// Close signals the refusal rather than closing the packet channel: a producer racing
// shutdown would panic on a send to a closed channel, and the sync.Once is what makes
// the second close the interface documents a no-op.
func (c *channelEdge[T]) Close() error {
	c.stop.Do(func() { close(c.done) })
	return nil
}
