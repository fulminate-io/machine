package membership

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/ledger"
	"github.com/whitaker-io/machine/raft/transport"
)

// clusterNode is one worker in a test cluster: its shared listener, its manager,
// and the address peers reach it at.
// testGeneration is the one deployment epoch every node in a test shares, so a
// helper cannot accidentally build two nodes that refuse each other.
const testGeneration = 7

type clusterNode struct {
	mgr  *Manager
	mux  *transport.Mux
	id   string
	addr string
}

// newClusterNode builds a worker whose peer set is supplied directly rather than
// resolved, because several instances on distinct ports of one loopback host is
// a set DNS cannot express.
func newClusterNode(t *testing.T, id string, flows []string, expect int) *clusterNode {
	t.Helper()
	mux, err := transport.New(transport.Config{
		BindAddr:         "127.0.0.1:0",
		HandshakeTimeout: 2 * time.Second,
		RPCTimeout:       2 * time.Second,
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	addr := mux.Addr().String()
	cfg := Config{
		Node: id, Advertise: addr, Mux: mux, Logger: hclog.NewNullLogger(),
		Flows: flows, Expect: expect,
		Open: func(flow string) (*ledger.Ledger, error) {
			return ledger.Open(ledger.Config{Flow: flow, LocalID: id, Mux: mux})
		},
	}
	// THE GENERATION IS SET UNCONDITIONALLY, unlike Peers. It is a property of
	// the deployment rather than of whether this node has a peers address, and a
	// helper that set it only alongside Peers builds a leader at generation zero
	// whose followers are at seven — which the acceptor correctly refuses, and
	// which reads as a join that hangs.
	cfg.Generation = testGeneration
	if expect > 0 {
		cfg.Peers = "peers.invalid:0"
	}
	mgr, err := New(cfg)
	if err != nil {
		_ = mux.Close()
		t.Fatalf("membership.New(%s): %v", id, err)
	}
	t.Cleanup(func() {
		_ = mgr.Close()
		_ = mux.Close()
	})
	return &clusterNode{mgr: mgr, mux: mux, id: id, addr: addr}
}

// peering points a node's discovery at an explicit address set.
func (n *clusterNode) peering(addrs ...string) {
	n.mgr.resolve = func(context.Context, string) ([]string, error) {
		return append([]string(nil), addrs...), nil
	}
}

// start settles the node's flows, failing the test if it cannot.
func (n *clusterNode) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := n.mgr.Start(ctx); err != nil {
		t.Fatalf("Start(%s): %v", n.id, err)
	}
}

// raftFor reaches a flow's raft handle on this node.
func (n *clusterNode) raftFor(t *testing.T, flow string) *raft.Raft {
	t.Helper()
	l, ok := n.mgr.Ledger(flow)
	if !ok {
		t.Fatalf("node %s does not host flow %q", n.id, flow)
	}
	return l.Raft()
}

// awaitLeader blocks until a flow has a leader on this node's view.
func (n *clusterNode) awaitLeader(t *testing.T, flow string) {
	t.Helper()
	r := n.raftFor(t, flow)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if r.State() == raft.Leader {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("node %s never became leader of flow %q", n.id, flow)
}

// suffrageIn reports how id is recorded in a flow's committed configuration.
func suffrageIn(t *testing.T, r *raft.Raft, id string) (raft.ServerSuffrage, bool) {
	t.Helper()
	future := r.GetConfiguration()
	if err := future.Error(); err != nil {
		t.Fatalf("GetConfiguration: %v", err)
	}
	for _, server := range future.Configuration().Servers {
		if string(server.ID) == id {
			return server.Suffrage, true
		}
	}
	return 0, false
}

// awaitSuffrage waits for id to reach want in a flow's committed configuration.
func awaitSuffrage(t *testing.T, r *raft.Raft, id string, want raft.ServerSuffrage, window time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if got, ok := suffrageIn(t, r, id); ok && got == want {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func TestAnnounceStagesTheJoinerAsANonvoter(t *testing.T) {
	leader := newClusterNode(t, "a-leader", []string{"alpha"}, 0)
	leader.start(t)
	leader.awaitLeader(t, "alpha")

	joiner := newClusterNode(t, "b-joiner", []string{"alpha"}, 2)
	joiner.peering(leader.addr)
	joiner.start(t)

	r := leader.raftFor(t, "alpha")
	got, ok := suffrageIn(t, r, "b-joiner")
	if !ok {
		t.Fatal("the joiner is absent from the leader's committed configuration")
	}
	// THE DISCRIMINATING ASSERTION. An implementation that staged with AddVoter
	// would satisfy "present in the configuration" and report Voter here.
	if got != raft.Nonvoter {
		t.Fatalf("the joiner is recorded at suffrage %v, want Nonvoter: admission must not raise quorum", got)
	}
}

func TestAnnounceToANonLeaderRedirectsToTheFlowsLeader(t *testing.T) {
	leader := newClusterNode(t, "a-leader", []string{"alpha"}, 0)
	leader.start(t)
	leader.awaitLeader(t, "alpha")

	follower := newClusterNode(t, "b-follower", []string{"alpha"}, 2)
	follower.peering(leader.addr)
	follower.start(t)
	// THE PRECONDITION, and it is a precondition rather than a workaround. A
	// redirect names the address raft believes leads the flow, so a node that has
	// been staged but has not yet received a single AppendEntries has nothing to
	// name and correctly refuses instead. MEASURED before this wait existed: 2
	// failures in 8 runs, every one of them "flow alpha has no known leader to
	// redirect to" — the implementation behaving correctly against a test that had
	// not established the state it was asserting about.
	awaitKnowsLeader(t, follower, "alpha")

	// The follower hosts the flow and does not lead it, which is the case that
	// must produce a redirect rather than a refusal.
	third := newClusterNode(t, "c-third", []string{"alpha"}, 3)
	third.peering(follower.addr, leader.addr)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	reply, err := third.mgr.announceTo(ctx, follower.addr, "alpha")
	if err != nil {
		t.Fatalf("announce to the follower: %v", err)
	}
	to, ok := reply.Redirects["alpha"]
	if !ok {
		t.Fatalf("the follower did not redirect; it answered staged=%v refused=%v", reply.Staged, reply.Refused)
	}
	if to != leader.addr {
		t.Fatalf("the redirect named %q, want the flow's leader %q", to, leader.addr)
	}

	// AND THE REDIRECT STAGES: the joiner re-announces to the address it named.
	third.start(t)
	if _, ok := suffrageIn(t, leader.raftFor(t, "alpha"), "c-third"); !ok {
		t.Fatal("the re-announce to the redirected address did not stage the joiner")
	}
}

func TestAnnounceRefusesAFlowTheReceiverDoesNotHost(t *testing.T) {
	host := newClusterNode(t, "a-host", []string{"alpha"}, 0)
	host.start(t)
	host.awaitLeader(t, "alpha")

	asker := newClusterNode(t, "b-asker", nil, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	reply, err := asker.mgr.announceTo(ctx, host.addr, "bravo")
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if len(reply.Staged) != 0 {
		t.Fatalf("a flow the receiver does not host was staged: %v", reply.Staged)
	}
	reason, ok := reply.Refused["bravo"]
	if !ok {
		t.Fatal("the unhosted flow was omitted rather than refused: a silent omission is indistinguishable " +
			"from a lost message")
	}
	if !strings.Contains(reason, "bravo") {
		t.Fatalf("the refusal %q does not name the flow it refused", reason)
	}
	// THE CONTROL: the same receiver DOES answer for the flow it hosts, so the
	// refusal above is about the flow rather than about a node that answers
	// nothing.
	ok2, err := asker.mgr.announceTo(ctx, host.addr, "alpha")
	if err != nil {
		t.Fatalf("CONTROL FAILED: announce for the hosted flow: %v", err)
	}
	if len(ok2.Staged) == 0 && len(ok2.Redirects) == 0 {
		t.Fatalf("CONTROL FAILED: the hosted flow was neither staged nor redirected: %+v", ok2)
	}
}

func TestAFlowNeitherBootstrappedNorReachableIsAnError(t *testing.T) {
	// THE INPUT THAT MATTERS: a node whose peers probe has NOT seen Expect
	// instances answer. Nothing hosts the flow and nobody else is up, so the
	// count-and-lowest-id rule is unsatisfied and stays that way.
	lonely := newClusterNode(t, "a-lonely", []string{"orphan"}, 3)
	lonely.peering()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := lonely.mgr.Start(ctx)
	if err == nil {
		t.Fatal("Start created a group for an unplaceable flow: two workers each creating a one-voter group " +
			"for one flow produce two logs that can never merge")
	}
	if !errors.Is(err, ErrFlowUnplaced) {
		t.Fatalf("err = %v, want ErrFlowUnplaced", err)
	}
	if !strings.Contains(err.Error(), "orphan") {
		t.Fatalf("the error %q does not name the flow it refused", err)
	}

	// AND NOTHING WAS CREATED: the ledger exists but its group does not, which is
	// what separates a refusal from a self-bootstrap that also happened to error.
	r := lonely.raftFor(t, "orphan")
	future := r.GetConfiguration()
	if future.Error() == nil && len(future.Configuration().Servers) != 0 {
		t.Fatalf("a group was created anyway, with %d servers", len(future.Configuration().Servers))
	}
}

// newIdentityNode builds a worker whose MANAGER node id and whose LEDGER raft
// server id are supplied SEPARATELY, which is exactly how a caller supplies them
// today: membership reaches a ledger only through Config.Open, which builds the
// ledger.Config out of this package's sight. Passing the same string twice is the
// agreed shape; passing two different ones is the divergence the guard refuses.
func newIdentityNode(t *testing.T, node, ledgerID string, flows []string) *clusterNode {
	t.Helper()
	mux, err := transport.New(transport.Config{
		BindAddr:         "127.0.0.1:0",
		HandshakeTimeout: 2 * time.Second,
		RPCTimeout:       2 * time.Second,
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	addr := mux.Addr().String()
	mgr, err := New(Config{
		Node: node, Advertise: addr, Mux: mux, Logger: hclog.NewNullLogger(), Flows: flows,
		Open: func(flow string) (*ledger.Ledger, error) {
			return ledger.Open(ledger.Config{Flow: flow, LocalID: ledgerID, Mux: mux})
		},
	})
	if err != nil {
		_ = mux.Close()
		t.Fatalf("membership.New(%s): %v", node, err)
	}
	t.Cleanup(func() {
		_ = mgr.Close()
		_ = mux.Close()
	})
	return &clusterNode{mgr: mgr, mux: mux, id: node, addr: addr}
}

// TestALedgerWhoseIdentityDisagreesWithTheManagerIsRefused gates the first of the
// two open sites. Config.Node stamps every signal this package publishes and the
// ledger's LocalID stamps every entry in the configuration; supplied separately
// and left to agree by luck, a node bootstraps a group under one identity and
// evaluates its own membership under the other.
func TestALedgerWhoseIdentityDisagreesWithTheManagerIsRefused(t *testing.T) {
	node := newIdentityNode(t, "a-node", "someone-else", []string{"alpha"})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := node.mgr.Start(ctx)
	if err == nil {
		t.Fatal("Start accepted a ledger running under a raft server id this manager does not use: the node " +
			"would bootstrap under one identity and evaluate its own membership under the other")
	}
	if !errors.Is(err, ErrIdentityDiverged) {
		t.Fatalf("err = %v, want ErrIdentityDiverged", err)
	}
	// BOTH VALUES, because an operator supplied them as two separate fields and a
	// refusal naming neither is one they cannot act on.
	if !strings.Contains(err.Error(), "someone-else") || !strings.Contains(err.Error(), "a-node") {
		t.Fatalf("the refusal %q names fewer than both values: the operator supplied ledger.Config.LocalID "+
			"and membership Config.Node separately and has to be told which two disagree", err)
	}
	if _, adopted := node.mgr.Ledger("alpha"); adopted {
		t.Fatal("the refused flow was adopted anyway: a refusal that still hosts the flow refuses nothing")
	}
	// THE REFUSED LEDGER WAS CLOSED. An open one holds the flow's group id bound on
	// the mux, so if the guard leaked it this second open fails with a bind refusal
	// naming an unrelated cause — which is what an operator would then chase.
	second, openErr := ledger.Open(ledger.Config{Flow: "alpha", LocalID: "a-node", Mux: node.mux})
	if openErr != nil {
		t.Fatalf("re-opening the flow after the refusal: %v — the refused ledger was left holding the "+
			"flow's group id on the mux", openErr)
	}
	t.Cleanup(func() { _ = second.Close() })

	t.Logf("Start refused a diverged identity naming both values, and released the flow's group id: %v", err)
}

// TestAgreedIdentitiesStartWithoutTheRefusalFiring is the CONTROL that keeps the
// two refusal arms from being satisfied by a manager that refuses every ledger it
// ever opens. The agreed shape has to still start, reach leadership, and report a
// ledger whose raft server id is this manager's node id.
func TestAgreedIdentitiesStartWithoutTheRefusalFiring(t *testing.T) {
	node := newIdentityNode(t, "a-node", "a-node", []string{"alpha"})
	node.start(t)
	node.awaitLeader(t, "alpha")

	l, ok := node.mgr.Ledger("alpha")
	if !ok {
		t.Fatal("the agreed shape did not adopt its flow")
	}
	if l.LocalID() != node.mgr.cfg.Node {
		t.Fatalf("the ledger reports %q and Config.Node is %q on the shape that is supposed to agree",
			l.LocalID(), node.mgr.cfg.Node)
	}

	t.Logf("agreed identities start cleanly and the guard does not fire: flow %q reached leadership with "+
		"the ledger reporting %q and Config.Node %q", "alpha", l.LocalID(), node.mgr.cfg.Node)
}

// TestJoiningAFlowAlsoRefusesADivergedIdentity gates the SECOND open site. Start
// and joinFlow open on the same terms through one helper, and the moment they
// stop doing so is the drift this package's flow-set half is written to avoid —
// so the two sites are gated separately rather than one argued to cover the other.
func TestJoiningAFlowAlsoRefusesADivergedIdentity(t *testing.T) {
	node := newIdentityNode(t, "a-node", "someone-else", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Starts with no flows, so the divergence is never seen by Start: the flow is
	// taken on afterwards, which is the path SetFlows drives.
	if err := node.mgr.Start(ctx); err != nil {
		t.Fatalf("Start with no flows: %v", err)
	}

	err := node.mgr.SetFlows(ctx, []string{"beta"})
	if err == nil {
		t.Fatal("a flow taken on after start adopted a ledger running under a raft server id this manager " +
			"does not use, which Start refuses")
	}
	if !errors.Is(err, ErrIdentityDiverged) {
		t.Fatalf("err = %v, want ErrIdentityDiverged", err)
	}
	if !strings.Contains(err.Error(), "someone-else") || !strings.Contains(err.Error(), "a-node") ||
		!strings.Contains(err.Error(), "beta") {
		t.Fatalf("the refusal %q names fewer than the flow and both identities", err)
	}
	if _, adopted := node.mgr.Ledger("beta"); adopted {
		t.Fatal("the refused flow was adopted anyway")
	}

	t.Logf("the second open site refuses a diverged identity too: %v", err)
}

// TestADivergedIdentityWouldMakeTheSelfExclusionFailOpen is A RECORD OF WHY THE
// GUARD IS LOAD-BEARING, NOT A GATE ON IT, and it is written that way on purpose.
//
// It drives noteHealth DIRECTLY, below the guard, because the guard is what makes
// this state unreachable through Start — so it passes with the guard present and
// without it. Its value is that the consequence is demonstrated by execution
// rather than argued in prose: the self-exclusion compares a raft ServerID out of
// autopilot's state against Config.Node, so under divergence it never matches and
// the leader publishes a peer-unreachable signal naming its own raft identity. If
// the exclusion is ever rewritten in a way that changes this, the change is
// visible here.
func TestADivergedIdentityWouldMakeTheSelfExclusionFailOpen(t *testing.T) {
	mgr, mux := testNode(t, "a-node")
	l, err := ledger.Open(ledger.Config{Flow: "alpha", LocalID: "someone-else", Mux: mux})
	if err != nil {
		t.Fatalf("opening a ledger under a divergent raft server id: %v", err)
	}
	mgr.addFlow("alpha", l)
	if l.LocalID() == mgr.cfg.Node {
		t.Fatal("CONTROL FAILED: the fixture is not diverged, so nothing below observes what divergence does")
	}

	// Autopilot's FIRST published state, in which nothing has been stable for
	// ServerStabilizationTime yet and every member reads unhealthy — the node
	// running this loop included, under the raft id it actually runs under.
	pilot := &flowPilot{mgr: mgr, flow: "alpha", done: make(chan struct{}), healthy: map[raft.ServerID]bool{}}
	pilot.noteHealth(&autopilotState{health: map[raft.ServerID]bool{
		raft.ServerID(l.LocalID()): false,
		"b-peer":                   false,
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	batch, _, err := mgr.Watch(ctx, 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	peerSignals, namedSelf := 0, false
	for _, sig := range batch {
		switch sig.Kind {
		case SignalPeerUnreachable, SignalPeerReturned, SignalPeerEvicted:
			peerSignals++
			if sig.Node == l.LocalID() {
				namedSelf = true
			}
		case SignalMembershipChanged:
			// Carries this node's own id BY DESIGN — the every-node half of the seam.
		}
	}
	if peerSignals == 0 {
		t.Fatal("CONTROL FAILED: no peer-kind signal was published, so the observation below would be vacuous")
	}
	if !namedSelf {
		t.Fatalf("the diverged node published %d peer signals and none named its own raft identity %q: the "+
			"recorded consequence no longer reproduces, so either the exclusion or the identity plumbing "+
			"changed and this record needs re-reading", peerSignals, l.LocalID())
	}

	t.Logf("the self-exclusion fails open and the node names itself: with Config.Node %q and the raft server "+
		"id %q this node actually runs under, a peer-unreachable signal named %q — a consumer reads that to "+
		"decide a peer's datums may be orphaned", mgr.cfg.Node, l.LocalID(), l.LocalID())
}

// TestARedirectWithNoKnownLeaderIsRefusedByName pins the arm a node reaches when
// it hosts a flow and knows of no leader for it: it refuses by name rather than
// inventing an address. This is the path a freshly staged nonvoter occupies
// before its first AppendEntries, and a redirect naming an empty address would
// send a joiner nowhere while reading as success.
func TestARedirectWithNoKnownLeaderIsRefusedByName(t *testing.T) {
	node := newClusterNode(t, "a-node", nil, 0)
	// A ledger opened but never placed: it hosts the flow and its group has never
	// formed, so raft knows no leader.
	l, err := ledger.Open(ledger.Config{Flow: "orphan", LocalID: node.id, Mux: node.mux})
	if err != nil {
		t.Fatalf("opening an unplaced ledger: %v", err)
	}
	node.mgr.addFlow("orphan", l)

	reply := node.mgr.answerAnnounce(announce{Node: "b-joiner", Address: "127.0.0.1:1", Flows: []string{"orphan"}, Generation: testGeneration})
	if to, redirected := reply.Redirects["orphan"]; redirected {
		t.Fatalf("a node that knows no leader redirected to %q: a joiner would be sent nowhere while this "+
			"read as success", to)
	}
	if len(reply.Staged) != 0 {
		t.Fatalf("a node that leads nothing staged a joiner: %v", reply.Staged)
	}
	reason, refused := reply.Refused["orphan"]
	if !refused {
		t.Fatal("the flow was neither staged, redirected nor refused: a joiner cannot act on silence")
	}
	if !strings.Contains(reason, "orphan") {
		t.Fatalf("the refusal %q does not name the flow", reason)
	}
}

// TestTheLowestIdRuleIsWhatMakesGroupCreationSingleWriter drives the comparison
// the creation rule turns on. It is the whole reason two workers deploying the
// same new flow concurrently do not each bootstrap a one-voter group, and those
// two logs could never merge — so the comparison deserves a test of its own
// rather than only being exercised through a cluster.
func TestTheLowestIdRuleIsWhatMakesGroupCreationSingleWriter(t *testing.T) {
	mgr, _ := testNode(t, "b-middle")
	for _, tc := range []struct {
		name    string
		answers map[string]announceReply
		want    bool
	}{
		{"nobody else answered", map[string]announceReply{}, true},
		{"this node is lowest", map[string]announceReply{
			"x": {Node: "c-high"}, "y": {Node: "d-higher"},
		}, true},
		{"another node is lower", map[string]announceReply{
			"x": {Node: "a-low"}, "y": {Node: "c-high"},
		}, false},
		{"the lowest is lower by one character", map[string]announceReply{
			"x": {Node: "b-middl"},
		}, false},
		{"an answer that did not identify its sender is ignored", map[string]announceReply{
			"x": {Node: ""},
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mgr.lowestID(tc.answers); got != tc.want {
				t.Fatalf("lowestID = %v, want %v: this decides which node creates the group, and both "+
					"answers being wrong produces two logs that can never merge", got, tc.want)
			}
		})
	}
}
