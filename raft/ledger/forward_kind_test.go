// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

// followerOf returns a node of the group that is not the leader.
func followerOf(t *testing.T, nodes []*clusterNode, leader *clusterNode) *clusterNode {
	t.Helper()

	for _, node := range nodes {
		if node != leader {
			return node
		}
	}
	t.Fatal("a multi-node group produced no follower")

	return nil
}

func TestAFollowersClaimForwardsAndLandsOnTheLeader(t *testing.T) {
	// THIS IS THE HALF A KIND ADDED WITHOUT THE WIRE EXTENSION FAILS SILENTLY. A
	// claim that has no representation on the wire is appended locally on the
	// follower, where it refuses — and nothing anywhere reports it.
	muxes := newMuxes(t, 3)
	nodes := newClusterOn(t, "flow-claimwire", muxes)
	leader := waitClusterLeader(t, nodes)
	follower := followerOf(t, nodes, leader)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// CONTROL: a KindSet from the SAME follower over the same path lands, so a claim
	// failure below is about the kind rather than about a follower that forwards
	// nothing.
	if _, err := follower.ledger.Append(ctx, Entry{Kind: KindSet, Path: "datum/control", Value: []byte("v")}); err != nil {
		t.Fatalf("CONTROL FAILED: a follower's KindSet append: %v", err)
	}

	if _, err := follower.ledger.Append(ctx,
		Entry{Kind: KindClaim, Path: "datum/7", Value: []byte("worker-a")}); err != nil {
		t.Fatalf("a follower's claim of datum/7: %v; a kind with no wire representation refuses on every follower", err)
	}

	// It landed on the LEADER's state machine, which is the property: a claim that
	// stayed leader-local would have refused above rather than replicating.
	if held := leader.ledger.fsm.claimant("datum/7"); held != "worker-a" {
		t.Fatalf("after a follower's claim the LEADER holds datum/7 as %q, want worker-a", held)
	}

	// And a competing claim from the leader is refused by the replicated state.
	_, err := leader.ledger.Append(ctx, Entry{Kind: KindClaim, Path: "datum/7", Value: []byte("worker-b")})
	if !errors.Is(err, ErrClaimHeld) {
		t.Fatalf("a second worker's claim of the forwarded datum/7 gave %v, want ErrClaimHeld", err)
	}
}

func TestAFollowersRetireForwardsAndLandsOnTheLeader(t *testing.T) {
	muxes := newMuxes(t, 3)
	nodes := newClusterOn(t, "flow-retirewire", muxes)
	leader := waitClusterLeader(t, nodes)
	follower := followerOf(t, nodes, leader)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := leader.ledger.Append(ctx, Entry{Kind: KindSet, Path: "datum/7", Value: []byte("progress")}); err != nil {
		t.Fatalf("checkpointing datum/7: %v", err)
	}
	if _, err := leader.ledger.Append(ctx, Entry{Kind: KindClaim, Path: "datum/7", Value: []byte("worker-a")}); err != nil {
		t.Fatalf("claiming datum/7: %v", err)
	}
	// CONTROL: both halves are live on the leader before the follower's retire.
	if _, present, err := leader.ledger.Get(ctx, "datum/7"); err != nil || !present {
		t.Fatalf("CONTROL FAILED: datum/7 is not checkpointed before the retire (present=%v, err %v)", present, err)
	}

	if _, err := follower.ledger.Append(ctx, Entry{Kind: KindRetire, Path: "datum/7"}); err != nil {
		t.Fatalf("a follower's retire of datum/7: %v", err)
	}

	if _, present, err := leader.ledger.Get(ctx, "datum/7"); err != nil || present {
		t.Fatalf("after a follower's retire datum/7 is still checkpointed on the leader (present=%v, err %v)", present, err)
	}
	if held := leader.ledger.fsm.claimant("datum/7"); held != "" {
		t.Fatalf("after a follower's retire datum/7 is still held by %q on the leader", held)
	}
}

func TestAClaimIsWonByTheFirstAppenderAndRefusedToEveryLater(t *testing.T) {
	// THE RACE IS RUN ACROSS REAL NODES rather than against the state machine alone,
	// because the refusal has to survive the forwarding wire: a loser that learns its
	// fate only on the leader is a loser that never learns it.
	muxes := newMuxes(t, 3)
	nodes := newClusterOn(t, "flow-claimrace", muxes)
	leader := waitClusterLeader(t, nodes)
	follower := followerOf(t, nodes, leader)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The FIRST appender wins.
	if _, err := leader.ledger.Append(ctx, Entry{Kind: KindClaim, Path: "datum/7", Value: []byte("worker-a")}); err != nil {
		t.Fatalf("the first claim of datum/7: %v", err)
	}

	// A LATER claim, issued on a FOLLOWER so it travels the forwarding wire, is
	// refused — and the refusal arrives as the sentinel rather than as a bare
	// message, which is the distinction the recovery loop turns on.
	_, err := follower.ledger.Append(ctx, Entry{Kind: KindClaim, Path: "datum/7", Value: []byte("worker-b")})
	if !errors.Is(err, ErrClaimHeld) {
		t.Fatalf("a follower's losing claim of datum/7 gave %v, want ErrClaimHeld over the wire", err)
	}
	t.Log("the losing claim was refused on a follower after forwarding")

	// EVERY later one, not merely the second.
	if _, err := leader.ledger.Append(ctx,
		Entry{Kind: KindClaim, Path: "datum/7", Value: []byte("worker-c")}); !errors.Is(err, ErrClaimHeld) {
		t.Fatalf("a third worker's claim of datum/7 gave %v, want ErrClaimHeld", err)
	}

	// CONTROL: the same follower's claim of an UNCLAIMED datum is admitted, so the
	// refusals above are the held claim rather than a follower whose claims all fail.
	if _, err := follower.ledger.Append(ctx,
		Entry{Kind: KindClaim, Path: "datum/8", Value: []byte("worker-b")}); err != nil {
		t.Fatalf("CONTROL FAILED: the same follower's claim of the unclaimed datum/8: %v", err)
	}

	// The winner is unchanged by either refusal.
	if held := leader.ledger.fsm.claimant("datum/7"); held != "worker-a" {
		t.Fatalf("after two refused claims datum/7 is held by %q, want worker-a", held)
	}
}

func TestAnUndeclaredKindOnTheForwardingWireIsRefusedNotCoerced(t *testing.T) {
	// THE ZERO KIND IS THE REAL CASE: an older peer's save carries no kind at all.
	// Coercing it to KindSet would turn a request this build cannot interpret into a
	// silent assignment.
	mux := testMux(t)
	l := openTestLedger(t, Config{Flow: "flow-kindguard", LocalID: "n0", Mux: mux, Bootstrap: true})
	waitLeadership(t, l)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, kind := range []Kind{0, Kind(99)} {
		reply := forwardRoundTrip(t, l, mux.Addr().String(),
			forwardRequest{Op: opSave, Kind: kind, Path: "heap/undeclared-kind", Value: []byte("must-not-land")})
		if err := reply.rebuild(); err == nil {
			t.Fatalf("kind %d rebuilt to a nil error: an undeclared kind must reach the caller as a refusal", kind)
		} else if !errors.Is(err, ErrPoisonedJournal) {
			t.Fatalf("kind %d rebuilt as %v, want a wrapped ErrPoisonedJournal", kind, err)
		}
	}

	// IT LEFT NO STATE BEHIND — the refusal is not a coercion that also wrote.
	if _, present, err := l.Get(ctx, "heap/undeclared-kind"); err != nil || present {
		t.Fatalf("after two undeclared kinds the path is present=%v (err %v); the kind was coerced rather than refused", present, err)
	}

	// CONTROL: a DECLARED kind over the same stream IS served, so the refusals above
	// are about the kind rather than about a dead stream.
	reply := forwardRoundTrip(t, l, mux.Addr().String(),
		forwardRequest{Op: opSave, Kind: KindSet, Path: "heap/undeclared-kind", Value: []byte("landed")})
	if err := reply.rebuild(); err != nil {
		t.Fatalf("CONTROL FAILED: a declared kind over the same stream: %v", err)
	}
}

func TestALostClaimCarriesItsOwnRefusalIdentityAndIsNotRetryable(t *testing.T) {
	// Without its own code a lost claim takes codeOther, which is non-retryable and
	// therefore behaves correctly, but arrives as a bare message — leaving a caller
	// unable to tell "you lost the race" from "something unclassified went wrong".
	if got := classifyCode(t, ErrClaimHeld); got != codeClaimHeld {
		t.Fatalf("a lost claim classified as code %d, want codeClaimHeld", got)
	}
	if codeClaimHeld.retryable() {
		t.Fatal("codeClaimHeld is retryable; a lost claim is settled and a retry would spin to the bound against a condition no retry repairs")
	}
	if sentinel := codeClaimHeld.sentinel(); !errors.Is(sentinel, ErrClaimHeld) {
		t.Fatalf("codeClaimHeld rebuilds to %v, want ErrClaimHeld", sentinel)
	}

	// CONTROL: the table still admits a retryable code, so the false above is this
	// code's disposition rather than a retryable set that is empty.
	if !codeNotLeader.retryable() {
		t.Fatal("CONTROL FAILED: codeNotLeader is not retryable, so the retryable set is empty and the assertion above proves nothing")
	}
}

// classifyCode reports the wire code an error classifies to.
func classifyCode(t *testing.T, err error) errCode {
	t.Helper()

	code, message := classify(err)
	if message == "" {
		t.Fatalf("classifying %v produced an empty message; the leader's own wording is lost", err)
	}

	return code
}
