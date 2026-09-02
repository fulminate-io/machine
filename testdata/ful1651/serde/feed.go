package main

import (
	"context"
	"fmt"

	machine "github.com/whitaker-io/machine/v4"
)

// Order is the payload the serde flow carries. Nothing about it is unusual; the
// fixture's subject is the STATE block, not the payload.
type Order struct {
	ID string
}

func Feed() machine.EdgeFactory[Order] {
	return func(node string, report machine.Report) (machine.Edge[Order], error) {
		return &feedEdge{ch: make(chan machine.Packet[Order], 1)}, nil
	}
}

type feedEdge struct{ ch chan machine.Packet[Order] }

func (e *feedEdge) Start(ctx context.Context) error { return nil }
func (e *feedEdge) Send(ctx context.Context, p machine.Packet[Order]) error {
	select {
	case e.ch <- p:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (e *feedEdge) Receive() <-chan machine.Packet[Order] { return e.ch }
func (e *feedEdge) Close() error                          { return nil }

func Count(f machine.Frame[Order]) Order  { return f.Value() }
func Report(f machine.Frame[Order]) Order { return f.Value() }
func Alert(e machine.NodeError[any])      { fmt.Println("handler:", e) }
