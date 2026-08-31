// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ledger

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
)

// Journal refusals, declared beside the code that returns them.
var (
	// ErrPoisonedJournal reports a journal entry this build cannot interpret. It is
	// returned rather than skipped: a peer that ignored an entry it did not
	// understand would apply a different sequence than its neighbors and diverge.
	ErrPoisonedJournal = errors.New("ledger: journal entry carries an unknown kind")
)

// Kind tags what a journal entry asks the state machine to do. The zero value is
// deliberately not a member: an entry decoded from zeroed or truncated bytes names
// no kind and is refused instead of silently reading as the first arm.
type Kind uint8

const (
	// KindSet stores Entry.Value at Entry.Path.
	KindSet Kind = iota + 1
	// KindEpoch carries no data. It exists so a leadership term commits something
	// the state machine applies, which is what lets the first read of that term
	// converge; see the package comment.
	KindEpoch
)

// Entry is one journal record. It is the replicated vocabulary, so every field
// here is part of the contract between peers.
//
// Value IS OPAQUE BYTES AND NOT any, and that is the load-bearing choice. A state
// machine storing a decoded any would need every peer to have the concrete type
// registered at APPLY time, and a peer that did not would diverge from its
// neighbors. Bytes move decoding to the READING node, so an unregistered type
// fails one reader loudly instead of poisoning replicated state — and the apply
// path needs no type registry at all, which is what lets the encoding be retargeted
// later without touching the state machine.
type Entry struct {
	Kind  Kind
	Path  string
	Value []byte
}

// EncodeEntry renders an entry as the bytes raft replicates.
func EncodeEntry(entry Entry) ([]byte, error) {
	var sink bytes.Buffer
	if err := gob.NewEncoder(&sink).Encode(entry); err != nil {
		return nil, fmt.Errorf("ledger: encoding a %v entry for %q failed: %w", entry.Kind, entry.Path, err)
	}

	return sink.Bytes(), nil
}

// DecodeEntry rebuilds an entry from replicated bytes, refusing any kind outside
// the declared set with a wrapped ErrPoisonedJournal. Bad input errors here; it is
// never defaulted to a known kind and never skipped.
func DecodeEntry(data []byte) (Entry, error) {
	var entry Entry
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&entry); err != nil {
		return Entry{}, fmt.Errorf("ledger: decoding a journal entry failed: %w", err)
	}

	if entry.Kind != KindSet && entry.Kind != KindEpoch {
		return Entry{}, fmt.Errorf("ledger: entry for %q names kind %d, which this build does not declare: %w",
			entry.Path, uint8(entry.Kind), ErrPoisonedJournal)
	}

	return entry, nil
}
