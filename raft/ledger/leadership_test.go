package ledger

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/transport"
)

// clusterNode is one member of a real multi-node group: its own mux, its own
// ledger, its own raft server id.
type clusterNode struct {
	id     string
	mux    *transport.Mux
	ledger *Ledger
}

// newCluster stands up n ledgers for one flow on n fresh muxes.
func newCluster(t *testing.T, flow string, n int) []*clusterNode {
	t.Helper()

	return newClusterOn(t, flow, newMuxes(t, n))
}

// newMuxes binds n shared listeners, one per node.
func newMuxes(t *testing.T, n int) []*transport.Mux {
	t.Helper()

	muxes := make([]*transport.Mux, 0, n)
	for range n {
		muxes = append(muxes, testMux(t))
	}

	return muxes
}

// newClusterOn stands up one flow's group across already-bound muxes, so several
// flows can be raised on the SAME listeners — which is the whole point of the mux.
//
// Bootstrap is left off on the Config because Open's bootstrap elects a SINGLE
// voter; a real group is bootstrapped once, naming every member.
func newClusterOn(t *testing.T, flow string, muxes []*transport.Mux) []*clusterNode {
	t.Helper()

	nodes := make([]*clusterNode, 0, len(muxes))
	for i, mux := range muxes {
		id := fmt.Sprintf("n%d", i)
		nodes = append(nodes, &clusterNode{
			id:     id,
			mux:    mux,
			ledger: openTestLedger(t, Config{Flow: flow, LocalID: id, Mux: mux}),
		})
	}

	servers := make([]raft.Server, 0, len(nodes))
	for _, node := range nodes {
		servers = append(servers, raft.Server{
			ID:      raft.ServerID(node.id),
			Address: raft.ServerAddress(node.mux.Addr().String()),
		})
	}
	if err := nodes[0].ledger.Raft().BootstrapCluster(raft.Configuration{Servers: servers}).Error(); err != nil {
		t.Fatalf("bootstrapping the %q group: %v", flow, err)
	}

	return nodes
}

func waitClusterLeader(t *testing.T, nodes []*clusterNode) *clusterNode {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, node := range nodes {
			if node.ledger.Raft().State() == raft.Leader {
				return node
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no leader was elected within 30s")

	return nil
}

// fencedPath and fencedValue are committed on the FIRST term of the tests below and
// read back on every later one.
const (
	fencedPath  = "heap/fenced"
	fencedValue = "committed-on-an-earlier-term"
)

// readFirstOfTerm takes THE FIRST READ of a term with no write before it and
// requires it to return the value committed on an earlier term.
//
// TWO PROPERTIES, AND THE SECOND IS THE ONE THIS GATE EXISTS FOR. Convergence — the
// read completing rather than expiring — was the original property and it remains.
// It is NOT sufficient: a barrier whose target is trivially satisfied converges
// instantly and answers ABSENT for committed data, which is exactly what a barrier
// reading a fresh leader's commit index does. So the value is asserted, not the
// completion.
//
// The no-preceding-write clause is load-bearing: a write on the new term advances
// the tracked index past the election no-op by itself and masks BOTH failure modes.
func readFirstOfTerm(t *testing.T, node *clusterNode, term string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	commit, applied := node.ledger.raft.CommitIndex(), node.ledger.fsm.appliedIndex()
	entry, ok, err := node.ledger.Get(ctx, fencedPath)
	if err != nil {
		t.Fatalf("%s: the first read of the term on %s did not converge (CommitIndex %d, tracked %d): %v",
			term, node.id, commit, applied, err)
	}
	if !ok {
		t.Fatalf("%s: the first read of the term on %s answered ABSENT for a value committed on an earlier term (CommitIndex %d, tracked %d): the read completed and was WRONG",
			term, node.id, commit, applied)
	}
	if got := string(entry.Value); got != fencedValue {
		t.Fatalf("%s: the first read of the term on %s returned %q, want %q", term, node.id, got, fencedValue)
	}
}

// awaitEstablished blocks until this term's epoch entry has been appended AND
// applied, so nothing this ledger does asynchronously can still move the log index.
//
// A test that cannot confirm establishment fails as a CONTROL rather than sampling
// anyway, because an unfenced sample is exactly the contaminated measurement this
// helper exists to prevent.
func awaitEstablished(t *testing.T, l *Ledger) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if epoch, ok := l.establishment(l.raft.CurrentTerm()); ok && l.fsm.appliedIndex() >= epoch {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("CONTROL FAILED: flow %q never established its term within 30s, so any sample taken now would be unfenced", l.Flow())
}

// commitFencedValue writes the cell every later term's first read must return.
func commitFencedValue(t *testing.T, node *clusterNode) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := node.ledger.Append(ctx, Entry{Kind: KindSet, Path: fencedPath, Value: []byte(fencedValue)}); err != nil {
		t.Fatalf("committing the fenced value on %s: %v", node.id, err)
	}
}

func TestLinearizableReadConvergesOnTheFirstReadOfALeadershipTerm(t *testing.T) {
	nodes := newCluster(t, "flow-term-one", 3)
	first := waitClusterLeader(t, nodes)

	// Commit the value on THIS term, then move to a new one. The read under test is
	// the first of the NEW term, so the value it must return was committed on an
	// earlier term — the case a fresh leader's commit index cannot account for.
	commitFencedValue(t, first)

	second := otherThan(nodes, first)
	transferLeadership(t, first, second)
	readFirstOfTerm(t, second, "term 2")

	// CONTROL: the read really did go through the barrier on a leader rather than
	// returning early from some path that skips it. A follower is refused.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, node := range nodes {
		if node == second {
			continue
		}
		if _, _, err := node.ledger.Get(ctx, fencedPath); !errors.Is(err, ErrNotLeader) {
			t.Fatalf("CONTROL FAILED: a read on the follower %s gave %v, want ErrNotLeader", node.id, err)
		}

		break
	}
}

func TestFirstReadOfASecondLeadershipTermConverges(t *testing.T) {
	nodes := newCluster(t, "flow-terms", 3)

	first := waitClusterLeader(t, nodes)
	commitFencedValue(t, first)

	// Term 2: kill the leader outright. The two survivors hold quorum.
	if err := first.ledger.Close(); err != nil {
		t.Fatalf("closing the term-1 leader: %v", err)
	}
	survivors := make([]*clusterNode, 0, 2)
	for _, node := range nodes {
		if node != first {
			survivors = append(survivors, node)
		}
	}
	second := waitClusterLeader(t, survivors)
	readFirstOfTerm(t, second, "term 2")

	// Term 3: hand leadership to the other survivor.
	third := otherThan(survivors, second)
	transferLeadership(t, second, third)
	readFirstOfTerm(t, third, "term 3")

	// Term 4 IS THE ONE THAT CLOSES THE BOUND: leadership goes BACK to the node
	// that led term 2 and has already appended an epoch entry once. An
	// implementation that establishes a term once per NODE rather than once per
	// TERM passes terms 1 through 3 and stalls here.
	transferLeadership(t, third, second)
	if second.ledger.Raft().State() != raft.Leader {
		t.Fatalf("term 4 was expected to land back on %s, which has already led; it is %s",
			second.id, second.ledger.Raft().State())
	}
	readFirstOfTerm(t, second, "term 4 (returning leader)")
}

func otherThan(nodes []*clusterNode, exclude *clusterNode) *clusterNode {
	for _, node := range nodes {
		if node != exclude {
			return node
		}
	}

	return nil
}

func transferLeadership(t *testing.T, from, to *clusterNode) {
	t.Helper()

	future := from.ledger.Raft().LeadershipTransferToServer(
		raft.ServerID(to.id), raft.ServerAddress(to.mux.Addr().String()))
	if err := future.Error(); err != nil {
		t.Fatalf("transferring leadership from %s to %s: %v", from.id, to.id, err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if to.ledger.Raft().State() == raft.Leader {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never became leader after the transfer from %s", to.id, from.id)
}

func TestASlowEpochAppendDoesNotStallCommits(t *testing.T) {
	// This ledger is built by hand rather than opened: the property under test is
	// the drain loop's, and driving it directly is what lets the epoch append be
	// made slow without a cluster to slow down.
	l := &Ledger{
		cfg:    Config{Flow: "flow-saturate"},
		logger: hclog.NewNullLogger(),
		notify: make(chan bool, leadershipNotifyBuffer),
		done:   make(chan struct{}),
	}
	started := make(chan struct{}, leadershipNotifyBuffer+2)
	l.establish = func() {
		started <- struct{}{}
		time.Sleep(3 * time.Second)
	}
	l.startLeadershipDrain()
	t.Cleanup(func() { _ = l.Close() })

	// SATURATE THE CHANNEL. A single notification is absorbed by the buffer even by
	// a drain that is parked awaiting its append, so one transition would pass
	// against the very defect this gate exists to catch. Driving buffer+2 forces the
	// loop to be receivable while an append is still outstanding.
	notifications := leadershipNotifyBuffer + 2
	for i := range notifications {
		select {
		case l.notify <- true:
		case <-time.After(2 * time.Second):
			t.Fatalf("the leadership drain stopped receiving after %d of %d notifications, well inside a 3s epoch append: it is awaiting the append inline and raft's blocking send would wedge here",
				i, notifications)
		}
	}

	// CONTROL: the appends really were outstanding while those sends completed. If
	// nothing had started, the sends above would prove nothing about concurrency.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("CONTROL FAILED: no epoch append ever started, so the sends above did not race an outstanding append")
	}
}

func TestPrescribedWaitStallsWithoutTheLeadershipEntry(t *testing.T) {
	// The reproduction guard. It builds the prescribed read WITHOUT this package's
	// epoch entry — a raft instance over the ledger's own state machine and no
	// leadership drain — so the mechanism is proven necessary rather than assumed.
	f := newFSM()
	mux := testMux(t)
	group, err := mux.Bind(transport.GroupID("flow-norepro"))
	if err != nil {
		t.Fatalf("binding the group: %v", err)
	}
	t.Cleanup(func() { _ = group.Close() })

	cfg := raft.DefaultConfig()
	cfg.LocalID = raft.ServerID("n0")
	cfg.Logger = hclog.NewNullLogger()
	fastElections(cfg)
	r, err := raft.NewRaft(cfg, f, raft.NewInmemStore(), raft.NewInmemStore(), raft.NewInmemSnapshotStore(), group.Transport())
	if err != nil {
		t.Fatalf("constructing raft: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown().Error() })

	configuration := raft.Configuration{Servers: []raft.Server{{
		ID: raft.ServerID("n0"), Address: raft.ServerAddress(mux.Addr().String()),
	}}}
	if err := r.BootstrapCluster(configuration).Error(); err != nil {
		t.Fatalf("bootstrapping: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && r.State() != raft.Leader {
		time.Sleep(20 * time.Millisecond)
	}
	if r.State() != raft.Leader {
		t.Fatalf("no leader was elected; state is %s", r.State())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	target := r.CommitIndex()
	waitErr := f.waitApplied(ctx, target)
	tracked := f.appliedIndex()

	t.Logf("without a leadership entry: observed CommitIndex=%d tracked=%d converged=%v", target, tracked, waitErr == nil)
	if !errors.Is(waitErr, ErrReadTimeout) {
		t.Fatalf("the prescribed wait converged (%v) with no leadership entry appended; the epoch mechanism this package builds would then be unnecessary, which contradicts raft dispatching a no-op no state machine ever applies", waitErr)
	}
	if tracked >= target {
		t.Fatalf("the state machine tracked %d against a commit index of %d, so nothing was ever behind and this reproduction proves nothing", tracked, target)
	}
}
