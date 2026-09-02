package main

import (
	"context"
	"fmt"
	"sync"

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
var done = make(chan struct{})
var emitOnce sync.Once

// Report prints the LOCKED TOKEN from the fixture's own body once a datum has
// crossed the subflow the `use` binds.
func Report(f machine.Frame[Order]) Order {
	emitOnce.Do(func() {
		fmt.Println("flow-sub: through=1")
		close(done)
	})

	return f.Value()
}
func Alert(e machine.NodeError[any])      { fmt.Println("handler:", e) }

func Clean(f machine.Frame[Order]) bool { return f.Value().ID != "" }
