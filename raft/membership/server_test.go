package membership

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/whitaker-io/machine/raft/transport"
)

// testNode builds a mux and a manager over it, cleaned up in reverse order.
func testNode(t *testing.T, node string) (*Manager, *transport.Mux) {
	t.Helper()
	mux, err := transport.New(transport.Config{
		BindAddr:         "127.0.0.1:0",
		HandshakeTimeout: 2 * time.Second,
		RPCTimeout:       2 * time.Second,
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	mgr, err := New(Config{Node: node, Advertise: mux.Addr().String(), Mux: mux, Logger: hclog.NewNullLogger()})
	if err != nil {
		_ = mux.Close()
		t.Fatalf("membership.New: %v", err)
	}
	t.Cleanup(func() {
		_ = mgr.Close()
		_ = mux.Close()
	})
	return mgr, mux
}

// countingDialer wraps a manager's dial path so a test can count the connections
// a stats round opens. It replaces the field rather than observing the socket,
// because what is under test is how many times the client CHOSE to dial.
func countingDialer(m *Manager) *int64 {
	var mu sync.Mutex
	var dials int64
	inner := m.peers.dial
	m.peers.dial = func(address string, timeout time.Duration) (net.Conn, error) {
		mu.Lock()
		dials++
		mu.Unlock()
		return inner(address, timeout)
	}
	return &dials
}

// answersFlows makes a node report itself on every flow named.
func answersFlows(m *Manager, flows ...string) {
	known := make(map[string]bool, len(flows))
	for _, f := range flows {
		known[f] = true
	}
	m.SetLocalStats(func(flow string) (FlowStats, bool) {
		if !known[flow] {
			return FlowStats{}, false
		}
		return FlowStats{Term: 7, LastIndex: 42, LastContact: time.Millisecond, Voter: true}, true
	})
}

func TestOnePeerConnectionServesEveryFlowInOneRound(t *testing.T) {
	asker, _ := testNode(t, "asker")
	answerer, answererMux := testNode(t, "answerer")
	// THREE flows rather than one is the discriminating input: a per-flow
	// implementation passes any single-flow test.
	flows := []string{"alpha", "bravo", "charlie"}
	answersFlows(answerer, flows...)

	dials := countingDialer(asker)
	asker.SetPeers([]string{answererMux.Addr().String()}, flows)

	view := asker.peers.statsView()
	byFlow, ok := view[answererMux.Addr().String()]
	if !ok {
		t.Fatalf("the peer is absent from the view; failures: %v", asker.PeerFailures())
	}
	if len(byFlow) != len(flows) {
		t.Fatalf("the round returned %d flows, want %d: one round must carry the whole flow list", len(byFlow), len(flows))
	}
	for _, f := range flows {
		if _, ok := byFlow[f]; !ok {
			t.Fatalf("flow %q is missing from a round that asked for it", f)
		}
	}
	if *dials < 1 {
		t.Fatal("CONTROL FAILED: the recording dialer observed no dial at all, so a round that opened nothing " +
			"would read as efficient")
	}
	if *dials != 1 {
		t.Fatalf("three shared flows opened %d connections, want exactly 1", *dials)
	}
}

func TestManyLedFlowsCoalesceIntoOneStatsRoundPerPeer(t *testing.T) {
	asker, _ := testNode(t, "asker")
	answerer, answererMux := testNode(t, "answerer")
	const led = 10
	flows := make([]string, 0, led)
	for i := 0; i < led; i++ {
		flows = append(flows, fmt.Sprintf("flow-%02d", i))
	}
	answersFlows(answerer, flows...)

	dials := countingDialer(asker)
	asker.SetPeers([]string{answererMux.Addr().String()}, flows)

	// ONE PROMOTER PER FLOW is the shape above this client: each asks on its own
	// schedule. Ten independent per-flow reads inside one interval must still be
	// one round, or the flow list in the request is defeated by the fan-out.
	var wg sync.WaitGroup
	for _, f := range flows {
		wg.Add(1)
		go func(flow string) {
			defer wg.Done()
			if got := asker.Stats(flow); len(got) == 0 {
				t.Errorf("flow %q got no peer stats", flow)
			}
		}(f)
	}
	wg.Wait()

	t.Logf("dials observed: %d for %d led flows", *dials, led)
	if *dials != 1 {
		t.Fatalf("%d led flows issued %d stats rounds to one peer in a single interval, want exactly 1: "+
			"the per-flow callers must share one view", led, *dials)
	}
}

func TestASlowMembershipHandlerDoesNotDelayAnotherConnection(t *testing.T) {
	asker, _ := testNode(t, "asker")
	answerer, answererMux := testNode(t, "answerer")

	parked := make(chan struct{})
	released := make(chan struct{})
	var once sync.Once
	answerer.SetLocalStats(func(flow string) (FlowStats, bool) {
		if flow == "slow" {
			once.Do(func() { close(parked) })
			<-released
		}
		return FlowStats{Term: 1}, true
	})
	defer close(released)

	// Park one handler.
	go func() {
		asker.SetPeers([]string{answererMux.Addr().String()}, []string{"slow"})
		_ = asker.peers.statsView()
	}()
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("CONTROL FAILED: the slow handler never parked, so this test proves nothing about isolation")
	}

	// A SECOND, INDEPENDENT client must complete its exchange while that handler
	// is still parked. The ceiling makes a block fail rather than pass slowly.
	other, _ := testNode(t, "other")
	other.SetPeers([]string{answererMux.Addr().String()}, []string{"fast"})
	done := make(chan map[string]map[string]FlowStats, 1)
	go func() { done <- other.peers.statsView() }()
	start := time.Now()
	select {
	case view := <-done:
		if len(view) == 0 {
			t.Fatalf("the second exchange returned nothing; failures: %v", other.PeerFailures())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a second connection's exchange did not complete while one handler was parked: " +
			"the acceptor is serving connections on one goroutine")
	}
	t.Logf("the second exchange completed in %v with one handler still parked", time.Since(start))
}

func TestAnOversizedControlMessageIsRefusedAtTheCeiling(t *testing.T) {
	// THE KNOWN-POSITIVE the ceiling obligation requires: a head announcing a
	// length far above the ceiling, with NO body behind it. If the reader sized
	// anything by that length it would go on to read the body and fail with an
	// unexpected EOF; returning the NAMED refusal instead is what proves the
	// ceiling engaged before the length was used.
	head := make([]byte, controlHeadLen)
	head[0] = byte(msgStats)
	binary.BigEndian.PutUint32(head[1:], ^uint32(0))
	if _, _, err := readMessage(bytes.NewReader(head)); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("err = %v, want ErrMessageTooLarge: an attacker-supplied length reached an allocation", err)
	}

	// THE CONTROL, same instrument: a head announcing a legal length, with its
	// body, decodes. Without it a reader that refused everything would pass.
	var body bytes.Buffer
	if err := gob.NewEncoder(&body).Encode(statsRequest{Flows: []string{"alpha"}}); err != nil {
		t.Fatal(err)
	}
	legal := make([]byte, controlHeadLen)
	legal[0] = byte(msgStats)
	binary.BigEndian.PutUint32(legal[1:], uint32(body.Len()))
	kind, got, err := readMessage(bytes.NewReader(append(legal, body.Bytes()...)))
	if err != nil {
		t.Fatalf("CONTROL FAILED: a legally sized message was refused: %v", err)
	}
	if kind != msgStats {
		t.Fatalf("CONTROL FAILED: kind = %d, want %d", uint8(kind), uint8(msgStats))
	}
	var decoded statsRequest
	if err := decodeMessage(got, &decoded); err != nil || len(decoded.Flows) != 1 {
		t.Fatalf("CONTROL FAILED: the legal message did not decode: %v %+v", err, decoded)
	}

	// AND END TO END through the real acceptor: an oversized head on a real
	// control connection is refused and the connection closed, rather than the
	// node allocating what the peer asked it to.
	answerer, answererMux := testNode(t, "answerer")
	conn := dialControl(t, answererMux)
	if _, err := conn.Write(head); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("an oversized control message was served rather than refused")
	}
	waitFor(t, 3*time.Second, func() bool { return answerer.InFlight() == 0 },
		"the handler for an oversized message never exited")
}

// dialControl opens a raw control connection to a node's shared listener,
// completing the transport handshake and session exchange but writing no control
// message, so a test can drive hostile bytes by hand.
func dialControl(t *testing.T, mux *transport.Mux) net.Conn {
	t.Helper()
	link, err := mux.BindMembership()
	if err == nil {
		// The node under test binds the control channel; if this succeeded the
		// manager is not serving and the test would prove nothing.
		_ = link.Close()
		t.Fatal("CONTROL FAILED: the membership link was unbound, so no manager is serving it")
	}
	dialer, err := transport.New(transport.Config{BindAddr: "127.0.0.1:0", HandshakeTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialer.Close() })
	dl, err := dialer.BindMembership()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dl.Dial(mux.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("control dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// waitFor polls until cond holds or the window expires.
func waitFor(t *testing.T, window time.Duration, cond func() bool, why string) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(why)
}

func TestCloseReturnsPromptlyWithAOneByteSilentPeerAttached(t *testing.T) {
	mgr, mux := testNode(t, "target")
	exited := make(chan struct{})
	var once sync.Once
	mgr.mu.Lock()
	mgr.onHandlerExit = func() {
		// THE SEAM DELIBERATELY TAKES MEASURABLE TIME, and that is what makes the
		// handler-exit assertion below discriminating rather than a race the
		// handler usually wins. A Close that JOINS its handlers cannot return
		// until this returns; a Close that abandons them returns while this is
		// still sleeping. Without the delay, an abandoning Close is the FASTEST
		// implementation and satisfies the ceiling perfectly — measured: with a
		// zero-cost seam, deleting Close's wait left this test green.
		time.Sleep(100 * time.Millisecond)
		once.Do(func() { close(exited) })
	}
	mgr.mu.Unlock()

	// THE HOSTILE INPUT: a peer that completes the handshake, sends ONE BYTE of
	// a control message, and then goes silent WITHOUT closing. An io.LimitedReader
	// cannot bound that, and the connection carries no deadline of its own.
	conn := dialControl(t, mux)
	if _, err := conn.Write([]byte{byte(msgStats)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mgr.InFlight() >= 1 },
		"CONTROL FAILED: the silent peer never reached a handler, so a shutdown that ran with nothing attached "+
			"would read as prompt")
	t.Log("the silent peer was accepted and dispatched")

	start := time.Now()
	if err := mgr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("Close returned in %v", elapsed)

	select {
	case <-exited:
		t.Log("handler exited before Close returned")
	default:
		t.Fatal("Close returned while its handler was still running: a Close that abandons its handlers would " +
			"tear down state underneath one")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Close took %v, above the 500ms ceiling: the in-flight connections must be closed before the "+
			"handlers are joined, or Close waits out the read deadline", elapsed)
	}
}

func TestASilentPeersHandlerIsReapedWithoutClose(t *testing.T) {
	mgr, mux := testNode(t, "target")
	const silent = 5
	var mu sync.Mutex
	reaped := 0
	done := make(chan struct{})
	mgr.mu.Lock()
	mgr.onHandlerExit = func() {
		mu.Lock()
		reaped++
		full := reaped == silent
		mu.Unlock()
		if full {
			close(done)
		}
	}
	mgr.mu.Unlock()

	// NO CLOSE IS INVOLVED. This is the steady state: five half-open peers attach
	// and nothing else happens. The registry cannot help here — only the read
	// deadline reaps them, and without it each holds a goroutine and a file
	// descriptor for the process lifetime.
	for i := 0; i < silent; i++ {
		conn := dialControl(t, mux)
		if _, err := conn.Write([]byte{byte(msgStats)}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 5*time.Second, func() bool { return mgr.InFlight() >= silent },
		"CONTROL FAILED: fewer than five silent peers reached a handler, so a run where nobody attached would "+
			"read as prompt reaping")

	start := time.Now()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		mu.Lock()
		got := reaped
		mu.Unlock()
		t.Fatalf("only %d of %d silent handlers were reaped within 4s: the per-message read deadline is not "+
			"reaching the socket", got, silent)
	}
	mu.Lock()
	got := reaped
	mu.Unlock()
	t.Logf("reaped %d of %d silent handlers in %v", got, silent, time.Since(start))
}

// TestAnUnservedOrUndeclaredControlKindIsRefusedByName separates the two
// refusals the wire vocabulary distinguishes, which mean different things to an
// operator. A REPLY kind arriving at an acceptor is declared by this build and
// simply has no arm here; a kind outside the declared set is a peer speaking
// something this build does not know. Collapsing them would hide a version skew
// behind a routing bug.
func TestAnUnservedOrUndeclaredControlKindIsRefusedByName(t *testing.T) {
	mgr, _ := testNode(t, "a-node")
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	for _, kind := range []msgKind{msgAnnounceReply, msgStatsReply, msgLeaveReply} {
		err := mgr.answer(server, kind, nil)
		if !errors.Is(err, ErrUnservedMessage) {
			t.Fatalf("kind %d: err = %v, want ErrUnservedMessage", uint8(kind), err)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%d", uint8(kind))) {
			t.Fatalf("the refusal %q does not name the kind it refused", err)
		}
	}

	undeclared := msgKind(200)
	err := mgr.answer(server, undeclared, nil)
	if !errors.Is(err, ErrUnknownMessage) {
		t.Fatalf("an undeclared kind: err = %v, want ErrUnknownMessage — a version skew must stay "+
			"distinguishable from a kind this build declares but does not serve", err)
	}

	// THE CONTROL: a kind this node DOES serve is not refused by either sentinel,
	// so the refusals above are about the kinds rather than about an arm that
	// refuses everything. The write half fails on an unread pipe, which is not
	// what is under test here.
	body, encodeErr := encodeForTest(statsRequest{Flows: []string{"alpha"}})
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	go func() { _, _ = io.Copy(io.Discard, client) }()
	if err := mgr.answer(server, msgStats, body); errors.Is(err, ErrUnservedMessage) ||
		errors.Is(err, ErrUnknownMessage) {
		t.Fatalf("CONTROL FAILED: a served kind was refused as unserved or unknown: %v", err)
	}
}

// encodeForTest renders a control message body the way writeMessage does.
func encodeForTest(payload any) ([]byte, error) {
	var body bytes.Buffer
	if err := gob.NewEncoder(&body).Encode(payload); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}
