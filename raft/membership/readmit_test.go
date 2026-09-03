// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"context"
	"net"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/whitaker-io/machine/raft/ledger"
	"github.com/whitaker-io/machine/raft/transport"
)

// deadPeerAddress returns a 127.0.0.1 address with nothing listening on it, so a
// dial fails immediately rather than waiting out the control timeout.
func deadPeerAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// TestTheAnnounceTargetSetFollowsTheRegistryAcrossAGeneration is the regression
// for the measured rolling-update failure: the announce target set was resolved
// once, inside Start, and held for the life of the process while the leader-only
// eviction path re-resolved every round.
//
// THE PEERS ADDRESS IS INSTALLED AFTER New RATHER THAN THROUGH IT, deliberately,
// so these bytes compile and run unchanged against the tree that carries the
// defect and against the tree that fixes it. It is the shape evictionCluster
// already uses.
func TestTheAnnounceTargetSetFollowsTheRegistryAcrossAGeneration(t *testing.T) {
	genA := []string{deadPeerAddress(t), deadPeerAddress(t)}
	genB := []string{deadPeerAddress(t), deadPeerAddress(t)}

	mux, err := transport.New(transport.Config{
		BindAddr: "127.0.0.1:0", HandshakeTimeout: time.Second, RPCTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	defer func() { _ = mux.Close() }()
	self := mux.Addr().String()

	mgr, err := New(Config{
		Node: "node-new-generation", Advertise: self, Mux: mux,
		Logger: hclog.NewNullLogger(), Flows: []string{"smoke"},
		Open: func(flow string) (*ledger.Ledger, error) {
			return ledger.Open(ledger.Config{Flow: flow, LocalID: "node-new-generation", Mux: mux})
		},
	})
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	defer func() { _ = mgr.Close() }()
	mgr.cfg.Peers = "peers.invalid:7946"
	mgr.cfg.Expect = 3
	// The refresh cadence and the eviction cadence are ONE number, and this is
	// where the test sets it. A refresh that ignored this knob would run at the
	// ten-second default and fail the window below.
	mgr.cfg.Autopilot.ReconcileInterval = 300 * time.Millisecond

	var resMu sync.Mutex
	current := genA
	mgr.resolve = func(context.Context, string) ([]string, error) {
		resMu.Lock()
		defer resMu.Unlock()
		return append([]string(nil), current...), nil
	}

	var dialMu sync.Mutex
	dialed := map[string]int{}
	inner := mgr.peers.dial
	mgr.peers.dial = func(address string, timeout time.Duration) (net.Conn, error) {
		dialMu.Lock()
		dialed[address]++
		dialMu.Unlock()
		return inner(address, timeout)
	}
	seen := func() map[string]int {
		dialMu.Lock()
		defer dialMu.Unlock()
		out := map[string]int{}
		for a, n := range dialed {
			out[a] = n
		}
		return out
	}
	countIn := func(m map[string]int, set []string) int {
		total := 0
		for _, a := range set {
			total += m[a]
		}
		return total
	}

	// Start never returns while the flow stays unplaced: Expect is 3 and nothing
	// answers, so placeFlow retries until the context expires. That retry loop IS
	// the announce loop under measurement.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()

	time.Sleep(1500 * time.Millisecond)
	beforeFlip := seen()
	if countIn(beforeFlip, genA) == 0 {
		t.Fatalf("KNOWN-POSITIVE FAILED: the announce path never dialed generation A %v; observed %v",
			genA, beforeFlip)
	}
	t.Logf("announce dials to generation A before the flip: %d", countIn(beforeFlip, genA))

	// THE ROLLING UPDATE: generation A is gone, generation B is live.
	resMu.Lock()
	current = genB
	resMu.Unlock()

	// CONTROL, SAME RUN, SAME MANAGER, SAME RESOLVER: the eviction path's
	// resolution reflects the flip immediately. Without it, "no generation B
	// dials" would be indistinguishable from a flip that never took effect.
	live, err := mgr.resolveLive(context.Background())
	if err != nil {
		t.Fatalf("resolveLive: %v", err)
	}
	sort.Strings(live)
	wantB := append([]string(nil), genB...)
	sort.Strings(wantB)
	if len(live) != len(wantB) || live[0] != wantB[0] || live[1] != wantB[1] {
		t.Fatalf("CONTROL FAILED: resolveLive returned %v, want generation B %v", live, wantB)
	}
	t.Logf("CONTROL: the eviction-side resolution reads generation B %v", live)

	aAtFlip := countIn(seen(), genA)

	// ONE REFRESH INTERVAL, PLUS THE PLACE RETRY, PLUS SLACK.
	time.Sleep(2 * time.Second)
	after := seen()
	aAfter := countIn(after, genA)
	bAfter := countIn(after, genB)
	t.Logf("announce dials after the flip: generation A %d, generation B %d", aAfter-aAtFlip, bAfter)

	if bAfter == 0 {
		t.Fatalf("the announce path never dialed the live generation %v after the registry moved to it; "+
			"observed %v — the target set did not follow the registry", genB, after)
	}
	if aAfter != aAtFlip {
		t.Fatalf("the announce path dialed the dead generation %d more times after the flip; "+
			"the stale set was not replaced, it was added to", aAfter-aAtFlip)
	}
	t.Log("the announce target set followed the registry: dials to the live generation, none to the dead one")

	cancel()
	<-done
}

// TestTheAnnounceRoundResolvesAtMostOncePerRefreshInterval is the cost shape of
// the per-round refresh.
//
// THE PLACE LOOP RETRIES FIFTY TIMES FASTER THAN THE REFRESH INTERVAL —
// placeRetryInterval is 200ms against a default eviction cadence of ten
// seconds — so an announce round that resolved unconditionally would put fifty
// registry lookups per interval per unplaced flow onto whatever serves the
// peers address. The coalescing is the same shape peers.statsView uses for the
// stats round, and this is what holds it there.
func TestTheAnnounceRoundResolvesAtMostOncePerRefreshInterval(t *testing.T) {
	mux, err := transport.New(transport.Config{
		BindAddr: "127.0.0.1:0", HandshakeTimeout: time.Second, RPCTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	defer func() { _ = mux.Close() }()
	self := mux.Addr().String()

	mgr, err := New(Config{
		Node: "a-node", Advertise: self, Mux: mux,
		Logger: hclog.NewNullLogger(), Flows: []string{"smoke"},
		Open: func(flow string) (*ledger.Ledger, error) {
			return ledger.Open(ledger.Config{Flow: flow, LocalID: "a-node", Mux: mux})
		},
	})
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	defer func() { _ = mgr.Close() }()
	mgr.cfg.Peers = "peers.invalid:7946"
	mgr.cfg.Expect = 3
	interval := 500 * time.Millisecond
	mgr.cfg.Autopilot.ReconcileInterval = interval

	var mu sync.Mutex
	resolves := 0
	dead := []string{deadPeerAddress(t)}
	mgr.resolve = func(context.Context, string) ([]string, error) {
		mu.Lock()
		resolves++
		mu.Unlock()
		return append([]string(nil), dead...), nil
	}

	window := 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- mgr.Start(ctx) }()
	<-done
	elapsed := time.Since(start)

	mu.Lock()
	got := resolves
	mu.Unlock()

	// THE KNOWN-POSITIVE: the place loop really did run many rounds in this
	// window, so a low resolve count cannot be a loop that never turned.
	rounds := int(elapsed / placeRetryInterval)
	if rounds < 5 {
		t.Fatalf("KNOWN-POSITIVE FAILED: only %d place rounds fit in %s, so the count below measures nothing",
			rounds, elapsed)
	}
	// THE CEILING: one resolution per interval, plus the one Start takes, plus
	// one for rounding. Measured on the compliant implementation at 7 for a 3s
	// window at a 500ms interval; an uncoalesced implementation resolves once per
	// place round, which is 15 at the same settings.
	ceiling := int(elapsed/interval) + 2
	t.Logf("resolutions %d over %s at a %s interval across %d place rounds; ceiling %d",
		got, elapsed, interval, rounds, ceiling)
	if got > ceiling {
		t.Fatalf("the announce path resolved %d times in %s at a %s refresh interval, above the ceiling of "+
			"%d: the refresh is not coalesced and rides the place-retry cadence instead", got, elapsed, interval, ceiling)
	}
}
