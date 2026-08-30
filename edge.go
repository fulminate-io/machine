// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import "context"

// Edge is the transport seam. Edges carry Frame values rather than bare payloads,
// because the execution state must travel with the datum; the in-memory channel
// edge passes the frame by reference, and a remote transport marshals it with a
// Codec. A node owns its INBOUND edge, selected with WithEdge.
type Edge[T any] interface {
	Start(ctx context.Context) error
	Send(ctx context.Context, frame Frame[T]) error
	Receive() <-chan Frame[T]
	Close() error
}

// EdgeFactory constructs the edge that delivers into the named node. It takes no
// context because the graph is declared before it runs: factories are invoked at
// declaration time, and Machine.Start brings the returned edge up via Edge.Start.
//
// It is a function type rather than an interface because Go forbids type parameters
// on interface methods, so no interface can express a generic constructor.
type EdgeFactory[T any] func(node string) (Edge[T], error)

// Codec marshals and unmarshals a Frame for a remote transport. It is stated in
// terms of Frame rather than a bare payload because the execution state must cross
// the wire with the datum: an implementation projects the state with Frame.Data and
// restores it with RebuildFrame.
type Codec[T any] interface {
	Marshal(frame Frame[T]) ([]byte, error)
	Unmarshal(data []byte) (Frame[T], error)
}

type channelEdge[T any] struct {
	ch chan Frame[T]
}

// Channel returns the default in-memory transport. The buffer size is a property of
// the channel edge rather than of the machine, because buffering is meaningful only
// to a channel edge and not to a network or pub/sub transport.
func Channel[T any](buffer int) EdgeFactory[T] {
	return func(_ string) (Edge[T], error) {
		return &channelEdge[T]{ch: make(chan Frame[T], buffer)}, nil
	}
}

func (*channelEdge[T]) Start(_ context.Context) error { return nil }

// Send blocks until the frame is accepted or the context is done. The done case is
// what makes a canceled machine unblock: a bare channel send never does.
func (c *channelEdge[T]) Send(ctx context.Context, frame Frame[T]) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.ch <- frame:
		return nil
	}
}

func (c *channelEdge[T]) Receive() <-chan Frame[T] { return c.ch }

func (c *channelEdge[T]) Close() error {
	close(c.ch)
	return nil
}
