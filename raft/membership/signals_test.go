package membership

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/ledger"
)

// awaitSignal watches until a signal of the given kind arrives, or the window
// expires.
func awaitSignal(t *testing.T, mgr *Manager, kind SignalKind, window time.Duration) (Signal, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	var cursor uint64
	for {
		batch, next, err := mgr.Watch(ctx, cursor)
		if err != nil {
			return Signal{}, false
		}
		for _, sig := range batch {
			if sig.Kind == kind {
				return sig, true
			}
		}
		cursor = next
	}
}

// voterIDs reports the voting members of a flow's committed configuration.
func voterIDs(t *testing.T, r *raft.Raft) []string {
	t.Helper()
	future := r.GetConfiguration()
	if err := future.Error(); err != nil {
		t.Fatalf("GetConfiguration: %v", err)
	}
	out := []string{}
	for _, server := range future.Configuration().Servers {
		if server.Suffrage == raft.Voter {
			out = append(out, string(server.ID))
		}
	}
	return out
}

// memberIDs reports every member of a flow's committed configuration.
func memberIDs(t *testing.T, r *raft.Raft) []string {
	t.Helper()
	future := r.GetConfiguration()
	if err := future.Error(); err != nil {
		t.Fatalf("GetConfiguration: %v", err)
	}
	out := []string{}
	for _, server := range future.Configuration().Servers {
		out = append(out, string(server.ID))
	}
	return out
}

// addStaleVoter puts a member into a flow's configuration at an address nothing
// answers, which is the shape a replaced pod leaves behind under ephemeral
// identity: the id is gone and the configuration still carries it.
//
// It uses AddVoter deliberately and it is TEST SCAFFOLDING, not a join path: it
// CONSTRUCTS the measured state the eviction rule exists to clean up. The
// production rule against AddVoter is a corpus check over non-test source.
func addStaleVoter(t *testing.T, r *raft.Raft, id, addr string) {
	t.Helper()
	if err := r.AddVoter(raft.ServerID(id), raft.ServerAddress(addr), 0, 0).Error(); err != nil {
		t.Fatalf("planting the stale member %s: %v", id, err)
	}
}

func TestEveryNodeObservesAMembershipChangeThroughItsFSM(t *testing.T) {
	leader := newClusterNode(t, "a-leader", []string{"alpha"}, 0)
	leader.start(t)
	leader.awaitLeader(t, "alpha")

	follower := newClusterNode(t, "b-follower", []string{"alpha"}, 2)
	follower.peering(leader.addr)
	follower.start(t)

	// THE EVERY-NODE HALF, and the leg a leader-only implementation fails: the
	// FOLLOWER sees the membership change, because the signal rides its own state
	// machine's configuration commit rather than a leader-side observation.
	sig, ok := awaitSignal(t, follower.mgr, SignalMembershipChanged, 20*time.Second)
	if !ok {
		t.Fatal("the follower never observed a membership change through its own state machine")
	}
	if sig.Flow != "alpha" {
		t.Fatalf("the signal names flow %q, want alpha", sig.Flow)
	}
	t.Logf("membership change observed on a follower: %+v", sig)
}

func TestAnUnreachablePeerRaisesALeaderDrivenSignal(t *testing.T) {
	leader := newClusterNode(t, "a-leader", []string{"alpha"}, 0)
	leader.mgr.cfg.Autopilot = promotionTuning
	leader.start(t)
	leader.awaitLeader(t, "alpha")
	reportsSelfAs(leader, func() FlowStats {
		r := leader.raftFor(t, "alpha")
		return FlowStats{Term: r.CurrentTerm(), LastIndex: r.LastIndex(), LastContact: time.Millisecond, Leader: true}
	})

	// A member that never answers: staged, present, and silent.
	ghost := unreachableAddress(t)
	reply := leader.mgr.answerAnnounce(announce{Node: "b-ghost", Address: ghost, Flows: []string{"alpha"}})
	if len(reply.Staged) != 1 {
		t.Fatalf("the ghost was not staged: %+v", reply)
	}

	sig, ok := awaitSignal(t, leader.mgr, SignalPeerUnreachable, 30*time.Second)
	if !ok {
		t.Fatal("a peer that answers nothing raised no unreachable signal on the leader")
	}
	if sig.Flow != "alpha" || sig.Node != "b-ghost" {
		t.Fatalf("the signal names flow %q node %q, want alpha and b-ghost", sig.Flow, sig.Node)
	}
	if sig.Since.IsZero() {
		t.Fatal("the signal carries no Since time")
	}
}

func TestAStaleCursorReceivesEverySignalSince(t *testing.T) {
	mgr, _ := testNode(t, "a-node")
	for i := 0; i < 5; i++ {
		mgr.signals.publish(Signal{Kind: SignalMembershipChanged, Flow: fmt.Sprintf("flow-%d", i), Node: "a-node"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A CURSOR OF ZERO IS THE STALEST THERE IS. An implementation built on a
	// lossy channel delivers only the newest; a cursor delivers everything since.
	batch, cursor, err := mgr.Watch(ctx, 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if len(batch) != 5 {
		t.Fatalf("a stale cursor received %d signals, want all 5: signals were dropped rather than retained",
			len(batch))
	}
	if cursor != batch[len(batch)-1].Index {
		t.Fatalf("the returned cursor %d is not the last signal's index %d", cursor, batch[len(batch)-1].Index)
	}
	// AND THE CURSOR ADVANCES: reading again from it yields nothing new.
	short, shortCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer shortCancel()
	if _, _, err := mgr.Watch(short, cursor); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a caught-up reader got %v, want a wait: the cursor is not advancing", err)
	}
}

func TestASignalCursorOlderThanTheRetentionWindowIsRefusedNotSilentlySkipped(t *testing.T) {
	mgr, _ := testNode(t, "a-node")
	for i := 0; i < maxRetainedSignals+50; i++ {
		mgr.signals.publish(Signal{Kind: SignalMembershipChanged, Flow: "alpha", Node: "a-node"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := mgr.Watch(ctx, 0)
	if !errors.Is(err, ErrCursorTooOld) {
		t.Fatalf("err = %v, want ErrCursorTooOld: a reader served from the oldest available signal would see "+
			"what looks like a complete history and is a gap", err)
	}
	// THE REFUSAL NAMES THE OLDEST INDEX STILL HELD, which is what a consumer
	// needs in order to know how far it must rebuild from Membership.
	//
	// THE EXPECTATION IS READ FROM THE LOG, and only the DERIVATION moved: a
	// cursor now packs this log's incarnation, so a bare sequence handed back to
	// Watch would be a FOREIGN cursor rather than an old one. The assertion's
	// content is unchanged — the oldest retained SEQUENCE is still 51 after 1074
	// publishes into a 1024-slot window — and the refusal must still name a
	// cursor the consumer can actually use.
	mgr.signals.mu.Lock()
	oldest := mgr.signals.retained[0].Index
	mgr.signals.mu.Unlock()
	if got := oldest & signalSequenceMask; got != 51 {
		t.Fatalf("the oldest retained sequence is %d, want 51", got)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("oldest retained %d", oldest)) {
		t.Fatalf("the refusal %q does not name the oldest retained index %d", err, oldest)
	}
	// THE CONTROL: a cursor INSIDE the window is still served, so the refusal is
	// about the window rather than about a reader that refuses everything.
	batch, _, err := mgr.Watch(ctx, oldest)
	if err != nil {
		t.Fatalf("CONTROL FAILED: a cursor inside the window was refused: %v", err)
	}
	if len(batch) == 0 {
		t.Fatal("CONTROL FAILED: a cursor inside the window received nothing")
	}
}

func TestTheSignalWatchNeverBlocksRaft(t *testing.T) {
	leader := newClusterNode(t, "a-leader", []string{"alpha"}, 0)
	leader.start(t)
	leader.awaitLeader(t, "alpha")

	// A watcher that parks forever and never reads again.
	parked := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		close(parked)
		_, _, _ = leader.mgr.Watch(ctx, 0)
		<-ctx.Done()
	}()
	<-parked

	// Writes must keep committing underneath it. A callback that blocked would
	// park the state machine; a buffered channel that filled would drop.
	for i := 0; i < 25; i++ {
		if err := commitWrite(t, leader, "alpha", fmt.Sprintf("under-a-parked-watcher/%d", i)); err != nil {
			t.Fatalf("write %d failed while a watcher was parked: %v", i, err)
		}
	}
}

// evictionCluster builds a leader and a live follower, both in the flow's
// configuration as voters, plus whatever stale identities the caller plants.
func evictionCluster(t *testing.T, expect int) (leader, live *clusterNode) {
	t.Helper()
	leader = newClusterNode(t, "a-leader", []string{"alpha"}, 0)
	leader.start(t)
	leader.awaitLeader(t, "alpha")
	live = newClusterNode(t, "b-live", []string{"alpha"}, 2)
	live.peering(leader.addr)
	live.start(t)
	// The live follower is a VOTER, which is the state a promoted member reaches
	// and the state the eviction bounds are written against.
	addStaleVoter(t, leader.raftFor(t, "alpha"), "b-live", live.addr)
	leader.mgr.cfg.Expect = expect
	leader.mgr.cfg.Peers = "peers.invalid:0"
	return leader, live
}

func TestAFailedPeerIsSignalledAndEvictedFromTheConfiguration(t *testing.T) {
	leader, live := evictionCluster(t, 2)
	// THE MEASURED KUBERNETES SHAPE: a configuration carrying a replaced pod's
	// stale identity while the registry reports the full complement.
	stale := unreachableAddress(t)
	addStaleVoter(t, leader.raftFor(t, "alpha"), "c-stale", stale)

	// A COMPLETE resolution naming exactly the live instances.
	leader.mgr.resolve = func(context.Context, string) ([]string, error) {
		return []string{leader.addr, live.addr}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	leader.mgr.evictRound(ctx, "alpha")

	if !awaitAbsent(t, leader.raftFor(t, "alpha"), "c-stale", 15*time.Second) {
		t.Fatalf("the stale identity was not pruned; members are %v", memberIDs(t, leader.raftFor(t, "alpha")))
	}
	t.Logf("stale identity pruned under a complete resolution: members are now %v",
		memberIDs(t, leader.raftFor(t, "alpha")))

	sig, ok := awaitSignal(t, leader.mgr, SignalPeerEvicted, 10*time.Second)
	if !ok {
		t.Fatal("the eviction raised no SignalPeerEvicted, so a consumer cannot act on it")
	}
	if sig.Node != "c-stale" || sig.Flow != "alpha" {
		t.Fatalf("the eviction signal names flow %q node %q, want alpha and c-stale", sig.Flow, sig.Node)
	}
}

func TestEvictionRequiresACompleteResolutionAndIsBoundedToOnePerRound(t *testing.T) {
	leader, _ := evictionCluster(t, 3)
	stale := unreachableAddress(t)
	addStaleVoter(t, leader.raftFor(t, "alpha"), "c-stale", stale)
	before := voterIDs(t, leader.raftFor(t, "alpha"))

	// THE DISCRIMINATING INPUT: a resolution that is NON-EMPTY, CONTAINS THIS
	// NODE, and is a STRICT SUBSET of the live set. That exact shape passed the
	// earlier bounds and evicted a live follower, and across rounds the
	// one-per-round bound compounded rather than capped.
	leader.mgr.resolve = func(context.Context, string) ([]string, error) {
		return []string{leader.addr}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for round := 0; round < 4; round++ {
		leader.mgr.evictRound(ctx, "alpha")
		time.Sleep(100 * time.Millisecond)
	}

	after := voterIDs(t, leader.raftFor(t, "alpha"))
	if len(after) != len(before) {
		t.Fatalf("four rounds under an incomplete resolution changed the voter set from %v to %v: "+
			"the completeness bound does not hold, and the one-per-round bound compounds rather than caps",
			before, after)
	}
}

func TestEvictionNeverShrinksTheConfigurationBelowTheLiveCount(t *testing.T) {
	leader, live := evictionCluster(t, 2)
	before := voterIDs(t, leader.raftFor(t, "alpha"))

	// THE CASE COMPLETENESS ALONE DOES NOT CATCH: a resolution of the RIGHT SIZE
	// but the WRONG membership. Expect instances answer, but one of them is a
	// newcomer and a live member is momentarily absent.
	newcomer := unreachableAddress(t)
	leader.mgr.resolve = func(context.Context, string) ([]string, error) {
		return []string{leader.addr, newcomer}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	leader.mgr.evictRound(ctx, "alpha")
	time.Sleep(300 * time.Millisecond)

	after := voterIDs(t, leader.raftFor(t, "alpha"))
	if len(after) != len(before) {
		t.Fatalf("a right-size wrong-membership resolution changed the voter set from %v to %v: "+
			"completeness passed and the residual bound did not hold", before, after)
	}
	if _, ok := suffrageIn(t, leader.raftFor(t, "alpha"), "b-live"); !ok {
		t.Fatalf("the live follower %s was evicted on a resolution that merely failed to mention it", live.id)
	}
}

func TestAnEvictedPeersDatumsAreNeverClaimedHere(t *testing.T) {
	leader, live := evictionCluster(t, 2)
	stale := unreachableAddress(t)
	addStaleVoter(t, leader.raftFor(t, "alpha"), "c-stale", stale)

	// A datum the evicted member was responsible for.
	l, ok := leader.mgr.Ledger("alpha")
	if !ok {
		t.Fatal("the leader does not host the flow")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := l.Append(ctx, ledger.Entry{
		Kind: ledger.KindSet, Path: "work/owned-by-c-stale", Value: []byte("c-stale"),
	}); err != nil {
		t.Fatalf("seeding the datum: %v", err)
	}

	leader.mgr.resolve = func(context.Context, string) ([]string, error) {
		return []string{leader.addr, live.addr}, nil
	}
	leader.mgr.evictRound(ctx, "alpha")
	if !awaitAbsent(t, leader.raftFor(t, "alpha"), "c-stale", 15*time.Second) {
		t.Fatal("the stale member was not evicted, so this test observes nothing about what follows one")
	}

	// THE LANE BOUNDARY. Eviction removes a SERVER from a configuration. Claiming
	// the WORK that server was doing belongs to the consumer that reads the
	// signal, and nothing here may reassign, rewrite or recover it.
	entry, found, err := l.Get(ctx, "work/owned-by-c-stale")
	if err != nil {
		t.Fatalf("reading the datum after the eviction: %v", err)
	}
	if !found {
		t.Fatal("the evicted peer's datum was removed by the eviction path")
	}
	if string(entry.Value) != "c-stale" {
		t.Fatalf("the evicted peer's datum was reassigned to %q: claiming an evicted peer's work is not "+
			"this package's decision", entry.Value)
	}
}

// addStaleNonvoter puts a NONVOTER into a flow's configuration at an address
// nothing answers. It mirrors addStaleVoter and exists because the two cases are
// what the suffrage bound distinguishes: a staged joiner that died before
// promotion costs the group no voting power to remove.
func addStaleNonvoter(t *testing.T, r *raft.Raft, id, addr string) {
	t.Helper()
	if err := r.AddNonvoter(raft.ServerID(id), raft.ServerAddress(addr), 0, 0).Error(); err != nil {
		t.Fatalf("planting the stale nonvoter %s: %v", id, err)
	}
}

func TestAPeerSignalNeverNamesThePublishingNode(t *testing.T) {
	mgr, _ := testNode(t, "a-leader")
	// noteHealth is driven DIRECTLY with autopilot's FIRST published state: the
	// one in which nothing has yet been stable for ServerStabilizationTime, so
	// every member reads unhealthy — the leader included. That is the state in
	// which a self-naming signal is published, and driving it by hand makes the
	// assertion deterministic rather than a race against a reconcile round.
	pilot := &flowPilot{mgr: mgr, flow: "alpha", done: make(chan struct{}), healthy: map[raft.ServerID]bool{}}
	pilot.noteHealth(&autopilotState{health: map[raft.ServerID]bool{
		raft.ServerID(mgr.cfg.Node): false,
		"b-peer":                    false,
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	batch, _, err := mgr.Watch(ctx, 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	peerSignals := 0
	for _, sig := range batch {
		switch sig.Kind {
		case SignalPeerUnreachable, SignalPeerReturned, SignalPeerEvicted:
			peerSignals++
			if sig.Node == mgr.cfg.Node {
				t.Fatalf("a peer signal of kind %v names the publishing node %q: a node reporting itself "+
					"unreachable is definitionally reachable, and a consumer would treat the live leader's "+
					"own work as orphaned", sig.Kind, sig.Node)
			}
		case SignalMembershipChanged:
			// Carries this node's own id BY DESIGN — it is this node reporting that
			// IT applied a configuration, which is the every-node half of the seam.
		}
	}
	// THE VACUITY CONTROL. "No signal in this set names me" is satisfied trivially
	// by an empty set, so the peer signal has to have been published at all.
	if peerSignals == 0 {
		t.Fatal("CONTROL FAILED: no peer-kind signal was published, so the never-names-self assertion " +
			"was vacuous")
	}
	t.Logf("no peer signal named the publishing node across %d peer-kind signals", peerSignals)
}

func TestManagerMembershipAgreesWithTheConfigurationTheCursorSignalled(t *testing.T) {
	leader := newClusterNode(t, "a-leader", []string{"alpha"}, 0)
	leader.start(t)
	leader.awaitLeader(t, "alpha")
	if _, ok := awaitSignal(t, leader.mgr, SignalMembershipChanged, 20*time.Second); !ok {
		t.Fatal("no membership signal arrived, so there is nothing to agree with")
	}

	// THIS IS THE SURFACE A CONSUMER REBUILDS FROM when its cursor is refused with
	// ErrCursorTooOld. The refusal was gated while the surface it names was
	// executed by nothing at all.
	configuration, index, hosted := leader.mgr.Membership("alpha")
	if !hosted {
		t.Fatal("Membership reports the flow as unhosted on the node that hosts it")
	}
	if index == 0 {
		t.Fatal("Membership reports index 0 after a configuration was signaled")
	}
	got := make([]string, 0, len(configuration.Servers))
	for _, server := range configuration.Servers {
		got = append(got, string(server.ID))
	}
	sort.Strings(got)
	want := memberIDs(t, leader.raftFor(t, "alpha"))
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Membership reports %v, raft's committed configuration reports %v", got, want)
	}

	// AND A FLOW THIS NODE DOES NOT HOST IS REPORTED UNHOSTED rather than as an
	// empty configuration, which a consumer could not tell from a real one.
	if _, _, hosted := leader.mgr.Membership("no-such-flow"); hosted {
		t.Fatal("Membership reports an unhosted flow as hosted")
	}
}

func TestEvictionSubtractsAVoteOnlyWhenTheVictimHoldsOne(t *testing.T) {
	// ARM ONE: a staged joiner that died before promotion. It is a NONVOTER, so
	// removing it costs the group no voting power — and under the unconditional
	// subtraction it was un-evictable whenever the voter count equalled the live
	// count, which is the steady state, so stale nonvoters accumulated forever.
	t.Run("a stale nonvoter is evicted", func(t *testing.T) {
		leader, live := evictionCluster(t, 2)
		addStaleNonvoter(t, leader.raftFor(t, "alpha"), "c-ghost", unreachableAddress(t))
		leader.mgr.resolve = func(context.Context, string) ([]string, error) {
			return []string{leader.addr, live.addr}, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		leader.mgr.evictRound(ctx, "alpha")
		if !awaitAbsent(t, leader.raftFor(t, "alpha"), "c-ghost", 15*time.Second) {
			t.Fatalf("a stale NONVOTER was not evicted though its removal costs no vote; members are %v",
				memberIDs(t, leader.raftFor(t, "alpha")))
		}
		t.Logf("a stale nonvoter was evicted under a complete resolution: members are now %v",
			memberIDs(t, leader.raftFor(t, "alpha")))
	})

	// ARM TWO IS THE CONTROL THAT KEEPS THE BOUND HONEST. Without it a fix that
	// simply deleted bound 2 would satisfy arm one. A stale VOTER whose removal
	// would drop the voter count below the number of live instances must still be
	// refused.
	t.Run("a stale voter that would drop the count is refused", func(t *testing.T) {
		leader := newClusterNode(t, "a-leader", []string{"alpha"}, 0)
		leader.start(t)
		leader.awaitLeader(t, "alpha")
		leader.mgr.cfg.Expect = 2
		leader.mgr.cfg.Peers = "peers.invalid:0"
		addStaleVoter(t, leader.raftFor(t, "alpha"), "d-stale", unreachableAddress(t))
		before := voterIDs(t, leader.raftFor(t, "alpha"))

		// Two voters in the configuration, two live instances reported, and the
		// stale voter absent from the resolution: removing it would leave one voter
		// against two live instances.
		newcomer := unreachableAddress(t)
		leader.mgr.resolve = func(context.Context, string) ([]string, error) {
			return []string{leader.addr, newcomer}, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		leader.mgr.evictRound(ctx, "alpha")
		time.Sleep(300 * time.Millisecond)

		after := voterIDs(t, leader.raftFor(t, "alpha"))
		if len(after) != len(before) {
			t.Fatalf("a stale VOTER was evicted though its removal drops the voter set from %v to %v, "+
				"below the live count: the conditional cost deleted bound 2 rather than refining it",
				before, after)
		}
	})
}

// TestSignalKindNamesEveryKind pins the rendering a consumer and every failure
// message in this package read. An unnamed kind reaching a log or an assertion
// message reads as a number nobody can act on, and the default arm is what keeps
// a kind added later from rendering as an empty string.
func TestSignalKindNamesEveryKind(t *testing.T) {
	for kind, want := range map[SignalKind]string{
		SignalMembershipChanged: "membership-changed",
		SignalPeerUnreachable:   "peer-unreachable",
		SignalPeerReturned:      "peer-returned",
		SignalPeerEvicted:       "peer-evicted",
	} {
		if got := kind.String(); got != want {
			t.Fatalf("SignalKind(%d).String() = %q, want %q", uint8(kind), got, want)
		}
	}
	// A kind outside the declared set renders as something an operator can still
	// act on rather than as an empty string.
	undeclared := SignalKind(200).String()
	if undeclared == "" || !strings.Contains(undeclared, "200") {
		t.Fatalf("an undeclared kind rendered as %q, which names neither the kind nor its number", undeclared)
	}
}

// publishInto puts one signal into a log directly, so a test can drive the
// numbering without standing up a cluster.
func publishInto(log *signalLog, node string) {
	log.publish(Signal{Kind: SignalPeerEvicted, Flow: "alpha", Node: node})
}

// lastIndex reports the cursor a reader would be holding after the last publish.
func lastIndex(t *testing.T, log *signalLog) uint64 {
	t.Helper()
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.retained) == 0 {
		t.Fatal("the log holds no signals, so there is no cursor to carry")
	}
	return log.retained[len(log.retained)-1].Index
}

func TestASignalCursorFromAnotherIncarnationIsRefusedNotSilentlyEmpty(t *testing.T) {
	// THE RESTART SHAPE. A cursor minted before a restart is applied to a fresh
	// log numbering from the beginning again. Unguarded it clears the retention
	// check — the fresh log's oldest is 1, so nothing is "older" — and the
	// collection loop then finds nothing, so the reader is told NOTHING.
	before := newSignalLog()
	for i := 0; i < 3; i++ {
		publishInto(before, "n")
	}
	carried := lastIndex(t, before)

	after := newSignalLog()
	for i := 0; i < 3; i++ {
		publishInto(after, "n")
	}
	if before.incarnation == after.incarnation {
		t.Fatal("CONTROL FAILED: two logs minted the same incarnation, so this arm cannot distinguish them")
	}

	batch := after.since(carried)
	if batch.err == nil {
		t.Fatalf("a pre-restart cursor was answered with %d signals and no error: that is silence, and the "+
			"blocking read parks on it exactly when a recovery loop needs it", len(batch.signals))
	}
	if !errors.Is(batch.err, ErrCursorTooOld) {
		t.Fatalf("err = %v, does not satisfy errors.Is against ErrCursorTooOld: a consumer keyed on the "+
			"sentinel this plan published would not recognize it", batch.err)
	}
	if !errors.Is(batch.err, ErrCursorForeignIncarnation) {
		t.Fatalf("err = %v, want it to also name the foreign incarnation so a restart is distinguishable "+
			"from a retention overrun", batch.err)
	}

	// THE CONTROL: the fresh log still serves from the beginning, so the refusal
	// above is about the cursor rather than a log that refuses everything.
	fresh := after.since(0)
	if fresh.err != nil {
		t.Fatalf("CONTROL FAILED: the fresh log refused a zero cursor: %v", fresh.err)
	}
	if len(fresh.signals) != 3 {
		t.Fatalf("CONTROL FAILED: the fresh log served %d signals from the beginning, want 3", len(fresh.signals))
	}
	t.Logf("a pre-restart cursor is refused by name and the fresh log still serves from the beginning: %v",
		batch.err)

	// THE CROSS-NODE SHAPE, GATED SEPARATELY rather than argued to be subsumed by
	// the restart one. The cursor space carries no instance identity, so a
	// consumer that reconnects to a DIFFERENT node applies one instance's
	// numbering to another's — and under ephemeral identity peer rotation is the
	// designed steady state, not an exceptional case.
	nodeA, _ := testNode(t, "a-node")
	nodeB, _ := testNode(t, "b-node")
	for i := 0; i < 3; i++ {
		publishInto(nodeA.signals, "peer")
		publishInto(nodeB.signals, "peer")
	}
	if nodeA.signals.incarnation == nodeB.signals.incarnation {
		t.Fatal("CONTROL FAILED: two nodes minted the same incarnation")
	}
	crossed := nodeB.signals.since(lastIndex(t, nodeA.signals))
	if crossed.err == nil {
		t.Fatalf("a cursor from another node was answered with %d signals and no error", len(crossed.signals))
	}
	if !errors.Is(crossed.err, ErrCursorTooOld) {
		t.Fatalf("err = %v, does not satisfy errors.Is against ErrCursorTooOld", crossed.err)
	}
	t.Logf("a cursor carried to a different node is refused by name: %v", crossed.err)
}

func TestWatchRefusesAForeignCursorRatherThanParkingOnIt(t *testing.T) {
	mgr, _ := testNode(t, "a-node")
	for i := 0; i < 3; i++ {
		publishInto(mgr.signals, "peer")
	}
	// A cursor whose incarnation is definitely not this log's.
	foreign := (mgr.signals.incarnation + 1) << signalIncarnationShift

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err := mgr.Watch(ctx, foreign)
	elapsed := time.Since(start)

	// NEVER SILENCE AND NEVER A PARK. This is what makes the defect a blocking
	// one rather than a nuisance: a recovery loop hangs exactly when recovery is
	// needed.
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Watch parked on a foreign cursor for %v and returned the context error: a consumer's "+
			"recovery loop hangs here", elapsed)
	}
	if !errors.Is(err, ErrCursorTooOld) {
		t.Fatalf("err = %v, want a refusal satisfying ErrCursorTooOld", err)
	}
	// A gate that accepted a SLOW refusal would pass against an implementation
	// that still parked and merely happened to be woken.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("the refusal took %v, which is slow enough that it may have parked and been woken", elapsed)
	}

	// THE CONTROL THAT KEEPS THE GUARD FROM BEING A BLANKET REFUSAL: a
	// SAME-incarnation cursor must still resume, and from the right place.
	batch, cursor, err := mgr.Watch(ctx, 0)
	if err != nil || len(batch) != 3 {
		t.Fatalf("CONTROL FAILED: reading from the beginning gave %d signals, err=%v", len(batch), err)
	}
	for i := 0; i < 2; i++ {
		publishInto(mgr.signals, "later")
	}
	resumed, _, err := mgr.Watch(ctx, cursor)
	if err != nil {
		t.Fatalf("CONTROL FAILED: a same-incarnation cursor was refused: %v", err)
	}
	if len(resumed) != 2 {
		t.Fatalf("CONTROL FAILED: resuming gave %d signals, want exactly the 2 published after the cursor",
			len(resumed))
	}
	t.Logf("a foreign cursor is refused immediately and a same-incarnation cursor still resumes: "+
		"refused in %v, resumed with %d", elapsed, len(resumed))
}

func TestASequenceOverflowRollsTheIncarnationAndRefusesOutstandingCursors(t *testing.T) {
	log := newSignalLog()
	publishInto(log, "early")
	outstanding := lastIndex(t, log)
	first := log.incarnation

	// Drive the sequence to its last usable value. Unreachable in production,
	// because signals are published on membership events — and gated anyway,
	// because a silent wrap corrupts the discriminator undetectably.
	log.mu.Lock()
	log.next = signalSequenceMask
	log.mu.Unlock()
	publishInto(log, "at-the-edge")

	edge := lastIndex(t, log)
	if edge&signalSequenceMask != signalSequenceMask {
		t.Fatalf("the edge signal carries sequence %d, want the mask %d", edge&signalSequenceMask,
			signalSequenceMask)
	}
	if edge>>signalIncarnationShift != first {
		t.Fatalf("the sequence carried into the incarnation bits at the edge: incarnation reads %d, want %d",
			edge>>signalIncarnationShift, first)
	}

	// The next publish would carry, so it must roll instead.
	publishInto(log, "after-the-roll")
	log.mu.Lock()
	rolled := log.incarnation
	retained := append([]Signal(nil), log.retained...)
	log.mu.Unlock()

	if rolled == first {
		t.Fatal("the sequence reached the mask and the incarnation did not roll: the next publish carries " +
			"into the incarnation bits and the discriminator is silently corrupt")
	}
	if len(retained) != 1 {
		t.Fatalf("the retained window holds %d signals after the roll, want only the post-roll one: the old "+
			"window belongs to the old numbering and the sequence comparison assumes one numbering",
			len(retained))
	}
	if got := retained[0].Index & signalSequenceMask; got != 1 {
		t.Fatalf("the first post-roll signal carries sequence %d, want 1", got)
	}
	if got := retained[0].Index >> signalIncarnationShift; got != rolled {
		t.Fatalf("the first post-roll signal carries incarnation %d, want the rolled %d", got, rolled)
	}

	// A cursor outstanding across the roll SHOULD be refused, which is exactly
	// what a restart already gets.
	batch := log.since(outstanding)
	if batch.err == nil {
		t.Fatalf("a cursor outstanding across the roll was answered with %d signals and no error",
			len(batch.signals))
	}
	if !errors.Is(batch.err, ErrCursorTooOld) {
		t.Fatalf("err = %v, want a refusal satisfying ErrCursorTooOld", batch.err)
	}
	t.Logf("a sequence overflow rolls the incarnation, drops the old window, and refuses outstanding "+
		"cursors: %v", batch.err)
}
