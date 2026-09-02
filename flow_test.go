// Package machine - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package machine

import (
	"context"
	"strings"
	"testing"
)

// TestWithCodecSetsTheCodecWithoutCheckpointing covers both arms of the
// codec-only option: the set path and the nil refusal.
//
// THE TWO HALVES OF ITS NAME NEED DIFFERENT EVIDENCE, and neither implies the
// other. "Sets the codec" is observed on the emitter the successor's codec is
// bound onto, and by a completion-anchored predecessor that Start ACCEPTS where
// the same flow without the option is refused. "Without checkpointing" is
// observed on a machine carrying NO journal: a checkpointed node there is a
// declaration-time refusal, so a node that starts clean is one the runtime does
// not consider checkpointed. An option that set the checkpoint flag as well
// would pass the first half and fail the second.
func TestWithCodecSetsTheCodecWithoutCheckpointing(t *testing.T) {
	t.Run("supplies a completion codec that Start accepts", func(t *testing.T) {
		// THE EXACT SHAPE THE OPTION EXISTS FOR. The mapper anchors on completion,
		// so what journals its record is its SUCCESSOR's codec. Before this option
		// the only way to give the sink one was WithCheckpoint, which would also
		// have journaled the sink.
		m := New("flow-withcodec", OptionJournal(&orderedJournal{}), OptionFIFO)

		src, _ := m.Source[string]("src")
		mapped := src.Map("mapper", func(f Frame[string]) int {
			return len(f.Value())
		}, WithCheckpoint[string](GobCodec[string]{}))
		mapped.Output("sink", WithCodec[int](GobCodec[int]{}))

		if mapped.out.codec == nil {
			t.Fatal("declaring the successor with WithCodec left the emitter with no codec, " +
				"so a completion checkpoint has nothing to marshal with")
		}
		startFlow(t, m)
		t.Log("a completion-anchored node was accepted with a successor carrying only WithCodec")
	})

	t.Run("CONTROL: the same flow without the option is refused", func(t *testing.T) {
		// The discriminating control for the arm above. Identical but for the
		// option, so the accept there is attributable to WithCodec and not to
		// something else about the flow.
		m := New("flow-withcodec-control", OptionJournal(&orderedJournal{}), OptionFIFO)

		src, _ := m.Source[string]("src")
		mapped := src.Map("mapper", func(f Frame[string]) int {
			return len(f.Value())
		}, WithCheckpoint[string](GobCodec[string]{}))
		mapped.Output("sink")

		if err := m.Start(context.Background()); err == nil {
			t.Fatal("the control flow was accepted without any successor codec, " +
				"so the arm above proves nothing about WithCodec")
		}
	})

	t.Run("does not checkpoint the node it is given to", func(t *testing.T) {
		// NO JOURNAL. A node the runtime considers checkpointed is refused here at
		// declaration time, so starting clean is the observation that this option
		// did NOT set the checkpoint flag.
		m := New("flow-withcodec-nojournal")

		src, _ := m.Source[string]("src")
		mapped := src.Map("worker", func(f Frame[string]) string { return f.Value() },
			WithCodec[string](GobCodec[string]{}))
		mapped.Output("sink", WithCodec[string](GobCodec[string]{}))

		startFlow(t, m)
		t.Log("a WithCodec node started on a machine with no journal, so it carries no checkpoint")
	})

	t.Run("CONTROL: WithCheckpoint on the same machine IS refused", func(t *testing.T) {
		// Pins the instrument: the no-journal refusal really does fire on this
		// shape, so the clean start above is a property of WithCodec rather than a
		// check that never runs.
		m := New("flow-withcheckpoint-nojournal")

		src, _ := m.Source[string]("src")
		mapped := src.Map("worker", func(f Frame[string]) string { return f.Value() },
			WithCheckpoint[string](GobCodec[string]{}))
		mapped.Output("sink", WithCheckpoint[string](GobCodec[string]{}))

		err := m.Start(context.Background())
		if err == nil {
			t.Fatal("a checkpointed node on a machine with no journal was accepted, " +
				"so the arm above cannot distinguish a checkpoint from its absence")
		}
		if !strings.Contains(err.Error(), "no journal") {
			t.Fatalf("the refusal %q is not the no-journal one this control needs", err)
		}
	})

	t.Run("nil codec", func(t *testing.T) {
		// The refusal arm. A declared codec that is nil errors at the point of the
		// mistake rather than being defaulted, on the terms WithCheckpoint(nil) is.
		m := New("flow-withcodec-nil", OptionJournal(&orderedJournal{}))

		src, _ := m.Source[string]("src")
		mapped := src.Map("worker", func(f Frame[string]) string { return f.Value() },
			WithCodec[string](nil))
		mapped.Output("sink", WithCodec[string](GobCodec[string]{}))

		err := m.Start(context.Background())
		if err == nil {
			t.Fatal("a codec declared as nil was accepted")
		}
		for _, want := range []string{"worker", "nil", "WithCodec"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the refusal %q does not name %q", err, want)
			}
		}
	})
}
