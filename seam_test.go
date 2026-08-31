// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// reportingEdge wraps a channel edge, keeps the Report the factory handed it and
// records that Close was called. It is a NON-GENERIC Edge implementation, which is the
// shape a third-party transport takes, so the Close-idempotence contract binds it
// exactly as it binds the transports.
type reportingEdge struct {
	inner  Edge[int]
	report Report
	closed atomic.Bool
	once   sync.Once
}

// reportingFactory delegates construction to Channel so the fixture exercises the
// WIDENED factory rather than bypassing it, and publishes the built edge through hold
// so a test can drive it.
func reportingFactory(hold **reportingEdge) EdgeFactory[int] {
	return func(node string, report Report) (Edge[int], error) {
		inner, err := Channel[int](0)(node, report)
		if err != nil {
			return nil, err
		}
		edge := &reportingEdge{inner: inner, report: report}
		*hold = edge
		return edge, nil
	}
}

func (e *reportingEdge) Start(ctx context.Context) error { return e.inner.Start(ctx) }

func (e *reportingEdge) Send(ctx context.Context, frame Frame[int]) error {
	return e.inner.Send(ctx, frame)
}

func (e *reportingEdge) Receive() <-chan Frame[int] { return e.inner.Receive() }

// Close guards its WHOLE body rather than leaning on the inner edge's own idempotence:
// that would be a hop to a different receiver, and this fixture's contract should not
// depend on what it happens to wrap.
func (e *reportingEdge) Close() error {
	var err error
	e.once.Do(func() {
		e.closed.Store(true)
		err = e.inner.Close()
	})
	return err
}

// closeFailingEdge is the fixture for the other half of the closer contract: a Close
// that FAILS is reported through the same node-attributed path an edge failure takes.
type closeFailingEdge struct {
	ch   chan Frame[int]
	err  error
	once sync.Once
}

func closeFailingFactory(err error) EdgeFactory[int] {
	return func(string, Report) (Edge[int], error) {
		return &closeFailingEdge{ch: make(chan Frame[int]), err: err}, nil
	}
}

func (*closeFailingEdge) Start(context.Context) error            { return nil }
func (*closeFailingEdge) Send(context.Context, Frame[int]) error { return nil }
func (e *closeFailingEdge) Receive() <-chan Frame[int]           { return e.ch }

func (e *closeFailingEdge) Close() error {
	var err error
	e.once.Do(func() { err = e.err })
	return err
}

func TestShutdownClosesEveryEdge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var sourceEdge, dropEdge *reportingEdge
	m := New("shutdown")
	src, _ := m.Source[int]("shutdown.source", WithEdge(reportingFactory(&sourceEdge)))
	src.Drop("shutdown.drop", WithEdge(reportingFactory(&dropEdge)))

	startMachine(t, ctx, m)

	// The first half of the lifecycle, and what stops this test passing against an
	// implementation that closes edges eagerly: nothing is closed while the context lives.
	if sourceEdge.closed.Load() || dropEdge.closed.Load() {
		t.Fatal("an edge was closed while the machine's context was still live")
	}

	cancel()
	pollUntil(t, "every constructed edge was closed once the machine's context ended", func() bool {
		return sourceEdge.closed.Load() && dropEdge.closed.Load()
	})
}

// TestEdgeReportRoutesToTheReceivingNodesHandler gates the seam's whole point: an
// edge-originated failure is attributed to the node the edge DELIVERS INTO, not to the
// node that produced towards it.
func TestEdgeReportRoutesToTheReceivingNodesHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refused := errors.New("the inbound message was refused")
	errs := make(chan NodeError[int], 1)
	var dropEdge *reportingEdge
	m := New("edge-report")
	src, _ := m.Source[int]("edge-report.source")
	src.Drop("edge-report.drop",
		WithEdge(reportingFactory(&dropEdge)),
		WithErrorHandler(func(e NodeError[int]) { errs <- e }))

	startMachine(t, ctx, m)
	dropEdge.report(ctx, refused)

	failure := awaitError(t, errs)
	if failure.Node != "edge-report.drop" {
		t.Fatalf("the handler received node %q, want the RECEIVING node %q", failure.Node, "edge-report.drop")
	}
	if failure.Panic {
		t.Fatal("the handler received Panic=true for an edge-originated failure, which never panicked")
	}
	if failure.Payload != 0 {
		t.Fatalf("the handler received payload %d, want the zero payload: an edge failure has no datum",
			failure.Payload)
	}
	if !errors.Is(failure.Err, refused) {
		t.Fatalf("the handler received %v, want the edge's own %v", failure.Err, refused)
	}
}

func TestShutdownReportsAnEdgeThatFailsToClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	refused := errors.New("the transport would not shut down")
	errs := make(chan NodeError[int], 1)
	m := New("close-failure")
	src, _ := m.Source[int]("close-failure.source")
	src.Drop("close-failure.drop",
		WithEdge(closeFailingFactory(refused)),
		WithErrorHandler(func(e NodeError[int]) { errs <- e }))

	startMachine(t, ctx, m)
	cancel()

	failure := awaitError(t, errs)
	if failure.Node != "close-failure.drop" {
		t.Fatalf("the handler received node %q, want %q", failure.Node, "close-failure.drop")
	}
	if !errors.Is(failure.Err, refused) {
		t.Fatalf("the handler received %v, want an error wrapping the edge's own %v", failure.Err, refused)
	}
}

// THE BUFFER IS THE WHOLE POINT, so do not "simplify" it away. On an UNBUFFERED edge a
// closed edge's send arm is never ready, so a single select over the done channel and
// the frame channel refuses correctly by accident and this test cannot tell the correct
// implementation from the defective one. Measured over 2000 closed-edge sends per shape:
// the single-select form accepted 1028 of 2000 at buffer 4 and 0 of 2000 at buffer 0.
// The 200-send loop turns a probabilistic acceptance into a certain failure.
func TestClosedEdgeRefusesSendWithoutPanicking(t *testing.T) {
	edge, err := Channel[int](4)("closed", func(context.Context, error) {})
	if err != nil {
		t.Fatalf("channel factory: %v", err)
	}
	if failed := edge.Close(); failed != nil {
		t.Fatalf("the first Close returned %v, want nil", failed)
	}
	if failed := edge.Close(); failed != nil {
		t.Fatalf("the second Close returned %v, want nil: Close must be idempotent", failed)
	}

	ctx := context.Background()
	store := NewMemStore()
	for i := 1; i <= 200; i++ {
		failed := edge.Send(ctx, newFrame("closed", i, store))
		if !errors.Is(failed, ErrEdgeClosed) {
			t.Fatalf("send %d of 200 into a closed buffer-4 edge returned %v, want ErrEdgeClosed", i, failed)
		}
	}
}

// TestConcurrentSendAndCloseNeverPanics is the regression for the hazard wiring Close
// into shutdown introduces. Measured: against a channelEdge whose Close closes the FRAME
// channel this dies with a send on a closed channel; against the done-channel form it
// passes. It also covers the done arm of the main select, which releases a producer that
// was already parked when the edge closed.
func TestConcurrentSendAndCloseNeverPanics(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	for round := 1; round <= 200; round++ {
		edge, err := Channel[int](1)("race", func(context.Context, error) {})
		if err != nil {
			t.Fatalf("channel factory: %v", err)
		}
		var group sync.WaitGroup
		group.Add(9)
		for sender := 0; sender < 8; sender++ {
			go func() {
				defer group.Done()
				_ = edge.Send(ctx, newFrame("race", round, store))
			}()
		}
		go func() {
			defer group.Done()
			_ = edge.Close()
		}()
		group.Wait()
	}
}
