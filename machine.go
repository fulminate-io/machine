// Package machine - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package machine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Machine is the supervisor. It owns the node registry, the heap store, the
// telemetry handles and the declaration errors, and it brings the declared graph
// up in Start.
//
// Its exported method set is deliberately CLOSED at Host, Name, Source and Start.
// No exported method hands a node function anything state-shaped: the heap store is
// private, and a node reaches it only through the capability-gated Frame accessors.
// Adding a public accessor is a gate failure, not a review miss.
type Machine struct {
	name      string
	cfg       *config
	telemetry *telemetry
	mutex     sync.Mutex
	started   bool
	errs      []error
	nodes     map[string]*node
	order     []*node
	edges     []func(ctx context.Context) error
	closers   []func(ctx context.Context)
	checks    []func() error
}

// config holds the machine-wide settings an Option writes.
type config struct {
	fifo           bool
	maxConcurrency int
	store          Store
	journal        Journal
	handler        ErrorHandler[any]
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
}

// Option configures a Machine at construction.
type Option func(*config)

// OptionFIFO forces serial processing: the machine waits for one datum to be
// processed before starting the next.
var OptionFIFO Option = func(c *config) { c.fifo = true }

// OptionMaxConcurrency bounds the number of data a node processes at once when
// FIFO is off. Zero, the default, is unbounded.
func OptionMaxConcurrency(n int) Option {
	return func(c *config) { c.maxConcurrency = n }
}

// OptionErrorHandler registers the global fallback handler. It is erased because a
// machine's nodes carry many payload types; a node can register a typed handler of
// its own with WithErrorHandler, which wins over this one.
func OptionErrorHandler(h ErrorHandler[any]) Option {
	return func(c *config) { c.handler = h }
}

// OptionStore replaces the machine's heap store. The replacement is reached only
// through the capability-gated Frame accessors and through Host.
func OptionStore(s Store) Option {
	return func(c *config) { c.store = s }
}

// WithTracerProvider sets the provider the machine resolves its tracer from. It
// defaults to otel.GetTracerProvider(). A nil provider is a declaration-time
// programmer error and panics rather than silently substituting the global.
func WithTracerProvider(provider trace.TracerProvider) Option {
	if provider == nil {
		panic("machine: WithTracerProvider was given a nil provider")
	}
	return func(c *config) { c.tracerProvider = provider }
}

// WithMeterProvider sets the provider the machine resolves its meter from. It
// defaults to otel.GetMeterProvider(). A nil provider is a declaration-time
// programmer error and panics rather than silently substituting the global.
func WithMeterProvider(provider metric.MeterProvider) Option {
	if provider == nil {
		panic("machine: WithMeterProvider was given a nil provider")
	}
	return func(c *config) { c.meterProvider = provider }
}

// New returns a Machine with the given options applied. The telemetry providers
// are seeded from the otel globals BEFORE the options run and resolved ONCE here,
// so a provider registered globally afterwards cannot reach this machine.
func New(name string, options ...Option) *Machine {
	cfg := &config{
		store:          NewMemStore(),
		tracerProvider: otel.GetTracerProvider(),
		meterProvider:  otel.GetMeterProvider(),
	}
	for _, option := range options {
		option(cfg)
	}
	m := &Machine{name: name, cfg: cfg, nodes: map[string]*node{}}
	instrumentation, err := newTelemetry(cfg)
	if err != nil {
		m.errs = append(m.errs, fmt.Errorf("machine: instrument creation failed: %w", err))
	}
	m.telemetry = instrumentation
	return m
}

// Name returns the machine's name.
func (m *Machine) Name() string { return m.name }

// HasJournal reports whether a journal is wired, so a caller can refuse a flow that
// needs one BEFORE declaring any of it.
//
// WHY THE PREDICATE EXISTS RATHER THAN LETTING Start REFUSE. The runtime does check
// — newWorker refuses a checkpointed node on a journal-less machine — but that
// refusal reaches the caller only from Start, because the check calls fail, which
// appends to the error set Start returns. Wire declares a flow; it does not start
// one. So a generated Wire has no way to learn the fact from the runtime at the
// moment it needs it, which is before it declares its first node. This is that fact,
// available at declaration time.
//
// IT READS AND NOTHING MORE. It deliberately does not return the journal: the
// journal is the HOST's to configure through OptionJournal, and generated code must
// not be able to reach its configuration. A bool is the whole of what a caller
// deciding whether to proceed needs.
func (m *Machine) HasJournal() bool { return m.cfg.journal != nil }

// HostState is the HOST-ONLY view of a machine's heap state. It exists so a program
// can seed heap cells before Start and inspect them after, from OUTSIDE flow
// execution. A node function must NEVER call it: it bypasses the capability gate
// entirely, and a node reaches the heap through the frame's Load, Save and Update.
//
// Enforcement is structural and static — the Machine's closed exported method set,
// plus static analysis over node function bodies. There is deliberately no runtime
// caller check: a stack walk is unsound across goroutine boundaries, and false
// assurance is worse than none.
type HostState struct {
	store Store
}

// Host returns the HOST-ONLY heap accessor. See HostState.
func (m *Machine) Host() HostState { return HostState{store: m.cfg.store} }

// Load returns the value held in a heap cell and whether it was present. HOST-ONLY.
//
// It takes a context because the store may block — a replicated implementation reads
// through a quorum round trip — and because the host calls from OUTSIDE flow
// execution, where there is no frame context to borrow.
//
// On a non-nil error the value is the zero value of V and present is false: the store
// did not answer, which is not the same as answering absent.
func (h HostState) Load[V any](ctx context.Context, c Cell[V]) (V, bool, error) {
	value, ok, err := h.store.Load(ctx, c.Name())
	if !ok || err != nil {
		var zero V
		return zero, false, err
	}
	return value.(V), true, nil
}

// Save writes a value into a heap cell. HOST-ONLY.
//
// It takes a context for the same reason Load does: the store may block, and the host
// calls from OUTSIDE flow execution and so has no frame context to borrow.
func (h HostState) Save[V any](ctx context.Context, c Cell[V], value V) error {
	return h.store.Save(ctx, c.Name(), value)
}

// node is the control-plane record for one declared node. It carries no type
// parameters: the run closure is synthesized inside the generic builder methods,
// where the payload type is still concrete, so the registry never sees it.
type node struct {
	name        string
	run         func(ctx context.Context)
	instruments *instruments
}

// register returns the record for a node. On a duplicate name it records an error
// and returns a detached record, so declaration continues and Start reports every
// problem at once rather than the first.
func (m *Machine) register(name string) *node {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	registered := &node{name: name, instruments: newInstruments(m.telemetry, m.name, name)}
	if _, ok := m.nodes[name]; ok {
		m.errs = append(m.errs, fmt.Errorf("machine: duplicate node name %q", name))
		return registered
	}
	m.nodes[name] = registered
	m.order = append(m.order, registered)
	return registered
}

func (m *Machine) addEdge(start func(ctx context.Context) error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.edges = append(m.edges, start)
}

// addCloser registers an edge teardown to run when the machine's context ends. It
// mirrors addEdge, which registers the matching bring-up.
func (m *Machine) addCloser(closer func(ctx context.Context)) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.closers = append(m.closers, closer)
}

func (m *Machine) addCheck(check func() error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.checks = append(m.checks, check)
}

func (m *Machine) fail(err error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.errs = append(m.errs, err)
}

// Start validates the declared graph, brings up every edge and then spawns every
// node. Validation runs to completion and joins every declaration error, and Start
// returns before spawning anything, so a mis-declared graph is inert rather than
// half-running.
func (m *Machine) Start(ctx context.Context) error {
	if err := m.begin(); err != nil {
		return err
	}
	if err := m.validate(); err != nil {
		return err
	}
	if err := m.startEdges(ctx); err != nil {
		return err
	}
	m.spawn(ctx)
	go m.shutdown(ctx)
	return nil
}

// shutdown closes every constructed edge once the machine's context ends. Machine
// shutdown IS context cancellation: Start is the only lifecycle entry, so there is no
// Stop to call and no exported method is added here.
//
// Each closer runs under a context DERIVED FROM the canceled one with its cancellation
// stripped, because a transport's own teardown — draining a subscription, shutting an
// http server — needs a live context to do the work being asked of it.
func (m *Machine) shutdown(ctx context.Context) {
	<-ctx.Done()
	m.mutex.Lock()
	closers := make([]func(ctx context.Context), len(m.closers))
	copy(closers, m.closers)
	m.mutex.Unlock()
	teardown := context.WithoutCancel(ctx)
	for _, closer := range closers {
		closer(teardown)
	}
}

func (m *Machine) begin() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if m.started {
		return fmt.Errorf("machine: %q was already started", m.name)
	}
	m.started = true
	return nil
}

func (m *Machine) validate() error {
	m.mutex.Lock()
	errs := make([]error, len(m.errs))
	copy(errs, m.errs)
	checks := make([]func() error, len(m.checks))
	copy(checks, m.checks)
	m.mutex.Unlock()
	for _, check := range checks {
		if err := check(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Machine) startEdges(ctx context.Context) error {
	m.mutex.Lock()
	edges := make([]func(ctx context.Context) error, len(m.edges))
	copy(edges, m.edges)
	m.mutex.Unlock()
	for _, start := range edges {
		if err := start(ctx); err != nil {
			return err
		}
	}
	return nil
}

// spawn starts every node's read loop. A terminal declared with Output has no read
// loop of its own — the caller drains its edge — so its record carries no run.
func (m *Machine) spawn(ctx context.Context) {
	m.mutex.Lock()
	order := make([]*node, len(m.order))
	copy(order, m.order)
	m.mutex.Unlock()
	for _, spawning := range order {
		if spawning.run != nil {
			spawning.run(ctx)
		}
	}
}
