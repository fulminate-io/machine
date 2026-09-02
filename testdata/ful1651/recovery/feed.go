package main

// The node bodies and payload for step 4.4's recovery fixture. This file is
// copied into the harness package at image build and compiled beside the code
// flowc generates from recover.flow.
//
// THE COMPLETION TOKEN IS PRINTED HERE, by the fixture's own func body, and the
// recovery recorder never prints it. That is the same discipline every other
// token in this plan follows, and it is what stops the recorder greening itself:
// the recorder can only ever LOOK for a string that a pod's own flow produced.

import (
	"context"
	"fmt"
	"sync"

	machine "github.com/whitaker-io/machine/v4"
)

// Job is the datum. It carries only an id: the observation is about WHERE the
// datum completes and WHO claimed it, never about payload fidelity.
type Job struct {
	ID string
}

// holdGate blocks the node function on the pod that is told to hold, and only on
// that pod.
//
// WHY A PER-POD IN-MEMORY GATE RATHER THAN A FLAG ON THE DATUM. The recovering
// pod must run the SAME node function and NOT block, or the datum would stall
// again and the resume could not be observed completing. A flag riding the datum
// would travel with it through the checkpoint record and block every pod in
// turn. A gate held in the owning pod's memory dies with that pod, which is
// exactly the semantics the observation needs and costs nothing to reason about.
type holdGate struct {
	mu       sync.Mutex
	held     bool
	released chan struct{}
}

var gate = &holdGate{released: make(chan struct{})}

// Hold blocks while this pod is holding, then returns the datum unchanged.
func (g *holdGate) Hold() {
	g.mu.Lock()
	held := g.held
	ch := g.released
	g.mu.Unlock()
	if !held {
		return
	}
	<-ch
}

// Set makes this pod hold the next datum it processes.
func (g *holdGate) Set() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.held {
		g.held = true
		g.released = make(chan struct{})
	}
}

// Release unblocks whatever this pod is holding and stops it holding again.
func (g *holdGate) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held {
		g.held = false
		close(g.released)
	}
}

// Held reports whether this pod is currently holding.
func (g *holdGate) Held() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held
}

// Feed is the source edge. THE TRANSPORT DELIVERS NOTHING into it: every datum
// the flow sees arrives through the generated package's exported typed ingest,
// which the harness calls from its own HTTP surface. That is what makes the
// completion token attributable to a specific push.
func Feed() machine.EdgeFactory[Job] {
	return func(node string, report machine.Report) (machine.Edge[Job], error) {
		return &feedEdge{ch: make(chan machine.Packet[Job], 4)}, nil
	}
}

type feedEdge struct{ ch chan machine.Packet[Job] }

func (e *feedEdge) Start(ctx context.Context) error { return nil }

func (e *feedEdge) Send(ctx context.Context, p machine.Packet[Job]) error {
	select {
	case e.ch <- p:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *feedEdge) Receive() <-chan machine.Packet[Job] { return e.ch }

func (e *feedEdge) Close() error { return nil }

// Hold is the checkpointed, idempotent node. A checkpoint record is journaled on
// ARRIVAL, before this body runs, so a record with a live owner exists while
// this blocks. It announces both edges of the block so a reader of the pod log
// can tell "the datum arrived and is parked" from "the datum never arrived".
func Hold(f machine.Frame[Job]) Job {
	v := f.Value()
	fmt.Printf("flow-recover: entered=%s holding=%t\n", v.ID, gate.Held())
	gate.Hold()
	fmt.Printf("flow-recover: released=%s\n", v.ID)
	return v
}

// Done is the sink. IT PRINTS THE COMPLETION TOKEN, and it is the only thing in
// this smoke that does.
func Done(f machine.Frame[Job]) Job {
	v := f.Value()
	fmt.Printf("flow-recover: completed=%s\n", v.ID)
	return v
}

// Alert is the flow-level error handler.
func Alert(e machine.NodeError[any]) { fmt.Println("flow-recover: handler:", e) }
