// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ledger

import (
	"context"
	"errors"
	"fmt"
)

// Read refusals, declared beside the code that returns them.
var (
	// ErrReadTimeout reports that the state machine did not reach the index a read
	// was waiting for before the caller's context expired. It names the target and
	// the index actually observed, because an unbounded block is indistinguishable
	// from a hang and tells an operator nothing.
	ErrReadTimeout = errors.New("ledger: timed out waiting for the applied index to reach a committed read")
)

// barrier is the linearizable read this package BUILDS, because hashicorp/raft
// v1.7.3 exposes no ReadIndex to borrow one from.
//
// Two steps, and both are necessary. VerifyLeader proves against a quorum that this
// node still holds its term, so the value about to be read is not a deposed
// leader's. The commit index observed AFTER that proof is then waited for on this
// node's OWN state machine, because raft's commit index counts entries this state
// machine may not have applied yet — reading at the commit index alone returns a
// value that is stale by exactly those entries.
//
// It never acquires raft's Barrier: that is a full replicated write, and a read
// path must not append to the log to answer a question.
func (l *Ledger) barrier(ctx context.Context) error {
	if err := l.raft.VerifyLeader().Error(); err != nil {
		return fmt.Errorf("ledger: verifying leadership for flow %q: %w", l.cfg.Flow, translateRaftError(err))
	}
	target := l.raft.CommitIndex()

	if l.cfg.ReadTimeout <= 0 {
		return l.fsm.waitApplied(ctx, target)
	}

	// The ReadTimeout is materialized ONLY on the branch that actually waits. A
	// read whose state machine is already caught up is the common case, and giving
	// it a per-call deadline context would allocate on every one of them.
	applied, _, poison := l.fsm.observe()
	switch {
	case poison != nil:
		return poison
	case applied >= target:
		return nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, l.cfg.ReadTimeout)
	defer cancel()

	return l.fsm.waitApplied(waitCtx, target)
}

// waitApplied blocks until the state machine has applied everything up to target,
// the ledger is poisoned, or ctx expires.
//
// THE ORDERING HERE IS LOAD-BEARING. The tracked index is compared FIRST and the
// context is touched only on the branch that actually waits. Materializing a
// per-call deadline before the comparison costs four allocations on a path a
// satisfied read takes every time — measured at 0 objects and ~3.6ns this way
// against 4 objects (272B) and ~164ns the other way. A read on a local group
// reaches this comparison and returns, so that cost would be pure overhead on the
// barrier it is part of.
//
// A nil ctx is a programming error rather than a state to tolerate, and it is
// refused with ErrNilContext by the Store methods above this, before any raft call.
func (f *fsm) waitApplied(ctx context.Context, target uint64) error {
	for {
		applied, wake, poison := f.observe()
		if poison != nil {
			return poison
		}
		if applied >= target {
			return nil
		}

		select {
		case <-wake:
		case <-ctx.Done():
			return fmt.Errorf("ledger: waited for applied index %d, observed %d: %w", target, applied, ErrReadTimeout)
		}
	}
}

// observe reads the tracked index, the current wake channel and the poison as one
// consistent triple. Taking the channel under the same lock is what closes the race
// where a waiter reads a stale index, then parks on a channel that was already
// closed and replaced.
func (f *fsm) observe() (uint64, chan struct{}, error) {
	f.mutex.RLock()
	defer f.mutex.RUnlock()

	return f.applied, f.wake, f.poison
}
