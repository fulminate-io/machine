// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package http

import (
	"bytes"
	"context"
	"encoding/gob"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whitaker-io/machine/v4"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// step is the two-field stack value the round trip carries, so the hop is asserted on a
// struct as well as on a number.
type step struct {
	Node  string
	Depth int
}

// The keys are named for this module: the machine declaration namespace is process-wide.
var (
	httpAttempts = machine.NewKey("edge.http.attempts", func(v int) int { return v })
	httpTrail    = machine.NewKey("edge.http.trail", func(v step) step { return v })
)

// captured stands in for the supervisor. It satisfies the machine.Report signature and
// records what an edge reported, which is what makes the report path observable at
// transport level with no machine running — and it keeps the CONTEXT, so the propagation
// assertions can read the trace a refusal was reported under.
type captured struct {
	mutex sync.Mutex
	ctx   context.Context
	err   error
	fired chan struct{}
}

func newCaptured() *captured { return &captured{fired: make(chan struct{}, 1)} }

func (c *captured) report(ctx context.Context, err error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.ctx, c.err = ctx, err
	select {
	case c.fired <- struct{}{}:
	default:
	}
}

func (c *captured) await(t *testing.T) (context.Context, error) {
	t.Helper()
	select {
	case <-c.fired:
	case <-time.After(5 * time.Second):
		t.Fatal("no failure was reported to the supervisor")
	}
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.ctx, c.err
}

// serve resolves the construction order: the edge needs the server's URL and the server
// needs the edge as its handler, so the mounted handler forwards to a variable build
// fills in the moment it returns.
//
// It also counts the requests that reached the handler. Without that count the round-trip
// assertions would pass just as well over a local channel edge, so the count is what
// makes them assertions about an HTTP hop.
func serve[T any](t *testing.T, build func(target string) *Edge[T]) (*Edge[T], *httptest.Server, *atomic.Int64) {
	t.Helper()
	var edge *Edge[T]
	served := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		served.Add(1)
		edge.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	edge = build(server.URL)
	return edge, server, served
}

// wireEnvelope mirrors the codec's own envelope so a test can forge one WITHOUT going
// through RebuildPacket, which is the only way to put an undeclared key on the wire: the
// declaration registry is process-wide, so anything this process could declare would be
// declared on the receiving side too.
type wireEnvelope struct {
	Frame   machine.FrameData
	Payload int
}

func forge(t *testing.T, key string, payload int) []byte {
	t.Helper()
	var sink bytes.Buffer
	body := wireEnvelope{
		Frame:   machine.FrameData{ID: "forged", Source: "peer", Node: "peer", Values: map[string]any{key: payload}},
		Payload: payload,
	}
	if err := gob.NewEncoder(&sink).Encode(body); err != nil {
		t.Fatalf("forging an envelope: %v", err)
	}
	return sink.Bytes()
}

func knownTrace(t *testing.T) (trace.SpanContext, trace.TraceID) {
	t.Helper()
	traceID, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	if err != nil {
		t.Fatalf("building the known trace id: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("0102030405060708")
	if err != nil {
		t.Fatalf("building the known span id: %v", err)
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}), traceID
}

// observation is what the far side of the hop saw, lifted out of the receiving node.
type observation struct {
	attempts int
	trail    step
	source   string
	id       string
}

func TestFrameSurvivesHTTPRoundTrip(t *testing.T) {
	gob.Register(step{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seen := make(chan observation, 1)
	edge, _, served := serve(t, func(target string) *Edge[int] { return New[int](target, WithBuffer[int](4)) })

	m := machine.New("http-round-trip")
	src, ingest := m.Source[int]("in")
	// Source cannot set stack keys itself — its runner is a fixed identity transform — so
	// the writing node is a Map.
	src.Map("stamp", func(f machine.Frame[int]) int {
		f.Set(httpAttempts, 3)
		f.Set(httpTrail, step{Node: "stamp", Depth: 1})
		return f.Value()
	}, machine.WithWrites[int](httpAttempts, httpTrail)).
		Map("remote", func(f machine.Frame[int]) int {
			seen <- observation{attempts: f.Get(httpAttempts), trail: f.Get(httpTrail), source: f.Source(), id: f.ID()}
			return f.Value()
		}, machine.WithEdge(edge.Factory()), machine.WithReads[int](httpAttempts, httpTrail)).
		Drop("sink")

	if err := m.Start(ctx); err != nil {
		t.Fatalf("starting the machine: %v", err)
	}
	if err := ingest(ctx, 42); err != nil {
		t.Fatalf("ingesting: %v", err)
	}

	var got observation
	select {
	case got = <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("no frame arrived on the far side of the http hop")
	}
	if got.attempts != 3 {
		t.Errorf("the stack int arrived as %d, want 3", got.attempts)
	}
	if got.trail != (step{Node: "stamp", Depth: 1}) {
		t.Errorf("the stack struct arrived as %+v, want {stamp 1}", got.trail)
	}
	if got.source != "in" {
		t.Errorf("Source arrived as %q, want %q", got.source, "in")
	}
	if got.id == "" {
		t.Error("the frame arrived with an empty ID, so its lineage did not survive the hop")
	}
	if served.Load() < 1 {
		t.Error("no request reached the mounted handler, so the frame did not cross an http hop at all")
	}
}

func TestHTTPRefusesUndeclaredKeyAndReportsIt(t *testing.T) {
	seen := newCaptured()
	edge, server, _ := serve(t, func(target string) *Edge[int] { return New[int](target) })
	if _, err := edge.Factory()("receiver", seen.report); err != nil {
		t.Fatalf("binding the edge to a node: %v", err)
	}

	response, err := http.Post(server.URL, contentType, bytes.NewReader(forge(t, "nobody.declared.this", 1)))
	if err != nil {
		t.Fatalf("posting the forged envelope: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("the peer answered %s, want 400", response.Status)
	}
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the refusal body: %v", err)
	}
	if !strings.Contains(string(answer), "nobody.declared.this") {
		t.Errorf("the refusal body does not name the undeclared key: %q", answer)
	}

	_, reported := seen.await(t)
	if !strings.Contains(reported.Error(), "nobody.declared.this") {
		t.Errorf("the reported failure does not name the undeclared key: %v", reported)
	}
	if !strings.Contains(reported.Error(), "receiver") {
		t.Errorf("the reported failure does not name the RECEIVING node: %v", reported)
	}
}

func TestHTTPInjectsTheSendersTraceContext(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	spanContext, traceID := knownTrace(t)
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)

	headers := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		headers <- request.Header.Clone()
		writer.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	packet, err := machine.RebuildPacket(machine.FrameData{ID: "id-1", Source: "peer", Node: "peer"}, 1)
	if err != nil {
		t.Fatalf("building the packet: %v", err)
	}
	if err = New[int](server.URL).Send(ctx, packet); err != nil {
		t.Fatalf("sending: %v", err)
	}

	got := (<-headers).Get("Traceparent")
	if !strings.Contains(got, traceID.String()) {
		t.Errorf("the request carried traceparent %q, want one containing the sender's trace id %s",
			got, traceID)
	}
}

func TestHTTPExtractsThePeersTraceContextForARefusal(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	spanContext, traceID := knownTrace(t)

	seen := newCaptured()
	edge, server, _ := serve(t, func(target string) *Edge[int] { return New[int](target) })
	if _, err := edge.Factory()("receiver", seen.report); err != nil {
		t.Fatalf("binding the edge to a node: %v", err)
	}

	request, err := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader(forge(t, "also.undeclared", 2)))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.Header.Set("Content-Type", contentType)
	otel.GetTextMapPropagator().Inject(trace.ContextWithSpanContext(context.Background(), spanContext),
		propagation.HeaderCarrier(request.Header))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("posting the forged envelope: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	reportedUnder, _ := seen.await(t)
	got := trace.SpanContextFromContext(reportedUnder)
	if !got.IsValid() {
		t.Fatal("the refusal was reported under a context carrying no span context at all")
	}
	if got.TraceID() != traceID {
		t.Errorf("the refusal was reported under trace %s, want the peer's %s", got.TraceID(), traceID)
	}
}

func TestHTTPTransportErrorReachesHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "peer is down", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	errs := make(chan machine.NodeError[int], 1)
	m := machine.New("http-transport-error")
	// The handler goes on the SOURCE, not on the node the edge delivers into: a send
	// failure is dispatched by the worker that produced the datum.
	src, ingest := m.Source[int]("in", machine.WithErrorHandler(func(e machine.NodeError[int]) { errs <- e }))
	src.Drop("dead", machine.WithEdge(New[int](server.URL).Factory()))

	if err := m.Start(ctx); err != nil {
		t.Fatalf("starting the machine: %v", err)
	}
	if err := ingest(ctx, 7); err != nil {
		t.Fatalf("ingesting: %v", err)
	}

	select {
	case failure := <-errs:
		if failure.Panic {
			t.Error("the handler received Panic=true for a transport failure")
		}
		if failure.Node != "in" {
			t.Errorf("the handler received node %q, want the SENDING node %q", failure.Node, "in")
		}
		if failure.Payload != 7 {
			t.Errorf("the handler received payload %d, want 7", failure.Payload)
		}
		if !strings.Contains(failure.Err.Error(), "peer is down") {
			t.Errorf("the handler received %v, want an error carrying the peer's own detail", failure.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the transport failure never reached the registered handler")
	}
}

func TestEdgeServesExactlyOneNode(t *testing.T) {
	edge := New[int]("http://127.0.0.1:1")
	noop := func(context.Context, error) {}
	if _, err := edge.Factory()("first", noop); err != nil {
		t.Fatalf("the first factory call failed: %v", err)
	}
	_, err := edge.Factory()("second", noop)
	if err == nil {
		t.Fatal("one edge accepted a second node")
	}
	for _, name := range []string{"first", "second"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal %v does not name %q", err, name)
		}
	}
}

func TestEdgeRejectsAFactoryCallWithoutAnErrorPath(t *testing.T) {
	_, err := New[int]("http://127.0.0.1:1").Factory()("orphan", nil)
	if err == nil {
		t.Fatal("the edge was constructed with no route to the supervisor's handlers")
	}
	if !strings.Contains(err.Error(), "orphan") {
		t.Errorf("the refusal %v does not name the node", err)
	}
}
