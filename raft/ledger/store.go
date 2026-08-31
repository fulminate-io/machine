// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ledger

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
)

// Store is the ledger's implementation of the machine heap-storage seam. It is a
// replicated store, and its guarantees are ITS OWN rather than the seam's — the
// interface promises only that a method reports failure and honors its context.
//
// UPDATE IS NOT ATOMIC HERE. The in-memory store holds one lock across the whole
// read-modify-write, so a heap update through it is atomic under concurrent nodes.
// This implementation cannot do that and does not pretend to: it reads through the
// linearizable barrier, applies fn AT THE CALLER, and replicates the result as an
// ordinary write. Two concurrent Updates to the same path can therefore both read
// the same prior value and the later append wins, losing the earlier one. Funnelling
// fn to the leader to compute, and replicating the closure itself as a command, were
// both considered and rejected: the first makes every update a round trip carrying
// caller code, and the second requires every peer to run a build that can execute
// that closure deterministically.
//
// WHAT MAKES THE WEAKER GUARANTEE SAFE IS THE SINGLE-WRITER-PER-DATUM PREMISE: a
// flow's heap path is written by one owner, so the lost-update window above needs
// two writers to the same datum, which the model does not produce. A caller that
// breaks that premise is depending on atomicity this store does not provide, and
// should not read the in-memory store's guarantee across to here.
//
// EVERY METHOD IS LEADER-ONLY, AND THAT IS AN INTERIM RATHER THAN THE SETTLED
// CONTRACT. raft refuses both an append and a leadership verification on a node that
// is not the leader, so Load, Save and Update on a follower return an error wrapping
// ErrNotLeader rather than serving a value this node cannot prove current. The
// settled design forwards a non-leader Save, Update and linearizable Load to the
// flow group's leader from the client side, and that successor work is lane C2. A
// flow author should therefore treat the refusal as a condition to report, not as a
// permanent shape to design around.
//
// Nothing here holds a lock across a raft append. Concurrent appends are the whole
// throughput lever on a replicated log, and serializing them behind a mutex would
// cap a flow at the serial-writer ceiling.
type Store struct {
	ledger *Ledger
}

// Store returns the machine heap-storage view of this ledger.
func (l *Ledger) Store() *Store {
	return &Store{ledger: l}
}

// Load reads one path linearizably and decodes the stored value.
//
// A non-nil error returns no value and reports NOT PRESENT: the store did not
// answer, which is a different outcome from answering that the path is absent. A
// caller must not read the false as "no such value" when the error is non-nil.
func (s *Store) Load(ctx context.Context, path string) (any, bool, error) {
	if ctx == nil {
		return nil, false, ErrNilContext
	}

	entry, ok, err := s.ledger.Get(ctx, path)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	value, err := decodeValue(entry.Value)
	if err != nil {
		return nil, false, err
	}

	return value, true, nil
}

// Save replicates one value and returns once this node has applied it.
func (s *Store) Save(ctx context.Context, path string, value any) error {
	if ctx == nil {
		return ErrNilContext
	}

	data, err := encodeValue(value)
	if err != nil {
		return err
	}

	// The journal index Append reports is not part of this seam's vocabulary; a
	// caller that needs it reaches for Ledger.Append directly.
	if _, err := s.ledger.Append(ctx, Entry{Kind: KindSet, Path: path, Value: data}); err != nil {
		return err
	}

	return nil
}

// Update reads through the barrier, computes fn at the caller and replicates the
// result. See the type's contract: this is NOT atomic, and it rests on the
// single-writer-per-datum premise.
func (s *Store) Update(ctx context.Context, path string, fn func(any) any) (any, error) {
	if ctx == nil {
		return nil, ErrNilContext
	}

	current, _, err := s.Load(ctx, path)
	if err != nil {
		return nil, err
	}
	updated := fn(current)
	if err := s.Save(ctx, path, updated); err != nil {
		return nil, err
	}

	return updated, nil
}

// encodeValue erases a heap value to the bytes the journal replicates.
//
// A value type outside gob's built-ins must be registered with encoding/gob before
// it crosses this boundary — measured: a string and an int round-trip inside an
// interface, while a map or a named struct is refused with "type not registered for
// interface". gob's own error at encode time is the enforcement, and it fails the
// WRITER loudly rather than letting an undecodable value reach the journal.
func encodeValue(value any) ([]byte, error) {
	var sink bytes.Buffer
	if err := gob.NewEncoder(&sink).Encode(&value); err != nil {
		return nil, fmt.Errorf("ledger: encoding a heap value failed: %w", err)
	}

	return sink.Bytes(), nil
}

// decodeValue rebuilds a heap value at the READING node, which is why an
// unregistered type fails one reader instead of diverging every peer.
func decodeValue(data []byte) (any, error) {
	var value any
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&value); err != nil {
		return nil, fmt.Errorf("ledger: decoding a heap value failed: %w", err)
	}

	return value, nil
}
