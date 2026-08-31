package ledger

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestThreeNodeLedgerReplicatesHeapWrites(t *testing.T) {
	// TWO FLOWS ON THE SAME THREE MUXES. That is the shape the shared listener
	// exists for, and raising ledgers on top of it must not disturb the isolation
	// the transport already proves for bare raft groups.
	muxes := newMuxes(t, 3)
	alpha := newClusterOn(t, "flow-alpha", muxes)
	beta := newClusterOn(t, "flow-beta", muxes)

	alphaLeader := waitClusterLeader(t, alpha)
	betaLeader := waitClusterLeader(t, beta)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const writes = 5
	for i := range writes {
		if err := alphaLeader.ledger.Store().Save(ctx, fmt.Sprintf("heap/alpha-%d", i), fmt.Sprintf("alpha-%d", i)); err != nil {
			t.Fatalf("saving alpha-%d: %v", i, err)
		}
		if err := betaLeader.ledger.Store().Save(ctx, fmt.Sprintf("heap/beta-%d", i), fmt.Sprintf("beta-%d", i)); err != nil {
			t.Fatalf("saving beta-%d: %v", i, err)
		}
	}

	// Every PEER applies what the leader committed, not just the leader.
	awaitReplication(t, alpha, "heap/alpha-", writes)
	awaitReplication(t, beta, "heap/beta-", writes)

	for _, node := range alpha {
		assertHoldsOnly(t, node, "flow-alpha", "heap/alpha-", "heap/beta-", writes)
	}
	for _, node := range beta {
		assertHoldsOnly(t, node, "flow-beta", "heap/beta-", "heap/alpha-", writes)
	}
}

// awaitReplication waits until every node's state machine holds all n values.
func awaitReplication(t *testing.T, nodes []*clusterNode, prefix string, n int) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if replicatedEverywhere(nodes, prefix, n) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, node := range nodes {
		t.Logf("node %s holds %d of %d %s* entries", node.id, countWithPrefix(node, prefix), n, prefix)
	}
	t.Fatalf("the %s* writes did not reach every peer within 30s", prefix)
}

func replicatedEverywhere(nodes []*clusterNode, prefix string, n int) bool {
	for _, node := range nodes {
		if countWithPrefix(node, prefix) < n {
			return false
		}
	}

	return true
}

func countWithPrefix(node *clusterNode, prefix string) int {
	node.ledger.fsm.mutex.RLock()
	defer node.ledger.fsm.mutex.RUnlock()

	count := 0
	for path := range node.ledger.fsm.values {
		if strings.HasPrefix(path, prefix) {
			count++
		}
	}

	return count
}

// assertHoldsOnly checks a node applied its OWN flow's writes with the right values
// and none of the sibling flow's — an entry crossing groups is the failure the mux
// exists to prevent.
func assertHoldsOnly(t *testing.T, node *clusterNode, flow, own, foreign string, n int) {
	t.Helper()

	for i := range n {
		entry, ok := node.ledger.fsm.get(fmt.Sprintf("%s%d", own, i))
		if !ok {
			t.Fatalf("%s node %s is missing %s%d", flow, node.id, own, i)
		}
		value, err := decodeValue(entry.Value)
		if err != nil {
			t.Fatalf("%s node %s stored an undecodable value at %s%d: %v", flow, node.id, own, i, err)
		}
		if want := fmt.Sprintf("%s%d", strings.TrimPrefix(own, "heap/"), i); value != want {
			t.Fatalf("%s node %s holds %v at %s%d, want %q", flow, node.id, value, own, i, want)
		}
	}
	if crossed := countWithPrefix(node, foreign); crossed != 0 {
		t.Fatalf("%s node %s applied %d %s* entries: a write crossed groups on the shared listener", flow, node.id, crossed, foreign)
	}
}

func TestConcurrentSavesAllCommit(t *testing.T) {
	// The write path must admit concurrent in-flight appends rather than
	// serializing them behind a lock. A serial writer against a durable log tops
	// out around two orders of magnitude below what concurrent producers reach, so
	// this is a throughput property and not only a correctness one.
	l := openTestLedger(t, Config{Flow: "flow-concurrent", LocalID: "n0", Mux: testMux(t), Bootstrap: true})
	waitLeadership(t, l)
	store := l.Store()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const writers = 64
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	wg.Add(writers)
	start := make(chan struct{})
	for i := range writers {
		go func() {
			defer wg.Done()
			<-start
			errs <- store.Save(ctx, fmt.Sprintf("heap/concurrent-%d", i), fmt.Sprintf("value-%d", i))
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent save failed: %v", err)
		}
	}

	// All of them are visible, so none was silently dropped or overwritten.
	for i := range writers {
		value, ok, err := store.Load(ctx, fmt.Sprintf("heap/concurrent-%d", i))
		if err != nil || !ok {
			t.Fatalf("heap/concurrent-%d loaded present=%v err=%v after all writers returned", i, ok, err)
		}
		if want := fmt.Sprintf("value-%d", i); value != want {
			t.Fatalf("heap/concurrent-%d holds %v, want %q", i, value, want)
		}
	}
}
