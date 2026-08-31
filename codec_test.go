// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"encoding/gob"
	"encoding/json"
	"strings"
	"testing"
)

// trailStep is the two-field stack value the codec fixture carries, so the round trip
// is asserted on a struct as well as on a number.
type trailStep struct {
	Node  string
	Depth int
}

// unregisteredValue is deliberately never gob.Register'd. It is the known-positive for
// Marshal's error path: gob refuses an unregistered concrete type inside an interface,
// which is the enforcement GobCodec's doc comment points at.
type unregisteredValue struct {
	Name string
}

// The three keys below are declared at package scope with names distinct from every
// other key in this module: NewKey panics on a duplicate name in the one process-wide
// declaration namespace.
var (
	codecAttempts     = NewKey("codec.attempts", func(v int) int { return v })
	codecTrail        = NewKey("codec.trail", func(v trailStep) trailStep { return v })
	codecUnregistered = NewKey("codec.unregistered", func(v unregisteredValue) unregisteredValue { return v })
)

// codecFixture builds the packet both codecs are driven with. It goes through
// RebuildPacket rather than a running machine, because a codec is off the node path.
func codecFixture(t *testing.T) Packet[string] {
	t.Helper()
	packet, err := RebuildPacket(FrameData{
		ID:     "id-1",
		Parent: "id-0",
		Source: "src",
		Node:   "n1",
		Values: map[string]any{
			codecAttempts.Name(): 7,
			codecTrail.Name():    trailStep{Node: "n1", Depth: 2},
		},
	}, "payload")
	if err != nil {
		t.Fatalf("building the codec fixture failed: %v", err)
	}
	return packet
}

// jsonCodec is the KNOWN-NEGATIVE, and the only JSON-shaped codec in this repo. It
// lives in a test file because the ticket forbids shipping one: its whole purpose is
// to demonstrate the loss GobCodec's doc comment describes.
type jsonCodec[T any] struct{}

func (jsonCodec[T]) Marshal(packet Packet[T]) ([]byte, error) {
	return json.Marshal(envelope[T]{Frame: packet.Data(), Payload: packet.Value()})
}

func (jsonCodec[T]) Unmarshal(data []byte) (Packet[T], error) {
	var body envelope[T]
	if err := json.Unmarshal(data, &body); err != nil {
		return Packet[T]{}, err
	}
	return RebuildPacket(body.Frame, body.Payload)
}

// TestGobCodecPreservesStackValueTypes reads the restored values through Data(): a
// packet has no gated accessor at all, so the projection is the only way to see the
// restored stack values, and it is the surface a remote transport actually uses.
func TestGobCodecPreservesStackValueTypes(t *testing.T) {
	gob.Register(trailStep{})
	codec := GobCodec[string]{}

	data, err := codec.Marshal(codecFixture(t))
	if err != nil {
		t.Fatalf("marshalling the fixture failed: %v", err)
	}
	restored, err := codec.Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshalling the fixture failed: %v", err)
	}

	values := restored.Data().Values
	attempts, ok := values[codecAttempts.Name()].(int)
	if !ok {
		t.Fatalf("the stack int came back as %T, not an int", values[codecAttempts.Name()])
	}
	if attempts != 7 {
		t.Errorf("the stack int came back as %d, want 7", attempts)
	}
	trail, ok := values[codecTrail.Name()].(trailStep)
	if !ok {
		t.Fatalf("the stack struct came back as %T, not a trailStep", values[codecTrail.Name()])
	}
	if trail != (trailStep{Node: "n1", Depth: 2}) {
		t.Errorf("the stack struct came back as %+v, want {n1 2}", trail)
	}

	assertLineage(t, restored)
	if restored.Value() != "payload" {
		t.Errorf("the payload came back as %q, want %q", restored.Value(), "payload")
	}
}

func assertLineage(t *testing.T, packet Packet[string]) {
	t.Helper()
	for _, check := range []struct{ name, got, want string }{
		{"ID", packet.ID(), "id-1"},
		{"Parent", packet.Parent(), "id-0"},
		{"Source", packet.Source(), "src"},
		{"Node", packet.Node(), "n1"},
	} {
		if check.got != check.want {
			t.Errorf("%s came back as %q, want %q", check.name, check.got, check.want)
		}
	}
}

// TestJSONShapedCodecLosesStackValueTypes is what proves the gob test discriminates: a
// codec that preserved nothing would still let that test read as meaningful alone.
func TestJSONShapedCodecLosesStackValueTypes(t *testing.T) {
	codec := jsonCodec[string]{}

	data, err := codec.Marshal(codecFixture(t))
	if err != nil {
		t.Fatalf("marshalling the fixture failed: %v", err)
	}
	restored, err := codec.Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshalling the fixture failed: %v", err)
	}

	values := restored.Data().Values
	if _, ok := values[codecAttempts.Name()].(int); ok {
		t.Fatalf("the JSON-shaped codec preserved the stack int, so it is not a known-negative")
	}
	if _, ok := values[codecAttempts.Name()].(float64); !ok {
		t.Errorf("the stack int came back as %T, want the float64 the ticket measured",
			values[codecAttempts.Name()])
	}
	if _, ok := values[codecTrail.Name()].(trailStep); ok {
		t.Errorf("the JSON-shaped codec preserved the stack struct, so it is not a known-negative")
	}
}

func TestGobCodecMarshalReportsAnUnregisteredStackValue(t *testing.T) {
	packet, err := RebuildPacket(FrameData{
		ID:     "id-2",
		Source: "src",
		Node:   "n1",
		Values: map[string]any{codecUnregistered.Name(): unregisteredValue{Name: "nope"}},
	}, "payload")
	if err != nil {
		t.Fatalf("building the unregistered fixture failed: %v", err)
	}

	if _, err = (GobCodec[string]{}).Marshal(packet); err == nil {
		t.Fatal("marshalling an unregistered stack value succeeded, so gob refused nothing")
	}
	if !strings.Contains(err.Error(), `encoding frame "id-2" failed`) {
		t.Errorf("the marshal error does not name the frame: %v", err)
	}
}

func TestGobCodecUnmarshalReportsACorruptStream(t *testing.T) {
	if _, err := (GobCodec[string]{}).Unmarshal([]byte("this is not a gob stream")); err == nil {
		t.Fatal("unmarshalling a corrupt stream succeeded")
	}
}
