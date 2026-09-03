// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package recovery

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/checkpoint"
	"github.com/whitaker-io/machine/raft/membership"
)

// healthView is the harness view plus the thing the landed one never exercised: it
// DELIVERS peer-health signals and lets a test move the committed configuration
// under the detector, which is how an eviction and a readmission are expressed.
type healthView struct {
	mutex   sync.Mutex
	servers []raft.ServerID
	script  [][]membership.Signal
	round   int
}

func newHealthView(ids []string, script ...[]membership.Signal) *healthView {
	v := &healthView{script: script}
	for _, id := range ids {
		v.servers = append(v.servers, raft.ServerID(id))
	}

	return v
}

func (v *healthView) setServers(ids ...string) {
	v.mutex.Lock()
	defer v.mutex.Unlock()
	v.servers = nil
	for _, id := range ids {
		v.servers = append(v.servers, raft.ServerID(id))
	}
}

func (v *healthView) Membership(string) (raft.Configuration, uint64, bool) {
	v.mutex.Lock()
	defer v.mutex.Unlock()
	servers := make([]raft.Server, 0, len(v.servers))
	for _, id := range v.servers {
		servers = append(servers, raft.Server{ID: id})
	}

	return raft.Configuration{Servers: servers}, uint64(len(servers)), true
}

// Watch hands back the next scripted batch, then parks — the real one blocks until
// the membership moves, and a fake that returned instantly would turn the detector's
// park into a hot spin and measure the fake.
func (v *healthView) Watch(ctx context.Context, _ uint64) ([]membership.Signal, uint64, error) {
	v.mutex.Lock()
	round := v.round
	v.round++
	var batch []membership.Signal
	if round < len(v.script) {
		batch = v.script[round]
	}
	v.mutex.Unlock()

	if batch != nil {
		return batch, uint64(round + 1), nil
	}
	<-ctx.Done()

	return nil, 0, ctx.Err()
}

func (v *healthView) rounds() int {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	return v.round
}

func unreachable(flow, node string) []membership.Signal {
	return []membership.Signal{{
		Kind: membership.SignalPeerUnreachable, Flow: flow, Node: node, Since: time.Now(),
	}}
}

// TestAConfiguredButUnreachableOwnerIsOfferedAndAnEvictionIsNotTerminal drives the
// ruled predicate: an owner is dead when it is in the committed configuration AND
// the leader's health view marks it unreachable.
func TestAConfiguredButUnreachableOwnerIsOfferedAndAnEvictionIsNotTerminal(t *testing.T) {
	nodes := newGroup(t, "alpha", 3)
	leader := awaitLeader(t, nodes)

	// ARM 1: the dead owner is STILL CONFIGURED — the state a refused eviction
	// leaves behind — and the leader has published that it is unreachable.
	journalCheckpoint(t, leader.ledger, "c-dead", "d1", "progress")
	stuck := newHealthView(
		[]string{leader.id, "b-live", "c-dead", "d-replacement"},
		unreachable("alpha", "c-dead"),
	)
	detector := New(leader.ledger, stuck, "alpha")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	orphans, err := detector.Orphans(ctx, "alpha")
	t.Logf("ARM 1 (configured but unreachable): orphans=%v err=%v rounds=%d",
		datums(orphans), err, stuck.rounds())
	if err != nil {
		t.Fatalf("ARM 1: Orphans errored: %v", err)
	}
	if len(orphans) != 1 || orphans[0].Datum != "d1" || orphans[0].Owner != "c-dead" {
		t.Fatalf("ARM 1: orphans=%v, want d1 owned by c-dead offered while it is STILL "+
			"in the configuration; the health arm is not deciding", datums(orphans))
	}

	// ARM 2: THE DISCRIMINATING CONTROL. Same configuration, same checkpoint, and
	// the ONE input that moves is the health signal. An implementation that offered
	// every configured owner, or that ignored the configuration entirely, passes
	// arm 1 and fails here.
	journalCheckpoint(t, leader.ledger, "b-live", "d2", "progress")
	quiet := newHealthView([]string{leader.id, "b-live", "c-dead", "d-replacement"})
	control := New(leader.ledger, quiet, "alpha")
	short, cancelShort := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelShort()
	orphans, err = control.Orphans(short, "alpha")
	t.Logf("ARM 2 (no health signal at all): orphans=%v err=%v", datums(orphans), err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CONTROL FAILED: arm 2 ended with %v rather than a parked call hitting its "+
			"deadline, so an empty orphan set here is not evidence that nothing was offered", err)
	}
	if len(orphans) != 0 {
		t.Fatalf("CONTROL FAILED: %v offered with no unreachable signal published. A live "+
			"owner's work is not an orphan, and a member the leader has published nothing "+
			"about has never been reachable and owns no datum", datums(orphans))
	}

	// ARM 3: THE RETIRE-CLAIM REACH. The dead owner also HOLDS a claim on its own
	// datum. Decision 66776f77's re-offer is unreachable while the holder reads
	// alive, so this is the leg that proves the retire path is reachable at all.
	claimOwner(t, leader.ledger, "d1", "c-dead")
	held := newHealthView(
		[]string{leader.id, "b-live", "c-dead", "d-replacement"},
		unreachable("alpha", "c-dead"),
	)
	retiring := New(leader.ledger, held, "alpha")
	orphans, err = retiring.Orphans(ctx, "alpha")
	t.Logf("ARM 3 (departed holder still configured): orphans=%v err=%v", datums(orphans), err)
	if err != nil {
		t.Fatalf("ARM 3: Orphans errored: %v", err)
	}
	claimant, stillHeld, err := leader.ledger.Claimant(ctx, checkpoint.ClaimPath("d1"))
	if err != nil {
		t.Fatalf("ARM 3: reading the claim state: %v", err)
	}
	t.Logf("ARM 3 claim state after the round: claimant=%q held=%v", claimant, stillHeld)
	if stillHeld {
		t.Fatalf("ARM 3: the departed holder's claim is still held by %q; the leader-appended "+
			"retire-claim never fired and every survivor loses the race forever", claimant)
	}

	// ARM 4: AN EVICTION IS NOT TERMINAL FOR AN OWNER. Lane D's re-place round can
	// evict a LIVE member and readmit it under the same id, so an owner that left
	// the configuration and came back must read ALIVE again — with no returned
	// signal, because a first sighting publishes nothing in either direction.
	//
	// IT RUNS ON ITS OWN GROUP AND ITS OWN FLOW, and that is a correction rather
	// than tidiness: on the shared ledger the arm-1 datum is an orphan by the
	// ABSENCE arm from the very first round, so Orphans returned it before await
	// ever ran and the pre-check below passed on a datum this arm is not about.
	gammaNodes := newGroup(t, "gamma", 3)
	gamma := awaitLeader(t, gammaNodes)
	journalCheckpoint(t, gamma.ledger, "e-flapper", "d3", "progress")
	flap := newHealthView(
		[]string{gamma.id, "b-live", "e-flapper"},
		unreachable("gamma", "e-flapper"),
	)
	readmit := New(gamma.ledger, flap, "gamma")
	orphans, err = readmit.Orphans(ctx, "gamma")
	t.Logf("ARM 4 pre-check (marked unreachable while configured): orphans=%v err=%v",
		datums(orphans), err)
	if err != nil || len(orphans) != 1 || orphans[0].Owner != "e-flapper" {
		t.Fatalf("ARM 4 PRE-CHECK FAILED: want exactly d3 owned by e-flapper offered, got %v "+
			"(%v); the readmission below would prove nothing", datums(orphans), err)
	}

	// It is evicted, then re-announces and is readmitted under the same id.
	flap.setServers(gamma.id, "b-live")
	if _, err := readmit.Orphans(ctx, "gamma"); err != nil {
		t.Fatalf("ARM 4: the round taken while the flapper was evicted errored: %v", err)
	}
	flap.setServers(gamma.id, "b-live", "e-flapper")
	after, cancelAfter := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancelAfter()
	orphans, err = readmit.Orphans(after, "gamma")
	t.Logf("ARM 4 (evicted then readmitted): orphans=%v err=%v", datums(orphans), err)
	if len(orphans) != 0 {
		t.Fatalf("ARM 4: %v is offered though its owner was readmitted to the configuration. "+
			"An eviction is NOT terminal for an owner: a member evicted while live "+
			"re-announces and rejoins, and its datums are not orphans", datums(orphans))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ARM 4: the readmitted round ended with %v rather than a parked call hitting "+
			"its deadline, so the empty set is not evidence the owner reads alive", err)
	}
}
