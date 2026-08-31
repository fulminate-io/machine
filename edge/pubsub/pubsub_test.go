// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package pubsub

import (
	"bytes"
	"context"
	"encoding/gob"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"github.com/whitaker-io/machine/v4"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	project      = "machine-test"
	topicName    = "projects/machine-test/topics/frames"
	subscription = "projects/machine-test/subscriptions/frames"
)

// step is the two-field stack value the round trip carries.
type step struct {
	Node  string
	Depth int
}

// The keys are named for this module: the machine declaration namespace is process-wide.
var (
	psAttempts = machine.NewKey("edge.pubsub.attempts", func(v int) int { return v })
	psTrail    = machine.NewKey("edge.pubsub.trail", func(v step) step { return v })
)

// fake stands up an in-memory Pub/Sub server and a client wired to it, with the topic and
// subscription this suite uses already created. It returns the SERVER too, because the
// settle and injection assertions read what the broker recorded.
func fake(t *testing.T, options ...pstest.ServerReactorOption) (*pubsub.Client, *pstest.Server) {
	t.Helper()
	server := pstest.NewServer(options...)
	t.Cleanup(func() { _ = server.Close() })

	conn, err := grpc.NewClient(server.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialing the fake broker: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx := context.Background()
	client, err := pubsub.NewClient(ctx, project, option.WithGRPCConn(conn), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if _, err = client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName}); err != nil {
		t.Fatalf("creating the topic: %v", err)
	}
	_, err = client.SubscriptionAdminClient.CreateSubscription(ctx,
		&pubsubpb.Subscription{Name: subscription, Topic: topicName})
	if err != nil {
		t.Fatalf("creating the subscription: %v", err)
	}
	return client, server
}

// captured stands in for the supervisor. It keeps the CONTEXT so the propagation
// assertions can read the trace a refusal was reported under, and a CALL COUNT so
// "reported once" is assertable rather than assumed.
type captured struct {
	mutex sync.Mutex
	ctx   context.Context
	err   error
	calls int
	fired chan struct{}
}

func newCaptured() *captured { return &captured{fired: make(chan struct{}, 1)} }

func (c *captured) report(ctx context.Context, err error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.ctx, c.err = ctx, err
	c.calls++
	select {
	case c.fired <- struct{}{}:
	default:
	}
}

func (c *captured) await(t *testing.T) (context.Context, error) {
	t.Helper()
	select {
	case <-c.fired:
	case <-time.After(10 * time.Second):
		t.Fatal("no failure was reported to the supervisor")
	}
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.ctx, c.err
}

func (c *captured) count() int {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.calls
}

// wireEnvelope mirrors the codec's own envelope so a test can forge one WITHOUT going
// through RebuildFrame, which is the only way to put an undeclared key on the wire: the
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

func TestFrameSurvivesPubSubRoundTrip(t *testing.T) {
	gob.Register(step{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, _ := fake(t)
	seen := make(chan observation, 1)

	m := machine.New("pubsub-round-trip")
	src, ingest := m.Source[int]("in")
	// Source cannot set stack keys itself — its runner is a fixed identity transform — so
	// the writing node is a Map.
	src.Map("stamp", func(f machine.Frame[int]) int {
		f.Set(psAttempts, 3)
		f.Set(psTrail, step{Node: "stamp", Depth: 1})
		return f.Value()
	}, machine.WithWrites[int](psAttempts, psTrail)).
		Map("remote", func(f machine.Frame[int]) int {
			seen <- observation{attempts: f.Get(psAttempts), trail: f.Get(psTrail), source: f.Source(), id: f.ID()}
			return f.Value()
		}, machine.WithEdge(New[int](client, topicName, subscription).Factory()),
			machine.WithReads[int](psAttempts, psTrail)).
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
	case <-time.After(30 * time.Second):
		t.Fatal("no frame arrived on the far side of the pubsub hop")
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
}

// TestPubSubRefusalIsReportedThenSettled is the ruling's own gate. The delivery count is
// the precise, fast form of "no redelivery loop": a timing window alone would be flaky,
// and a report-count check alone would not see a broker redelivering into a handler that
// had already stopped reporting.
func TestPubSubRefusalIsReportedThenSettled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, server := fake(t)
	seen := newCaptured()
	edge := New[int](client, topicName, subscription)
	if _, err := edge.Factory()("receiver", seen.report); err != nil {
		t.Fatalf("binding the edge to a node: %v", err)
	}
	if err := edge.Start(ctx); err != nil {
		t.Fatalf("starting the edge: %v", err)
	}

	id := server.Publish(topicName, forge(t, "nobody.declared.this", 1), nil)

	_, reported := seen.await(t)
	if !strings.Contains(reported.Error(), "nobody.declared.this") {
		t.Errorf("the reported failure does not name the undeclared key: %v", reported)
	}
	if !strings.Contains(reported.Error(), "receiver") {
		t.Errorf("the reported failure does not name the RECEIVING node: %v", reported)
	}

	deadline := time.Now().Add(10 * time.Second)
	for server.Message(id).Acks < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if acks := server.Message(id).Acks; acks < 1 {
		t.Fatalf("the refused message was never acknowledged: acks=%d", acks)
	}

	// Hold the window open: a nacked message comes straight back, and the delivery count
	// is what makes that visible rather than inferred.
	settled := seen.count()
	time.Sleep(2 * time.Second)
	if deliveries := server.Message(id).Deliveries; deliveries != 1 {
		t.Errorf("the broker delivered the refused message %d times, want exactly 1", deliveries)
	}
	if grown := seen.count(); grown != settled {
		t.Errorf("the refusal was reported %d times after settling, was %d: it is being redelivered", grown, settled)
	}
}

func TestPubSubInjectsTheSendersTraceContext(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	spanContext, traceID := knownTrace(t)
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)

	client, server := fake(t)
	frame, err := machine.RebuildFrame(machine.FrameData{ID: "id-1", Source: "peer", Node: "peer"}, 1)
	if err != nil {
		t.Fatalf("building the frame: %v", err)
	}
	if err = New[int](client, topicName, subscription).Send(ctx, frame); err != nil {
		t.Fatalf("sending: %v", err)
	}

	published := server.Messages()
	if len(published) != 1 {
		t.Fatalf("the broker recorded %d messages, want 1", len(published))
	}
	got := published[0].Attributes["traceparent"]
	if !strings.Contains(got, traceID.String()) {
		t.Errorf("the message carried traceparent %q, want one containing the sender's trace id %s", got, traceID)
	}
}

func TestPubSubExtractsThePeersTraceContextForARefusal(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	spanContext, traceID := knownTrace(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, server := fake(t)
	seen := newCaptured()
	edge := New[int](client, topicName, subscription)
	if _, err := edge.Factory()("receiver", seen.report); err != nil {
		t.Fatalf("binding the edge to a node: %v", err)
	}
	if err := edge.Start(ctx); err != nil {
		t.Fatalf("starting the edge: %v", err)
	}

	attributes := map[string]string{}
	otel.GetTextMapPropagator().Inject(trace.ContextWithSpanContext(context.Background(), spanContext),
		propagation.MapCarrier(attributes))
	server.Publish(topicName, forge(t, "also.undeclared", 2), attributes)

	reportedUnder, _ := seen.await(t)
	got := trace.SpanContextFromContext(reportedUnder)
	if !got.IsValid() {
		t.Fatal("the refusal was reported under a context carrying no span context at all")
	}
	if got.TraceID() != traceID {
		t.Errorf("the refusal was reported under trace %s, want the peer's %s", got.TraceID(), traceID)
	}
}

// TestPubSubTransportErrorReachesHandler injects a NON-retryable code deliberately:
// Unavailable is retryable, so the client would retry to the 60-second publish deadline
// and the error that finally surfaced would be a deadline rather than the injection.
func TestPubSubTransportErrorReachesHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, _ := fake(t, pstest.WithErrorInjection("Publish", codes.InvalidArgument, "injected publish failure"))

	errs := make(chan machine.NodeError[int], 1)
	m := machine.New("pubsub-transport-error")
	// The handler goes on the SOURCE, not on the node the edge delivers into: a publish
	// failure is dispatched by the worker that produced the datum.
	src, ingest := m.Source[int]("in", machine.WithErrorHandler(func(e machine.NodeError[int]) { errs <- e }))
	src.Drop("dead", machine.WithEdge(New[int](client, topicName, subscription).Factory()))

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
		if !strings.Contains(failure.Err.Error(), "injected publish failure") {
			t.Errorf("the handler received %v, want an error carrying the injected failure", failure.Err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the transport failure never reached the registered handler")
	}
}

func TestEdgeServesExactlyOneNode(t *testing.T) {
	client, _ := fake(t)
	edge := New[int](client, topicName, subscription)
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
	client, _ := fake(t)
	_, err := New[int](client, topicName, subscription).Factory()("orphan", nil)
	if err == nil {
		t.Fatal("the edge was constructed with no route to the supervisor's handlers")
	}
	if !strings.Contains(err.Error(), "orphan") {
		t.Errorf("the refusal %v does not name the node", err)
	}
}
