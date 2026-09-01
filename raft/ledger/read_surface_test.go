// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ledger

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// pathsOf reports the paths of an enumeration's entries, which is what the set
// assertions below compare.
func pathsOf(entries []Entry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}

	return paths
}

// seedRecoveryState writes three checkpoints and two claims that SHARE the enumerated
// prefix.
//
// THE SHARED PREFIX IS THE WHOLE POINT. Were the claims written under a prefix the
// enumeration does not ask for, "excludes the claims" would be satisfied by prefix
// matching alone and would say nothing about which state the implementation reads.
// Here an implementation walking both maps returns five paths where three are
// correct.
func seedRecoveryState(t *testing.T, l *Ledger, ctx context.Context) []string {
	t.Helper()

	checkpoints := []string{"state/checkpoint/datum-1", "state/checkpoint/datum-2", "state/checkpoint/datum-3"}
	for _, path := range checkpoints {
		if _, err := l.Append(ctx, Entry{Kind: KindSet, Path: path, Value: []byte("progress")}); err != nil {
			t.Fatalf("checkpointing %s: %v", path, err)
		}
	}
	for _, datum := range []string{"state/claim/datum-1", "state/claim/datum-2"} {
		if _, err := l.Append(ctx, Entry{Kind: KindClaim, Path: datum, Value: []byte("worker-a")}); err != nil {
			t.Fatalf("claiming %s: %v", datum, err)
		}
	}

	return checkpoints
}

func TestEnumerationReturnsEveryCheckpointAndNoClaim(t *testing.T) {
	l := openTestLedger(t, Config{Flow: "flow-enumerate", LocalID: "n0", Mux: testMux(t), Bootstrap: true})
	waitLeadership(t, l)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	want := seedRecoveryState(t, l, ctx)

	entries, err := l.List(ctx, "state/")
	if err != nil {
		t.Fatalf("enumerating under state/: %v", err)
	}
	got := pathsOf(entries)

	// CONTROL: the enumeration returned SOMETHING. Without this, an implementation
	// returning nothing at all satisfies "excludes the claims" perfectly.
	if len(got) == 0 {
		t.Fatal("CONTROL FAILED: the enumeration returned no keys at all, so excluding the claims proves nothing")
	}

	if !slices.Equal(got, want) {
		t.Fatalf("enumerating under state/ returned %v, want exactly the checkpoints %v; a claim among them would be read as a datum's own progress", got, want)
	}

	// The entries carry their values, not merely their keys — a resume reads the
	// checkpoint's contents.
	for _, entry := range entries {
		if string(entry.Value) != "progress" {
			t.Fatalf("the entry for %s carries value %q, want the checkpointed bytes", entry.Path, entry.Value)
		}
	}

	// DISCLOSURE: report the two sets this run actually observed.
	t.Logf("enumeration returned the checkpoint keys and excluded the claim keys: got %v with the claims state/claim/datum-1 and state/claim/datum-2 held but absent", got)
}

func TestAHeldClaimIsReadableAndNamesItsOwner(t *testing.T) {
	l := openTestLedger(t, Config{Flow: "flow-claimread", LocalID: "n0", Mux: testMux(t), Bootstrap: true})
	waitLeadership(t, l)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// CONTROL: BEFORE the claim, the datum reads back unclaimed through the same
	// accessor. A reader that always reported "held" would otherwise pass below.
	if owner, held, err := l.Claimant(ctx, "state/claim/datum-1"); err != nil || held {
		t.Fatalf("CONTROL FAILED: an unclaimed datum reads back held=%v owner=%q (err %v)", held, owner, err)
	}

	if _, err := l.Append(ctx,
		Entry{Kind: KindClaim, Path: "state/claim/datum-1", Value: []byte("worker-a")}); err != nil {
		t.Fatalf("claiming state/claim/datum-1: %v", err)
	}

	owner, held, err := l.Claimant(ctx, "state/claim/datum-1")
	if err != nil {
		t.Fatalf("reading the claim back: %v", err)
	}
	if !held {
		t.Fatal("a held claim reads back as unclaimed; the already-claimed filter is inert and every claimed datum is handed out a second time")
	}
	if owner != "worker-a" {
		t.Fatalf("the held claim names owner %q, want worker-a", owner)
	}

	// THE REPRODUCTION THIS ACCESSOR EXISTS FOR: the same claim through Get reports
	// ABSENT, which is why a filter built on Get observes nothing.
	if _, present, err := l.Get(ctx, "state/claim/datum-1"); err != nil {
		t.Fatalf("reading the claim path through Get: %v", err)
	} else if present {
		t.Fatal("a claim is visible through Get; Get's reply synthesizes a KindSet entry and would mislabel it as an assignment")
	}

	// DISCLOSURE: report the read-back itself.
	t.Logf("the held claim reads back naming its owner: state/claim/datum-1 held=%v owner=%q", held, owner)
}

func TestTheRecoveryReadersRefuseANilContextAndAClosedLedger(t *testing.T) {
	// Both refusals are declared surface rather than incidental: a nil context is a
	// programming error, and a closed ledger never reopens. Bad input errors here —
	// it is never defaulted into a background context or an empty answer, which
	// would hand recovery a confident "no orphans" from a ledger it cannot read.
	l := openTestLedger(t, Config{Flow: "flow-readguards", LocalID: "n0", Mux: testMux(t), Bootstrap: true})
	waitLeadership(t, l)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// CONTROL: both readers SERVE on this open ledger, so the refusals below are the
	// guards firing rather than a ledger that answers nothing.
	if _, err := l.List(ctx, "state/"); err != nil {
		t.Fatalf("CONTROL FAILED: enumerating an open ledger: %v", err)
	}
	if _, _, err := l.Claimant(ctx, "state/claim/datum-1"); err != nil {
		t.Fatalf("CONTROL FAILED: reading claim state on an open ledger: %v", err)
	}

	//nolint:staticcheck // passing a nil context is precisely the refusal under test.
	if _, err := l.List(nil, "state/"); !errors.Is(err, ErrNilContext) {
		t.Fatalf("enumerating with a nil context gave %v, want ErrNilContext", err)
	}
	//nolint:staticcheck // passing a nil context is precisely the refusal under test.
	if _, _, err := l.Claimant(nil, "state/claim/datum-1"); !errors.Is(err, ErrNilContext) {
		t.Fatalf("reading claim state with a nil context gave %v, want ErrNilContext", err)
	}

	if err := l.Close(); err != nil {
		t.Fatalf("closing the ledger: %v", err)
	}
	if _, err := l.List(ctx, "state/"); !errors.Is(err, ErrClosed) {
		t.Fatalf("enumerating a closed ledger gave %v, want ErrClosed", err)
	}
	if _, _, err := l.Claimant(ctx, "state/claim/datum-1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("reading claim state on a closed ledger gave %v, want ErrClosed", err)
	}
}

func TestEnumerationOnAFollowerAgreesWithTheLeader(t *testing.T) {
	// THE DISPOSITION UNDER TEST: enumeration forwards, so it is linearizable on
	// every node. A local non-linearizable read would leave a follower answering
	// from whatever it happened to have applied, and recovery deciding orphanhood
	// from that answer either claims live work or abandons dead work.
	muxes := newMuxes(t, 3)
	nodes := newClusterOn(t, "flow-enumfollow", muxes)
	leader := waitClusterLeader(t, nodes)
	follower := followerOf(t, nodes, leader)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	want := seedRecoveryState(t, leader.ledger, ctx)

	// FORCE THE FOLLOWER'S OWN STATE TO DISAGREE, or this leg does not test what it
	// claims. Measured while authoring: with the two nodes merely replicating
	// normally, a LOCAL non-linearizable enumeration on the follower agrees with the
	// leader's anyway, because the follower has applied everything by the time the
	// read runs — so the whole leg passed against an implementation that never
	// forwarded at all. A divergent entry applied to the follower's state machine
	// alone is what a lagging or diverged follower actually looks like, and it makes
	// the two implementations answer differently rather than identically.
	follower.ledger.fsm.Apply(commandAt(t, follower.ledger.raft.LastIndex()+1,
		Entry{Kind: KindSet, Path: "state/checkpoint/datum-local-only", Value: []byte("never replicated")}))

	// CONTROL: the divergence really is present in the follower's own state, or the
	// agreement below would be trivial again.
	if _, present := follower.ledger.fsm.get("state/checkpoint/datum-local-only"); !present {
		t.Fatal("CONTROL FAILED: the divergent entry is absent from the follower's state machine, so a local read and a forwarded read would agree anyway and this leg proves nothing")
	}

	// DISCLOSURE: report the planted divergence on the SUCCESS path. The control
	// above speaks only by failing, so without this line a run in which the
	// divergence was never planted is indistinguishable from one in which it was.
	t.Logf("the follower's own state machine carries a divergent entry the leader never replicated: %s",
		"state/checkpoint/datum-local-only")

	leaderEntries, err := leader.ledger.List(ctx, "state/")
	if err != nil {
		t.Fatalf("the leader's enumeration: %v", err)
	}
	followerEntries, err := follower.ledger.List(ctx, "state/")
	if err != nil {
		t.Fatalf("the follower's enumeration: %v; enumeration must serve on a follower rather than refusing", err)
	}

	leaderPaths, followerPaths := pathsOf(leaderEntries), pathsOf(followerEntries)

	// CONTROL: compare each against the FIXTURE, not only against each other. Two
	// enumerations that both returned nothing are equal and prove nothing.
	if !slices.Equal(leaderPaths, want) {
		t.Fatalf("the leader enumerated %v, want the seeded checkpoints %v", leaderPaths, want)
	}
	if !slices.Equal(followerPaths, want) {
		t.Fatalf("the follower enumerated %v, want the seeded checkpoints %v; its own state machine carries a divergent entry, so an enumeration returning that entry read local state instead of forwarding to the leader", followerPaths, want)
	}

	// DISCLOSURE: report that a follower genuinely ran this, so a leg that only ever
	// ran on a leader cannot satisfy the agreement claim trivially.
	t.Logf("the follower enumerated the same key set the leader did: %v (follower %s, leader %s)",
		followerPaths, follower.ledger.LocalID(), leader.ledger.LocalID())
}
