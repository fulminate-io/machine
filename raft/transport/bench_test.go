package transport

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

type benchTransport struct {
	trans raft.Transport
	addr  string
	close func()
}

func muxedTransport(tb testing.TB) benchTransport {
	m, err := New(Config{BindAddr: "127.0.0.1:0", RPCTimeout: 2 * time.Second})
	if err != nil {
		tb.Fatal(err)
	}
	g, err := m.Bind("bench")
	if err != nil {
		tb.Fatal(err)
	}
	return benchTransport{g.Transport(), m.Addr().String(), func() { _ = g.Close(); _ = m.Close() }}
}

func stockTransport(tb testing.TB) benchTransport {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	tr, err := raft.NewTCPTransport(addr, nil, 3, 2*time.Second, io.Discard)
	if err != nil {
		tb.Fatal(err)
	}
	return benchTransport{tr, string(tr.LocalAddr()), func() { _ = tr.Close() }}
}

// benchApply builds an identical three-node single-group cluster for either
// transport and measures sequential Apply. The builder is SHARED between the
// two benchmarks deliberately: two separately written harnesses would be
// measuring two different things, and the comparison is the whole point.
func benchApply(b *testing.B, build func(testing.TB) benchTransport) {
	transports := make([]benchTransport, 3)
	servers := make([]raft.Server, 3)
	for i := range transports {
		transports[i] = build(b)
		servers[i] = raft.Server{ID: raft.ServerID(fmt.Sprintf("b%d", i)), Address: raft.ServerAddress(transports[i].addr)}
	}
	rafts := make([]*raft.Raft, 3)
	for i, t := range transports {
		cfg := raft.DefaultConfig()
		cfg.LocalID = raft.ServerID(fmt.Sprintf("b%d", i))
		cfg.LogOutput = io.Discard
		cfg.HeartbeatTimeout = 200 * time.Millisecond
		cfg.ElectionTimeout = 200 * time.Millisecond
		cfg.LeaderLeaseTimeout = 100 * time.Millisecond
		cfg.CommitTimeout = 20 * time.Millisecond
		r, err := raft.NewRaft(cfg, &recorderFSM{}, raft.NewInmemStore(), raft.NewInmemStore(), raft.NewInmemSnapshotStore(), t.trans)
		if err != nil {
			b.Fatal(err)
		}
		rafts[i] = r
	}
	defer func() {
		for _, r := range rafts {
			_ = r.Shutdown().Error()
		}
		for _, t := range transports {
			t.close()
		}
	}()
	if err := rafts[0].BootstrapCluster(raft.Configuration{Servers: servers}).Error(); err != nil {
		b.Fatal(err)
	}
	var leader *raft.Raft
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && leader == nil {
		for _, r := range rafts {
			if r.State() == raft.Leader {
				leader = r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if leader == nil {
		b.Fatal("no leader")
	}
	payload := []byte("0123456789abcdef")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := leader.Apply(payload, 5*time.Second).Error(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkApplyOverMux(b *testing.B)   { benchApply(b, muxedTransport) }
func BenchmarkApplyOverStock(b *testing.B) { benchApply(b, stockTransport) }

// benchSink keeps the encoded preamble live so the compiler cannot delete the
// call being measured. THE SINK IS REQUIRED, not stylistic: written without it
// the compiler eliminated the call entirely and the benchmark reported
// 0.375 ns/op at 0 allocs/op — a number that looked excellent and measured
// nothing at all.
var benchSink []byte

// benchSigner keys the benchmark to a real token so BenchmarkEncodePreamble
// measures the SIGNED path. An empty accepted set makes signer.sign a no-op, so
// benchmarking through one would report the unsigned cost and the allocation
// budget it gates would pass without ever exercising the authentication block.
var benchSigner = newSigner([]Token{"benchmark-token"})

func BenchmarkEncodePreamble(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf, err := encodePreamble(KindRaft, "flow-alpha", benchSigner)
		if err != nil {
			b.Fatal(err)
		}
		benchSink = buf
	}
	if len(benchSink) == 0 {
		b.Fatal("CONTROL FAILED: nothing was encoded")
	}
}
