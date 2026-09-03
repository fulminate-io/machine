package membership

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/whitaker-io/machine/raft/ledger"
	"github.com/whitaker-io/machine/raft/transport"
)

// TestResolvePeersRefusesAnAddressThatIsNotHostPort covers the whole discovery
// contract's entry point: one configured address in, every instance behind it
// out. It is the operator-facing surface, so its refusal has to name the field
// an operator would go and fix rather than surfacing a bare parse error.
func TestResolvePeersRefusesAnAddressThatIsNotHostPort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, bad := range []string{"not-a-host-port", "", "127.0.0.1"} {
		_, err := resolvePeers(ctx, bad)
		if err == nil {
			t.Fatalf("resolvePeers(%q) returned no error: bad input errors, it is never coerced", bad)
		}
		if !strings.Contains(err.Error(), "Config.Peers") {
			t.Fatalf("resolvePeers(%q) failed with %q, which does not name Config.Peers: an operator "+
				"cannot act on a refusal that does not say which field is wrong", bad, err)
		}
	}

	// A RESOLUTION FAILURE IS REPORTED, NOT SWALLOWED INTO AN EMPTY SET. An empty
	// set reads as "nobody is out there", which is the one answer that must never
	// be manufactured — the creation rule acts on it.
	if _, err := resolvePeers(ctx, "no-such-host.invalid:8300"); err == nil {
		t.Fatal("resolving a host that does not exist reported success, so an unresolvable registry would " +
			"read as an empty one")
	}

	// THE CONTROL: a well-formed address that DOES resolve comes back carrying the
	// port, so the refusals above are about the input rather than about a
	// resolver that fails everything.
	addrs, err := resolvePeers(ctx, "localhost:8300")
	if err != nil {
		t.Fatalf("CONTROL FAILED: a resolvable host:port was refused: %v", err)
	}
	if len(addrs) == 0 {
		t.Fatal("CONTROL FAILED: a resolvable host:port produced no addresses")
	}
	for _, addr := range addrs {
		host, port, splitErr := net.SplitHostPort(addr)
		if splitErr != nil {
			t.Fatalf("resolved %q, which is not host:port: %v", addr, splitErr)
		}
		if port != "8300" {
			t.Fatalf("resolved %q, which does not carry the configured port", addr)
		}
		if host == "" {
			t.Fatalf("resolved %q with an empty host", addr)
		}
	}
}

// TestConfigValidateNamesTheFieldItRefuses pins the operator-facing half of the
// configuration contract. Bad input errors and is never coerced, and the refusal
// names the field an operator must go and fix — a config rejected without saying
// which knob was wrong is a support ticket rather than an error.
func TestConfigValidateNamesTheFieldItRefuses(t *testing.T) {
	mux, err := transportForConfigTest(t)
	if err != nil {
		t.Fatal(err)
	}
	base := Config{Node: "a-node", Advertise: "127.0.0.1:1", Mux: mux}

	for _, tc := range []struct {
		name  string
		cfg   Config
		field string
		want  error
	}{
		{"no node", Config{Advertise: "127.0.0.1:1", Mux: mux}, "Config.Node", ErrConfigMissing},
		{"no advertise", Config{Node: "a", Mux: mux}, "Config.Advertise", ErrConfigMissing},
		{"no mux", Config{Node: "a", Advertise: "127.0.0.1:1"}, "Config.Mux", ErrConfigMissing},
		{"flows without open", withFlows(base, []string{"alpha"}), "Config.Open", ErrConfigMissing},
		{"peers without expect", withPeers(base, "peers:1", 0), "Config.Expect", ErrConfigRange},
		{"peers with expect below one", withPeers(base, "peers:1", -1), "Config.Expect", ErrConfigRange},
		{"peers without generation", withPeers(base, "peers:1", 3), "Config.Generation", ErrConfigRange},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("the refusal %q does not name %s", err, tc.field)
			}
		})
	}

	// THE CONTROL: a complete config is accepted, so the refusals above are about
	// the missing fields rather than about a validate that rejects everything.
	ok := withPeers(withFlows(base, []string{"alpha"}), "peers:1", 3)
	ok.Generation = testGeneration
	ok.Open = func(string) (*ledger.Ledger, error) { return nil, nil }
	if err := ok.validate(); err != nil {
		t.Fatalf("CONTROL FAILED: a complete config was refused: %v", err)
	}
}

func withFlows(c Config, flows []string) Config { c.Flows = flows; return c }

func withPeers(c Config, peers string, expect int) Config {
	c.Peers = peers
	c.Expect = expect
	return c
}

func transportForConfigTest(t *testing.T) (*transport.Mux, error) {
	t.Helper()
	m, err := transport.New(transport.Config{BindAddr: "127.0.0.1:0"})
	if err == nil {
		t.Cleanup(func() { _ = m.Close() })
	}
	return m, err
}
