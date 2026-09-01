package transport

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// idOffset is where the group id begins in an encoded preamble: past the fixed
// head and past the authentication block. Every sign and verify call takes it.
const idOffset = preambleLen + authLen

// tokenMux builds a loopback mux carrying tokens. Loopback is what lets a test
// mux hold tokens at all without tripping the advertisement rule, and holding
// them is what makes the admission gate live rather than a no-op.
func tokenMux(t *testing.T, tokens ...Token) *Mux {
	t.Helper()
	m, err := New(Config{
		BindAddr:         "127.0.0.1:0",
		HandshakeTimeout: 500 * time.Millisecond,
		RPCTimeout:       time.Second,
		Tokens:           tokens,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// dialRaw dials m and writes head verbatim, so a test can present bytes no
// signer would produce.
func dialRaw(t *testing.T, m *Mux, head []byte) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", m.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Write(head); err != nil {
		t.Fatalf("write preamble: %v", err)
	}
	return c
}

// headUnder mints a preamble announcing kind and id under tok specifically,
// rather than under the mux's own dialing token.
func headUnder(t *testing.T, kind StreamKind, id GroupID, tok Token) []byte {
	t.Helper()
	head, err := encodePreamble(kind, id, newSigner([]Token{tok}))
	if err != nil {
		t.Fatalf("encodePreamble: %v", err)
	}
	return head
}

// dialUnder dials m under tok and returns the RAW connection, for the refusal
// cases: a refused connection never reaches a session exchange.
func dialUnder(t *testing.T, m *Mux, kind StreamKind, id GroupID, tok Token) net.Conn {
	t.Helper()
	return dialRaw(t, m, headUnder(t, kind, id, tok))
}

// dialHeadSession dials m, writes a hand-minted preamble, and completes the
// client half of the session exchange under the token that preamble was signed
// with — which is what a DELIVERED connection's peer must do.
func dialHeadSession(t *testing.T, m *Mux, head []byte, tok Token) net.Conn {
	t.Helper()
	c := dialRaw(t, m, head)
	wrapped, err := wrapDialed(c, tok, head[nonceOff:stampOff], 3*time.Second)
	if err != nil {
		t.Fatalf("session exchange: %v", err)
	}
	return wrapped
}

// assertRefused fails unless the mux CLOSED c without writing a byte back.
//
// THE TIMEOUT LEG IS WHAT MAKES THIS AN ASSERTION. A closed connection and an
// idle one both fail a read, and only one of them is a refusal — so a helper
// that stopped at "the read errored" would pass for a connection the mux
// ACCEPTED and simply never wrote to, which is precisely the state a broken
// admission gate leaves behind.
func assertRefused(t *testing.T, c net.Conn, why string) {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := c.Read(make([]byte, 1))
	if err == nil {
		t.Fatalf("%s: the connection was not refused", why)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("%s: the read timed out rather than the peer closing, so the connection was ADMITTED and left "+
			"idle rather than refused: %v", why, err)
	}
}

// assertDelivered fails unless the connection reached s's accept queue carrying
// its payload, which is what proves it was routed rather than merely tolerated.
// The accept wait is BOUNDED. groupStream.Accept parks on a channel, so a
// connection the mux refused instead of routing would hang this helper until the
// whole suite hit its own timeout — turning a clear assertion failure into a
// dead run that reports nothing about which property broke.
func assertDelivered(t *testing.T, s *groupStream, c net.Conn, payload, why string) {
	t.Helper()
	if _, err := c.Write([]byte(payload)); err != nil {
		t.Fatalf("%s: write: %v", why, err)
	}
	type arrival struct {
		conn net.Conn
		err  error
	}
	ch := make(chan arrival, 1)
	go func() {
		conn, err := s.Accept()
		ch <- arrival{conn, err}
	}()
	var arrived arrival
	select {
	case arrived = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: nothing reached the accept queue within five seconds: the connection was refused, not routed",
			why)
	}
	accepted, err := arrived.conn, arrived.err
	if err != nil {
		t.Fatalf("%s: Accept: %v", why, err)
	}
	defer func() { _ = accepted.Close() }()
	if err := accepted.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(accepted, got); err != nil {
		t.Fatalf("%s: read: %v", why, err)
	}
	if string(got) != payload {
		t.Fatalf("%s: delivered %q, want %q", why, got, payload)
	}
}

func TestSignedPreambleIsAdmittedAndAnUnsignedOneIsRefusedAndCounted(t *testing.T) {
	m := tokenMux(t, "join-token")
	s, err := m.bindStream("known")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	assertDelivered(t, s, dialSessionTagged(t, m, KindRaft, "known"), "SIGNED",
		"a preamble signed with the accepted token")

	// THE DISCRIMINATING CASE. The unsigned connection announces a group this
	// node does NOT host. An implementation that looked the group up first and
	// verified second would count it as an unknown group, telling an
	// unauthenticated peer which ids exist here. It must count as unauthenticated.
	unsigned, err := encodePreamble(KindRaft, "not-bound", openSigner)
	if err != nil {
		t.Fatal(err)
	}
	assertRefused(t, dialRaw(t, m, unsigned), "an unsigned preamble against a token-carrying mux")

	st := m.Stats()
	if st.RejectedUnauthenticated != 1 {
		t.Fatalf("RejectedUnauthenticated = %d, want 1: every refusal moves a counter", st.RejectedUnauthenticated)
	}
	if st.RejectedUnknownGroup != 0 {
		t.Fatalf("RejectedUnknownGroup = %d, want 0: the proof must be verified before the routing decision, "+
			"or an unauthenticated peer learns which group ids this node hosts", st.RejectedUnknownGroup)
	}
	if st.RejectedMalformed != 0 {
		t.Fatalf("RejectedMalformed = %d, want 0: an unsigned handshake is unauthenticated, not malformed",
			st.RejectedMalformed)
	}
	if st.Handshakes != 1 {
		t.Fatalf("Handshakes = %d, want 1: the signed connection must have been admitted", st.Handshakes)
	}
}

func TestPreambleOutsideTheClockWindowIsRefusedAndCounted(t *testing.T) {
	const tok Token = "join-token"
	m := tokenMux(t, tok)
	s, err := m.bindStream("known")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	// staleAt mints a preamble whose stamp sits at skew and whose tag COVERS
	// that stamp, so the refusal can only be the window and never a broken MAC.
	staleAt := func(skew time.Duration) []byte {
		signing := newSigner([]Token{tok})
		head, encErr := encodePreamble(KindRaft, "known", signing)
		if encErr != nil {
			t.Fatal(encErr)
		}
		putStamp(head, time.Now().Add(skew))
		signing.snapshot()[0].mac(head[macOff:macOff:idOffset], head, idOffset)
		return head
	}

	// The control: the same mint at no skew is admitted, so a refusal below is
	// the stamp rather than anything else about these bytes.
	assertDelivered(t, s, dialHeadSession(t, m, staleAt(0), tok), "FRESH", "a freshly stamped proof")

	assertRefused(t, dialRaw(t, m, staleAt(-2*clockSkewWindow)), "a proof stamped two windows in the past")
	assertRefused(t, dialRaw(t, m, staleAt(2*clockSkewWindow)), "a proof stamped two windows in the future")

	if st := m.Stats(); st.RejectedUnauthenticated != 2 {
		t.Fatalf("RejectedUnauthenticated = %d, want 2: both directions of the skew window must refuse and count",
			st.RejectedUnauthenticated)
	}
}

func TestAProofMintedForOneGroupDoesNotAdmitAnother(t *testing.T) {
	const tok Token = "join-token"
	m := tokenMux(t, tok)
	// BOTH ids are bound, so a refusal cannot be an unknown group. They are the
	// same length so the lifted proof needs no re-framing of the preamble.
	alpha, err := m.bindStream("alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = alpha.Close() }()
	bravo, err := m.bindStream("bravo")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bravo.Close() }()

	head, err := encodePreamble(KindRaft, "alpha", newSigner([]Token{tok}))
	if err != nil {
		t.Fatal(err)
	}
	copy(head[idOffset:], "bravo")
	assertRefused(t, dialRaw(t, m, head), "a proof minted for alpha presented on a connection announcing bravo")

	st := m.Stats()
	if st.RejectedUnauthenticated != 1 {
		t.Fatalf("RejectedUnauthenticated = %d, want 1: the tag must cover the group id", st.RejectedUnauthenticated)
	}
	if st.RejectedUnknownGroup != 0 {
		t.Fatalf("RejectedUnknownGroup = %d, want 0: bravo is bound, so this refusal must be the proof",
			st.RejectedUnknownGroup)
	}
}

func TestAProofMintedForOneKindDoesNotAdmitAnother(t *testing.T) {
	const tok Token = "join-token"
	m := tokenMux(t, tok)
	s, err := m.bindStream("known")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	// A membership proof lifted onto a raft connection. The kind lives in the
	// head, which the tag covers, so rewriting byte 5 invalidates it.
	head, err := encodePreamble(KindMembership, "known", newSigner([]Token{tok}))
	if err != nil {
		t.Fatal(err)
	}
	head[5] = byte(KindRaft)
	assertRefused(t, dialRaw(t, m, head), "a membership proof presented on a raft connection")

	st := m.Stats()
	if st.RejectedUnauthenticated != 1 {
		t.Fatalf("RejectedUnauthenticated = %d, want 1: the tag must cover the handshake kind",
			st.RejectedUnauthenticated)
	}
	if st.RejectedUnknownGroup != 0 {
		t.Fatalf("RejectedUnknownGroup = %d, want 0: known is bound, so this refusal must be the proof",
			st.RejectedUnknownGroup)
	}
}

func TestTheEncodedPreambleNeverCarriesTheTokenBytes(t *testing.T) {
	const tok Token = "a-distinctive-join-token-value"
	signing := newSigner([]Token{tok})
	// Many encodes, because the nonce moves every one of them: a layout that
	// leaked the secret in some encodes and not others would survive a single
	// sample.
	for i := 0; i < 256; i++ {
		head, err := encodePreamble(KindRaft, "flow-alpha", signing)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(head, []byte(tok)) {
			t.Fatalf("encode %d put the token on the wire: the handshake must carry a MAC, never the secret", i)
		}
		// The control: the bytes the token DOES authenticate are present, so a
		// pass above is an absent secret rather than an empty search.
		if !bytes.Contains(head, []byte("flow-alpha")) {
			t.Fatal("CONTROL FAILED: the encoded preamble does not contain the group id it announces")
		}
	}
}

func TestUnauthenticatedMuxIsRefusedOnANonLoopbackAdvertisement(t *testing.T) {
	routable := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 8300}

	m, err := New(Config{BindAddr: "127.0.0.1:0", Advertise: routable})
	if !errors.Is(err, ErrTokenRequired) {
		if m != nil {
			_ = m.Close()
		}
		t.Fatalf("err = %v, want ErrTokenRequired: a mux accepting every proof must not advertise a routable address",
			err)
	}

	// Two same-run controls. The same routable advertisement WITH a token opens,
	// so the refusal above is the empty token set and not the address; and the
	// same empty token set on loopback opens, so dev and tests stay zero-config.
	withToken, err := New(Config{BindAddr: "127.0.0.1:0", Advertise: routable, Tokens: []Token{"join-token"}})
	if err != nil {
		t.Fatalf("CONTROL FAILED: a routable advertisement carrying a token was refused: %v", err)
	}
	_ = withToken.Close()

	onLoopback, err := New(Config{BindAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("CONTROL FAILED: an unauthenticated loopback mux was refused: %v", err)
	}
	_ = onLoopback.Close()
}

func TestRotationAdmitsBothTokensDuringOverlapAndOnlyTheNewOneAfter(t *testing.T) {
	const outgoing Token = "outgoing-token"
	const incoming Token = "incoming-token"

	t.Run("overlap admits both", func(t *testing.T) {
		m := tokenMux(t, outgoing)
		s, err := m.bindStream("known")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		m.SetTokens(incoming, outgoing)
		assertDelivered(t, s, dialHeadSession(t, m, headUnder(t, KindRaft, "known", outgoing), outgoing),
			"OLD", "a peer still holding the outgoing token")
		assertDelivered(t, s, dialHeadSession(t, m, headUnder(t, KindRaft, "known", incoming), incoming),
			"NEW", "a peer already holding the incoming token")
	})

	t.Run("narrowed admits only the new one", func(t *testing.T) {
		m := tokenMux(t, outgoing)
		s, err := m.bindStream("known")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		m.SetTokens(incoming, outgoing)
		m.SetTokens(incoming)
		assertDelivered(t, s, dialHeadSession(t, m, headUnder(t, KindRaft, "known", incoming), incoming),
			"NEW", "a peer holding the incoming token")
		assertRefused(t, dialUnder(t, m, KindRaft, "known", outgoing), "a peer holding the withdrawn token")
		if st := m.Stats(); st.RejectedUnauthenticated != 1 {
			t.Fatalf("RejectedUnauthenticated = %d, want 1: withdrawing a token must refuse and count",
				st.RejectedUnauthenticated)
		}
	})

	// THE DISCRIMINATING LEG. An implementation that grew a separate revoke path
	// would pass both cases above. This one proves that narrowing the accepted
	// set is ITSELF what withdraws admission: the same SetTokens call, given one
	// element, is the whole revocation.
	t.Run("revocation is the same narrowing", func(t *testing.T) {
		const compromised Token = "compromised-token"
		m := tokenMux(t, compromised, incoming)
		s, err := m.bindStream("known")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		assertDelivered(t, s, dialHeadSession(t, m, headUnder(t, KindRaft, "known", compromised), compromised),
			"BEFORE", "a peer holding the token that is about to be revoked")
		m.SetTokens(incoming)
		assertRefused(t, dialUnder(t, m, KindRaft, "known", compromised),
			"the same peer after the accepted set was narrowed to one element")
		if st := m.Stats(); st.RejectedUnauthenticated != 1 {
			t.Fatalf("RejectedUnauthenticated = %d, want 1: revocation is a narrowing of the accepted set, "+
				"and it must refuse and count exactly as any other refusal does", st.RejectedUnauthenticated)
		}
	})
}
