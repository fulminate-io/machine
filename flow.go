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
	factory    EdgeFactory[T]
	handler    ErrorHandler[T]
	reads      []KeyRef
	writes     []KeyRef
	codec      Codec[T]
	checkpoint bool
	// codecDeclared records that WithCodec was declared, on the same terms and for
	// the same reason checkpoint is recorded apart from codec: WithCodec(nil) is a
	// declaration with a missing codec and must be refused, while a node that never
	// declared one must not be. It is deliberately NOT the checkpoint flag — a codec
	// without a checkpoint is the whole point of that option.
	codecDeclared bool
	idempotent    bool
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

// WithCheckpoint declares that this node journals its datum's progress, marshaling
// the packet with the given codec.
//
// THE CODEC IS REQUIRED AND NEVER DEFAULTED. A nil codec is refused at declaration
// time rather than silently substituting the gob codec: bad input errors here, and a
// machine that quietly picked an encoding would journal bytes the reading build may
// not decode.
//
// WHICH SIDE of the node function the record is written on is decided by
// WithIdempotent rather than here; see that option for the two anchors.
//
// The declaration is recorded SEPARATELY from the codec, because those are two
// different facts: WithCheckpoint(nil) is a declaration with a missing codec and must
// be refused, while a node that never declared a checkpoint must not be.
//
// It is WithCodec PLUS the checkpoint flag, and the two read as one mechanism: this
// option gives a node a codec AND journals it; WithCodec gives it a codec and does
// not.
func WithCheckpoint[T any](codec Codec[T]) NodeOption[T] {
	return func(c *nodeConfig[T]) { c.checkpoint = true; c.codec = codec }
}

// WithCodec gives a node a codec WITHOUT checkpointing it.
//
// WHY THE RUNTIME NEEDS AN OPTION SETTING ONLY HALF OF WithCheckpoint. A node anchored
// on COMPLETION journals what it PRODUCED, and it reaches the codec for that through
// its outbound emitter: bind copies the CONSUMER's codec into the emitter, so the
// codec marshaling a completion record belongs to the SUCCESSOR rather than to the
// checkpointed node. Without this option the only way to give a successor a codec was
// WithCheckpoint, which also journals it — leaving an author whose successor should
// not be journaled to choose between an unwanted checkpoint and marking a node
// idempotent that is not. Both remedies make the author pay for an implementation
// seam.
//
// A NIL CODEC IS REFUSED AT DECLARATION, on exactly the terms WithCheckpoint(nil) is:
// bad input errors at the point of the mistake rather than being defaulted, because a
// silently substituted encoding would journal bytes the reading build may not decode.
// The declaration is recorded SEPARATELY from the codec for the reason it is there —
// WithCodec(nil) is a declaration with a missing codec and must be refused, while a
// node that never declared one must not be.
//
// It selects no anchor and implies no checkpoint. A node carrying only this option
// journals nothing of its own.
func WithCodec[T any](codec Codec[T]) NodeOption[T] {
	return func(c *nodeConfig[T]) { c.codecDeclared = true; c.codec = codec }
}

// WithIdempotent marks the node safe to run again on the same datum, which SELECTS
// THE CHECKPOINT ANCHOR.
//
// A MARKED node anchors on ARRIVAL: the input packet is journaled BEFORE the node
// function runs, and recovery hands that record back to this same node, which runs
// again — safe precisely because the author declared it so.
//
// An UNMARKED node anchors on COMPLETION, and completion is the DEFAULT: the output
// is journaled AFTER the node function returns, and resume re-injects it into the
// node's SUCCESSORS without re-running the node, which is what keeps a
// non-idempotent node's side effects from happening twice.
//
// It takes no argument, mirroring the grammar's bare clause.
func WithIdempotent[T any]() NodeOption[T] {
	return func(c *nodeConfig[T]) { c.idempotent = true }
}

// journalArrival writes the ARRIVAL record: the packet the edge delivered, BEFORE
// the node function runs. It is uniform across every runner kind, because every kind
// receives a packet whatever it goes on to do with it.
//
// A JOURNAL FAILURE IS NOT SWALLOWED. It routes through dispatch, the single funnel
// every node failure already passes, so it reaches the node's typed handler or the
// machine's global one and is counted on the datum's span. The datum still proceeds:
// refusing to process it would convert a durability failure into a liveness failure,
// and that trade belongs to the caller's handler rather than to this runtime.
func (w *worker[I]) journalArrival(ctx context.Context, p Packet[I]) {
	if !w.checkpoint || !w.idempotent || w.codec == nil {
		return
	}

	data, err := w.codec.Marshal(p)
	if err != nil {
		w.dispatch(ctx, NodeError[I]{Node: w.name, Payload: p.Value(),
			Err: fmt.Errorf("machine: marshaling the arrival checkpoint for node %q: %w", w.name, err)})

		return
	}
	w.write(ctx, AnchorArrival, p.ID(), data, p.Value())
}

// write hands one record to the machine's journal and routes a failure.
func (w *worker[I]) write(ctx context.Context, anchor, datum string, data []byte, payload I) {
	record := CheckpointRecord{
		Flow: w.machine.name, Datum: datum, Node: w.name, Anchor: anchor, Data: data,
	}
	if err := w.machine.cfg.journal.Checkpoint(ctx, record); err != nil {
		w.dispatch(ctx, NodeError[I]{Node: w.name, Payload: payload,
			Err: fmt.Errorf("machine: journaling the %s checkpoint for node %q: %w", anchor, w.name, err)})
	}
}

// journalCompletion writes the COMPLETION record: what the node PRODUCED, after its
// function returned and before the value is sent onward.
//
// IT IS A FUNCTION RATHER THAN A METHOD because it spans two type parameters. The
// node's input type is the worker's; the produced value's type is the emitter's, and
// on a Map those differ — which is the whole reason the codec is read off the
// emitter rather than off the worker.
func journalCompletion[I, O any](ctx context.Context, w *worker[I], out *emitter[O], f Frame[O], payload I) {
	if !w.checkpoint || w.idempotent || out.codec == nil {
		return
	}

	packet := packetOf(f)
	data, err := out.codec.Marshal(packet)
	if err != nil {
		w.dispatch(ctx, NodeError[I]{Node: w.name, Payload: payload,
			Err: fmt.Errorf("machine: marshaling the completion checkpoint for node %q: %w", w.name, err)})

		return
	}

	// THE RECORD NAMES THE EMITTER, NOT THE NODE, and on a single-outlet node those
	// are the same string. They differ on a BRANCHING node, where the emitter is
	// name.left or name.right — and that difference is load-bearing: a route journals
	// at whichever branch its filter chose and a split journals at BOTH, so a record
	// that named only the node could not tell resume which outlet to re-inject it
	// into. Naming the emitter makes each record self-placing.
	record := CheckpointRecord{
		Flow: w.machine.name, Datum: packet.ID(), Node: out.producer,
		Anchor: AnchorCompletion, Data: data,
	}
	if err := w.machine.cfg.journal.Checkpoint(ctx, record); err != nil {
		w.dispatch(ctx, NodeError[I]{Node: w.name, Payload: payload,
			Err: fmt.Errorf("machine: journaling the completion checkpoint for node %q: %w", w.name, err)})
	}
}

// resume re-places the datums a dead worker left behind for this node.
//
// IT HONORS THE ANCHOR THE RECORD WAS WRITTEN AT, which is the whole point of
// carrying the anchor rather than re-deriving it. An ARRIVAL record is this node's
// INPUT, so it goes back onto this node's own edge and the node RUNS AGAIN — safe
// because the author declared it idempotent. A COMPLETION record is this node's
// OUTPUT, so it goes onto the OUTBOUND edge and is re-injected into the SUCCESSORS;
// this node is never re-run, which is what keeps a non-idempotent node's side effects
// from happening twice.
//
// IT PARKS IN Orphans ITSELF rather than reading a shutdown flag and then waiting on
// something taken separately. A wake signal and the state it announces living under
// different locks is how a missed wakeup happens; here there is one call, and its
// context is the machine's, so the loop ends when the machine does.
func resume[I, O any](ctx context.Context, w *worker[I], out *emitter[O]) {
	if !w.checkpoint || w.machine.cfg.journal == nil {
		return
	}

	for {
		records, err := w.machine.cfg.journal.Orphans(ctx, w.machine.name)
		if err != nil {
			if ctx.Err() == nil {
				w.report(ctx, fmt.Errorf("machine: reading orphans for node %q: %w", w.name, err))
			}

			return
		}
		for _, record := range records {
			reclaim(ctx, w, out, record)
		}
	}
}

// reclaim claims one orphaned record and re-places it if this worker won.
//
// A LOST CLAIM IS ORDINARY AND IS NOT REPORTED. Claim returning false means another
// survivor won the datum, which is the recovery protocol working rather than a
// failure. Only a claim that ERRORED reaches the handler.
func reclaim[I, O any](ctx context.Context, w *worker[I], out *emitter[O], record CheckpointRecord) {
	arrival := record.Anchor == AnchorArrival && record.Node == w.name
	completion := record.Anchor == AnchorCompletion && record.Node == out.producer
	if !arrival && !completion {
		return
	}

	won, err := w.machine.cfg.journal.Claim(ctx, record.Flow, record.Datum, w.machine.name)
	if err != nil {
		w.report(ctx, fmt.Errorf("machine: claiming datum %q for node %q: %w", record.Datum, w.name, err))

		return
	}
	if !won {
		return
	}

	if arrival {
		rerun(ctx, w, record)

		return
	}
	reinject(ctx, w, out, record)
}

// rerun puts an ARRIVAL record back on the node's own edge, so the node processes the
// datum again.
func rerun[I any](ctx context.Context, w *worker[I], record CheckpointRecord) {
	packet, err := w.codec.Unmarshal(record.Data)
	if err != nil {
		w.report(ctx, fmt.Errorf(
			"machine: rebuilding the arrival record for datum %q: %w", record.Datum, err))

		return
	}
	if err := w.edge.Send(ctx, packet); err != nil {
		w.report(ctx, fmt.Errorf(
			"machine: re-running datum %q on node %q: %w", record.Datum, w.name, err))
	}
}

// reinject puts a COMPLETION record on the OUTBOUND edge, so the successors receive
// what the node produced and the node itself is never run again.
func reinject[I, O any](ctx context.Context, w *worker[I], out *emitter[O], record CheckpointRecord) {
	packet, err := out.codec.Unmarshal(record.Data)
	if err != nil {
		w.report(ctx, fmt.Errorf(
			"machine: rebuilding the completion record for datum %q: %w", record.Datum, err))

		return
	}
	if err := out.edge.Send(ctx, packet); err != nil {
		w.report(ctx, fmt.Errorf(
			"machine: re-injecting datum %q past node %q: %w", record.Datum, w.name, err))
	}
}

// retire drops a completed datum's record and its claim together.
//
// IT FIRES UNCONDITIONALLY rather than being gated on whether the flow declared a
// checkpoint. Retiring a datum that was never checkpointed is a no-op on the journal
// side, and gating it would mean tracking per-datum whether a checkpoint was ever
// written — state this path does not have and does not need.
//
// CRASH WINDOW, stated rather than narrowed: a worker that dies between the datum
// completing and this retire landing leaves a record for a datum that is already
// done, and recovery will claim and re-run it. That is the at-least-once semantic
// this design documents rather than masks; a second mechanism to narrow it would be
// another thing that can disagree with the first.
func (w *worker[I]) retire(ctx context.Context, datum string) {
	if w.machine.cfg.journal == nil {
		return
	}
	if err := w.machine.cfg.journal.Retire(ctx, w.machine.name, datum); err != nil {
		w.report(ctx, fmt.Errorf("machine: retiring datum %q at node %q: %w", datum, w.name, err))
	}
}

// requireCompletionCodec refuses AT START when a completion-anchored checkpoint node
// has no codec to marshal what it produces.
//
// It rides the same addCheck hook newEmitter already uses for the never-consumed
// flow, so the refusal arrives with every other declaration error rather than as a
// runtime surprise. It names the node, the successor and the fix, because an author
// told only that something is wrong cannot act on it. An ARRIVAL-anchored node is
// never refused here: it marshals its own input with its own codec and needs no
// successor codec at all.
func requireCompletionCodec[I, O any](w *worker[I], out *emitter[O]) {
	if !w.checkpoint || w.idempotent {
		return
	}
	w.machine.addCheck(func() error {
		if out.codec != nil {
			return nil
		}

		return fmt.Errorf("machine: node %q checkpoints on completion but its successor %q declares no codec; "+
			"declare WithCheckpoint on %q so its codec can marshal what %q produces, "+
			"or mark %q idempotent to checkpoint on arrival instead",
			w.name, out.consumer, out.consumer, w.name, w.name)
	})
}

// outlets declares the two emitters a BRANCHING node produces, each carrying the
// completion-codec requirement in its own right: a branching node journals down
// whichever outlet its work selects, so either can be the one that needs a codec.
func outlets[U any](m *Machine, w *worker[U], name string) (left, right *emitter[U]) {
	left = newEmitter[U](m, name+leftSuffix)
	right = newEmitter[U](m, name+rightSuffix)
	requireCompletionCodec(w, left)
	requireCompletionCodec(w, right)

	return left, right
}

// runBranching starts a two-outlet node: its read loop, and one resume loop per
// outlet, because a completion record names the outlet it was written at.
func runBranching[U any](w *worker[U], run runner[U], left, right *emitter[U]) {
	w.record.run = func(ctx context.Context) {
		go w.readLoop(ctx, run)
		go resume(ctx, w, left)
		go resume(ctx, w, right)
	}
}

// runner processes one datum for a node. It takes the PACKET the edge delivered; the
// frame is minted inside the node's own bind, so the capability view a node sees is
// always the one it declared.
type runner[I any] func(ctx context.Context, p Packet[I])

// emitter is a node's outbound hook. Because a node owns its inbound edge, an
// emitter is bound only once the downstream node is declared, which is why the
// graph is declared lazily and Start does real work.
// THE CODEC IS THE CONSUMER'S, and that is what makes a completion checkpoint
// possible at all. A completion record holds what the producing node OUTPUT, which
// is exactly this emitter's payload type and therefore its consumer's INPUT type, so
// the consumer's codec is the type-correct one. The producer's own codec cannot
// serve: a node's options carry its INPUT type, so a Map from U to V holds a
// Codec[U] while the record it must journal is a V. It is nil until the downstream
// node is declared, for the reason stated above this type.
type emitter[T any] struct {
	machine  *Machine
	producer string
	edge     Edge[T]
	consumer string
	codec    Codec[T]
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

func (e *emitter[T]) bind(consumer string, edge Edge[T], codec Codec[T]) {
	if e.edge != nil {
		e.machine.fail(fmt.Errorf("machine: the flow produced by %q is consumed by both %q and %q",
			e.producer, e.consumer, consumer))
		return
	}
	e.edge = edge
	e.consumer = consumer
	e.codec = codec
}

// send is the frame boundary on the way out: it converts to a packet, which carries no
// capability view and declares no gated accessor, so state access is possible only
// inside a node that declared it.
func (e *emitter[T]) send(ctx context.Context, f Frame[T]) error {
	return e.edge.Send(ctx, packetOf(f))
}

// worker is the shared per-node runtime every builder method drives.
type worker[I any] struct {
	machine *Machine
	name    string
	edge    Edge[I]
	handler ErrorHandler[I]
	caps    *capabilities
	record  *node
	// codec marshals this node's own INPUT, which is what the arrival anchor
	// journals. The completion anchor journals what the node PRODUCED and reaches
	// its codec through the outbound emitter instead.
	codec Codec[I]
	// checkpoint records that WithCheckpoint was declared, which is not the same
	// fact as codec being non-nil: WithCheckpoint(nil) must be refused, and a node
	// that never declared a checkpoint must not be.
	checkpoint bool
	// idempotent selects the ARRIVAL anchor. Unmarked is completion, the default.
	idempotent bool
}

func newWorker[I any](m *Machine, name string, opts ...NodeOption[I]) *worker[I] {
	cfg := &nodeConfig[I]{factory: Channel[I](0)}
	for _, opt := range opts {
		opt(cfg)
	}
	w := &worker[I]{
		machine: m, name: name, handler: cfg.handler, caps: newCapabilities(name, cfg),
		codec: cfg.codec, checkpoint: cfg.checkpoint, idempotent: cfg.idempotent,
	}
	if cfg.checkpoint && cfg.codec == nil {
		m.fail(fmt.Errorf("machine: node %q declares a checkpoint with a nil codec; "+
			"pass the codec that marshals its payload, as WithCheckpoint(machine.GobCodec[T]{})", name))
	}
	if cfg.codecDeclared && cfg.codec == nil {
		m.fail(fmt.Errorf("machine: node %q declares a codec that is nil; "+
			"pass the codec that marshals its payload, as WithCodec(machine.GobCodec[T]{})", name))
	}
	if cfg.checkpoint && m.cfg.journal == nil {
		m.fail(fmt.Errorf("machine: node %q declares a checkpoint but the machine has no journal; "+
			"wire one with machine.OptionJournal", name))
	}
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
		case p, ok := <-in:
			if !ok {
				return
			}
			w.step(ctx, sem, run, p)
		}
	}
}

// step dispatches one datum: inline under FIFO, through the semaphore when bounded,
// and as a bare goroutine when unbounded, which is the default and the prior
// behavior.
func (w *worker[I]) step(ctx context.Context, sem chan struct{}, run runner[I], p Packet[I]) {
	// THE ARRIVAL ANCHOR SITS HERE, before the datum is handed to any runner, which
	// is what makes "journaled BEFORE the node function ran" true on every dispatch
	// path — inline under FIFO, through the semaphore when bounded, and as a bare
	// goroutine when unbounded.
	w.journalArrival(ctx, p)

	if w.machine.cfg.fifo {
		run(ctx, p)
		return
	}
	if sem == nil {
		go run(ctx, p)
		return
	}
	sem <- struct{}{}
	go func() {
		defer func() { <-sem }()
		run(ctx, p)
	}()
}

// bind is the MINT: it turns the packet the edge delivered into the frame this node
// sees, and it is the ONLY place a node is given reach into state. It stamps the node
// name, attaches THIS node's capability view and attaches the machine's private heap
// store, so the view a frame carries is the receiving node's declared one regardless of
// where the packet came from. A frame that was never bound has a nil capability view,
// so every gated accessor fails loudly.
func (w *worker[I]) bind(p Packet[I]) Frame[I] {
	p.state.node = w.name
	p.state.store = w.machine.cfg.store
	return Frame[I]{payload: p.payload, state: p.state, caps: w.caps}
}

// guard is the panic boundary and runs for every datum in every node kind. It
// recovers inside the spawned goroutine, because a panic in a child goroutine is
// not catchable from its parent and would take the process down. A recovered error
// value is preserved rather than flattened to a string, so a handler can recover a
// typed error such as *CapabilityError with errors.As.
//
// Being the universal per-datum path, it is also the span boundary AND the one place
// the datum's execution context is stamped onto the frame — the frame is guard's own
// by-value copy, so the stamp reaches the node function and nothing else. The order in
// the deferred block is load-bearing: dispatch, then the frame state is released, and
// only then finish, which ends the span — so an ended span orders an outside reader
// after the reclaim.
func (w *worker[I]) guard(ctx context.Context, f Frame[I], fn func(ctx context.Context, f Frame[I])) {
	spanCtx, span := w.record.instruments.start(ctx)
	f.ctx = spanCtx
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
	return func(ctx context.Context, p Packet[I]) {
		w.guard(ctx, w.bind(p), func(spanCtx context.Context, inner Frame[I]) {
			next := rewrap(inner, fn(inner))
			// COMPLETION: the produced value, journaled through the SUCCESSOR's
			// codec, before it is sent onward.
			journalCompletion(spanCtx, w, out, next, inner.Value())
			if err := out.send(spanCtx, next); err != nil {
				w.dispatch(spanCtx, NodeError[I]{Node: w.name, Err: err, Payload: inner.Value()})
			}
		})
	}
}

func (w *worker[I]) route(onTrue, onFalse *emitter[I], fn Filter[I]) runner[I] {
	return func(ctx context.Context, p Packet[I]) {
		w.guard(ctx, w.bind(p), func(spanCtx context.Context, inner Frame[I]) {
			out := onFalse
			if fn(inner) {
				out = onTrue
			}
			// COMPLETION on a node that FORWARDS rather than produces. The payload
			// equals the one received, so the anchor is the moment rather than the
			// value: at this point the branch is already chosen, and re-injecting
			// this record into the chosen successor is exactly what "never re-run
			// the node" means for a node whose work IS the choice.
			journalCompletion(spanCtx, w, out, inner, inner.Value())
			w.emit(spanCtx, out, inner)
		})
	}
}

func (w *worker[I]) split(onLeft, onRight *emitter[I], fn Duplicator[I]) runner[I] {
	return func(ctx context.Context, p Packet[I]) {
		w.guard(ctx, w.bind(p), func(spanCtx context.Context, inner Frame[I]) {
			left, right := fn(inner.Value())
			leftFrame := Frame[I]{payload: left, state: inner.state.clone()}
			rightFrame := Frame[I]{payload: right, state: inner.state.clone()}
			// COMPLETION on a split is TWO records, one per branch. clone() mints a
			// fresh identity for each, so they are two datums; journaling one would
			// leave the other branch unrecoverable.
			journalCompletion(spanCtx, w, onLeft, leftFrame, inner.Value())
			journalCompletion(spanCtx, w, onRight, rightFrame, inner.Value())
			w.emit(spanCtx, onLeft, leftFrame)
			w.emit(spanCtx, onRight, rightFrame)
		})
	}
}

func (w *worker[I]) drain() runner[I] {
	return func(ctx context.Context, p Packet[I]) {
		w.guard(ctx, w.bind(p), func(spanCtx context.Context, inner Frame[I]) {
			// THE RETIREMENT TRIGGER. A datum that has left the flow is exactly a
			// datum whose recovery record is no longer wanted, so this is the same
			// terminal path that already reclaims the frame's state.
			w.retire(spanCtx, inner.ID())
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
		go resume(ctx, w, out)
	}
	ingest := func(ctx context.Context, payload T) error {
		return w.edge.Send(ctx, packetOf(newFrame(name, payload, m.cfg.store)))
	}
	return Flow[T, T]{machine: m, out: out}, ingest
}

// Map applies fn to each frame and forwards the transformed payload, changing the
// flow's payload type. V is inferred from the transformation.
//
// Map is also where recursion lives. Recursion is plain Go inside the body: declare
// a recursive closure over the payload and call it. The frame stays the runtime's,
// so declared handles keep working inside the recursion exactly as they do in any
// other Map body.
//
//	.Map("walk", func(f machine.Frame[*tree]) *tree {
//		var visit func(n *node) *node
//		visit = func(n *node) *node {
//			if n == nil {
//				return nil
//			}
//			n.left, n.right = visit(n.left), visit(n.right)
//			return n
//		}
//		return &tree{root: visit(f.Value().root)}
//	})
//
// Memoization is the caller's: a map closed over by the body memoizes within one
// datum, and a heap Cell memoizes across data.
func (f Flow[T, U]) Map[V any](name string, fn Transformation[U, V], opts ...NodeOption[U]) Flow[T, V] {
	w := newWorker[U](f.machine, name, opts...)
	f.out.bind(name, w.edge, w.codec)
	out := newEmitter[V](f.machine, name)
	requireCompletionCodec(w, out)
	w.record.run = func(ctx context.Context) {
		go w.readLoop(ctx, w.transform(out, fn))
		go resume(ctx, w, out)
	}
	return Flow[T, V]{machine: f.machine, out: out}
}

// If splits the flow, routing the INTACT frame down one branch: no copy and no
// reparenting, so identity and lineage survive the branch unchanged. The branches
// are named with a .left and .right suffix, so an unconsumed-branch error at Start
// identifies which side was left dangling.
func (f Flow[T, U]) If(name string, fn Filter[U], opts ...NodeOption[U]) (left, right Flow[T, U]) {
	w := newWorker[U](f.machine, name, opts...)
	f.out.bind(name, w.edge, w.codec)
	onTrue, onFalse := outlets(f.machine, w, name)
	runBranching(w, w.route(onTrue, onFalse, fn), onTrue, onFalse)
	return Flow[T, U]{machine: f.machine, out: onTrue}, Flow[T, U]{machine: f.machine, out: onFalse}
}

// Tee duplicates the flow, DEEP-COPYING the envelope into both branches so the
// split is a functional, non-locking process. The payload is split by the caller's
// Duplicator and the frame is cloned by the runtime; both branches get fresh
// identities and both report the upstream frame as their parent.
func (f Flow[T, U]) Tee(name string, fn Duplicator[U], opts ...NodeOption[U]) (left, right Flow[T, U]) {
	w := newWorker[U](f.machine, name, opts...)
	f.out.bind(name, w.edge, w.codec)
	onLeft, onRight := outlets(f.machine, w, name)
	runBranching(w, w.split(onLeft, onRight, fn), onLeft, onRight)
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
	f.out.bind(target.out.consumer, target.out.edge, target.out.codec)
}

// Drop terminates the flow, discarding each frame and reclaiming its stack state at
// traversal end.
func (f Flow[T, U]) Drop(name string, opts ...NodeOption[U]) {
	w := newWorker[U](f.machine, name, opts...)
	f.out.bind(name, w.edge, w.codec)
	w.record.run = func(ctx context.Context) {
		go w.readLoop(ctx, w.drain())
	}
}

// Output is the terminal consumption surface: it hands the caller the channel of
// packets leaving the flow. Packets reaching it belong to the CALLER and are NOT
// reclaimed. Output does not process, so it does not advance the datum's Node stamp
// and has no read loop of its own.
//
// What leaves the flow is what the edge carried: a packet, with the identity
// accessors and the projection and no reach into state. That is not a narrowing — a
// frame leaving a flow never carried a capability view either, so every gated
// accessor on it panicked; the difference is that the call now fails to compile
// instead.
//
// WITH A JOURNAL WIRED IT HANDS BACK A FORWARDING CHANNEL RATHER THAN THE EDGE'S OWN,
// because this is a terminal and retirement fires where a datum leaves the flow. The
// handoff stays synchronous — the forwarding channel is unbuffered, so a send still
// blocks until the caller reads — and with no journal the edge's own channel is
// returned unchanged.
func (f Flow[T, U]) Output(name string, opts ...NodeOption[U]) <-chan Packet[U] {
	w := newWorker[U](f.machine, name, opts...)
	f.out.bind(name, w.edge, w.codec)
	if w.edge == nil {
		return nil
	}
	if f.machine.cfg.journal == nil {
		return w.edge.Receive()
	}

	return w.retiring(w.edge.Receive())
}

// retiring forwards packets to the caller, retiring each datum as it leaves.
//
// The retire happens BEFORE the handoff rather than after, so a caller that never
// reads cannot leave the record outstanding for a datum the flow has already
// finished with.
func (w *worker[I]) retiring(in <-chan Packet[I]) <-chan Packet[I] {
	out := make(chan Packet[I])
	ctx, cancel := context.WithCancel(context.Background())
	w.machine.addCloser(func(context.Context) { cancel() })

	go func() {
		defer close(out)
		for p := range in {
			w.retire(ctx, p.ID())
			select {
			case out <- p:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out
}
