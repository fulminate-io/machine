package main

import (
	"context"
	"fmt"
	"sync"

	machine "github.com/whitaker-io/machine/v4"
)

type Order struct {
	ID   string
	Kind string
	Seen int
}

func Feed() machine.EdgeFactory[Order] {
	return func(node string, report machine.Report) (machine.Edge[Order], error) {
		return &feedEdge{ch: make(chan machine.Packet[Order], 4)}, nil
	}
}

type feedEdge struct{ ch chan machine.Packet[Order] }

func (e *feedEdge) Start(ctx context.Context) error            { return nil }
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

var mu sync.Mutex

func Count(f machine.Frame[Order]) Order {
	seen, _, err := f.Load(processed)
	if err != nil {
		panic(err)
	}
	if err := f.Save(processed, seen+1); err != nil {
		panic(err)
	}
	f.Set(attempt, f.Get(attempt)+1)
	out := f.Value()
	out.Seen = seen + 1
	return out
}

func Clean(f machine.Frame[Order]) bool { return f.Value().Kind != "" }

func Report(f machine.Frame[Order]) Order {
	v := f.Value()
	Emit(v.Seen)
	return v
}

func Audit(f machine.Frame[Order]) Order  { return f.Value() }
func Backoff(f machine.Frame[Order]) Order { return f.Value() }
func Alert(e machine.NodeError[any]) { fmt.Println("handler:", e) }

var done = make(chan struct{})
var emitOnce sync.Once

func Emit(n int) {
	emitOnce.Do(func() {
		fmt.Printf("flow-smoke: processed=%d\n", n)
		close(done)
	})
}
