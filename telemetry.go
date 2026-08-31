// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope every span and metric this package emits
// is attributed to. A consumer selects this library's telemetry by it.
const ScopeName = "github.com/whitaker-io/machine/v4"

// scopeVersion is the instrumentation scope version, reported alongside ScopeName.
const scopeVersion = "4.0.0"

// Version returns the version of this instrumentation library.
func Version() string { return scopeVersion }

// The instrument names are carried forward from the v3 telemetry so an existing
// dashboard keeps resolving. The duration unit deliberately does not: v3 recorded
// milliseconds, and OpenTelemetry states durations in seconds.
const (
	runsName     = "machine.runs"
	errorsName   = "machine.errors"
	durationName = "machine.duration"
)

// machineKey and nodeKey are the ONLY attribute keys this package ever sets, and
// both carry node identity rather than anything read out of a datum. An SDK
// aggregates at most 2000 attribute sets per instrument and folds everything past
// that into a single overflow set, so a payload-derived attribute is forbidden
// here: one unbounded stream would exhaust the cap and collapse every other series
// on the instrument along with it.
const (
	machineKey = "machine.name"
	nodeKey    = "machine.node"
)

const (
	datumUnit   = "{datum}"
	secondsUnit = "s"
)

// telemetry holds the machine-wide OpenTelemetry handles, resolved ONCE from the
// configured providers at construction. A provider registered globally after that
// cannot reach an existing machine.
type telemetry struct {
	tracer   trace.Tracer
	runs     metric.Int64Counter
	failures metric.Int64Counter
	duration metric.Float64Histogram
}

// newTelemetry resolves the tracer and the three instruments. A Meter returns a
// usable instrument ALONGSIDE any creation error, so every field is populated even
// on the error return and the caller records the error rather than substituting
// anything for it.
func newTelemetry(cfg *config) (*telemetry, error) {
	meter := cfg.meterProvider.Meter(ScopeName, metric.WithInstrumentationVersion(scopeVersion))
	runs, runsErr := meter.Int64Counter(runsName,
		metric.WithDescription("Data a node has begun processing."),
		metric.WithUnit(datumUnit))
	failures, failuresErr := meter.Int64Counter(errorsName,
		metric.WithDescription("Failures a node has reported to its error handler."),
		metric.WithUnit(datumUnit))
	duration, durationErr := meter.Float64Histogram(durationName,
		metric.WithDescription("Time a node spent processing one datum."),
		metric.WithUnit(secondsUnit))
	return &telemetry{
		tracer:   cfg.tracerProvider.Tracer(ScopeName, trace.WithInstrumentationVersion(scopeVersion)),
		runs:     runs,
		failures: failures,
		duration: duration,
	}, errors.Join(runsErr, failuresErr, durationErr)
}

// instruments is the per-node observation slot. It holds the node's identity
// attributes THREE ways as pre-built option slices, so the per-datum path passes a
// stored slice rather than allocating a variadic one, and builds no attribute set
// of its own.
type instruments struct {
	telemetry *telemetry
	name      string
	add       []metric.AddOption
	record    []metric.RecordOption
	spanOpts  []trace.SpanStartOption
}

// newInstruments builds a node's identity attribute set ONCE, at declaration time.
// This is the only place in the package that constructs an attribute.
func newInstruments(t *telemetry, machineName, nodeName string) *instruments {
	set := attribute.NewSet(attribute.String(machineKey, machineName), attribute.String(nodeKey, nodeName))
	return &instruments{
		telemetry: t,
		name:      nodeName,
		add:       []metric.AddOption{metric.WithAttributeSet(set)},
		record:    []metric.RecordOption{metric.WithAttributeSet(set)},
		spanOpts:  []trace.SpanStartOption{trace.WithAttributes(set.ToSlice()...)},
	}
}

// start opens the node's span for one datum and counts the run. It returns the
// context the datum runs under, which carries the span.
func (i *instruments) start(ctx context.Context) (context.Context, trace.Span) {
	spanCtx, span := i.telemetry.tracer.Start(ctx, i.name, i.spanOpts...)
	if i.telemetry.runs.Enabled(spanCtx) {
		i.telemetry.runs.Add(spanCtx, 1, i.add...)
	}
	return spanCtx, span
}

// finish records the datum's duration and ends the span. It runs for every datum,
// failed or not, so the histogram counts ATTEMPTS and a datum that panicked is
// counted by both the runs counter and the histogram.
func (i *instruments) finish(ctx context.Context, span trace.Span, started time.Time) {
	if i.telemetry.duration.Enabled(ctx) {
		i.telemetry.duration.Record(ctx, time.Since(started).Seconds(), i.record...)
	}
	span.End()
}

// observeError records a node failure on the datum's span and counts it. It ONLY
// observes: it calls no handler, returns nothing, and decides nothing about where
// the failure routes.
func (i *instruments) observeError(ctx context.Context, err error) {
	span := trace.SpanFromContext(ctx)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	if i.telemetry.failures.Enabled(ctx) {
		i.telemetry.failures.Add(ctx, 1, i.add...)
	}
}
