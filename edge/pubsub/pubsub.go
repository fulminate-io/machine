// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// Package pubsub is the Google Cloud Pub/Sub transport for a machine edge. One Edge
// value is both halves of a one-way hop: Send publishes the marshaled envelope to a
// topic, and a subscription delivers a peer's message into the node this edge feeds.
//
// This transport traffics in machine.Packet and never in machine.Frame. The runtime
// converts on the way out and mints the receiving node's frame on the way in, so nothing
// here can reach a node's capability-gated state.
//
// A TRANSPORT FAILURE LANDS IN ONE OF TWO PLACES, AND THEY ARE DIFFERENT NODES. A
// PUBLISH failure is returned from Send and dispatched by the SENDING worker, so it is
// attributed to the node that produced the datum. A RECEIVE-side refusal, and a
// subscription that stops for any reason other than the machine's own shutdown, are
// reported through the supervisor's report path, so they are attributed to the node the
// edge DELIVERS INTO. A user who registers an error handler on the wrong one of those
// two nodes sees silence.
//
// A REFUSED MESSAGE IS REPORTED AND THEN SETTLED. The report is the point; the
// acknowledgement is what stops the broker redelivering a message nothing in this
// process can ever decode. It is deliberately NOT nacked: with no redelivery limit a
// poison message would return forever, and a lane that can fire endlessly on one cause
// is a defect rather than a handled condition. Nothing here retries, backs off or
// redelivers — those are the broker's and the user's concerns.
//
// Trace context rides the message attributes, injected by Send and extracted by the
// subscription callback through the propagator resolved from the OpenTelemetry global at
// each point of use. No trace field is added to the packet projection.
package pubsub

import (
	"context"
	"fmt"
	"sync"

	"cloud.google.com/go/pubsub/v2"
	"github.com/whitaker-io/machine/v4"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// settings holds one edge's configured transport behavior.
type settings[T any] struct {
	codec  machine.Codec[T]
	buffer int
}

// Option configures an Edge at construction.
type Option[T any] func(*settings[T])

// WithCodec replaces the envelope codec, which defaults to machine.GobCodec[T]{}.
func WithCodec[T any](codec machine.Codec[T]) Option[T] {
	return func(s *settings[T]) { s.codec = codec }
}

// WithBuffer sets the depth of the inbound channel the subscription delivers into. It
// defaults to zero, an unbuffered handoff, so a slow node exerts backpressure on the
// callback rather than accumulating packets.
func WithBuffer[T any](buffer int) Option[T] {
	return func(s *settings[T]) { s.buffer = buffer }
}

// Edge is the Pub/Sub transport.
type Edge[T any] struct {
	publisher  *pubsub.Publisher
	subscriber *pubsub.Subscriber
	settings   settings[T]
	in         chan machine.Packet[T]
	done       chan struct{}
	once       sync.Once
	mutex      sync.Mutex
	node       string
	report     machine.Report
}

// New returns an edge publishing to topic and consuming from subscription. It takes the
// CALLER'S client, because credentials and endpoint configuration are theirs.
func New[T any](client *pubsub.Client, topic, subscription string, options ...Option[T]) *Edge[T] {
	config := settings[T]{codec: machine.GobCodec[T]{}}
	for _, option := range options {
		option(&config)
	}
	return &Edge[T]{
		publisher:  client.Publisher(topic),
		subscriber: client.Subscriber(subscription),
		settings:   config,
		in:         make(chan machine.Packet[T], config.buffer),
		done:       make(chan struct{}),
	}
}

// Factory returns the machine.EdgeFactory that hands this edge to a node. It REFUSES a
// nil report path, because an edge with no route to the supervisor's handlers would
// discover that only at its first refusal, and it refuses a second node, because one
// edge delivers into exactly one node.
func (e *Edge[T]) Factory() machine.EdgeFactory[T] {
	return func(node string, report machine.Report) (machine.Edge[T], error) {
		if report == nil {
			return nil, fmt.Errorf("machine/edge/pubsub: the edge for node %q was given no report path", node)
		}
		e.mutex.Lock()
		defer e.mutex.Unlock()
		if e.node != "" {
			return nil, fmt.Errorf("machine/edge/pubsub: the edge already delivers into node %q "+
				"and cannot also serve %q", e.node, node)
		}
		e.node = node
		e.report = report
		return e, nil
	}
}

// bound reads what Factory writes, under the same mutex.
func (e *Edge[T]) bound() (string, machine.Report) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return e.node, e.report
}

// Start opens the subscription. It FAILS when the edge was never bound to a node: an
// edge that feeds nothing has no business consuming a subscription.
func (e *Edge[T]) Start(ctx context.Context) error {
	node, report := e.bound()
	if report == nil {
		return fmt.Errorf("machine/edge/pubsub: the edge for topic %q was never bound to a node", e.publisher.ID())
	}
	go e.consume(ctx, node, report)
	return nil
}

// consume runs the subscription for the machine's lifetime. A Receive that returns an
// error while the machine's context is still live is a subscription failure rather than
// shutdown, and is reported as such.
func (e *Edge[T]) consume(ctx context.Context, node string, report machine.Report) {
	defer e.shut()
	err := e.subscriber.Receive(ctx, func(inner context.Context, message *pubsub.Message) {
		e.accept(inner, node, report, message)
	})
	if err != nil && ctx.Err() == nil {
		report(ctx, fmt.Errorf("machine/edge/pubsub: the subscription feeding node %q stopped: %w", node, err))
	}
}

// accept is report-then-settle, and the ORDER of its steps is the contract. Extraction
// comes first so a refusal is already reported under the peer's trace; the report comes
// before the acknowledgement because the report is the point; the acknowledgement
// follows because it is what stops an undecodable message returning forever.
func (e *Edge[T]) accept(inner context.Context, node string, report machine.Report, message *pubsub.Message) {
	ctx := otel.GetTextMapPropagator().Extract(inner, propagation.MapCarrier(message.Attributes))
	packet, err := e.settings.codec.Unmarshal(message.Data)
	if err != nil {
		report(ctx, fmt.Errorf("machine/edge/pubsub: node %q refused an inbound message: %w", node, err))
		message.Ack()
		return
	}
	message.Ack()
	select {
	case e.in <- packet:
	case <-ctx.Done():
	case <-e.done:
	}
}

// Send marshals the packet, injects the caller's trace context into the message
// attributes and publishes it. NOTHING IN THIS FILE PANICS: a failure is returned to the
// emitter, whose worker dispatches it as a node failure of the SENDING node.
func (e *Edge[T]) Send(ctx context.Context, packet machine.Packet[T]) error {
	node, _ := e.bound()
	body, err := e.settings.codec.Marshal(packet)
	if err != nil {
		return fmt.Errorf("machine/edge/pubsub: marshaling a packet for %q failed: %w", node, err)
	}
	// The attribute map is built FRESH on every send: Inject writes into the carrier, and
	// injecting into a nil map panics.
	attributes := map[string]string{}
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(attributes))
	if _, err = e.publisher.Publish(ctx, &pubsub.Message{Data: body, Attributes: attributes}).Get(ctx); err != nil {
		return fmt.Errorf("machine/edge/pubsub: publishing for %q: %w", node, err)
	}
	return nil
}

// Receive hands the runtime the channel the subscription delivers into.
func (e *Edge[T]) Receive() <-chan machine.Packet[T] { return e.in }

// Close is idempotent, as the machine.Edge contract requires. It does NOT close the
// inbound channel: a delivery already in flight would then send on a closed channel.
func (e *Edge[T]) Close() error {
	e.shut()
	e.publisher.Stop()
	return nil
}

func (e *Edge[T]) shut() { e.once.Do(func() { close(e.done) }) }

var _ machine.Edge[int] = (*Edge[int])(nil)
