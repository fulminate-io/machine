// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// passThrough is the do-nothing node function the declaration tests hang graphs off.
// They assert on what Start reports, so what the node computes is irrelevant.
func passThrough(f Frame[int]) int { return f.Value() }

// failingFactory is an EdgeFactory that never produces an edge, which leaves the node
// it was given to without a transport.
func failingFactory[T any](err error) EdgeFactory[T] {
	return func(string, Report) (Edge[T], error) { return nil, err }
}

// haltingEdge refuses to come up: Start returns an error rather than nil, which is the
// failure Machine.Start surfaces from startEdges before it spawns anything.
type haltingEdge[T any] struct {
	ch   chan Frame[T]
	err  error
	stop sync.Once
}

func (e *haltingEdge[T]) Start(context.Context) error        { return e.err }
func (*haltingEdge[T]) Send(context.Context, Frame[T]) error { return nil }
func (e *haltingEdge[T]) Receive() <-chan Frame[T]           { return e.ch }
func (e *haltingEdge[T]) Close() error                       { e.stop.Do(func() { close(e.ch) }); return nil }

func TestSecondStartIsRefused(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := New("restart")
	src, _ := m.Source[int]("restart.source")
	src.Drop("restart.drop")

	// The known positive: the first Start succeeds, so the refusal below is the second
	// call rather than a graph that could never come up.
	startMachine(t, ctx, m)

	err := m.Start(ctx)
	if err == nil {
		t.Fatal("Start accepted a second start")
	}
	if !strings.Contains(err.Error(), "was already started") {
		t.Fatalf("the second Start returned %v, want an error saying the machine was already started", err)
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Fatalf("the second Start returned %v, want an error naming the machine", err)
	}
}

func TestDoublyConsumedFlowFailsStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := New("doubly-consumed")
	src, _ := m.Source[int]("doubly-consumed.source")
	src.Drop("doubly-consumed.first")
	src.Drop("doubly-consumed.second")

	err := m.Start(ctx)
	if err == nil {
		t.Fatal("Start accepted a flow consumed by two nodes")
	}
	if !strings.Contains(err.Error(), "consumed by both") {
		t.Fatalf("Start returned %v, want an error saying the flow is consumed by both consumers", err)
	}
	// All three names, because the error's whole job is to say WHICH flow was consumed
	// twice and by whom.
	for _, name := range []string{"doubly-consumed.source", "doubly-consumed.first", "doubly-consumed.second"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("Start returned %v, want an error naming %q", err, name)
		}
	}
}

func TestEdgeFactoryFailureFailsStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refused := errors.New("no transport available")
	m := New("factory")
	src, _ := m.Source[int]("factory.source")
	src.Map("factory.node", passThrough, WithEdge(failingFactory[int](refused))).Drop("factory.drop")

	err := m.Start(ctx)
	if err == nil {
		t.Fatal("Start accepted a node whose edge factory failed")
	}
	if !errors.Is(err, refused) {
		t.Fatalf("Start returned %v, want an error wrapping the factory's own %v", err, refused)
	}
	if !strings.Contains(err.Error(), "factory.node") {
		t.Fatalf("Start returned %v, want an error naming the node whose factory failed", err)
	}
}

func TestOutputHandsBackNoChannelWhenItsEdgeFactoryFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refused := errors.New("terminal transport unavailable")
	m := New("output-factory")
	good, _ := m.Source[int]("output-factory.good")
	bad, _ := m.Source[int]("output-factory.bad")

	// The known positive: a terminal whose factory SUCCEEDS hands back a channel, so the
	// nil below is the refusal rather than an Output that never returns one.
	working := good.Output("output-factory.working")
	if working == nil {
		t.Fatal("Output returned no channel for a terminal whose edge factory succeeded")
	}

	broken := bad.Output("output-factory.broken", WithEdge(failingFactory[int](refused)))
	if broken != nil {
		t.Fatal("Output handed back a channel for a terminal whose edge factory failed")
	}

	err := m.Start(ctx)
	if err == nil {
		t.Fatal("Start accepted a terminal whose edge factory failed")
	}
	if !errors.Is(err, refused) {
		t.Fatalf("Start returned %v, want an error wrapping the factory's own %v", err, refused)
	}
	if !strings.Contains(err.Error(), "output-factory.broken") {
		t.Fatalf("Start returned %v, want an error naming the terminal whose factory failed", err)
	}
}

func TestSendToATargetWithNoConsumerFailsStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := New("premature-loop")
	src, _ := m.Source[int]("premature-loop.source")
	body := src.Map("premature-loop.body", passThrough)
	// Closing the loop onto a flow whose consumer has not been declared yet is exactly
	// what the Send contract forbids: the node being re-entered is declared first.
	body.Send(body)

	err := m.Start(ctx)
	if err == nil {
		t.Fatal("Start accepted a Send onto a target with no consumer")
	}
	if !strings.Contains(err.Error(), "has no consumer yet") {
		t.Fatalf("Start returned %v, want an error saying the Send target has no consumer yet", err)
	}
	if !strings.Contains(err.Error(), "premature-loop.body") {
		t.Fatalf("Start returned %v, want an error naming the target's producer", err)
	}
}

func TestStartReportsAnEdgeThatRefusesToComeUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refused := errors.New("edge refused to come up")
	m := New("halting")
	src, _ := m.Source[int]("halting.source", WithEdge[int](func(string, Report) (Edge[int], error) {
		return &haltingEdge[int]{ch: make(chan Frame[int]), err: refused}, nil
	}))
	src.Drop("halting.drop")

	err := m.Start(ctx)
	if err == nil {
		t.Fatal("Start accepted a machine whose edge refused to come up")
	}
	if !errors.Is(err, refused) {
		t.Fatalf("Start returned %v, want the edge's own %v", err, refused)
	}
}
