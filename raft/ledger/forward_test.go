package ledger

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/transport"
)

// forwardRoundTrip sends one request to the node at addr over from's forwarding
// stream and returns the reply. It is the client half phase 3 builds; here it exists
// so the leader-side server can be exercised before that client does.
func forwardRoundTrip(t *testing.T, from *Ledger, addr string, req forwardRequest) forwardReply {
	t.Helper()
	conn, err := from.group.DialForward(addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dialing the forwarding stream at %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()

	if err := gob.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encoding a forwarded request: %v", err)
	}
	var reply forwardReply
	if err := gob.NewDecoder(conn).Decode(&reply); err != nil {
		t.Fatalf("decoding the forwarded reply: %v", err)
	}

	return reply
}

// inflightForwards reports how many forwarded connections handlers are currently
// holding. It is the control the close and stalled-peer gates need: without it, a test
// that measured before any handler took its connection off the accept queue would pass
// vacuously. It lives here rather than in production because nothing in production
// reads it.
func (l *Ledger) inflightForwards() int {
	l.inflightMu.Lock()
	defer l.inflightMu.Unlock()

	return len(l.inflight)
}

func TestUndeclaredForwardOpIsRefused(t *testing.T) {
	mux := testMux(t)
	l := openTestLedger(t, Config{Flow: "flow-undeclared", LocalID: "n0", Mux: mux, Bootstrap: true})
	waitLeadership(t, l)

	const path = "heap/undeclared"
	// forwardOp(0) is the ZERO VALUE, which declared() deliberately excludes: a
	// request decoded from zeroed or truncated bytes names no operation. 99 is a
	// value no build has ever declared.
	for _, op := range []forwardOp{0, forwardOp(99)} {
		reply := forwardRoundTrip(t, l, mux.Addr().String(),
			forwardRequest{Op: op, Path: path, Value: []byte("must-not-land")})
		if reply.Code != codeOther {
			t.Fatalf("op %d replied with code %d, want codeOther: an undeclared operation is not one of the ledger's sentinels", op, reply.Code)
		}
		if !strings.Contains(reply.Message, ErrUndeclaredForwardOp.Error()) {
			t.Fatalf("op %d replied with message %q, which does not name the undeclared-operation refusal", op, reply.Message)
		}
		if err := reply.rebuild(); err == nil {
			t.Fatalf("op %d rebuilt to a nil error: a refusal must reach the caller as one", op)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// IT LEFT NO STATE BEHIND.
	if _, present, err := l.Get(ctx, path); err != nil || present {
		t.Fatalf("after two undeclared operations %q is present=%v (err %v); a refused operation writes nothing", path, present, err)
	}

	// CONTROL: a DECLARED operation on the same path over the same stream IS served,
	// so the refusals above are about the operation rather than about a dead stream.
	reply := forwardRoundTrip(t, l, mux.Addr().String(),
		forwardRequest{Op: opSave, Kind: KindSet, Path: path, Value: []byte("landed")})
	if err := reply.rebuild(); err != nil {
		t.Fatalf("CONTROL FAILED: a declared save over the same stream: %v", err)
	}
	if _, present, err := l.Get(ctx, path); err != nil || !present {
		t.Fatalf("CONTROL FAILED: a declared save left nothing at %q (present=%v, err %v)", path, present, err)
	}
}

// roundTripError classifies a local error, sends the reply THROUGH GOB, and rebuilds
// it on the far side. The wire is part of the property rather than scaffolding around
// it: the whole claim is that identity survives crossing a node boundary.
func roundTripError(t *testing.T, local error) (forwardReply, error) {
	t.Helper()
	code, message := classify(local)

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(forwardReply{Code: code, Message: message}); err != nil {
		t.Fatalf("encoding the reply: %v", err)
	}
	var decoded forwardReply
	if err := gob.NewDecoder(&buf).Decode(&decoded); err != nil {
		t.Fatalf("decoding the reply: %v", err)
	}

	return decoded, decoded.rebuild()
}

func TestForwardedErrorsPreserveTheLedgerSentinels(t *testing.T) {
	cases := []struct {
		name  string
		local error
		want  error
		retry bool
	}{
		{
			name:  "not-leader",
			local: fmt.Errorf("ledger: appending to flow %q: %w", "f", translateRaftError(raft.ErrNotLeader)),
			want:  ErrNotLeader,
			retry: true,
		},
		{
			name:  "poisoned-journal",
			local: fmt.Errorf("ledger: applying journal entry 7: %w", ErrPoisonedJournal),
			want:  ErrPoisonedJournal,
		},
		{
			name:  "read-timeout",
			local: fmt.Errorf("ledger: waited for applied index 9, observed 4: %w", ErrReadTimeout),
			want:  ErrReadTimeout,
		},
		{
			name:  "closed",
			local: ErrClosed,
			want:  ErrClosed,
		},
		{
			// The node addressed as leader has stopped its raft while its transport
			// is still bound. Before this arm existed the reply crossed as an
			// unclassified error and the retry loop treated it as terminal.
			name:  "leader-unavailable",
			local: raft.ErrRaftShutdown,
			want:  ErrLeaderUnavailable,
			retry: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, rebuilt := roundTripError(t, tc.local)
			if !errors.Is(rebuilt, tc.want) {
				t.Fatalf("a %s refusal rebuilt as %v, which does not satisfy errors.Is against the sentinel a local call would have returned", tc.name, rebuilt)
			}
			if got := reply.Code.retryable(); got != tc.retry {
				t.Fatalf("a %s refusal reports retryable=%v, want %v", tc.name, got, tc.retry)
			}
		})
	}

	// CONTROL: an error matching none of the five is NOT promoted to any of them.
	// Without this leg a repair that made the retry loop generous would look
	// identical to one that made it correct.
	reply, rebuilt := roundTripError(t, errors.New("ledger: the disk caught fire"))
	if rebuilt == nil {
		t.Fatal("CONTROL FAILED: an unclassified error rebuilt as a nil error")
	}
	if reply.Code != codeOther {
		t.Fatalf("CONTROL FAILED: an unclassified error took code %d, want codeOther", reply.Code)
	}
	if reply.Code.retryable() {
		t.Fatal("CONTROL FAILED: an unclassified error is retryable, so the loop would spin on a condition no retry repairs")
	}
	for _, sentinel := range []error{ErrNotLeader, ErrLeaderUnavailable, ErrPoisonedJournal, ErrReadTimeout, ErrClosed} {
		if errors.Is(rebuilt, sentinel) {
			t.Fatalf("CONTROL FAILED: an unclassified error was promoted to %v; the table is generous rather than correct", sentinel)
		}
	}
}

func TestAForwardedOpOnANonLeaderIsRefusedNotRelayed(t *testing.T) {
	muxes := newMuxes(t, 3)
	nodes := newClusterOn(t, "flow-norelay", muxes)
	leader := waitClusterLeader(t, nodes)

	var follower *clusterNode
	for _, node := range nodes {
		if node != leader {
			follower = node

			break
		}
	}
	if follower == nil {
		t.Fatal("a three-node group produced no follower")
	}

	// CONTROL FIRST: the same dial from the same node to the LEADER is served, so a
	// refusal below is about the receiver rather than about a broken stream.
	control := forwardRoundTrip(t, follower.ledger, leader.mux.Addr().String(),
		forwardRequest{Op: opSave, Kind: KindSet, Path: "heap/norelay-control", Value: []byte("v")})
	if control.Code != codeNone {
		t.Fatalf("CONTROL FAILED: a forwarded save to the leader replied with code %d (%v)", control.Code, control.rebuild())
	}

	reply := forwardRoundTrip(t, follower.ledger, follower.mux.Addr().String(),
		forwardRequest{Op: opSave, Kind: KindSet, Path: "heap/norelay", Value: []byte("v")})
	if reply.Code != codeNotLeader {
		t.Fatalf("a forwarded op on a non-leader replied with code %d, want codeNotLeader; it was relayed onward rather than refused", reply.Code)
	}
	if err := reply.rebuild(); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("the non-leader's refusal rebuilt as %v, want ErrNotLeader", err)
	}
}

// assertClosesWithin fails when Close does not return inside ceiling. The ceiling is
// the assertion rather than a mere timeout: a Close that eventually returns once a
// handler's read deadline expires is a DIFFERENT outcome from one that ends the
// connection, and only a ceiling below that deadline separates them.
func assertClosesWithin(t *testing.T, l *Ledger, ceiling time.Duration, what string) {
	t.Helper()
	done := make(chan error, 1)
	start := time.Now()

	go func() { done <- l.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close on %s: %v", what, err)
		}
		t.Logf("Close returned in %s on %s", time.Since(start), what)
	case <-time.After(ceiling):
		t.Fatalf("Close did not return within %s on %s", ceiling, what)
	}
}

func TestCloseReturnsWithTheForwardServerRunning(t *testing.T) {
	// Well below forwardRequestTimeout, so a deadline-only repair cannot pass.
	const ceiling = 2 * time.Second

	t.Run("drained-shutdown", func(t *testing.T) {
		mux := testMux(t)
		l := openTestLedger(t, Config{Flow: "flow-close-drained", LocalID: "n0", Mux: mux, Bootstrap: true})
		waitLeadership(t, l)

		assertClosesWithin(t, l, ceiling, "the ordinary drained shutdown path")
	})

	t.Run("undrained-shutdown", func(t *testing.T) {
		mux := testMux(t)
		l := openTestLedger(t, Config{Flow: "flow-close-undrained", LocalID: "n0", Mux: mux, Bootstrap: true})
		waitLeadership(t, l)

		// EXACTLY THE SHAPE ShutdownOnRemove PRODUCES: raft shuts itself down and
		// DISCARDS the future, so nothing has released the transport and the serve
		// loop is still parked in Accept when the wait is reached.
		_ = l.raft.Shutdown()

		assertClosesWithin(t, l, ceiling, "the ShutdownOnRemove-shaped undrained path")
	})

	t.Run("handler-parked-mid-request", func(t *testing.T) {
		mux := testMux(t)
		l := openTestLedger(t, Config{Flow: "flow-close-parked", LocalID: "n0", Mux: mux, Bootstrap: true})
		waitLeadership(t, l)

		// A peer that connects, sends one byte and goes silent parks the handler in
		// Decode: the transport hands this connection over with no deadline, by
		// design, because raft owns the per-RPC deadline on its own stream and this
		// stream has no raft to own it.
		conn, err := l.group.DialForward(mux.Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatalf("dialing the forwarding stream: %v", err)
		}
		defer func() { _ = conn.Close() }()
		if _, err := conn.Write([]byte{0x01}); err != nil {
			t.Fatalf("writing one byte: %v", err)
		}

		// CONTROL: wait until a handler actually HOLDS the connection. Without it, a
		// Close running before the handler took it off the accept queue would pass
		// while proving nothing.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) && l.inflightForwards() != 1 {
			time.Sleep(10 * time.Millisecond)
		}
		if got := l.inflightForwards(); got != 1 {
			t.Fatalf("CONTROL FAILED: %d forwarding connections in flight, want 1; no handler ever took the connection", got)
		}

		assertClosesWithin(t, l, ceiling, "with a handler parked reading from a silent peer")
	})
}

// forbiddenForwardCalls reports every forwarding-capable entry point or direct state
// machine read called inside a method of the LOCKED forwarding-handler set.
//
// TWO CLASSES, ONE DETECTOR. Calling Append or Get is RELAYING rather than serving:
// those resolve a leader and forward, so two peers whose leader resolution disagrees
// bounce an operation between them with no bound governing the chain. Reading through
// fsm.get SKIPS THE BARRIER: it answers from a local leadership belief with no quorum
// proof and no wait, which compiles, vets, and looks careful.
//
// WHAT IT CANNOT SEE, stated rather than implied: it is construction-scoped and
// name-scoped. A forbidden call reached through a helper one of these methods calls,
// or a handler added outside the locked set, is invisible to it. Keeping that set
// closed is a review duty on any later change to forward.go.
func forbiddenForwardCalls(fset *token.FileSet, file *ast.File) []string {
	locked := []string{"serveForward", "handleForward", "serveOne"}
	forbidden := []string{"Append", "Get", "get"}

	var found []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil || !slices.Contains(locked, fn.Name.Name) {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok && slices.Contains(forbidden, selector.Sel.Name) {
				found = append(found, fmt.Sprintf("%s calls %s at %s",
					fn.Name.Name, selector.Sel.Name, fset.Position(call.Pos())))
			}

			return true
		})
	}

	return found
}

// The fixtures. The pair varies BOTH axes the detector discriminates on: what is
// called, and which method calls it.
const (
	relayingAndBarrierSkippingHandler = `package p
func (l *Ledger) serveOne(ctx context.Context, req forwardRequest) forwardReply {
	if req.Op == opLoad {
		entry, present, err := l.Get(ctx, req.Path)
		cached, _ := l.fsm.get(req.Path)
		_, _, _, _ = entry, present, err, cached
	}
	index, err := l.Append(ctx, Entry{})
	_, _ = index, err
	return forwardReply{}
}
`
	leaderLocalHandler = `package p
func (l *Ledger) serveOne(ctx context.Context, req forwardRequest) forwardReply {
	if req.Op == opLoad {
		entry, present, err := l.getLocal(ctx, req.Path)
		_, _, _ = entry, present, err
	}
	index, err := l.appendLocal(ctx, Entry{})
	_, _ = index, err
	return forwardReply{}
}
`
	nonHandlerCallingTheEntryPoints = `package p
func (l *Ledger) somethingElse(ctx context.Context) {
	_, _, _ = l.Get(ctx, "p")
	_, _ = l.Append(ctx, Entry{})
}
`
)

func TestNoForwardingHandlerRelaysOrSkipsTheBarrier(t *testing.T) {
	fset := token.NewFileSet()
	fixture := func(name, src string) []string {
		parsed, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parsing the %s fixture: %v", name, err)
		}

		return forbiddenForwardCalls(fset, parsed)
	}

	// KNOWN POSITIVE: a handler doing all three forbidden things fires three times,
	// through the same detector, in this same run.
	if got := fixture("bad-relay.go", relayingAndBarrierSkippingHandler); len(got) != 3 {
		t.Fatalf("CONTROL FAILED: the detector found %d forbidden calls in a handler that relays AND skips the barrier, want 3: %v", len(got), got)
	}
	// KNOWN NEGATIVES, one per axis. The first varies WHAT is called, the second
	// varies WHICH method calls it.
	if got := fixture("good-local.go", leaderLocalHandler); len(got) != 0 {
		t.Fatalf("CONTROL FAILED: the detector fired on a handler using only the leader-local primitives, at %v", got)
	}
	if got := fixture("good-nonhandler.go", nonHandlerCallingTheEntryPoints); len(got) != 0 {
		t.Fatalf("CONTROL FAILED: the detector fired outside the locked handler set, at %v; it is not name-scoped", got)
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the ledger package directory: %v", err)
	}
	scanned := 0
	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		scanned++
		offenders = append(offenders, forbiddenForwardCalls(fset, parsed)...)
	}
	if scanned == 0 {
		t.Fatal("CONTROL FAILED: the walk parsed 0 production Go files, so a clean result is an empty walk")
	}

	t.Logf("scanned %d production Go files in the ledger package", scanned)
	if len(offenders) != 0 {
		t.Fatalf("a forwarding handler relays or skips the barrier: %v", offenders)
	}
}

// newForwardCluster raises an n-node group whose ledgers take an adjusted Config. The
// shared helper always applies shortened election timers and never sets a forwarding
// bound; the gates below need both knobs — a shorter ForwardTimeout so a bound gate
// need not wait the production ten seconds out, and raft's PRODUCTION timers, which
// the leadership-change legs must run against rather than the harness's.
func newForwardCluster(t *testing.T, flow string, n int, adjust func(*Config)) []*clusterNode {
	t.Helper()

	muxes := newMuxes(t, n)
	nodes := make([]*clusterNode, 0, len(muxes))
	for i, mux := range muxes {
		id := fmt.Sprintf("n%d", i)
		cfg := Config{Flow: flow, LocalID: id, Mux: mux}
		if adjust != nil {
			adjust(&cfg)
		}
		l, err := Open(cfg)
		if err != nil {
			t.Fatalf("Open(%s/%s): %v", flow, id, err)
		}
		t.Cleanup(func() { closeWithin(t, l, cleanupCloseCeiling) })
		nodes = append(nodes, &clusterNode{id: id, mux: mux, ledger: l})
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

// totalForwardHandshakes sums the forwarding connections every node in the group has
// accepted. It is the instrument the cost gate reads: a dial either happened or it did
// not, while a latency band on a loopback group is noise.
func totalForwardHandshakes(nodes []*clusterNode) uint64 {
	var total uint64
	for _, node := range nodes {
		total += node.mux.Stats().ForwardHandshakes
	}

	return total
}

// survivorsOf returns every node except the one named, for waiting out an election
// after a node is stopped.
func survivorsOf(nodes []*clusterNode, gone *clusterNode) []*clusterNode {
	survivors := make([]*clusterNode, 0, len(nodes))
	for _, node := range nodes {
		if node != gone {
			survivors = append(survivors, node)
		}
	}

	return survivors
}

func TestForwardBoundCoversRaftsOwnWorstElectionWindow(t *testing.T) {
	defaults := raft.DefaultConfig()

	// CONTROL: if raft's defaults ever read as zero the window is not measurable, and
	// this gate must say so rather than compare against nothing.
	if defaults.HeartbeatTimeout <= 0 || defaults.ElectionTimeout <= 0 {
		t.Fatalf("CONTROL FAILED: raft's defaults report HeartbeatTimeout %s and ElectionTimeout %s, so the worst compliant window is not measurable",
			defaults.HeartbeatTimeout, defaults.ElectionTimeout)
	}

	// raft's randomTimeout returns a value between its argument and TWICE it, so the
	// worst compliant window is two heartbeat timeouts of detection plus two election
	// timeouts of campaigning. Both terms are read from raft's own DefaultConfig, so a
	// library upgrade that raises either one reds this gate — and so does any
	// reduction of the bound. Neither is visible to a convergence measurement.
	worst := 2*defaults.HeartbeatTimeout + 2*defaults.ElectionTimeout
	const required = 2.0
	margin := float64(defaultForwardTimeout) / float64(worst)

	t.Logf("MEASURED bound coverage: defaultForwardTimeout %s against raft's worst compliant window %s (2 x HeartbeatTimeout %s + 2 x ElectionTimeout %s) is %.2fx, required %.2fx",
		defaultForwardTimeout, worst, defaults.HeartbeatTimeout, defaults.ElectionTimeout, margin, required)

	if margin < required {
		t.Fatalf("the forwarding bound %s covers raft's worst compliant detection-plus-election window %s only %.2fx, want at least %.2fx",
			defaultForwardTimeout, worst, margin, required)
	}
}

func TestSelfResolutionArmIsBoundedAndDoesNotHotLoop(t *testing.T) {
	mux := testMux(t)
	l := openTestLedger(t, Config{
		Flow: "flow-self", LocalID: "n0", Mux: mux, Bootstrap: true,
		ForwardTimeout: 20 * time.Second,
	})
	waitLeadership(t, l)

	// CONTROL: leadership really resolves to THIS node, so the arm held open below is
	// the self-resolution one rather than the no-leader-known one.
	if _, id := l.raft.LeaderWithID(); string(id) != l.cfg.LocalID {
		t.Fatalf("CONTROL FAILED: leadership resolved to %q, want this node %q", id, l.cfg.LocalID)
	}

	// The local attempt is a stub that refuses every time. forward takes it as a
	// parameter, so the arm is held open with no production test seam. The counter is
	// atomic because the watchdog below reads it from a different goroutine.
	var attempts atomic.Int64
	local := func() forwardReply {
		attempts.Add(1)

		return forwardReply{Code: codeNotLeader, Message: ErrNotLeader.Error()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// THE WATCHDOG MAKES THIS TEST THE REPORTER. Called synchronously, a loop that
	// observes neither the bound nor the caller's context never returns, so none of the
	// legs below ever run and the only reporter left is `go test -timeout` — measured
	// at 600 seconds with the named assertion buried under a goroutine dump.
	const watchdog = 20 * time.Second
	done := make(chan error, 1)
	start := time.Now()

	go func() {
		_, err := l.forward(ctx, forwardRequest{Op: opSave, Kind: KindSet, Path: "heap/self", Value: []byte("v")}, local)
		done <- err
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(watchdog):
		t.Fatalf("the self-resolution arm did not return within %s (made %d local attempts): it observes neither the bound nor the caller's context",
			watchdog, attempts.Load())
	}
	elapsed := time.Since(start)
	made := attempts.Load()

	t.Logf("MEASURED: the self-resolution arm made %d attempts in %s and ended with: %v", made, elapsed, err)

	// TERMINATION, at the caller's context and naming the attempts.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the self-resolution arm ended with %v, want the caller's context error", err)
	}
	if errors.Is(err, ErrForwardBoundExceeded) {
		t.Fatal("a context expiry was reported as the forwarding bound; those are different facts and a caller acts on them differently")
	}
	if !regexp.MustCompile(`\d+ attempts`).MatchString(err.Error()) {
		t.Fatalf("the failure %q does not name how many attempts were made", err)
	}
	// The last condition names errLeaderIsSelf, which is what confirms the arm actually
	// exercised was self-resolution rather than no-leader-known.
	if !strings.Contains(err.Error(), errLeaderIsSelf.Error()) {
		t.Fatalf("the failure %q does not name leadership resolving to this node, so the arm under test was not the self-resolution one", err)
	}
	if made < 2 {
		t.Fatalf("CONTROL FAILED: only %d attempt was made, so the loop never retried and the arm was not held open", made)
	}

	// THROTTLE. A loop that checks the deadline and the context but re-enters
	// immediately terminates correctly and still hammers raft — measured at millions
	// of attempts in the same window. Single figures is what routing every arm
	// through the one pause produces.
	if made > 10 {
		t.Fatalf("the self-resolution arm made %d attempts in %s: it is bounded but unthrottled, so an arm bypassed pause",
			made, elapsed)
	}
}

func TestFollowerStoreForwardsToTheLeader(t *testing.T) {
	nodes := newForwardCluster(t, "flow-store-forward", 3, func(c *Config) { c.tuning = fastElections })
	leader := waitClusterLeader(t, nodes)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if err := leader.ledger.Store().Save(ctx, "heap/seed", "seeded"); err != nil {
		t.Fatalf("seeding on the leader: %v", err)
	}

	checked := 0
	for _, node := range nodes {
		if node == leader {
			continue
		}
		store := node.ledger.Store()

		// CONTROL: this follower's LEADER-LOCAL read genuinely refuses, so the three
		// calls below exercise forwarding rather than a local success.
		if _, _, err := node.ledger.getLocal(ctx, "heap/seed"); !errors.Is(err, ErrNotLeader) {
			t.Fatalf("CONTROL FAILED: the leader-local read on the follower %s gave %v, want ErrNotLeader", node.id, err)
		}

		value, present, err := store.Load(ctx, "heap/seed")
		if err != nil || !present || value != "seeded" {
			t.Fatalf("Load on the follower %s gave %v present=%v err=%v, want the leader's value", node.id, value, present, err)
		}

		path := fmt.Sprintf("heap/%s-write", node.id)
		if err := store.Save(ctx, path, "follower-write"); err != nil {
			t.Fatalf("Save on the follower %s: %v", node.id, err)
		}

		// UPDATE'S FUNCTION RUNS AT THE CALLER. The closure mutates a variable in
		// THIS process: no closure crosses the wire, so the datum's own worker
		// computes and the leader only orders.
		ranAt := ""
		updated, err := store.Update(ctx, path, func(current any) any {
			ranAt = node.id

			return fmt.Sprintf("%v+updated", current)
		})
		if err != nil {
			t.Fatalf("Update on the follower %s: %v", node.id, err)
		}
		if ranAt != node.id {
			t.Fatalf("Update on the follower %s never ran its function in this process; a closure crossed the wire", node.id)
		}
		if updated != "follower-write+updated" {
			t.Fatalf("Update on %s returned %v, want the computed value", node.id, updated)
		}

		// THE LEADER HOLDS WHAT UPDATE PRODUCED, read leader-locally so this is the
		// leader's own state machine rather than another forwarded round trip.
		entry, held, err := leader.ledger.getLocal(ctx, path)
		if err != nil || !held {
			t.Fatalf("the leader does not hold %s after a forwarded Update (present=%v err=%v)", path, held, err)
		}
		decoded, err := decodeValue(entry.Value)
		if err != nil {
			t.Fatalf("decoding what the leader holds at %s: %v", path, err)
		}
		if decoded != "follower-write+updated" {
			t.Fatalf("the leader holds %v at %s, want the value Update produced", decoded, path)
		}
		checked++
	}
	if checked != 2 {
		t.Fatalf("CONTROL FAILED: only %d followers were exercised, want 2", checked)
	}
}

func TestLeaderPathTakesNoForwardingDetour(t *testing.T) {
	nodes := newForwardCluster(t, "flow-detour", 3, func(c *Config) { c.tuning = fastElections })
	leader := waitClusterLeader(t, nodes)
	forwarder := otherThan(nodes, leader)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	const writes = 50

	before := totalForwardHandshakes(nodes)
	leaderStart := time.Now()
	for i := range writes {
		if err := leader.ledger.Store().Save(ctx, fmt.Sprintf("heap/leader-%d", i), i); err != nil {
			t.Fatalf("leader-local save %d: %v", i, err)
		}
	}
	leaderElapsed := time.Since(leaderStart)
	afterLeader := totalForwardHandshakes(nodes)

	forwardedStart := time.Now()
	for i := range writes {
		if err := forwarder.ledger.Store().Save(ctx, fmt.Sprintf("heap/forwarded-%d", i), i); err != nil {
			t.Fatalf("forwarded save %d: %v", i, err)
		}
	}
	forwardedElapsed := time.Since(forwardedStart)
	afterForwarded := totalForwardHandshakes(nodes)

	leaderDials := afterLeader - before
	forwardedDials := afterForwarded - afterLeader

	t.Logf("MEASURED per operation: %d leader-local saves opened %d forwarding connections at %s each; %d forwarded saves opened %d at %s each",
		writes, leaderDials, leaderElapsed/writes, writes, forwardedDials, forwardedElapsed/writes)

	if leaderDials != 0 {
		t.Fatalf("a leader serving its OWN writes opened %d forwarding connections, want 0: the leader path is taking a forwarding detour", leaderDials)
	}
	// EQUALITY rather than a bound, and it is also the control proving the counter CAN
	// move — without it the zero above would prove only that nothing was ever counted.
	// A retry storm is invisible in a latency band and obvious here.
	if forwardedDials != writes {
		t.Fatalf("%d forwarded saves opened %d forwarding connections, want exactly %d: one dial per operation and no more",
			writes, forwardedDials, writes)
	}
}

func TestForwardingFailsLoudlyAtItsBound(t *testing.T) {
	nodes := newForwardCluster(t, "flow-bound", 3, func(c *Config) {
		c.tuning = fastElections
		c.ForwardTimeout = 2 * time.Second
	})
	leader := waitClusterLeader(t, nodes)
	forwarder := otherThan(nodes, leader)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// CONTROL: forwarding works on this group while quorum holds, so the failure below
	// is caused by the lost quorum rather than by a broken path.
	if err := forwarder.ledger.Store().Save(ctx, "heap/pre-quorum", "landed"); err != nil {
		t.Fatalf("CONTROL FAILED: a forwarded save while quorum held: %v", err)
	}

	// DESTROY QUORUM: one of three survives, so no leader can ever be elected again.
	// This is the condition no retry repairs.
	for _, node := range nodes {
		if node == forwarder {
			continue
		}
		if err := node.ledger.Close(); err != nil {
			t.Fatalf("closing %s: %v", node.id, err)
		}
	}

	start := time.Now()
	err := forwarder.ledger.Store().Save(ctx, "heap/no-quorum", "never")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrForwardBoundExceeded) {
		t.Fatalf("with quorum destroyed the save gave %v after %s, want an error satisfying ErrForwardBoundExceeded", err, elapsed)
	}
	if !regexp.MustCompile(`\d+ attempts`).MatchString(err.Error()) {
		t.Fatalf("the bound failure %q does not name how many attempts were made", err)
	}
	t.Logf("failed loudly after %s with: %v", elapsed, err)

	// It FAILS rather than retrying forever, which is the other half of the claim.
	const ceiling = 15 * time.Second
	if elapsed > ceiling {
		t.Fatalf("the bound fired only after %s, beyond the %s ceiling: it is retrying against a condition it cannot repair", elapsed, ceiling)
	}
}

func TestForwardingConvergesAcrossALeadershipChange(t *testing.T) {
	// PRODUCTION election timers deliberately: adjust is nil, so no tuning is applied
	// and raft's own DefaultConfig governs detection and campaigning.
	nodes := newForwardCluster(t, "flow-move", 3, nil)
	leader := waitClusterLeader(t, nodes)
	forwarder := otherThan(nodes, leader)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	// LEG 1: leadership lands ON the forwarding node while a save is in flight. This
	// is the case a loop treating self-resolution as a refusal never escapes.
	onto := make(chan error, 1)
	start := time.Now()
	go func() { onto <- forwarder.ledger.Store().Save(ctx, "heap/onto", "onto") }()
	transferLeadership(t, leader, forwarder)
	if err := <-onto; err != nil {
		t.Fatalf("a save in flight while leadership moved ONTO the forwarder: %v", err)
	}
	t.Logf("leadership onto the forwarder: converged in %s", time.Since(start))

	// LEG 2: leadership moves AWAY, so the forwarder must re-resolve to a leader whose
	// identity changed.
	third := otherThan(nodes, forwarder)
	transferLeadership(t, forwarder, third)

	start = time.Now()
	if err := forwarder.ledger.Store().Save(ctx, "heap/away", "away"); err != nil {
		t.Fatalf("a save after leadership moved AWAY from the forwarder: %v", err)
	}
	t.Logf("leadership away from the forwarder: converged in %s", time.Since(start))

	// Both values are CONFIRMED on the node that now leads, rather than accepted from
	// the writer's own success.
	for _, path := range []string{"heap/onto", "heap/away"} {
		entry, present, err := third.ledger.getLocal(ctx, path)
		if err != nil || !present {
			t.Fatalf("the leader does not hold %s (present=%v err=%v)", path, present, err)
		}
		if _, err := decodeValue(entry.Value); err != nil {
			t.Fatalf("decoding %s on the leader: %v", path, err)
		}
	}
}

func TestForwardingSurvivesALeaderCrash(t *testing.T) {
	// PRODUCTION election timers, as above.
	nodes := newForwardCluster(t, "flow-crash", 3, nil)
	leader := waitClusterLeader(t, nodes)
	forwarder := otherThan(nodes, leader)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	// CONTROL: a forwarded save worked on this group before the stop.
	if err := forwarder.ledger.Store().Save(ctx, "heap/pre-crash", "pre"); err != nil {
		t.Fatalf("CONTROL FAILED: a forwarded save before the crash: %v", err)
	}

	// AN ORDERLY STOP. Draining the shutdown future is exactly what RELEASES the
	// transport, so the group is unbound and peers learn by connection refusal rather
	// than by heartbeat timeout. The stopped-but-still-bound shape is a different
	// state with its own gate.
	if err := leader.ledger.Close(); err != nil {
		t.Fatalf("stopping the leader: %v", err)
	}

	start := time.Now()
	if err := forwarder.ledger.Store().Save(ctx, "heap/post-crash", "post"); err != nil {
		t.Fatalf("a forwarded save across the leader stop: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("MEASURED leader crash (orderly stop, transport released, peers detect by refusal): forwarded save converged in %s", elapsed)

	// CONFIRMED on the new leader rather than merely accepted from the writer.
	newLeader := waitClusterLeader(t, survivorsOf(nodes, leader))
	entry, present, err := newLeader.ledger.getLocal(ctx, "heap/post-crash")
	if err != nil || !present {
		t.Fatalf("the new leader %s does not hold the save made across the crash (present=%v err=%v)", newLeader.id, present, err)
	}
	decoded, err := decodeValue(entry.Value)
	if err != nil {
		t.Fatalf("decoding what the new leader holds: %v", err)
	}
	if decoded != "post" {
		t.Fatalf("the new leader holds %v, want the value written across the crash", decoded)
	}
}

func TestForwardingSurvivesALeaderStoppedWithItsTransportBound(t *testing.T) {
	const flow = "flow-stopped-bound"
	nodes := newForwardCluster(t, flow, 3, func(c *Config) { c.tuning = fastElections })
	leader := waitClusterLeader(t, nodes)
	forwarder := otherThan(nodes, leader)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// CONTROL 1: forwarding worked on this group before the stop.
	if err := forwarder.ledger.Store().Save(ctx, "heap/pre-stop", "pre"); err != nil {
		t.Fatalf("CONTROL FAILED: a forwarded save before the stop: %v", err)
	}

	// EXACTLY THE ShutdownOnRemove SHAPE: raft stops and its future is DISCARDED, so
	// nothing releases the transport and this node goes on ANSWERING forwarded
	// requests with something that is neither a refusal nor a success.
	_ = leader.ledger.raft.Shutdown()

	// CONTROL 2: the group is genuinely STILL BOUND. Against a released group this
	// would be exercising a dial error, which the loop already handled, and would
	// prove nothing about this state.
	if group, err := leader.mux.Bind(transport.GroupID(flow)); err == nil {
		_ = group.Close()
		t.Fatal("CONTROL FAILED: the stopped leader's group was already released, so this is not the stopped-but-bound state")
	} else if !errors.Is(err, transport.ErrGroupBound) {
		t.Fatalf("CONTROL FAILED: rebinding the stopped leader's group gave %v, want ErrGroupBound", err)
	}

	start := time.Now()
	err := forwarder.ledger.Store().Save(ctx, "heap/post-stop", "post")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("a forwarded save to a leader stopped with its transport bound failed after %s: %v; it died on an early attempt rather than retrying to the new leader",
			elapsed, err)
	}
	t.Logf("MEASURED stopped-but-bound leader: the forwarded save converged in %s", elapsed)

	newLeader := waitClusterLeader(t, survivorsOf(nodes, leader))
	entry, present, err := newLeader.ledger.getLocal(ctx, "heap/post-stop")
	if err != nil || !present {
		t.Fatalf("the new leader %s does not hold the forwarded save (present=%v err=%v)", newLeader.id, present, err)
	}
	if decoded, derr := decodeValue(entry.Value); derr != nil || decoded != "post" {
		t.Fatalf("the new leader holds %v (err %v), want the value the retry landed", decoded, derr)
	}
}

func TestAStalledPeerDoesNotAccumulateHandlers(t *testing.T) {
	mux := testMux(t)
	l := openTestLedger(t, Config{Flow: "flow-stalled", LocalID: "n0", Mux: mux, Bootstrap: true})
	waitLeadership(t, l)

	// The reply has to be big enough that the handler's Encode cannot simply hand it
	// to the socket buffer and finish. A large stored value plus a deliberately tiny
	// receive buffer on this end is what parks the handler in the WRITE.
	large := make([]byte, 1<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := l.appendLocal(ctx, Entry{Kind: KindSet, Path: "heap/stalled", Value: large}); err != nil {
		t.Fatalf("seeding the large value: %v", err)
	}

	conn, err := l.group.DialForward(mux.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dialing the forwarding stream: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.SetReadBuffer(1024); err != nil {
			t.Fatalf("shrinking the receive buffer: %v", err)
		}
	}

	// A VALID request, and then this peer never reads the reply. Close is unaffected
	// here — closeInflight ends the connection — so no close-path gate would notice;
	// what accumulates is a goroutine and a file descriptor per stalled peer.
	if err := gob.NewEncoder(conn).Encode(forwardRequest{Op: opLoad, Path: "heap/stalled"}); err != nil {
		t.Fatalf("encoding a valid request: %v", err)
	}

	// CONTROL: wait until the ledger reports exactly one in-flight handler, so the zero
	// asserted below cannot have come from nothing ever being served.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && l.inflightForwards() != 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := l.inflightForwards(); got != 1 {
		t.Fatalf("CONTROL FAILED: %d forwarding connections in flight, want 1; no handler ever took the request", got)
	}

	// THE CEILING IS THE ASSERTION. With only the read bounded the handler holds this
	// connection for the ledger's lifetime; with one deadline covering the whole
	// exchange it lets go when forwardRequestTimeout expires.
	ceiling := forwardRequestTimeout + 5*time.Second
	start := time.Now()
	released := time.Now().Add(ceiling)
	for time.Now().Before(released) && l.inflightForwards() != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	elapsed := time.Since(start)

	if got := l.inflightForwards(); got != 0 {
		t.Fatalf("a handler was still holding its connection %s after the peer stopped reading (%d in flight, bound %s): handlers accumulate for the ledger's lifetime",
			elapsed, got, forwardRequestTimeout)
	}
	t.Logf("MEASURED stalled peer: the handler released its connection after %s, against a %s bound on the whole exchange",
		elapsed, forwardRequestTimeout)
}

func TestConfigRefusesANegativeDurationRatherThanCoercingIt(t *testing.T) {
	// CONTROL FIRST: the identical Config with BOTH durations at their declared zero
	// opens and reaches leadership, so a refusal below cannot be coming from any other
	// field being wrong.
	control := openTestLedger(t, Config{
		Flow: "flow-negdur-control", LocalID: "n0", Mux: testMux(t), Bootstrap: true,
		ReadTimeout: 0, ForwardTimeout: 0,
	})
	waitLeadership(t, control)

	cases := []struct {
		name  string
		apply func(*Config)
	}{
		{"ReadTimeout", func(c *Config) { c.ReadTimeout = -1 * time.Second }},
		{"ForwardTimeout", func(c *Config) { c.ForwardTimeout = -1 * time.Second }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Flow: "flow-negdur-" + tc.name, LocalID: "n0", Mux: testMux(t), Bootstrap: true}
			tc.apply(&cfg)

			l, err := Open(cfg)
			if err == nil {
				closeWithin(t, l, cleanupCloseCeiling)
				t.Fatalf("Open with a negative %s gave <nil>, want an error satisfying ErrConfigNegativeDuration rather than a ledger running a coerced bound", tc.name)
			}
			if !errors.Is(err, ErrConfigNegativeDuration) {
				t.Fatalf("Open with a negative %s gave %v, want an error satisfying ErrConfigNegativeDuration", tc.name, err)
			}
			// It names the FIELD, so an operator reading the error knows which one they
			// mistyped rather than only that some duration was wrong.
			if !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("the refusal %q does not name the offending field %s", err, tc.name)
			}
		})
	}
}
