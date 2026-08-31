package transport

import (
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

type recorderFSM struct {
	mu   sync.Mutex
	logs []string
}

func (f *recorderFSM) Apply(l *raft.Log) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, string(l.Data))
	return nil
}

func (f *recorderFSM) Snapshot() (raft.FSMSnapshot, error) { return nil, fmt.Errorf("no snapshots") }
func (f *recorderFSM) Restore(io.ReadCloser) error         { return nil }

func (f *recorderFSM) entries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.logs...)
}

type testNode struct {
	mux   *Mux
	rafts map[GroupID]*raft.Raft
	fsms  map[GroupID]*recorderFSM
}

func newTestNode(t *testing.T, id string, groups []GroupID) *testNode {
	t.Helper()
	m, err := New(Config{BindAddr: "127.0.0.1:0", RPCTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	n := &testNode{mux: m, rafts: map[GroupID]*raft.Raft{}, fsms: map[GroupID]*recorderFSM{}}
	for _, gid := range groups {
		g, err := m.Bind(gid)
		if err != nil {
			t.Fatalf("Bind %s: %v", gid, err)
		}
		cfg := raft.DefaultConfig()
		cfg.LocalID = raft.ServerID(id)
		cfg.LogOutput = io.Discard
		cfg.HeartbeatTimeout = 200 * time.Millisecond
		cfg.ElectionTimeout = 200 * time.Millisecond
		cfg.LeaderLeaseTimeout = 100 * time.Millisecond
		cfg.CommitTimeout = 20 * time.Millisecond
		f := &recorderFSM{}
		r, err := raft.NewRaft(cfg, f, raft.NewInmemStore(), raft.NewInmemStore(), raft.NewInmemSnapshotStore(), g.Transport())
		if err != nil {
			t.Fatalf("NewRaft: %v", err)
		}
		n.rafts[gid] = r
		n.fsms[gid] = f
	}
	t.Cleanup(func() {
		for _, r := range n.rafts {
			_ = r.Shutdown().Error()
		}
		_ = m.Close()
	})
	return n
}

func bootstrap(t *testing.T, nodes []*testNode, groups []GroupID) {
	t.Helper()
	servers := make([]raft.Server, 0, len(nodes))
	for i, n := range nodes {
		servers = append(servers, raft.Server{
			ID:      raft.ServerID(fmt.Sprintf("n%d", i)),
			Address: raft.ServerAddress(n.mux.Addr().String()),
		})
	}
	for _, gid := range groups {
		if err := nodes[0].rafts[gid].BootstrapCluster(raft.Configuration{Servers: servers}).Error(); err != nil {
			t.Fatalf("bootstrap %s: %v", gid, err)
		}
	}
}

func waitLeader(t *testing.T, nodes []*testNode, gid GroupID) *raft.Raft {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			if n.rafts[gid].State() == raft.Leader {
				return n.rafts[gid]
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no leader elected for %s", gid)
	return nil
}

func TestTwoGroupsShareOneListenerAndStayIsolated(t *testing.T) {
	groups := []GroupID{"flow-alpha", "flow-beta"}
	nodes := []*testNode{
		newTestNode(t, "n0", groups), newTestNode(t, "n1", groups), newTestNode(t, "n2", groups),
	}
	bootstrap(t, nodes, groups)
	leaders := map[GroupID]*raft.Raft{}
	for _, gid := range groups {
		leaders[gid] = waitLeader(t, nodes, gid)
	}
	for i := 0; i < 5; i++ {
		if err := leaders["flow-alpha"].Apply([]byte(fmt.Sprintf("alpha-%d", i)), 5*time.Second).Error(); err != nil {
			t.Fatalf("apply alpha: %v", err)
		}
		if err := leaders["flow-beta"].Apply([]byte(fmt.Sprintf("beta-%d", i)), 5*time.Second).Error(); err != nil {
			t.Fatalf("apply beta: %v", err)
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if replicated(nodes, groups, 5) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i, n := range nodes {
		alpha, beta := n.fsms["flow-alpha"].entries(), n.fsms["flow-beta"].entries()
		if len(alpha) != 5 || len(beta) != 5 {
			t.Fatalf("node %d: alpha=%d beta=%d applied entries, want 5 and 5", i, len(alpha), len(beta))
		}
		for _, e := range alpha {
			if !hasPrefix(e, "alpha-") {
				t.Fatalf("node %d: the alpha FSM applied %q — a beta entry crossed groups", i, e)
			}
		}
		for _, e := range beta {
			if !hasPrefix(e, "beta-") {
				t.Fatalf("node %d: the beta FSM applied %q — an alpha entry crossed groups", i, e)
			}
		}
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

func replicated(nodes []*testNode, groups []GroupID, want int) bool {
	for _, n := range nodes {
		for _, gid := range groups {
			if len(n.fsms[gid].entries()) < want {
				return false
			}
		}
	}
	return true
}

func TestUndrainedObserverChannelDoesNotStallReplication(t *testing.T) {
	groups := []GroupID{"flow-obs"}
	nodes := []*testNode{
		newTestNode(t, "n0", groups), newTestNode(t, "n1", groups), newTestNode(t, "n2", groups),
	}
	bootstrap(t, nodes, groups)
	obsCh := make(chan raft.Observation, 1)
	observers := make([]*raft.Observer, 0, len(nodes))
	for _, n := range nodes {
		o := raft.NewObserver(obsCh, false, nil)
		observers = append(observers, o)
		n.rafts["flow-obs"].RegisterObserver(o)
	}
	leader := waitLeader(t, nodes, "flow-obs")
	for i := 0; i < 50; i++ {
		if err := leader.Apply([]byte(fmt.Sprintf("obs-%d", i)), 5*time.Second).Error(); err != nil {
			t.Fatalf("apply %d with an undrained observer channel: %v", i, err)
		}
	}
	var dropped uint64
	for _, o := range observers {
		dropped += o.GetNumDropped()
	}
	if dropped == 0 {
		t.Fatal("CONTROL FAILED: nothing was dropped, so the observer channel was never saturated")
	}
	t.Logf("50 entries committed with a saturated observer channel; numDropped=%d", dropped)
}
