// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"context"
	"testing"
)

// BenchmarkNodeExecution measures the per-datum cost of the instrumented chokepoint
// through a two-node FIFO pipeline driving one datum per iteration.
//
// The Machine takes NO provider options, so what it resolves is whatever the otel
// globals hold — which makes the REGIME part of the measurement rather than a
// footnote to it. A virgin global, explicit noop providers and a recording SDK give
// three different numbers, and a figure quoted without saying which one it came from
// says nothing. Every test in this package that installs recording globals restores
// NOOP providers in t.Cleanup for exactly this reason.
func BenchmarkNodeExecution(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := New("bench", OptionFIFO)
	src, ingest := m.Source[int]("bench.source", WithEdge(Channel[int](1)))
	out := src.Map("bench.node", func(f Frame[int]) int { return f.Value() },
		WithEdge(Channel[int](1))).Output("bench.out", WithEdge(Channel[int](1)))

	if err := m.Start(ctx); err != nil {
		b.Fatalf("Start: %v", err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if err := ingest(ctx, 1); err != nil {
			b.Fatalf("ingest: %v", err)
		}
		<-out
	}
}
