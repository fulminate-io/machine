package main

import (
	"context"
	"fmt"
	"sync"

	machine "github.com/whitaker-io/machine/v4"
)

// Order is the payload the consumer flow carries and the type the imported
// flow's branch predicate reads.
type Order struct {
	ID   string
	Kind string
}

func Feed() machine.EdgeFactory[Order] {
	return func(node string, report machine.Report) (machine.Edge[Order], error) {
		return &feedEdge{ch: make(chan machine.Packet[Order], 2)}, nil
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

func Alert(e machine.NodeError[any]) { fmt.Println("handler:", e) }

var done = make(chan struct{})
var emitOnce sync.Once

// Report prints the LOCKED TOKEN once a datum has crossed the imported flow.
func Report(f machine.Frame[Order]) Order {
	emitOnce.Do(func() {
		fmt.Println("flow-dotted: through=1")
		close(done)
	})

	return f.Value()
}
