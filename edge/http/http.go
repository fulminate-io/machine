// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// Package http is the HTTP transport for a machine edge. One Edge value is BOTH halves
// of a one-way hop: Send POSTs the marshaled envelope to a peer, and ServeHTTP accepts a
// peer's POST and delivers the rebuilt frame into the node this edge feeds. It is a
// one-way hop rather than a remote call, because a response body cannot carry the frame
// of a datum that is still traveling.
//
// A TRANSPORT FAILURE LANDS IN ONE OF TWO PLACES, AND THEY ARE DIFFERENT NODES. A SEND
// failure is returned to the emitter and dispatched by the SENDING worker, so it is
// attributed to the node that produced the datum. A RECEIVE-side refusal is reported
// through the supervisor's report path, so it is attributed to the node the edge
// DELIVERS INTO. A user who registers an error handler on the wrong one of those two
// nodes sees silence, so the choice is worth making deliberately.
//
// A refused frame is consequently loud on both sides: ServeHTTP reports the refusal
// locally AND answers 400, and the peer's Send turns that 400 into an error its own
// supervisor routes. Those are two distinct true facts — "I refused an inbound frame"
// and "my peer refused my frame" — which in a distributed deployment are observed in two
// different processes.
//
// Trace context crosses the boundary in the request headers, injected by Send and
// extracted by ServeHTTP through the propagator resolved from the OpenTelemetry global
// at each point of use. No trace field is added to the frame projection.
package http

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/whitaker-io/machine/v4"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

const (
	defaultMaxBody = 4 << 20
	contentType    = "application/octet-stream"
	// maxDetail bounds the body read back from a refusing peer, so a peer answering an
	// unbounded error page cannot make a send path accumulate without limit.
	maxDetail = 4 << 10
)

// settings holds one edge's configured transport behavior.
type settings[T any] struct {
	client  *http.Client
	codec   machine.Codec[T]
	buffer  int
	maxBody int64
}

// Option configures an Edge at construction.
type Option[T any] func(*settings[T])

// WithCodec replaces the envelope codec, which defaults to machine.GobCodec[T]{}.
func WithCodec[T any](codec machine.Codec[T]) Option[T] {
	return func(s *settings[T]) { s.codec = codec }
}

// WithClient replaces the http client, which defaults to http.DefaultClient.
func WithClient[T any](client *http.Client) Option[T] {
	return func(s *settings[T]) { s.client = client }
}

// WithBuffer sets the depth of the inbound channel ServeHTTP delivers into. It defaults
// to zero, an unbuffered handoff.
func WithBuffer[T any](buffer int) Option[T] {
	return func(s *settings[T]) { s.buffer = buffer }
}

// WithMaxBody bounds the request body ServeHTTP will read, which defaults to 4 MiB.
func WithMaxBody[T any](maxBody int64) Option[T] {
	return func(s *settings[T]) { s.maxBody = maxBody }
}

// Edge is the HTTP transport. It satisfies both machine.Edge and http.Handler, so
// pointing one edge's target at a server mounting that same edge exercises the genuine
// remote path in a single process.
type Edge[T any] struct {
	target   string
	settings settings[T]
	in       chan machine.Frame[T]
	done     chan struct{}
	once     sync.Once
	mutex    sync.Mutex
	node     string
	report   machine.Report
}

// New returns an edge that POSTs to target. Mount the same value as an http.Handler to
// receive on it.
func New[T any](target string, options ...Option[T]) *Edge[T] {
	config := settings[T]{client: http.DefaultClient, codec: machine.GobCodec[T]{}, maxBody: defaultMaxBody}
	for _, option := range options {
		option(&config)
	}
	return &Edge[T]{
		target:   target,
		settings: config,
		in:       make(chan machine.Frame[T], config.buffer),
		done:     make(chan struct{}),
	}
}

// Factory returns the machine.EdgeFactory that hands this edge to a node. It REFUSES a
// nil report path, because an edge with no route to the supervisor's handlers would
// discover that only at its first refusal, and it refuses a second node, because one
// edge delivers into exactly one node.
func (e *Edge[T]) Factory() machine.EdgeFactory[T] {
	return func(node string, report machine.Report) (machine.Edge[T], error) {
		if report == nil {
			return nil, fmt.Errorf("machine/edge/http: the edge for node %q was given no report path", node)
		}
		e.mutex.Lock()
		defer e.mutex.Unlock()
		if e.node != "" {
			return nil, fmt.Errorf("machine/edge/http: the edge already delivers into node %q and cannot also serve %q",
				e.node, node)
		}
		e.node = node
		e.report = report
		return e, nil
	}
}

// bound reads what Factory writes. It takes the mutex because a handler mounted before
// the factory runs would otherwise race the write.
func (e *Edge[T]) bound() (string, machine.Report) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return e.node, e.report
}

// Start ties the edge's lifetime to the machine's context. The runtime also closes the
// edge at shutdown, so both paths converge on the same sync.Once.
func (e *Edge[T]) Start(ctx context.Context) error {
	go func() {
		select {
		case <-ctx.Done():
			e.shut()
		case <-e.done:
		}
	}()
	return nil
}

// Send marshals the frame, injects the caller's trace context into the request headers
// and POSTs it. NOTHING IN THIS FILE PANICS: a failure is returned to the emitter, whose
// worker dispatches it as a node failure of the SENDING node.
func (e *Edge[T]) Send(ctx context.Context, frame machine.Frame[T]) error {
	body, err := e.settings.codec.Marshal(frame)
	if err != nil {
		return fmt.Errorf("machine/edge/http: marshaling a frame for %s failed: %w", e.target, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("machine/edge/http: building the request for %s failed: %w", e.target, err)
	}
	request.Header.Set("Content-Type", contentType)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(request.Header))
	response, err := e.settings.client.Do(request)
	if err != nil {
		return fmt.Errorf("machine/edge/http: posting a frame to %s failed: %w", e.target, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, response.Body); _ = response.Body.Close() }()
	return e.accepted(response)
}

// accepted turns a peer's non-2xx answer into the error the sending node's handler sees,
// reading at most maxDetail of the peer's explanation.
func (e *Edge[T]) accepted(response *http.Response) error {
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	detail, _ := io.ReadAll(io.LimitReader(response.Body, maxDetail))
	return fmt.Errorf("machine/edge/http: %s refused the frame: %s: %s",
		e.target, response.Status, strings.TrimSpace(string(detail)))
}

// Receive hands the runtime the channel ServeHTTP delivers into.
func (e *Edge[T]) Receive() <-chan machine.Frame[T] { return e.in }

// Close is idempotent, as the machine.Edge contract requires. It does NOT close the
// inbound channel: a delivery already in flight would then send on a closed channel.
func (e *Edge[T]) Close() error { e.shut(); return nil }

func (e *Edge[T]) shut() { e.once.Do(func() { close(e.done) }) }

// ServeHTTP accepts a peer's POST and delivers the rebuilt frame into the node this edge
// feeds. It EXTRACTS the peer's trace context FIRST, so a read failure or a codec
// refusal is already reported under the peer's trace id.
func (e *Edge[T]) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	ctx := otel.GetTextMapPropagator().Extract(request.Context(), propagation.HeaderCarrier(request.Header))
	node, report := e.bound()
	if report == nil {
		http.Error(writer, "machine/edge/http: this edge is not bound to a node yet", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, e.settings.maxBody))
	if err != nil {
		e.refuse(ctx, writer, node, report, err)
		return
	}
	frame, err := e.settings.codec.Unmarshal(body)
	if err != nil {
		e.refuse(ctx, writer, node, report, err)
		return
	}
	e.deliver(ctx, writer, frame)
}

// refuse is loud on both sides: the local supervisor hears it attributed to the
// RECEIVING node, and the peer hears it as the 400 its own Send turns into an error.
func (*Edge[T]) refuse(ctx context.Context, writer http.ResponseWriter, node string,
	report machine.Report, err error) {
	report(ctx, fmt.Errorf("machine/edge/http: node %q refused an inbound frame: %w", node, err))
	http.Error(writer, err.Error(), http.StatusBadRequest)
}

// deliver hands the frame to the node, or answers 503 if the request or the edge ends
// first. Nothing is retained for redelivery: that is the broker's or the user's concern.
func (e *Edge[T]) deliver(ctx context.Context, writer http.ResponseWriter, frame machine.Frame[T]) {
	select {
	case e.in <- frame:
		writer.WriteHeader(http.StatusAccepted)
	case <-ctx.Done():
		http.Error(writer, ctx.Err().Error(), http.StatusServiceUnavailable)
	case <-e.done:
		http.Error(writer, machine.ErrEdgeClosed.Error(), http.StatusServiceUnavailable)
	}
}

var (
	_ machine.Edge[int] = (*Edge[int])(nil)
	_ http.Handler      = (*Edge[int])(nil)
)
