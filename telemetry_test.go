// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// The node names the telemetry fixtures declare. They are constants because the
// assertions match on them and a drifting literal would assert nothing.
const (
	probeMachine = "probe"
	probeSource  = "probe.source"
	probeNode    = "probe.node"
	probeOut     = "probe.out"
)

// probe is the in-process OTEL assertion harness. It holds a span recorder and a
// manual metric reader together with the Machine options that wire the machine to
// them, so a test observes exactly the telemetry its own machine emitted and never
// the process globals.
type probe struct {
	spans   *tracetest.SpanRecorder
	reader  *sdkmetric.ManualReader
	options []Option
}

func newProbe() *probe {
	spans := tracetest.NewSpanRecorder()
	reader := sdkmetric.NewManualReader()
	return &probe{
		spans:  spans,
		reader: reader,
		options: []Option{
			WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))),
			WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))),
		},
	}
}

// scope collects and returns this package's ScopeMetrics, failing if the reader saw
// no metrics attributed to ScopeName at all.
func (p *probe) scope(t *testing.T) metricdata.ScopeMetrics {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := p.reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}
	for _, scope := range collected.ScopeMetrics {
		if scope.Scope.Name == ScopeName {
			return scope
		}
	}
	t.Fatalf("no ScopeMetrics for %q; got %d scopes", ScopeName, len(collected.ScopeMetrics))
	return metricdata.ScopeMetrics{}
}

// sum returns the value a named counter holds for one node, or zero when the
// instrument has no data point for that node yet.
func (p *probe) sum(t *testing.T, name, node string) int64 {
	t.Helper()
	for _, recorded := range p.scope(t).Metrics {
		if recorded.Name != name {
			continue
		}
		counter, ok := recorded.Data.(metricdata.Sum[int64])
		if !ok {
			t.Fatalf("%s carries %T, want a Sum[int64]", name, recorded.Data)
		}
		for _, point := range counter.DataPoints {
			if value, found := point.Attributes.Value(attribute.Key(nodeKey)); found && value.AsString() == node {
				return point.Value
			}
		}
	}
	return 0
}

// await receives one value on a deadline. A bare receive would hang the whole suite
// until the run timeout instead of naming what never reported, which is the repo's
// reason for the deadline in drain and pollUntil too.
func await[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not happen before the deadline", what)
	}
	var zero T
	return zero
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name())
	}
	return names
}

// straightPipeline declares the graph the telemetry tests drive: an ingestion node,
// one identity Map and a terminal Output, buffered so a test can feed before it
// drains.
func straightPipeline(m *Machine) (Ingest[int], <-chan Packet[int]) {
	src, ingest := m.Source[int](probeSource, WithEdge(Channel[int](16)))
	out := src.Map(probeNode, func(f Frame[int]) int { return f.Value() },
		WithEdge(Channel[int](16))).Output(probeOut, WithEdge(Channel[int](16)))
	return ingest, out
}

// failingEdge is an inbound transport that accepts nothing. A node given one makes
// its PRODUCER fail on the send path, which is the failure class guard's recover
// never sees and dispatch does.
type failingEdge[T any] struct {
	ch   chan Packet[T]
	err  error
	stop sync.Once
}

func (*failingEdge[T]) Start(context.Context) error             { return nil }
func (e *failingEdge[T]) Send(context.Context, Packet[T]) error { return e.err }
func (e *failingEdge[T]) Receive() <-chan Packet[T]             { return e.ch }
func (e *failingEdge[T]) Close() error                         { e.stop.Do(func() { close(e.ch) }); return nil }

// awaitSpan waits for a node to END a span and returns it. A span ends in guard's
// deferred finish, so it is not observable the moment the datum leaves the node.
func awaitSpan(t *testing.T, p *probe, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found sdktrace.ReadOnlySpan
	pollUntil(t, "a span named "+name+" ended", func() bool {
		for _, span := range p.spans.Ended() {
			if span.Name() == name {
				found = span
				return true
			}
		}
		return false
	})
	return found
}

// restoreGlobals returns the process-wide otel providers to NOOP ones. A test that
// installs recording globals must call it, and must NEVER restore a fresh live SDK
// provider: that leaves every later test and benchmark in this binary measuring a
// recording regime.
func restoreGlobals(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetMeterProvider(metricnoop.NewMeterProvider())
	})
}

func TestSpanPerNodeExecutionCarriesIdentityAttributes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const data = 3
	p := newProbe()
	m := New(probeMachine, p.options...)
	ingest, out := straightPipeline(m)

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, data, 1)
	drain(t, out, data)

	pollUntil(t, "both processing nodes ended a span for every datum", func() bool {
		sources, nodes := 0, 0
		for _, name := range spanNames(p.spans.Ended()) {
			switch name {
			case probeSource:
				sources++
			case probeNode:
				nodes++
			}
		}
		return sources >= data && nodes >= data
	})

	for _, span := range p.spans.Ended() {
		if span.Name() == probeOut {
			t.Fatalf("%s produced a span; an Output node does not process", probeOut)
		}
		want := attribute.NewSet(
			attribute.String(machineKey, probeMachine),
			attribute.String(nodeKey, span.Name()),
		)
		// The WHOLE set is compared, not merely searched: a containment check would
		// not see a payload-derived key added alongside the identity ones.
		if got := attribute.NewSet(span.Attributes()...); !got.Equals(&want) {
			t.Fatalf("span %q carries attributes %v, want exactly %v",
				span.Name(), got.ToSlice(), want.ToSlice())
		}
		scope := span.InstrumentationScope()
		if scope.Name != ScopeName || scope.Version != Version() {
			t.Fatalf("span %q reports scope %q version %q, want %q version %q",
				span.Name(), scope.Name, scope.Version, ScopeName, Version())
		}
	}
}

func TestRunsCounterAndDurationHistogramRecorded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const data = 5
	p := newProbe()
	m := New(probeMachine, p.options...)
	ingest, out := straightPipeline(m)

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, data, 1)
	drain(t, out, data)

	pollUntil(t, "the runs counter reached every datum", func() bool {
		return p.sum(t, runsName, probeNode) >= data
	})
	if version := p.scope(t).Scope.Version; version != Version() {
		t.Fatalf("the metric scope reports version %q, want %q", version, Version())
	}
	assertDurationHistogram(t, p, data)
	// The zero is meaningful because the panic and send-error tests drive the SAME
	// helper against the SAME instrument non-zero; a sum that could only return zero
	// would fail those.
	if failures := p.sum(t, errorsName, probeNode); failures != 0 {
		t.Fatalf("%s reads %d for %s on a clean run, want 0", errorsName, failures, probeNode)
	}
}

func assertDurationHistogram(t *testing.T, p *probe, want int) {
	t.Helper()
	for _, recorded := range p.scope(t).Metrics {
		if recorded.Name != durationName {
			continue
		}
		if recorded.Unit != secondsUnit {
			t.Fatalf("%s reports unit %q, want %q", durationName, recorded.Unit, secondsUnit)
		}
		histogram, ok := recorded.Data.(metricdata.Histogram[float64])
		if !ok {
			t.Fatalf("%s carries %T, want a Histogram[float64]", durationName, recorded.Data)
		}
		assertHistogramPoints(t, histogram, want)
		return
	}
	t.Fatalf("no %s instrument was recorded", durationName)
}

func assertHistogramPoints(t *testing.T, histogram metricdata.Histogram[float64], want int) {
	t.Helper()
	found := false
	for _, point := range histogram.DataPoints {
		if point.Attributes.Len() != 2 {
			t.Fatalf("%s has a data point carrying %d attributes, want exactly 2",
				durationName, point.Attributes.Len())
		}
		node, _ := point.Attributes.Value(attribute.Key(nodeKey))
		if node.AsString() != probeNode {
			continue
		}
		found = true
		if point.Count < uint64(want) {
			t.Fatalf("%s recorded %d durations for %s, want at least %d",
				durationName, point.Count, probeNode, want)
		}
	}
	if !found {
		t.Fatalf("%s has no data point for %s", durationName, probeNode)
	}
}

func TestPanicRecordsSpanStatusEventAndErrorCounterWithoutDisturbingRouting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handled := make(chan NodeError[any], 1)
	p := newProbe()
	options := append([]Option{OptionErrorHandler(func(e NodeError[any]) { handled <- e })}, p.options...)
	m := New(probeMachine, options...)
	src, ingest := m.Source[int](probeSource, WithEdge(Channel[int](16)))
	src.Map(probeNode, func(_ Frame[int]) int { panic(errors.New("boom")) },
		WithEdge(Channel[int](16))).Drop(probeOut, WithEdge(Channel[int](16)))

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 1)

	// Routing half: telemetry observes, the handler still acts.
	failure := await(t, "the panic reached the registered handler", handled)
	if failure.Node != probeNode {
		t.Fatalf("the handler received node %q, want %q", failure.Node, probeNode)
	}
	if !failure.Panic {
		t.Fatal("the handler received Panic=false for a recovered panic")
	}
	if failure.Err == nil || failure.Err.Error() != "boom" {
		t.Fatalf("the handler received err %v, want boom", failure.Err)
	}

	// Telemetry half.
	pollUntil(t, "the error counter recorded the panic", func() bool {
		return p.sum(t, errorsName, probeNode) >= 1
	})
	span := awaitSpan(t, p, probeNode)
	if span.Status().Code != codes.Error {
		t.Fatalf("the panicking node's span status is %v, want %v", span.Status().Code, codes.Error)
	}
	if span.Status().Description != "boom" {
		t.Fatalf("the span status description is %q, want boom", span.Status().Description)
	}
	events := span.Events()
	if len(events) == 0 || events[0].Name != "exception" {
		t.Fatalf("the span's first event is %v, want an exception event", events)
	}
}

func TestSendErrorIsRecordedAndRouted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handled := make(chan NodeError[any], 1)
	refused := errors.New("edge refused")
	p := newProbe()
	options := append([]Option{OptionErrorHandler(func(e NodeError[any]) { handled <- e })}, p.options...)
	m := New(probeMachine, options...)
	src, ingest := m.Source[int](probeSource, WithEdge(Channel[int](16)))
	// The refusing edge is the INBOUND transport of probe.node, so the failure is
	// raised by its PRODUCER on the send path — the class guard's recover never sees.
	src.Map(probeNode, func(f Frame[int]) int { return f.Value() },
		WithEdge[int](func(string, Report) (Edge[int], error) {
			return &failingEdge[int]{ch: make(chan Packet[int]), err: refused}, nil
		})).Drop(probeOut, WithEdge(Channel[int](16)))

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 1)

	failure := await(t, "the send failure reached the registered handler", handled)
	if failure.Node != probeSource {
		t.Fatalf("the handler received node %q, want %q", failure.Node, probeSource)
	}
	if failure.Panic {
		t.Fatal("the handler received Panic=true for a send error")
	}
	if !errors.Is(failure.Err, refused) {
		t.Fatalf("the handler received err %v, want %v", failure.Err, refused)
	}

	pollUntil(t, "the error counter recorded the send failure", func() bool {
		return p.sum(t, errorsName, probeSource) >= 1
	})
	span := awaitSpan(t, p, probeSource)
	if span.Status().Code != codes.Error {
		t.Fatalf("%s's span status is %v, want %v", probeSource, span.Status().Code, codes.Error)
	}
}

func TestProvidersDefaultToTheOTelGlobals(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const data = 2
	global := &probe{spans: tracetest.NewSpanRecorder(), reader: sdkmetric.NewManualReader()}
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(global.spans)))
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(global.reader)))
	restoreGlobals(t)

	m := New(probeMachine)
	ingest, out := straightPipeline(m)

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, data, 1)
	drain(t, out, data)

	pollUntil(t, "both nodes ended spans in the globally registered tracer provider", func() bool {
		ended := spanNames(global.spans.Ended())
		return slices.Contains(ended, probeSource) && slices.Contains(ended, probeNode)
	})
	if runs := global.sum(t, runsName, probeNode); runs < data {
		t.Fatalf("the globally registered meter provider recorded %d runs for %s, want at least %d",
			runs, probeNode, data)
	}
}

func TestProvidersResolveOnceAtConstruction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newProbe()
	m := New(probeMachine, p.options...)
	ingest, out := straightPipeline(m)

	late := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(late)))
	restoreGlobals(t)

	startMachine(t, ctx, m)
	feed(t, ctx, ingest, 1, 1)
	drain(t, out, 1)

	// The known positive: SOME recorder ended the node's span, so the late recorder's
	// zero below means the global was never consulted rather than that nothing ran.
	// It admits either recorder deliberately, so a machine that wrongly resolved the
	// global fails on the assertion that names that violation instead of on a
	// deadline that does not.
	pollUntil(t, "a recorder ended the node's span", func() bool {
		return slices.Contains(spanNames(p.spans.Ended()), probeNode) ||
			slices.Contains(spanNames(late.Ended()), probeNode)
	})
	if ended := late.Ended(); len(ended) != 0 {
		t.Fatalf("a provider registered after construction captured %v", spanNames(ended))
	}
	if !slices.Contains(spanNames(p.spans.Ended()), probeNode) {
		t.Fatalf("the machine's own recorder ended %v, want a span named %s",
			spanNames(p.spans.Ended()), probeNode)
	}
}

// erroringMeter hands back a USABLE instrument alongside a creation error, which is the
// contract newTelemetry is written against: every field is populated even on the error
// return, and the caller records the error rather than substituting anything for it.
type erroringMeter struct {
	metricnoop.Meter
	err error
}

func (m erroringMeter) Int64Counter(name string, opts ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	counter, _ := m.Meter.Int64Counter(name, opts...)
	return counter, m.err
}

func (m erroringMeter) Float64Histogram(
	name string, opts ...metric.Float64HistogramOption,
) (metric.Float64Histogram, error) {
	histogram, _ := m.Meter.Float64Histogram(name, opts...)
	return histogram, m.err
}

// erroringMeterProvider is a MeterProvider whose every instrument fails to be created.
// It is an ordinary implementation of the exported metric.MeterProvider interface, so
// the failure reaches New the same way a real SDK's would.
type erroringMeterProvider struct {
	metricnoop.MeterProvider
	err error
}

func (p erroringMeterProvider) Meter(string, ...metric.MeterOption) metric.Meter {
	return erroringMeter{err: p.err}
}

func TestNilProviderOptionsPanic(t *testing.T) {
	assertPanics(t, "WithTracerProvider given a nil provider", func() { WithTracerProvider(nil) })
	assertPanics(t, "WithMeterProvider given a nil provider", func() { WithMeterProvider(nil) })
	// The known positive: newProbe drives both options with REAL providers, so the panics
	// above are the nil rather than options that refuse everything.
	if options := newProbe().options; len(options) != 2 {
		t.Fatalf("newProbe built %d options from real providers, want 2", len(options))
	}
}

func TestInstrumentCreationFailureIsReportedFromStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	broken := errors.New("instrument unavailable")
	m := New(probeMachine, WithMeterProvider(erroringMeterProvider{err: broken}))

	err := m.Start(ctx)
	if err == nil {
		t.Fatal("Start accepted a machine whose instruments failed to be created")
	}
	if !errors.Is(err, broken) {
		t.Fatalf("Start returned %v, want an error wrapping the meter's own %v", err, broken)
	}
	if !strings.Contains(err.Error(), "instrument creation failed") {
		t.Fatalf("Start returned %v, want an error naming the instrument creation failure", err)
	}

	// The known positive: the same empty declaration starts clean with working
	// instruments, so the refusal above is the meter rather than a Start that refuses a
	// machine declaring no nodes.
	working := New(probeMachine, newProbe().options...)
	if err := working.Start(ctx); err != nil {
		t.Fatalf("a machine with working instruments failed to start: %v", err)
	}
}
