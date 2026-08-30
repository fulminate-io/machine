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
	"time"
)

// defaultMetricWindow is the number of durations a node's collector retains.
const defaultMetricWindow = 10000

// Machine is the supervisor. It owns the node registry, the heap store and the
// declaration errors, and it brings the declared graph up in Start.
//
// Its exported method set is deliberately CLOSED at Host, Metrics, Name, Source
// and Start. No exported method hands a node function anything state-shaped: the
// heap store is private, and a node reaches it only through the capability-gated
// Frame accessors. Adding a public accessor is a gate failure, not a review miss.
type Machine struct {
	name    string
	cfg     *config
	mutex   sync.Mutex
	started bool
	errs    []error
	nodes   map[string]*node
	order   []*node
	edges   []func(ctx context.Context) error
	checks  []func() error
}

// config holds the machine-wide settings an Option writes.
type config struct {
	fifo              bool
	maxConcurrency    int
	metricsWindowSize uint64
	store             Store
	handler           ErrorHandler[any]
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

// OptionMetricWindowSize sets the size of the slice holding duration metrics.
func OptionMetricWindowSize(size uint64) Option {
	return func(c *config) { c.metricsWindowSize = size }
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

// New returns a Machine with the given options applied.
func New(name string, options ...Option) *Machine {
	cfg := &config{metricsWindowSize: defaultMetricWindow, store: NewMemStore()}
	for _, option := range options {
		option(cfg)
	}
	return &Machine{name: name, cfg: cfg, nodes: map[string]*node{}}
}

// Name returns the machine's name.
func (m *Machine) Name() string { return m.name }

// Metrics returns the metric collector for a declared node, or nil if no node of
// that name was declared.
func (m *Machine) Metrics(node string) *MetricCollector {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if declaredNode, ok := m.nodes[node]; ok {
		return declaredNode.instruments.metrics
	}
	return nil
}

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
func (h HostState) Load[V any](c Cell[V]) (V, bool) {
	value, ok := h.store.Load(c.Name())
	if !ok {
		var zero V
		return zero, false
	}
	return value.(V), true
}

// Save writes a value into a heap cell. HOST-ONLY.
func (h HostState) Save[V any](c Cell[V], value V) { h.store.Save(c.Name(), value) }

// node is the control-plane record for one declared node. It carries no type
// parameters: the run closure is synthesized inside the generic builder methods,
// where the payload type is still concrete, so the registry never sees it.
type node struct {
	name        string
	run         func(ctx context.Context)
	instruments *instruments
}

// instruments is the per-node observation slot. observe opens a span for one datum
// and returns the context to run it under plus the closer that records the outcome.
type instruments struct {
	observe func(ctx context.Context, node string) (context.Context, func(err error))
	metrics *MetricCollector
}

func defaultInstruments(window uint64) *instruments {
	collector := newCollector(window)
	return &instruments{
		observe: func(ctx context.Context, _ string) (context.Context, func(err error)) {
			start := time.Now()
			return ctx, func(err error) { collector.Record(time.Since(start), err) }
		},
		metrics: collector,
	}
}

// register returns the record for a node. On a duplicate name it records an error
// and returns a detached record, so declaration continues and Start reports every
// problem at once rather than the first.
func (m *Machine) register(name string) *node {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	registered := &node{name: name, instruments: defaultInstruments(m.cfg.metricsWindowSize)}
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
	return nil
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
