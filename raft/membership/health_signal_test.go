// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// peerSignalsSince reads every peer-kind signal after a cursor and returns them
// with the new cursor, so each leg below reads only what IT published.
func peerSignalsSince(t *testing.T, m *Manager, cursor uint64) ([]Signal, uint64) {
	t.Helper()
	batch := m.signals.since(cursor)
	if batch.err != nil {
		t.Fatalf("reading signals since %d: %v", cursor, batch.err)
	}
	out := make([]Signal, 0, len(batch.signals))
	for _, sig := range batch.signals {
		switch sig.Kind {
		case SignalPeerUnreachable, SignalPeerReturned:
			out = append(out, sig)
		case SignalMembershipChanged, SignalPeerEvicted:
		}
	}
	return out, batch.cursor
}

// countKind counts signals of one kind and names every node they carry.
func countKind(sigs []Signal, kind SignalKind) (int, []string) {
	nodes := []string{}
	for _, sig := range sigs {
		if sig.Kind == kind {
			nodes = append(nodes, sig.Node)
		}
	}
	return len(nodes), nodes
}

// TestAnUnreachableSignalMeansAPeerWasReachableAndIsNotAnyMore gates the
// encoding of the peer-health signals.
//
// THE HAZARD IS A FIRST SIGHTING, NOT A FAILURE. Autopilot reports every member
// unhealthy until it has been stable for ServerStabilizationTime, so a node
// taking leadership for the first time publishes a first state in which every
// member is unhealthy for a group in which nothing is wrong. A consumer reading
// these signals as liveness would treat every live peer's work as orphaned for
// the whole window after each first-time failover, which is a single-writer
// violation reached by an ordinary leader election.
func TestAnUnreachableSignalMeansAPeerWasReachableAndIsNotAnyMore(t *testing.T) {
	mgr, _ := testNode(t, "a-leader")
	mgr.cfg.Autopilot.ServerStabilizationTime = 400 * time.Millisecond
	pilot := newPilotState(mgr, "alpha")
	var cursor uint64

	// LEG 1, THE RULED CASE: a fresh pilot's first state marks everything
	// unhealthy, including the leader. NOTHING is published, because there is no
	// prior state for any of it to be a transition from.
	pilot.noteHealth(&autopilotState{health: map[raft.ServerID]bool{
		"a-leader": false, "b-live": false, "c-live": false, "d-live": false,
	}})
	sigs, cursor := peerSignalsSince(t, mgr, cursor)
	if n, nodes := countKind(sigs, SignalPeerUnreachable); n != 0 {
		t.Fatalf("a fresh pilot's first all-unhealthy state published %d unreachable signals naming %v; "+
			"a consumer reading these as liveness would offer every live peer's work as orphaned for the "+
			"stabilization window after every first-time failover", n, nodes)
	}

	// LEG 2: stabilization completes. There is no episode to close, so no
	// returned signal is published either — a returned always has an opening.
	pilot.noteHealth(&autopilotState{health: map[raft.ServerID]bool{
		"a-leader": true, "b-live": true, "c-live": true, "d-live": true,
	}})
	sigs, cursor = peerSignalsSince(t, mgr, cursor)
	if n, nodes := countKind(sigs, SignalPeerReturned); n != 0 {
		t.Fatalf("stabilization published %d returned signals naming %v, closing episodes that were "+
			"never opened", n, nodes)
	}
	// THIS LEG IS THE CONTROL FOR THE PAIRING, and it names the implementation
	// it defeats: one that publishes a returned on EVERY unhealthy-to-healthy
	// transition, regardless of whether an unreachable was ever published for
	// the episode it claims to close. That implementation publishes three here.
	// A consumer reading orphanhood off these signals would be told three peers
	// came back that it was never told had left.
	t.Log("three peers stabilizing after a first sighting closed no episode: zero returned signals")

	// LEG 3, THE CASE THE SIGNAL IS NAMED FOR: a peer that WAS reachable stops
	// being reachable. Exactly one unreachable, naming exactly that peer, at once.
	pilot.noteHealth(&autopilotState{health: map[raft.ServerID]bool{
		"a-leader": true, "b-live": true, "c-live": false, "d-live": true,
	}})
	sigs, cursor = peerSignalsSince(t, mgr, cursor)
	n, nodes := countKind(sigs, SignalPeerUnreachable)
	if n != 1 || nodes[0] != "c-live" {
		t.Fatalf("a peer that was reachable and stopped being reachable published %d unreachable "+
			"signals naming %v, want exactly one naming c-live", n, nodes)
	}
	// THE VACUITY CONTROL FOR LEGS 1 AND 2, and it is this leg rather than a
	// separate one: the instrument demonstrably CAN publish an unreachable
	// signal, so the zeroes above are the encoding and not a reader that reads
	// nothing.
	t.Logf("a peer that was reachable and is not any more published %d unreachable signal naming %v", n, nodes)

	// Repeating the same unhealthy state publishes nothing further: one report
	// per episode.
	pilot.noteHealth(&autopilotState{health: map[raft.ServerID]bool{
		"a-leader": true, "b-live": true, "c-live": false, "d-live": true,
	}})
	sigs, cursor = peerSignalsSince(t, mgr, cursor)
	if len(sigs) != 0 {
		t.Fatalf("re-observing the same unhealthy state published %d further peer signals, want 0: "+
			"the report is once per episode", len(sigs))
	}

	// LEG 4: the episode closes. Exactly one returned, naming that peer.
	pilot.noteHealth(&autopilotState{health: map[raft.ServerID]bool{
		"a-leader": true, "b-live": true, "c-live": true, "d-live": true,
	}})
	sigs, cursor = peerSignalsSince(t, mgr, cursor)
	n, nodes = countKind(sigs, SignalPeerReturned)
	if n != 1 || nodes[0] != "c-live" {
		t.Fatalf("the peer coming back published %d returned signals naming %v, want exactly one "+
			"naming c-live", n, nodes)
	}
	t.Logf("the episode closed with exactly %d returned signal naming %v, pairing the one unreachable "+
		"that opened it", n, nodes)

	// LEG 5, THE BOUNDED LATENCY: a peer that is unhealthy from its FIRST
	// sighting and never becomes healthy is still reported — after the
	// stabilization window and not before it. Without this the suppression above
	// would lose a genuinely dead peer at a fresh leader's start entirely.
	fresh := newPilotState(mgr, "alpha")
	fresh.noteHealth(&autopilotState{health: map[raft.ServerID]bool{"e-dead": false}})
	sigs, cursor = peerSignalsSince(t, mgr, cursor)
	if n, nodes := countKind(sigs, SignalPeerUnreachable); n != 0 {
		t.Fatalf("a peer unhealthy at its first sighting was reported immediately (%d, %v); the report "+
			"is owed only once the stabilization window has passed", n, nodes)
	}
	fresh.noteHealth(&autopilotState{health: map[raft.ServerID]bool{"e-dead": false}})
	sigs, cursor = peerSignalsSince(t, mgr, cursor)
	if n, _ := countKind(sigs, SignalPeerUnreachable); n != 0 {
		t.Fatal("a peer still inside the stabilization window was reported unreachable")
	}
	time.Sleep(500 * time.Millisecond)
	fresh.noteHealth(&autopilotState{health: map[raft.ServerID]bool{"e-dead": false}})
	sigs, _ = peerSignalsSince(t, mgr, cursor)
	n, nodes = countKind(sigs, SignalPeerUnreachable)
	if n != 1 || nodes[0] != "e-dead" {
		t.Fatalf("a peer still unhealthy after the stabilization window published %d unreachable "+
			"signals naming %v, want exactly one naming e-dead: a genuinely dead peer at a fresh "+
			"leader's start must still be reported", n, nodes)
	}
	t.Logf("a peer unhealthy since its first sighting was reported after the %s window, naming %v",
		fresh.stabilization(), nodes)
}
