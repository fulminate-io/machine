// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"context"
	"fmt"
	"time"
)

const (
	leftSuffix  = ".left"
	rightSuffix = ".right"
)

// nodeConfig holds one node's declared inbound transport, error handling and
// capabilities. Go permits exactly one variadic parameter, so a node's option list
// carries exactly one type parameter, and decision-locked ErrorHandler[U] typing
// makes that parameter the node's INPUT type. A node therefore owns its INBOUND
// edge, which is what WithEdge selects.
type nodeConfig[T any] struct {
	factory EdgeFactory[T]
	handler ErrorHandler[T]
	reads   []KeyRef
	writes  []KeyRef
}

// NodeOption configures a node at declaration time.
type NodeOption[T any] func(*nodeConfig[T])

// WithEdge selects the transport that DELIVERS INTO the node. The default is
// Channel with an unbuffered in-memory channel.
func WithEdge[T any](f EdgeFactory[T]) NodeOption[T] {
	return func(c *nodeConfig[T]) { c.factory = f }
}

// WithErrorHandler registers the node's typed error handler, which wins over the
// machine's global handler.
func WithErrorHandler[T any](h ErrorHandler[T]) NodeOption[T] {
	return func(c *nodeConfig[T]) { c.handler = h }
}

// WithReads declares the handles the node may read. It takes KeyRef, so ONE
// declaration covers stack keys and heap cells together: they share one capability
// model over two namespaces. A node declaring nothing can call only Frame.Value.
//
// The type parameter cannot be inferred from a KeyRef list, so a call site writes
// it, as WithReads[*payload](aKey, aCell).
func WithReads[T any](refs ...KeyRef) NodeOption[T] {
	return func(c *nodeConfig[T]) { c.reads = append(c.reads, refs...) }
}

// WithWrites declares the handles the node may write. A write capability does NOT
// imply a read capability, so a node that reads, modifies and writes one handle
// declares it in both. See WithReads for the type-parameter note.
func WithWrites[T any](refs ...KeyRef) NodeOption[T] {
	return func(c *nodeConfig[T]) { c.writes = append(c.writes, refs...) }
}

// runner processes one datum for a node.
type runner[I any] func(ctx context.Context, f Frame[I])

// emitter is a node's outbound hook. Because a node owns its inbound edge, an
// emitter is bound only once the downstream node is declared, which is why the
// graph is declared lazily and Start does real work.
type emitter[T any] struct {
	machine  *Machine
	producer string
	edge     Edge[T]
	consumer string
}

func newEmitter[T any](m *Machine, producer string) *emitter[T] {
	hook := &emitter[T]{machine: m, producer: producer}
	m.addCheck(func() error {
		if hook.edge == nil {
			return fmt.Errorf("machine: the flow produced by %q is never consumed", producer)
		}
		return nil
	})
	return hook
}

func (e *emitter[T]) bind(consumer string, edge Edge[T]) {
	if e.edge != nil {
		e.machine.fail(fmt.Errorf("machine: the flow produced by %q is consumed by both %q and %q",
			e.producer, e.consumer, consumer))
		return
	}
	e.edge = edge
	e.consumer = consumer
}

// send clears the frame's capability view on the way out, so state access is
// possible only inside a node that declared it.
func (e *emitter[T]) send(ctx context.Context, f Frame[T]) error {
	return e.edge.Send(ctx, Frame[T]{payload: f.payload, state: f.state})
}

// worker is the shared per-node runtime every builder method drives.
type worker[I any] struct {
	machine *Machine
	name    string
	edge    Edge[I]
	handler ErrorHandler[I]
	caps    *capabilities
	record  *node
}

func newWorker[I any](m *Machine, name string, opts ...NodeOption[I]) *worker[I] {
	cfg := &nodeConfig[I]{factory: Channel[I](0)}
	for _, opt := range opts {
		opt(cfg)
	}
	w := &worker[I]{machine: m, name: name, handler: cfg.handler, caps: newCapabilities(name, cfg)}
	w.record = m.register(name)
	edge, err := cfg.factory(name, w.report)
	if err != nil {
		m.fail(fmt.Errorf("machine: the edge factory for node %q failed: %w", name, err))
		return w
	}
	w.edge = edge
	m.addEdge(edge.Start)
	m.addCloser(func(ctx context.Context) {
		if failure := edge.Close(); failure != nil {
			w.report(ctx, fmt.Errorf("machine: closing the edge for node %q: %w", name, failure))
		}
	})
	return w
}

func newCapabilities[I any](name string, cfg *nodeConfig[I]) *capabilities {
	caps := &capabilities{
		node:   name,
		reads:  make(map[string]struct{}, len(cfg.reads)),
		writes: make(map[string]struct{}, len(cfg.writes)),
	}
	for _, ref := range cfg.reads {
		caps.reads[ref.Name()] = struct{}{}
	}
	for _, ref := range cfg.writes {
		caps.writes[ref.Name()] = struct{}{}
	}
	return caps
}

// readLoop is the node's consumer. It returns on cancellation or on a closed
// inbound channel, and allocates the concurrency semaphore once rather than once
// per datum.
func (w *worker[I]) readLoop(ctx context.Context, run runner[I]) {
	var sem chan struct{}
	if w.machine.cfg.maxConcurrency > 0 {
		sem = make(chan struct{}, w.machine.cfg.maxConcurrency)
	}
	in := w.edge.Receive()
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-in:
			if !ok {
				return
			}
			w.step(ctx, sem, run, f)
		}
	}
}

// step dispatches one datum: inline under FIFO, through the semaphore when bounded,
// and as a bare goroutine when unbounded, which is the default and the prior
// behavior.
func (w *worker[I]) step(ctx context.Context, sem chan struct{}, run runner[I], f Frame[I]) {
	if w.machine.cfg.fifo {
		run(ctx, f)
		return
	}
	if sem == nil {
		go run(ctx, f)
		return
	}
	sem <- struct{}{}
	go func() {
		defer func() { <-sem }()
		run(ctx, f)
	}()
}

// bind is the state handoff, and the ONLY place a node is given reach into state.
// It stamps the node name, attaches THIS node's capability view and attaches the
// machine's private heap store. A frame that was never bound has a nil capability
// view, so every gated accessor fails loudly.
func (w *worker[I]) bind(f Frame[I]) Frame[I] {
	f.state.node = w.name
	f.state.store = w.machine.cfg.store
	return Frame[I]{payload: f.payload, state: f.state, caps: w.caps}
}

// guard is the panic boundary and runs for every datum in every node kind. It
// recovers inside the spawned goroutine, because a panic in a child goroutine is
// not catchable from its parent and would take the process down. A recovered error
// value is preserved rather than flattened to a string, so a handler can recover a
// typed error such as *CapabilityError with errors.As.
//
// Being the universal per-datum path, it is also the span boundary. The order in the
// deferred block is load-bearing: dispatch, then the frame state is released, and
// only then finish, which ends the span — so an ended span orders an outside reader
// after the reclaim.
func (w *worker[I]) guard(ctx context.Context, f Frame[I], fn runner[I]) {
	spanCtx, span := w.record.instruments.start(ctx)
	started := time.Now()
	defer func() {
		if r := recover(); r != nil {
			failure, ok := r.(error)
			if !ok {
				failure = fmt.Errorf("machine: node %q panicked: %v", w.name, r)
			}
			w.dispatch(spanCtx, NodeError[I]{Node: w.name, Err: failure, Payload: f.Value(), Panic: true})
			f.state.release()
		}
		w.record.instruments.finish(spanCtx, span, started)
	}()
	fn(spanCtx, f)
}

// dispatch routes a node failure: the typed per-node handler wins, otherwise the
// erased global handler, otherwise the default no-op drop. Nothing is retained for
// redelivery — retry and dead-lettering are composed by the user inside a handler.
//
// It is also the single funnel EVERY node failure passes through, so it is where the
// failure is recorded on the datum's span and counted. Telemetry observes there and
// decides nothing: the routing below is unchanged by it.
func (w *worker[I]) dispatch(ctx context.Context, err NodeError[I]) {
	w.record.instruments.observeError(ctx, err.Err)
	if w.handler != nil {
		w.handler(err)
		return
	}
	global := w.machine.cfg.handler
	if global == nil {
		return
	}
	global(NodeError[any]{Node: err.Node, Err: err.Err, Payload: err.Payload, Panic: err.Panic})
}

// report is the edge's half of the error contract, handed to every EdgeFactory. An
// edge-originated failure has no datum to attribute, so the NodeError carries the zero
// payload and Panic is false; routing below this point is the node's own dispatch,
// unchanged, which is what makes an edge failure and a node failure land in the same
// handler.
func (w *worker[I]) report(ctx context.Context, err error) {
	var zero I
	w.dispatch(ctx, NodeError[I]{Node: w.name, Err: err, Payload: zero, Panic: false})
}

func (w *worker[I]) emit(ctx context.Context, out *emitter[I], f Frame[I]) {
	if err := out.send(ctx, f); err != nil {
		w.dispatch(ctx, NodeError[I]{Node: w.name, Err: err, Payload: f.Value()})
	}
}

// transform is a generic method on the already-generic worker, carrying the OUTPUT
// type parameter. It re-wraps the node function's bare return value onto the SAME
// frame state: the runtime owns frame propagation, so a node can neither drop the
// frame nor forge one.
func (w *worker[I]) transform[O any](out *emitter[O], fn func(f Frame[I]) O) runner[I] {
	return func(ctx context.Context, f Frame[I]) {
		w.guard(ctx, w.bind(f), func(spanCtx context.Context, inner Frame[I]) {
			next := rewrap(inner, fn(inner))
			if err := out.send(spanCtx, next); err != nil {
				w.dispatch(spanCtx, NodeError[I]{Node: w.name, Err: err, Payload: inner.Value()})
			}
		})
	}
}

func (w *worker[I]) route(onTrue, onFalse *emitter[I], fn Filter[I]) runner[I] {
	return func(ctx context.Context, f Frame[I]) {
		w.guard(ctx, w.bind(f), func(spanCtx context.Context, inner Frame[I]) {
			out := onFalse
			if fn(inner) {
				out = onTrue
			}
			w.emit(spanCtx, out, inner)
		})
	}
}

func (w *worker[I]) split(onLeft, onRight *emitter[I], fn Duplicator[I]) runner[I] {
	return func(ctx context.Context, f Frame[I]) {
		w.guard(ctx, w.bind(f), func(spanCtx context.Context, inner Frame[I]) {
			left, right := fn(inner.Value())
			w.emit(spanCtx, onLeft, Frame[I]{payload: left, state: inner.state.clone()})
			w.emit(spanCtx, onRight, Frame[I]{payload: right, state: inner.state.clone()})
		})
	}
}

func (w *worker[I]) drain() runner[I] {
	return func(ctx context.Context, f Frame[I]) {
		w.guard(ctx, w.bind(f), func(_ context.Context, inner Frame[I]) {
			inner.state.release()
		})
	}
}

// Flow is a declared node's outbound handle. T is the source payload type and U the
// current one. It holds only the machine and the emitter, so it is cheap to pass by
// value and a Flow value denotes a node's outbound edge, not the node itself.
type Flow[T, U any] struct {
	machine *Machine
	out     *emitter[U]
}

// Source declares an ingestion node and returns its flow together with the Ingest
// closure that feeds it. The frame is born here, at ingestion, and nowhere else.
//
// Ingest returning does not mean the datum has finished traversing the graph:
// assertions about downstream effects must synchronize on a drained terminal or
// poll with a deadline.
func (m *Machine) Source[T any](name string, opts ...NodeOption[T]) (Flow[T, T], Ingest[T]) {
	w := newWorker[T](m, name, opts...)
	out := newEmitter[T](m, name)
	w.record.run = func(ctx context.Context) {
		go w.readLoop(ctx, w.transform(out, func(f Frame[T]) T { return f.Value() }))
	}
	ingest := func(ctx context.Context, payload T) error {
		return w.edge.Send(ctx, newFrame(name, payload, m.cfg.store))
	}
	return Flow[T, T]{machine: m, out: out}, ingest
}

// Map applies fn to each frame and forwards the transformed payload, changing the
// flow's payload type. V is inferred from the transformation.
func (f Flow[T, U]) Map[V any](name string, fn Transformation[U, V], opts ...NodeOption[U]) Flow[T, V] {
	w := newWorker[U](f.machine, name, opts...)
	f.out.bind(name, w.edge)
	out := newEmitter[V](f.machine, name)
	w.record.run = func(ctx context.Context) {
		go w.readLoop(ctx, w.transform(out, fn))
	}
	return Flow[T, V]{machine: f.machine, out: out}
}

// Recurse applies a recursive function to the payload through a Y Combinator. The
// recursive continuation arrives wrapped in a frame carrying the same state and
// capability view, so the recursion body can still reach declared handles.
func (f Flow[T, U]) Recurse(name string, fn Monad[Monad[U]], opts ...NodeOption[U]) Flow[T, U] {
	g := func(h recursiveBaseFn[U]) Monad[U] {
		return func(inner Frame[U]) U {
			return fn(rewrap(inner, h(h)))(inner)
		}
	}
	return f.Map(name, Transformation[U, U](g(g)), opts...)
}

// Memoize applies a recursive function through a Y Combinator and memoizes the
// results per datum, keyed by the index function.
func (f Flow[T, U]) Memoize(name string, fn Monad[Monad[U]], index func(U) string, opts ...NodeOption[U]) Flow[T, U] {
	g := func(h memoizedBaseFn[U], cache map[string]U) Monad[U] {
		return func(inner Frame[U]) U {
			id := index(inner.Value())
			if seen, ok := cache[id]; ok {
				return seen
			}
			cache[id] = fn(rewrap(inner, h(h, cache)))(inner)
			return cache[id]
		}
	}
	p := Monad[U](func(inner Frame[U]) U { return g(g, map[string]U{})(inner) })
	return f.Map(name, Transformation[U, U](p), opts...)
}

// If splits the flow, routing the INTACT frame down one branch: no copy and no
// reparenting, so identity and lineage survive the branch unchanged. The branches
// are named with a .left and .right suffix, so an unconsumed-branch error at Start
// identifies which side was left dangling.
func (f Flow[T, U]) If(name string, fn Filter[U], opts ...NodeOption[U]) (left, right Flow[T, U]) {
	w := newWorker[U](f.machine, name, opts...)
	f.out.bind(name, w.edge)
	onTrue := newEmitter[U](f.machine, name+leftSuffix)
	onFalse := newEmitter[U](f.machine, name+rightSuffix)
	w.record.run = func(ctx context.Context) {
		go w.readLoop(ctx, w.route(onTrue, onFalse, fn))
	}
	return Flow[T, U]{machine: f.machine, out: onTrue}, Flow[T, U]{machine: f.machine, out: onFalse}
}

// Tee duplicates the flow, DEEP-COPYING the envelope into both branches so the
// split is a functional, non-locking process. The payload is split by the caller's
// Duplicator and the frame is cloned by the runtime; both branches get fresh
// identities and both report the upstream frame as their parent.
func (f Flow[T, U]) Tee(name string, fn Duplicator[U], opts ...NodeOption[U]) (left, right Flow[T, U]) {
	w := newWorker[U](f.machine, name, opts...)
	f.out.bind(name, w.edge)
	onLeft := newEmitter[U](f.machine, name+leftSuffix)
	onRight := newEmitter[U](f.machine, name+rightSuffix)
	w.record.run = func(ctx context.Context) {
		go w.readLoop(ctx, w.split(onLeft, onRight, fn))
	}
	return Flow[T, U]{machine: f.machine, out: onLeft}, Flow[T, U]{machine: f.machine, out: onRight}
}

// Send merges this flow into the SAME downstream consumer that target already
// feeds. Closing a cycle therefore means passing the flow that PRECEDES the node to
// re-enter, not the flow that node produces.
//
// The target must already have a consumer, so the node being re-entered is declared
// BEFORE the Send that closes the loop; a target with no consumer yet is a
// declaration error reported from Start.
func (f Flow[T, U]) Send(target Flow[T, U]) {
	if target.out.edge == nil {
		f.machine.fail(fmt.Errorf("machine: the Send target produced by %q has no consumer yet; "+
			"declare the node being re-entered before closing the loop", target.out.producer))
		return
	}
	f.out.bind(target.out.consumer, target.out.edge)
}

// Drop terminates the flow, discarding each frame and reclaiming its stack state at
// traversal end.
func (f Flow[T, U]) Drop(name string, opts ...NodeOption[U]) {
	w := newWorker[U](f.machine, name, opts...)
	f.out.bind(name, w.edge)
	w.record.run = func(ctx context.Context) {
		go w.readLoop(ctx, w.drain())
	}
}

// Output is the terminal consumption surface: it hands the caller the channel of
// frames leaving the flow. Frames reaching it belong to the CALLER and are NOT
// reclaimed. Output does not process, so it does not advance the frame's Node stamp
// and has no read loop of its own.
func (f Flow[T, U]) Output(name string, opts ...NodeOption[U]) <-chan Frame[U] {
	w := newWorker[U](f.machine, name, opts...)
	f.out.bind(name, w.edge)
	if w.edge == nil {
		return nil
	}
	return w.edge.Receive()
}
