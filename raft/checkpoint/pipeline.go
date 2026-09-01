// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/whitaker-io/machine/raft/ledger"
)

// maxInFlightPerGroup bounds how many checkpoint appends are outstanding for ONE
// flow at a time.
//
// IT IS A COUNT, and the count is the bound doing the work. Bytes follow as a
// consequence of this many marshaled packets and are NOT independently bounded — a
// caller checkpointing enormous payloads is bounded by its own payload size, which
// this package cannot see and does not pretend to. Time is deliberately not bounded
// here either: the caller's context is the outer bound and is honored.
//
// SIXTY-FOUR IS raft's OWN MaxAppendEntries DEFAULT, which is the batch size the
// group-commit loop drains into. A window materially smaller than it cannot fill a
// batch, and the amortization measurement that produced the order-of-magnitude
// difference was taken at 64 concurrent producers.
//
// A SUBMISSION PAST THE WINDOW BLOCKS THE CALLER; it is never dropped. A dropped
// checkpoint is a datum that silently cannot be recovered, which is the one outcome
// this package exists to prevent.
const maxInFlightPerGroup = 64

// ErrClosed reports an append submitted to a pipeline that has been closed.
var ErrClosed = errors.New("checkpoint: pipeline is closed")

// Journal is the append surface a Pipeline drives. *ledger.Ledger satisfies it.
//
// IT IS AN INTERFACE SO THE WINDOW CAN BE MEASURED. The property this package is
// built for — many appends in flight for one group — is only observable against a
// journal that can be made to block, and a concrete dependency would make the
// pipeline's own concurrency untestable.
type Journal interface {
	Append(ctx context.Context, entry ledger.Entry) (uint64, error)
}

// Config describes one pipeline.
type Config struct {
	// Journal resolves the journal replicating a flow. It is called once per
	// submission rather than cached here, because which node leads a group changes
	// and the resolution is the caller's to own.
	Journal func(flow string) (Journal, error)
	// Failure receives an append that was submitted and did NOT land.
	//
	// IT IS REQUIRED. A checkpoint that failed leaves its datum unrecoverable from
	// that point, and only the caller can decide what that means for the flow — so
	// the failure reaches the flow's error handler rather than being logged here
	// and forgotten. A pipeline built without one is refused at construction.
	Failure func(flow, datum string, err error)
}

// Pipeline submits checkpoint appends without awaiting them one datum at a time.
//
// THE SHAPE IS THE MEASUREMENT. A serial per-datum writer — submit, await, submit —
// caps a flow at roughly nineteen checkpoints a second on a worker hosting sixteen
// co-resident groups, because every datum pays a full fsync. Holding many appends in
// flight for one group lets the group-commit loop amortize one fsync across a batch,
// which measured an order of magnitude better on the same harness. Append therefore
// SUBMITS AND RETURNS; it never awaits its own future inline.
type Pipeline struct {
	resolve func(flow string) (Journal, error)
	failure func(flow, datum string, err error)

	mutex   sync.Mutex
	windows map[string]chan struct{}
	closed  bool

	inflight sync.WaitGroup
	once     sync.Once

	failureMutex sync.Mutex
	failures     []error
}

// New builds a pipeline. It refuses an incomplete Config rather than inventing
// either half: a pipeline with no journal resolver can append nothing, and one with
// no failure handler would have nowhere to report a checkpoint that did not land.
func New(cfg Config) (*Pipeline, error) {
	if cfg.Journal == nil {
		return nil, fmt.Errorf("checkpoint: Config.Journal is required: %w", errIncompleteConfig)
	}
	if cfg.Failure == nil {
		return nil, fmt.Errorf("checkpoint: Config.Failure is required: %w", errIncompleteConfig)
	}

	return &Pipeline{
		resolve: cfg.Journal,
		failure: cfg.Failure,
		windows: map[string]chan struct{}{},
	}, nil
}

// errIncompleteConfig reports a Config missing something New cannot invent.
var errIncompleteConfig = errors.New("checkpoint: configuration is incomplete")

// Append journals a datum's progress and RETURNS WITHOUT AWAITING IT.
//
// The error it returns is about the SUBMISSION — a closed pipeline, a flow whose
// journal cannot be resolved, or a context that ended while the window was full. An
// append that was submitted and later failed does NOT come back here; it reaches
// Config.Failure, because by the time it resolves this call is long gone.
//
// It blocks when the flow's window is full, which is backpressure rather than a
// stall: the caller is told to slow down by being made to wait, and no checkpoint is
// discarded to keep it moving.
func (p *Pipeline) Append(ctx context.Context, flow, datum string, data []byte) error {
	if ctx == nil {
		return fmt.Errorf("checkpoint: a nil context reached the pipeline: %w", errIncompleteConfig)
	}

	journal, err := p.admit(flow)
	if err != nil {
		return err
	}

	slot, err := p.acquire(ctx, flow)
	if err != nil {
		return err
	}

	p.inflight.Add(1)
	go p.submit(ctx, submission{
		journal: journal,
		entry:   ledger.Entry{Kind: ledger.KindSet, Path: Path(datum), Value: data},
		flow:    flow,
		datum:   datum,
		slot:    slot,
	})

	return nil
}

// submission is one append in flight, carried as a value so the goroutine that runs
// it takes a single parameter rather than six positional ones.
type submission struct {
	journal Journal
	entry   ledger.Entry
	flow    string
	datum   string
	slot    chan struct{}
}

// admit resolves a flow's journal, refusing once the pipeline is closed.
func (p *Pipeline) admit(flow string) (Journal, error) {
	p.mutex.Lock()
	closed := p.closed
	p.mutex.Unlock()

	if closed {
		return nil, ErrClosed
	}

	journal, err := p.resolve(flow)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: resolving the journal for flow %q: %w", flow, err)
	}
	if journal == nil {
		return nil, fmt.Errorf("checkpoint: flow %q resolved to no journal: %w", flow, errIncompleteConfig)
	}

	return journal, nil
}

// acquire takes one slot in the flow's in-flight window, waiting when it is full.
//
// The wait honors the caller's context, so a caller that gave up is not held by
// backpressure it can no longer benefit from.
func (p *Pipeline) acquire(ctx context.Context, flow string) (chan struct{}, error) {
	p.mutex.Lock()
	window, ok := p.windows[flow]
	if !ok {
		window = make(chan struct{}, maxInFlightPerGroup)
		p.windows[flow] = window
	}
	p.mutex.Unlock()

	select {
	case window <- struct{}{}:
		return window, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("checkpoint: waiting for room in flow %q's window: %w", flow, ctx.Err())
	}
}

// submit runs one append to completion and releases its slot.
//
// A FAILURE IS REPORTED, NEVER SWALLOWED AND NEVER RETRIED HERE. This package has no
// retry loop of its own: the journal's own forwarding already retries across a
// leadership change, and a second loop layered on top would spin against conditions
// no retry repairs while the caller — the only party who can decide what an
// unrecoverable datum means — heard nothing.
func (p *Pipeline) submit(ctx context.Context, s submission) {
	defer p.inflight.Done()
	defer func() { <-s.slot }()

	if _, err := s.journal.Append(ctx, s.entry); err != nil {
		wrapped := fmt.Errorf("checkpoint: journaling datum %q on flow %q: %w", s.datum, s.flow, err)

		p.failureMutex.Lock()
		p.failures = append(p.failures, wrapped)
		p.failureMutex.Unlock()

		p.failure(s.flow, s.datum, wrapped)
	}
}

// Close stops admitting appends, DRAINS the ones already in flight, and returns
// every failure the drain observed, joined.
//
// IT DRAINS RATHER THAN ABANDONING. An append already submitted describes progress a
// recovery will read; dropping it on the way out would lose exactly the datum a
// clean shutdown should have preserved.
//
// The returned errors are the same ones Config.Failure already received live. They
// are joined here too so a caller that shuts down and checks one value learns what
// did not land, without having to have watched the callback.
//
// It is idempotent: a second Close returns the same joined result rather than
// draining again.
func (p *Pipeline) Close() error {
	p.once.Do(func() {
		p.mutex.Lock()
		p.closed = true
		p.mutex.Unlock()

		p.inflight.Wait()
	})

	p.failureMutex.Lock()
	defer p.failureMutex.Unlock()

	return errors.Join(p.failures...)
}
