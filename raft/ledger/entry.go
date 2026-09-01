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
	// ErrClaimHeld reports a datum another worker already claimed. It is the answer
	// to THIS operation rather than a transient condition, so a caller that receives
	// it has lost the race and must not retry: no retry repairs it.
	ErrClaimHeld = errors.New("ledger: the datum is already claimed by another worker")
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
	// KindClaim names Entry.Path's claimant in Entry.Value. It is NOT an assignment:
	// the FIRST claim of a datum wins and every later one by a different owner is
	// refused with ErrClaimHeld, which is what makes recovery ownership settle
	// through log order rather than through last-write-wins.
	KindClaim
	// KindRetire drops Entry.Path's checkpoint AND its claim together. IT NAMES BOTH
	// KEYS: Entry.Path is the datum's checkpoint path and Entry.Value carries its
	// claim path, because the two live in disjoint spaces this package cannot derive
	// between — the path vocabulary belongs to raft/checkpoint, which imports this
	// one. An entry naming only the checkpoint path deletes NO claim, which is a
	// claim that outlives every datum it was ever taken for.
	// Retiring a datum that was never checkpointed is not an error: the
	// retirement is driven by node completion, which fires whether or not a flow
	// declared a checkpoint, and the post-state is identical either way.
	KindRetire
	// KindRetireClaim drops Entry.Path's CLAIM and leaves every checkpoint alone. It
	// carries no value. It exists because a worker that dies while HOLDING a recovery
	// claim strands the datum: first-writer-wins refuses every later claimant, and the
	// state machine has no liveness view with which to judge that the holder is gone.
	// The flow's LEADER appends it on observing the holder depart, so the claim is
	// RETIRED rather than stolen and first-writer-wins is untouched.
	//
	// Retiring a claim nobody holds is a deliberate no-op, on the same reasoning
	// KindRetire's doc gives: the leader re-observes a departure across a rebuilt
	// membership cursor, so a repeat must reach the same post-state rather than fail.
	KindRetireClaim
)

// declared reports whether this build knows how to interpret a kind.
//
// THIS IS THE ONE PLACE THE DECLARED SET IS ENUMERATED. Both arrival paths consult
// it — DecodeEntry for an entry arriving by log, and the state machine's restore for
// entries arriving by snapshot — so a kind cannot be admitted on one path while the
// other refuses it.
func (k Kind) declared() bool {
	return k == KindSet || k == KindEpoch || k == KindClaim || k == KindRetire ||
		k == KindRetireClaim
}

// forwards reports whether Append sends this kind to the leader rather than
// appending it locally.
//
// THIS IS THE ONE PLACE THE FORWARDING SET IS ENUMERATED, for the reason
// errCode.retryable() gives about its own set: written inline at the call site, a
// new kind becomes an invisible consequence of a comparison rather than a one-line
// decision in a named place — and a kind that fails to forward refuses on every
// follower silently, with no gate that would notice.
//
// TWO KINDS ARE EXCLUDED AND EACH FOR ITS OWN REASON. KindEpoch is LEADER-INTERNAL:
// appendEpoch applies it through raft directly and it never reaches Append.
// KindRetireClaim is LEADER-ORIGINATED: only the node holding the flow's authoritative
// membership may judge that a claim holder departed, so a non-leader append of it is
// refused with ErrNotLeader rather than forwarded — which is what stops a DEMOTED
// leader from retiring a live worker's claim off a membership view that has gone
// stale. The remaining three are operations a non-leader worker genuinely originates.
func (k Kind) forwards() bool {
	return k == KindSet || k == KindClaim || k == KindRetire
}

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

	if !entry.Kind.declared() {
		return Entry{}, fmt.Errorf("ledger: entry for %q names kind %d, which this build does not declare: %w",
			entry.Path, uint8(entry.Kind), ErrPoisonedJournal)
	}

	return entry, nil
}
