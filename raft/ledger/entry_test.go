package ledger

import (
	"errors"
	"strings"
	"testing"
)

func TestUnknownEntryKindPoisonsTheLedgerLoudly(t *testing.T) {
	// CONTROL: a declared kind round-trips through the same encoder and decoder the
	// refusals below use. Without it, a DecodeEntry that refused everything would
	// be indistinguishable from one that discriminates on the kind.
	encoded, err := EncodeEntry(Entry{Kind: KindSet, Path: "heap/alpha", Value: []byte("payload")})
	if err != nil {
		t.Fatalf("encoding a declared kind: %v", err)
	}
	got, err := DecodeEntry(encoded)
	if err != nil {
		t.Fatalf("CONTROL FAILED: a declared kind was refused: %v", err)
	}
	if got.Kind != KindSet || got.Path != "heap/alpha" || string(got.Value) != "payload" {
		t.Fatalf("round trip gave %+v, want a KindSet entry for heap/alpha carrying payload", got)
	}
	// The epoch kind is declared too, so the check below is about the SET and not
	// about KindSet being the only value that survives.
	if _, err := DecodeEntry(mustEncode(t, Entry{Kind: KindEpoch})); err != nil {
		t.Fatalf("CONTROL FAILED: the epoch kind was refused: %v", err)
	}

	// An undeclared kind is refused, and the refusal names the kind so an operator
	// reading the log learns which vocabulary the peer was missing.
	poisoned, err := DecodeEntry(mustEncode(t, Entry{Kind: Kind(9), Path: "heap/alpha"}))
	if !errors.Is(err, ErrPoisonedJournal) {
		t.Fatalf("decoding kind 9 gave entry %+v and error %v, want a wrapped ErrPoisonedJournal", poisoned, err)
	}
	if poisoned.Kind != 0 || poisoned.Path != "" {
		t.Fatalf("a refused entry came back populated as %+v; a poisoned decode returns no usable entry", poisoned)
	}
	if !strings.Contains(err.Error(), "9") {
		t.Fatalf("the refusal %q does not name the offending kind", err)
	}

	// The zero kind is refused on the same terms. Zeroed or truncated bytes name no
	// kind, and reading them as the first declared arm would apply a write nobody
	// journaled.
	if _, err := DecodeEntry(mustEncode(t, Entry{Path: "heap/alpha"})); !errors.Is(err, ErrPoisonedJournal) {
		t.Fatalf("decoding the zero kind gave %v, want a wrapped ErrPoisonedJournal", err)
	}

	// Bytes that are not an entry at all fail as a decode error rather than as a
	// poisoned kind, so the two failures stay tellable apart in a log.
	if _, err := DecodeEntry([]byte("not a gob stream")); err == nil || errors.Is(err, ErrPoisonedJournal) {
		t.Fatalf("decoding garbage gave %v, want a decode failure that is not ErrPoisonedJournal", err)
	}
}

func mustEncode(t *testing.T, entry Entry) []byte {
	t.Helper()
	encoded, err := EncodeEntry(entry)
	if err != nil {
		t.Fatalf("encoding %+v: %v", entry, err)
	}

	return encoded
}
