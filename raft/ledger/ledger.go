// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ledger

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	boltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/whitaker-io/machine/raft/transport"
)

// leadershipNotifyBuffer bounds how many leadership transitions raft may hand a
// ledger that has not drained them yet.
//
// IT IS NOT A TUNING KNOB. raft's send on NotifyCh BLOCKS: runLeader sends in a
// select whose only alternative is raft's own shutdown channel, so an un-drained
// notification wedges the leader — raft goes on reporting the node healthy while
// every operation on it times out. This number is therefore the count of leadership
// transitions raft tolerates before runLeader parks.
const leadershipNotifyBuffer = 8

// snapshotsRetained is how many completed snapshots the file store keeps.
const snapshotsRetained = 2

// boltFileName is the single bolt database a Dir-backed ledger keeps its log and
// stable state in.
const boltFileName = "ledger.bolt"

// Lifecycle refusals, declared beside the code that returns them.
var (
	// ErrClosed reports a call on a ledger that has been closed.
	ErrClosed = errors.New("ledger: ledger is closed")
	// ErrConfigIncomplete reports a Config missing something Open cannot invent.
	ErrConfigIncomplete = errors.New("ledger: configuration is incomplete")
	// ErrNilContext reports a call carrying no context at all, which is a
	// programming error rather than a state to tolerate.
	ErrNilContext = errors.New("ledger: a nil context reached the ledger")
	// ErrNotLeader reports that this node does not lead the flow's group. Every
	// read and write here is leader-only, so this is the refusal a follower gives
	// rather than serving a value it cannot prove current.
	//
	// THIS REFUSAL IS AN INTERIM AND NOT THE SETTLED CONTRACT. The settled design
	// forwards a non-leader Save, Update and linearizable Load to the flow group's
	// leader from the client side, and lane C2 is the successor that replaces it. A
	// caller treats this as a condition to report, never as a permanent shape to
	// design around.
	//
	// An error carrying it also wraps the underlying raft sentinel, so a non-voter
	// catching up as a learner stays distinguishable from a plain non-leader
	// without matching on message text.
	ErrNotLeader = errors.New("ledger: this node does not lead the flow")
)

// translateRaftError maps raft's leadership refusals onto this package's own, so a
// caller matches one sentinel instead of four library errors it did not import.
// Anything else is passed through unchanged rather than flattened.
//
// BOTH ERRORS ARE WRAPPED, not one wrapped and one formatted. The underlying raft
// sentinel stays reachable through errors.Is, because these refusals are not
// interchangeable to a caller that cares: a non-voter has been added as a learner
// and is catching up, which calls for a different response than simply not leading.
// Formatting the cause with %v would leave that distinction visible only in the
// message text, and no caller should have to match on a string this package is free
// to reword.
func translateRaftError(err error) error {
	switch {
	case errors.Is(err, raft.ErrNotLeader),
		errors.Is(err, raft.ErrLeadershipLost),
		errors.Is(err, raft.ErrLeadershipTransferInProgress),
		errors.Is(err, raft.ErrNotVoter):
		return fmt.Errorf("%w: %w", ErrNotLeader, err)
	default:
		return err
	}
}

// Config describes one flow's ledger.
type Config struct {
	// Flow names the flow this ledger replicates. It doubles as the transport
	// group id, which is how N flows share one listener.
	Flow string
	// LocalID is this node's raft server id. It must be stable across restarts.
	LocalID string
	// Mux is the shared listener every group on this node is reached through. It
	// is required even for a one-voter ledger, because raft demands a Transport.
	Mux *transport.Mux
	// Dir is where this ledger keeps its log, its stable state and its snapshots.
	//
	// A NON-EMPTY Dir is the production selection: one raft-boltdb/v2 store serving
	// as both the log store and the stable store, beside a file snapshot store.
	//
	// An EMPTY Dir selects the zero-config dev mode, which holds all three in
	// memory. hashicorp's own documentation says its in-memory store should NOT
	// EVER be used for production, so that selection is deliberate and narrow: it
	// is a documented mode, not a fallback. Nothing degrades from bolt to memory on
	// error — a Dir that cannot be opened is an error out of Open, never a silent
	// substitution into the dev stores.
	Dir string
	// Bootstrap asks Open to elect a single-node cluster when this node has no
	// existing state. It is ignored when state already exists.
	Bootstrap bool
	// Logger receives raft's logs and this ledger's own. A nil Logger discards.
	Logger hclog.Logger
	// ReadTimeout bounds how long a linearizable read waits for this node's state
	// machine to catch up with the commit index it observed. Zero means the
	// caller's context is the only bound.
	ReadTimeout time.Duration

	// tuning is applied to the raft config last, after the defaults and this
	// ledger's own settings.
	//
	// It is UNEXPORTED DELIBERATELY. Production callers get raft's ruled compaction
	// triggers and cannot override them, which is the property this package is
	// gated on; tests in this package lower an election or snapshot threshold
	// through it to observe in seconds what defaults would take minutes to show.
	tuning func(*raft.Config)
}

// validate refuses a config Open cannot act on, naming the field rather than
// failing later inside raft.
func (c Config) validate() error {
	switch {
	case c.Flow == "":
		return fmt.Errorf("ledger: Config.Flow names the flow and its transport group: %w", ErrConfigIncomplete)
	case c.LocalID == "":
		return fmt.Errorf("ledger: Config.LocalID is this node's raft server id: %w", ErrConfigIncomplete)
	case c.Mux == nil:
		return fmt.Errorf("ledger: Config.Mux carries the shared listener raft dials through: %w", ErrConfigIncomplete)
	}

	return nil
}

func (c Config) logger() hclog.Logger {
	if c.Logger == nil {
		return hclog.NewNullLogger()
	}

	return c.Logger
}

// Ledger is one flow's raft-replicated recovery ledger.
type Ledger struct {
	cfg    Config
	logger hclog.Logger

	fsm   *fsm
	raft  *raft.Raft
	group *transport.Group
	bolt  *boltdb.BoltStore

	notify chan bool
	done   chan struct{}
	drain  sync.WaitGroup

	// establish is what a leadership acquisition triggers. It is appendEpoch in
	// production; it is a field so a test can drive the drain loop against a
	// deliberately slow append without standing up a raft cluster to slow down.
	establish func()

	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool
}

// stores is the log, stable and snapshot triple Open hands raft, plus the bolt
// handle when there is one to close later.
type stores struct {
	logs   raft.LogStore
	stable raft.StableStore
	snaps  raft.SnapshotStore
	bolt   *boltdb.BoltStore
}

// Open brings up one flow's ledger over the shared transport.
func Open(cfg Config) (*Ledger, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	l := &Ledger{
		cfg:    cfg,
		logger: cfg.logger(),
		fsm:    newFSM(),
		notify: make(chan bool, leadershipNotifyBuffer),
		done:   make(chan struct{}),
	}
	if err := l.start(); err != nil {
		return nil, err
	}

	return l, nil
}

// start binds the group, opens the stores and brings raft up. Everything it opened
// before a failure is released before it reports, so a failed Open leaves no group
// bound and no bolt file held.
func (l *Ledger) start() error {
	group, err := l.cfg.Mux.Bind(transport.GroupID(l.cfg.Flow))
	if err != nil {
		return fmt.Errorf("ledger: binding the transport group for flow %q: %w", l.cfg.Flow, err)
	}
	l.group = group

	opened, err := openStores(l.cfg, l.logger)
	if err != nil {
		_ = group.Close()

		return err
	}
	l.bolt = opened.bolt

	if err := l.startRaft(opened); err != nil {
		l.releasePartial()

		return err
	}
	l.startLeadershipDrain()

	return nil
}

// startLeadershipDrain begins draining raft's leadership notifications.
//
// EVERY LEADERSHIP TERM GETS ONE EPOCH ENTRY, appended on acquisition — not once
// per ledger and not once per node. raft dispatches a fresh no-op at the start of
// every term and that no-op never reaches a state machine, so a ledger that
// established only its first term would stall the first read of every term after
// it. A node that leads, loses the term and leads again needs one for each.
func (l *Ledger) startLeadershipDrain() {
	if l.establish == nil {
		l.establish = l.appendEpoch
	}
	l.drain.Add(1)

	go l.drainLeadership()
}

// drainLeadership receives leadership transitions for the life of the ledger.
//
// THIS LOOP MUST NEVER PARK. raft's send on NotifyCh is a blocking send guarded
// only by raft's own shutdown channel, so a drain that stops receiving wedges
// runLeader once the buffer fills — and raft goes on reporting a healthy leader
// whose every operation times out enqueuing.
func (l *Ledger) drainLeadership() {
	defer l.drain.Done()

	for {
		select {
		case leading := <-l.notify:
			if leading {
				l.establishTerm()
			}
		case <-l.done:
			return
		}
	}
}

// establishTerm starts the term's epoch append on ITS OWN goroutine and returns
// immediately.
//
// The append is a replicated write whose latency is unbounded from this loop's
// point of view, so awaiting it inline would park the drain for its duration. A
// bounded timeout on the append would only narrow that window rather than remove
// it, which is why the append is off the drain path regardless.
func (l *Ledger) establishTerm() {
	l.drain.Add(1)

	go func() {
		defer l.drain.Done()

		l.establish()
	}()
}

// appendEpoch replicates this term's epoch entry.
//
// A failure is reported and the term is left un-established; it is NOT retried in
// a loop. A leader that cannot append has either lost leadership or lost its log,
// and both resolve at the next election — a retry loop would spin against a
// condition it cannot fix. Reads in that window fail with ErrReadTimeout naming the
// index they waited for, which is the honest signal.
func (l *Ledger) appendEpoch() {
	data, err := EncodeEntry(Entry{Kind: KindEpoch})
	if err != nil {
		l.logger.Error("ledger: encoding the leadership epoch entry failed", "flow", l.cfg.Flow, "error", err)

		return
	}
	if err := l.raft.Apply(data, 0).Error(); err != nil {
		l.logger.Error(
			"ledger: appending the leadership epoch entry failed; this term is not established"+
				" and reads on it report a timeout until the next election",
			"flow", l.cfg.Flow, "error", err)
	}
}

// openStores selects this ledger's storage. The selection is decided by Config.Dir
// and nothing else; there is no path from a bolt failure to the in-memory stores.
func openStores(cfg Config, logger hclog.Logger) (stores, error) {
	if cfg.Dir == "" {
		return stores{
			logs:   raft.NewInmemStore(),
			stable: raft.NewInmemStore(),
			snaps:  raft.NewInmemSnapshotStore(),
		}, nil
	}

	bolt, err := boltdb.New(boltdb.Options{Path: filepath.Join(cfg.Dir, boltFileName)})
	if err != nil {
		return stores{}, fmt.Errorf("ledger: opening the bolt store for flow %q under %q: %w", cfg.Flow, cfg.Dir, err)
	}
	snaps, err := raft.NewFileSnapshotStoreWithLogger(cfg.Dir, snapshotsRetained, logger)
	if err != nil {
		_ = bolt.Close()

		return stores{}, fmt.Errorf(
			"ledger: opening the snapshot store for flow %q under %q: %w", cfg.Flow, cfg.Dir, err)
	}

	return stores{logs: bolt, stable: bolt, snaps: snaps, bolt: bolt}, nil
}

// startRaft constructs the raft instance and bootstraps a single-voter cluster when
// asked and when this node has no state of its own.
func (l *Ledger) startRaft(opened stores) error {
	r, err := raft.NewRaft(l.raftConfig(), l.fsm, opened.logs, opened.stable, opened.snaps, l.group.Transport())
	if err != nil {
		return fmt.Errorf("ledger: constructing raft for flow %q: %w", l.cfg.Flow, err)
	}
	l.raft = r

	if !l.cfg.Bootstrap {
		return nil
	}
	existing, err := raft.HasExistingState(opened.logs, opened.stable, opened.snaps)
	if err != nil {
		return fmt.Errorf("ledger: reading existing state for flow %q: %w", l.cfg.Flow, err)
	}
	if existing {
		return nil
	}

	return l.bootstrapSelf()
}

// bootstrapSelf elects a one-voter cluster naming only this node. Zero peers is the
// normal local case rather than a special one: the same code path runs, and
// VerifyLeader short-circuits without network or disk at quorum size one.
func (l *Ledger) bootstrapSelf() error {
	configuration := raft.Configuration{Servers: []raft.Server{{
		ID:      raft.ServerID(l.cfg.LocalID),
		Address: raft.ServerAddress(l.cfg.Mux.Addr().String()),
	}}}
	if err := l.raft.BootstrapCluster(configuration).Error(); err != nil {
		return fmt.Errorf("ledger: bootstrapping flow %q as a single voter: %w", l.cfg.Flow, err)
	}

	return nil
}

// raftConfig starts from raft's defaults and sets ONLY this ledger's identity, its
// logger and its leadership notification channel.
//
// It deliberately does not touch SnapshotInterval, SnapshotThreshold, TrailingLogs
// or ShutdownOnRemove. The first three defaults are the ruled compaction triggers,
// and the fourth is handled rather than flipped: Close frees the transport binding
// unconditionally, because a self-shutdown on removal never does.
func (l *Ledger) raftConfig() *raft.Config {
	cfg := raft.DefaultConfig()
	cfg.LocalID = raft.ServerID(l.cfg.LocalID)
	cfg.Logger = l.logger
	cfg.NotifyCh = l.notify
	if l.cfg.tuning != nil {
		l.cfg.tuning(cfg)
	}

	return cfg
}

// releasePartial unwinds a half-built ledger. It is the failure counterpart of
// Close, for the window before the caller ever holds the Ledger.
func (l *Ledger) releasePartial() {
	if l.raft != nil {
		_ = l.raft.Shutdown().Error()
	}
	if l.group != nil {
		_ = l.group.Close()
	}
	if l.bolt != nil {
		_ = l.bolt.Close()
	}
}

// Close shuts this ledger down and frees everything it holds. It is idempotent:
// a second and third call return the first call's result without repeating it.
//
// The ledger is marked closed BEFORE anything is torn down, so no call can find a
// half-live ledger and reach a raft instance that is already stopping.
func (l *Ledger) Close() error {
	l.closeOnce.Do(func() {
		l.closed.Store(true)
		l.closeErr = l.shutdown()
	})

	return l.closeErr
}

// shutdown runs the teardown in this order and no other.
//
// STEP 2 IS UNCONDITIONAL AND THAT IS THE WHOLE POINT. raft's ShutdownOnRemove
// defaults to true, and when a node is removed from its own configuration raft
// calls Shutdown itself and DISCARDS the future. Only a DRAINED shutdown future
// closes the transport, so a self-shutdown on removal never unbinds the group and
// the flow's id stays held forever. Close therefore cannot rely on raft having
// released anything, and Group.Close is safe to call in every ordering.
//
// The drain goroutine is stopped only AFTER raft's shutdown future is drained:
// raft's leadership-loss notification during step-down is a BLOCKING send, so a
// drain that exited first would wedge the very shutdown it is part of.
func (l *Ledger) shutdown() error {
	var errs []error
	if l.raft != nil {
		if err := l.raft.Shutdown().Error(); err != nil {
			errs = append(errs, fmt.Errorf("ledger: draining raft's shutdown for flow %q: %w", l.cfg.Flow, err))
		}
	}
	close(l.done)
	l.drain.Wait()

	if l.group != nil {
		if err := l.group.Close(); err != nil {
			errs = append(errs, fmt.Errorf("ledger: closing the transport group for flow %q: %w", l.cfg.Flow, err))
		}
	}
	if l.bolt != nil {
		if err := l.bolt.Close(); err != nil {
			errs = append(errs, fmt.Errorf("ledger: closing the bolt store for flow %q: %w", l.cfg.Flow, err))
		}
	}

	return errors.Join(errs...)
}

// Raft exposes the underlying instance for membership work that belongs to the
// lane owning it.
func (l *Ledger) Raft() *raft.Raft { return l.raft }

// Flow reports the flow this ledger replicates.
func (l *Ledger) Flow() string { return l.cfg.Flow }

// Append replicates one entry, waits for this node's state machine to apply it, and
// returns THE JOURNAL INDEX the entry landed at.
//
// The index is part of the contract rather than a convenience: a consumer recording
// a checkpoint references a journal position, and the index is free here because
// raft's ApplyFuture embeds IndexFuture — Index() is defined once Error() has
// returned on the same already-bound future.
//
// It holds no lock of this package's across the raft append: concurrent appends are
// the throughput lever here, and serializing them behind a mutex would cost roughly
// an order of magnitude.
func (l *Ledger) Append(ctx context.Context, entry Entry) (uint64, error) {
	if ctx == nil {
		return 0, ErrNilContext
	}
	if l.closed.Load() {
		return 0, ErrClosed
	}

	data, err := EncodeEntry(entry)
	if err != nil {
		return 0, err
	}
	future := l.raft.Apply(data, enqueueTimeout(ctx))
	if err := future.Error(); err != nil {
		return 0, fmt.Errorf("ledger: appending to flow %q: %w", l.cfg.Flow, translateRaftError(err))
	}
	if applied, ok := future.Response().(error); ok && applied != nil {
		return 0, applied
	}

	return future.Index(), nil
}

// Get reads one path linearizably: it proves this node still leads, then waits for
// its own state machine to catch up with what was committed at the moment of that
// proof.
//
// IT IS LEADER-ONLY, AND THAT IS AN INTERIM. A non-leader is refused with
// ErrNotLeader rather than served a value it cannot prove current; the settled
// design forwards the read to the flow group's leader from the client side, and
// lane C2 is the successor that replaces this refusal.
func (l *Ledger) Get(ctx context.Context, path string) (Entry, bool, error) {
	if ctx == nil {
		return Entry{}, false, ErrNilContext
	}
	if l.closed.Load() {
		return Entry{}, false, ErrClosed
	}

	if err := l.barrier(ctx); err != nil {
		return Entry{}, false, err
	}
	entry, ok := l.fsm.get(path)

	return entry, ok, nil
}

// enqueueTimeout turns the caller's deadline into the bound raft's Apply takes on
// enqueueing. Zero means no bound, which is raft's own convention.
func enqueueTimeout(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	if remaining := time.Until(deadline); remaining > 0 {
		return remaining
	}

	return time.Nanosecond
}
