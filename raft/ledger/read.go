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
// Four steps: prove leadership, await this term's ESTABLISHMENT, take the target as
// the greater of the commit index and this term's epoch position, then wait for the
// state machine to reach it.
//
// STEP TWO IS NOT OPTIONAL AND IT IS NOT A LIVENESS NICETY. raft advances a leader's
// commit index only once something commits in its CURRENT term, so a leader that has
// just replayed a log from disk reports a commit index far behind its last index —
// zero, in the measured case, while the log held committed entries. A barrier that
// went straight to the commit index there took a target an EMPTY state machine
// already satisfied, returned instantly, and answered ABSENT for data that was
// committed and on disk. That is a data-integrity defect, not a slow read.
//
// THE BARRIER THEREFORE MUST NOT FALL THROUGH TO THE COMMIT INDEX WHEN THE TERM IS
// UNESTABLISHED: falling through IS the defective state. An unestablished term is
// WAITED on, bounded by the caller's context, and reported as a wrapped
// ErrReadTimeout naming the term.
//
// The per-term epoch entry is the fence because it commits in the CURRENT term, so
// its apply implies every earlier entry in the log has applied too — exactly the
// marker a fresh leader lacks. Taking the target from LastIndex would also close the
// hole, and was refused: it makes a read wait on entries that are not yet committed
// and that a leadership loss can leave uncommitted forever, converting a fast correct
// read into a timeout during the very flap this barrier exists to survive.
//
// It never acquires raft's Barrier: that is a full replicated write, and a read path
// must not append to the log to answer a question.
func (l *Ledger) barrier(ctx context.Context) error {
	if err := l.raft.VerifyLeader().Error(); err != nil {
		return fmt.Errorf("ledger: verifying leadership for flow %q: %w", l.cfg.Flow, translateRaftError(err))
	}
	if satisfied, err := l.readSatisfied(); satisfied || err != nil {
		return err
	}

	waitCtx, cancel := l.readContext(ctx)
	defer cancel()

	epoch, err := l.awaitEstablishment(waitCtx)
	if err != nil {
		return err
	}

	return l.fsm.waitApplied(waitCtx, max(l.raft.CommitIndex(), epoch))
}

// readSatisfied reports whether this read needs no waiting at all: the term is
// established and the state machine has already reached the target.
//
// It is separated so the common case touches NO context. A read on a stable leader
// reaches this and returns, and materializing a per-call deadline ahead of it would
// allocate on every one of them.
func (l *Ledger) readSatisfied() (bool, error) {
	epoch, established := l.establishment(l.raft.CurrentTerm())
	if !established {
		return false, nil
	}

	applied, _, poison := l.fsm.observe()
	if poison != nil {
		return false, poison
	}

	return applied >= max(l.raft.CommitIndex(), epoch), nil
}

// awaitEstablishment blocks until this node has recorded an epoch entry for the term
// it currently holds, and returns the index that entry landed at.
//
// It re-reads the term on every pass: a flap mid-wait means the term this read must
// be fenced against has changed, and the establishment recorded for the old one says
// nothing about the new.
//
// THE WAKE CHANNEL IS TAKEN BEFORE THE ESTABLISHMENT IS CHECKED, AND THAT ORDER IS
// THE WHOLE CORRECTNESS ARGUMENT. The two live under different locks — the record
// under epochMu, the broadcast under the tracker's — so checking first and
// subscribing second leaves a gap: a record that lands entirely between them is
// missed by the check that already ran AND closes a channel this reader has not
// taken yet, so the reader goes on to park on the fresh replacement and is never
// woken. With no ReadTimeout and a deadline-less caller that is an indefinite block
// on the published read path, in exactly the fresh-leader window.
//
// Subscribing first removes the gap in both directions: a record BEFORE the channel
// read is seen by the re-check below, and one AFTER it closes the very channel this
// reader is holding. This mirrors why waitApplied is safe — observe() takes the index
// and the channel together under one lock — except that establishment cannot be read
// under the tracker's lock, so the ordering does the work the shared lock does there.
func (l *Ledger) awaitEstablishment(ctx context.Context) (uint64, error) {
	for {
		_, wake, poison := l.fsm.observe()
		if poison != nil {
			return 0, poison
		}
		if l.establishmentProbe != nil {
			l.establishmentProbe()
		}

		term := l.raft.CurrentTerm()
		if epoch, established := l.establishment(term); established {
			return epoch, nil
		}

		select {
		case <-wake:
		case <-ctx.Done():
			return 0, fmt.Errorf("ledger: term %d is not established, so no committed index can be trusted yet: %w",
				term, ErrReadTimeout)
		}
	}
}

// readContext applies Config.ReadTimeout, when there is one, to the branch that
// actually waits.
func (l *Ledger) readContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if l.cfg.ReadTimeout <= 0 {
		return ctx, func() {}
	}

	return context.WithTimeout(ctx, l.cfg.ReadTimeout)
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
