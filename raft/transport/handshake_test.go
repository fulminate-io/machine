package transport

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// openSigner is the signer an unauthenticated mux carries. An empty accepted set
// signs nothing and admits every proof, which is the zero-config loopback shape
// and exactly why the mux refuses to advertise a routable address holding one.
var openSigner = newSigner(nil)

func TestPreambleRoundTripAndNoOverRead(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	go func() {
		if _, err := writePreamble(client, KindRaft, "flow-alpha", time.Second, openSigner); err != nil {
			t.Error(err)
			return
		}
		if _, err := client.Write([]byte("RAFTBYTES")); err != nil {
			t.Error(err)
		}
	}()
	p, err := readPreamble(server, time.Second, openSigner)
	if err != nil {
		t.Fatalf("readPreamble: %v", err)
	}
	if p.ID != "flow-alpha" {
		t.Fatalf("group id = %q, want flow-alpha", p.ID)
	}
	rest := make([]byte, 9)
	if _, err := io.ReadFull(server, rest); err != nil {
		t.Fatalf("post-preamble read: %v", err)
	}
	if string(rest) != "RAFTBYTES" {
		t.Fatalf("post-preamble stream = %q, want RAFTBYTES: the handshake read past its own bytes", rest)
	}

	// The two writes above are handed over separately, and net.Pipe delivers
	// each on its own — which is why that case alone does NOT distinguish a raw
	// read from a buffered one. It was measured: a bufio.Reader spliced into
	// readPreamble passes the case above. A real TCP peer coalesces the
	// handshake and the first RPC bytes into one segment, so the case that
	// actually pins the invariant writes them as ONE write. A buffered read
	// draws the whole thing into its own buffer and the trailing bytes never
	// reach the caller, which shows up here as a read deadline expiring.
	coalescedClient, coalescedServer := net.Pipe()
	defer func() { _ = coalescedClient.Close() }()
	defer func() { _ = coalescedServer.Close() }()
	head, err := encodePreamble(KindRaft, "flow-beta", openSigner)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = coalescedClient.Write(append(head, []byte("RAFTBYTES")...)) }()
	coalesced, err := readPreamble(coalescedServer, time.Second, openSigner)
	if err != nil {
		t.Fatalf("readPreamble on a coalesced write: %v", err)
	}
	if coalesced.ID != "flow-beta" {
		t.Fatalf("coalesced group id = %q, want flow-beta", coalesced.ID)
	}
	if err := coalescedServer.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	coalescedRest := make([]byte, 9)
	if _, err := io.ReadFull(coalescedServer, coalescedRest); err != nil {
		t.Fatalf("coalesced post-preamble read: %v: the handshake buffered past its own bytes", err)
	}
	if string(coalescedRest) != "RAFTBYTES" {
		t.Fatalf("coalesced post-preamble stream = %q, want RAFTBYTES", coalescedRest)
	}
}

func TestPreambleRejections(t *testing.T) {
	good, err := encodePreamble(KindRaft, "g", openSigner)
	if err != nil {
		t.Fatal(err)
	}
	// The four landed fixtures are kept BYTE FOR BYTE across the version-2
	// layout, which is what keeps lane B's roster meaningful rather than merely
	// green: each one rewrites a field of the fixed head, and the head did not
	// move. bad kind is the fifth, rewriting byte 5 — the byte this version's
	// sibling lane gave a meaning — to a value no build declares.
	cases := []struct {
		name string
		head []byte
		want error
	}{
		{"bad magic", append([]byte("XXXX"), good[4:]...), ErrBadMagic},
		{"bad version", append(append([]byte{}, good[:4]...), append([]byte{9, 0, 0, 1}, good[8:]...)...), ErrBadVersion},
		{"bad kind", append(append([]byte{}, good[:5]...), append([]byte{99}, good[6:]...)...), ErrBadStreamKind},
		{"zero length", append(append([]byte{}, good[:6]...), 0, 0), ErrGroupIDRange},
		{"over length", append(append([]byte{}, good[:6]...), 0xFF, 0xFF), ErrGroupIDRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer func() { _ = client.Close() }()
			defer func() { _ = server.Close() }()
			go func() { _, _ = client.Write(tc.head) }()
			_, err := readPreamble(server, time.Second, openSigner)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPreambleShortReadIsRefused(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = server.Close() }()
	go func() {
		_, _ = client.Write([]byte("mrmx"))
		_ = client.Close()
	}()
	if _, err := readPreamble(server, time.Second, openSigner); err == nil {
		t.Fatal("a truncated handshake was accepted")
	}
}

func TestEncodePreambleRefusesOutOfRangeIDs(t *testing.T) {
	if _, err := encodePreamble(KindRaft, "", openSigner); !errors.Is(err, ErrGroupIDRange) {
		t.Fatalf("empty id: err = %v, want ErrGroupIDRange", err)
	}
	long := GroupID(strings.Repeat("x", MaxGroupIDLen+1))
	if _, err := encodePreamble(KindRaft, long, openSigner); !errors.Is(err, ErrGroupIDRange) {
		t.Fatalf("over-long id: err = %v, want ErrGroupIDRange", err)
	}
	if _, err := writePreamble(nil, KindRaft, long, time.Second, openSigner); !errors.Is(err, ErrGroupIDRange) {
		t.Fatalf("writePreamble over-long id: err = %v, want ErrGroupIDRange", err)
	}
}
