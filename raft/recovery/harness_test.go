// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package recovery

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	machine "github.com/whitaker-io/machine/v4"
	"github.com/whitaker-io/machine/raft/checkpoint"
	"github.com/whitaker-io/machine/raft/ledger"
	"github.com/whitaker-io/machine/raft/membership"
	"github.com/whitaker-io/machine/raft/transport"
)

// THE DETECTOR IS THE ROOT MODULE'S JOURNAL. The assertion lives here rather than in
// production source, which is the precedent the ledger set for the heap store: the
// raft module declares the dependency and the test proves the shapes agree, so a
// drift on either side of the seam breaks a build rather than an integration.
var _ machine.Journal = (*Detector)(nil)

// *membership.Manager satisfies the narrow view this package reads.
var _ Membership = (*membership.Manager)(nil)

// node is one member of a real raft group.
type node struct {
	id     string
	mux    *transport.Mux
	ledger *ledger.Ledger
}

// newGroup stands up a real n-node raft group over the shared transport.
//
// It bootstraps the FULL configuration up front rather than adding voters one at a
// time, which is the same shape the ledger's own cluster tests use.
func newGroup(t *testing.T, flow string, n int) []*node {
	t.Helper()

	nodes := make([]*node, 0, n)
	for i := range n {
		mux, err := transport.New(transport.Config{
			BindAddr:         "127.0.0.1:0",
			HandshakeTimeout: 2 * time.Second,
			RPCTimeout:       2 * time.Second,
		})
		if err != nil {
			t.Fatalf("transport.New: %v", err)
		}
		t.Cleanup(func() { _ = mux.Close() })

		id := fmt.Sprintf("n%d", i)
		l, err := ledger.Open(ledger.Config{Flow: flow, LocalID: id, Mux: mux})
		if err != nil {
			t.Fatalf("opening the ledger for %s: %v", id, err)
		}
		t.Cleanup(func() { _ = l.Close() })

		nodes = append(nodes, &node{id: id, mux: mux, ledger: l})
	}

	servers := make([]raft.Server, 0, len(nodes))
	for _, member := range nodes {
		servers = append(servers, raft.Server{
			ID:      raft.ServerID(member.id),
			Address: raft.ServerAddress(member.mux.Addr().String()),
		})
	}
	if err := nodes[0].ledger.Raft().BootstrapCluster(raft.Configuration{Servers: servers}).Error(); err != nil {
		t.Fatalf("bootstrapping the %q group: %v", flow, err)
	}

	return nodes
}

// awaitLeader waits for the group to elect one.
//
// A FRESH MEMBER IS A MEMBER BEFORE IT IS AN INFORMED ONE, so every test that asks a
// node what it knows waits here first rather than reading a node that has not yet
// learned who leads.
func awaitLeader(t *testing.T, nodes []*node) *node {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, member := range nodes {
			if member.ledger.Raft().State() == raft.Leader {
				return member
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no leader was elected within 30s")

	return nil
}

// followerOf returns a member that does not lead.
func followerOf(t *testing.T, nodes []*node, leader *node) *node {
	t.Helper()

	for _, member := range nodes {
		if member != leader {
			return member
		}
	}
	t.Fatal("the group produced no follower")

	return nil
}

// view is a membership view a test controls, so a node's liveness can be moved
// without tearing down a raft group to do it.
//
// It stands in for a DEPENDENCY rather than for the code under test: the manager's
// own behaviour is gated by its package's tests, and what matters here is what the
// detector does with the membership it is handed.
type view struct {
	mutex     sync.Mutex
	servers   []raft.ServerID
	watches   int
	err       error
	delivered bool
}

func newView(ids ...string) *view {
	v := &view{}
	for _, id := range ids {
		v.servers = append(v.servers, raft.ServerID(id))
	}

	return v
}

func (v *view) Membership(string) (raft.Configuration, uint64, bool) {
	servers := make([]raft.Server, 0, len(v.servers))
	for _, id := range v.servers {
		servers = append(servers, raft.Server{ID: id})
	}

	return raft.Configuration{Servers: servers}, uint64(len(servers)), true
}

// Watch mirrors the real one: it BLOCKS until there is something to report or the
// context ends. A fake that returned instantly would turn the detector's park into a
// hot spin and measure the fake rather than the detector.
func (v *view) Watch(ctx context.Context, _ uint64) ([]membership.Signal, uint64, error) {
	v.mutex.Lock()
	v.watches++
	err, delivered := v.err, v.delivered
	v.delivered = true
	v.mutex.Unlock()

	if err != nil {
		return nil, 0, err
	}
	if !delivered {
		return nil, 1, nil
	}

	<-ctx.Done()

	return nil, 0, ctx.Err()
}

// journalCheckpoint writes one datum's progress through the detector's own encoding,
// naming the worker that wrote it.
func journalCheckpoint(t *testing.T, l *ledger.Ledger, owner, datum, payload string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	value, err := encodeRecord(machine.CheckpointRecord{
		Datum: datum, Owner: owner, Node: "worker", Anchor: machine.AnchorCompletion,
		Data: []byte(payload),
	})
	if err != nil {
		t.Fatalf("encoding the checkpoint for %s: %v", datum, err)
	}
	if _, err := l.Append(ctx, ledger.Entry{
		Kind: ledger.KindSet, Path: checkpoint.Path(datum), Value: value,
	}); err != nil {
		t.Fatalf("checkpointing %s: %v", datum, err)
	}
}

// datums reports the datum ids of a record set.
func datums(records []machine.CheckpointRecord) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, record.Datum)
	}

	return out
}
