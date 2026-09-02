// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"encoding/gob"
	"strings"
	"testing"
)

// dropping is the SILENT-DROP class: an exported field the codec carries beside a
// chan and a func it discards while reporting success.
//
// It is the class with no runtime signal at all, which is what makes it the one a
// generation-time derivation has to refuse. gob errors only when NOTHING exported
// remains, so the ordinary field is load-bearing: without A this fixture would
// demonstrate a loud refusal instead of a silent drop.
type dropping struct {
	A int
	C chan int
	F func()
}

// hatched supplies BOTH halves of the gob escape hatch over a chan-bearing struct,
// with the conventional receiver split: GobEncode on the value, GobDecode on the
// pointer.
type hatched struct {
	A int
	C chan int
}

// GobEncode owns this type's bytes, which is what stops the chan being a drop.
func (h hatched) GobEncode() ([]byte, error) { return []byte{byte(h.A)}, nil }

// GobDecode rebuilds the value, INCLUDING the chan the codec now owns. The pointer
// carries it because a decoder must be able to fill the value.
func (h *hatched) GobDecode(b []byte) error {
	if len(b) > 0 {
		h.A = int(b[0])
	}

	h.C = make(chan int)

	return nil
}

// The stack keys these fixtures ride as. A stack value is how a declared var
// reaches FrameData.Values, and NewKey panics on a duplicate name in the one
// process-wide declaration namespace, so both are prefixed with this file's own
// subject.
var (
	checkpointDropping = NewKey("checkpointserialization.dropping", func(v dropping) dropping { return v })
	checkpointHatched  = NewKey("checkpointserialization.hatched", func(v hatched) hatched { return v })
)

// interfaceSitePacket builds a packet carrying value as a STACK VALUE, which is the
// interface site: FrameData.Values is a map[string]any, so every stack value sits
// in an interface slot and the decoder must reconstruct its concrete type by name.
//
// The frame ID is "id" because the codec names the frame in its refusal and this
// suite's locked log lines quote that message.
func interfaceSitePacket(t *testing.T, key string, value any) Packet[string] {
	t.Helper()

	packet, err := RebuildPacket(FrameData{
		ID:     "id",
		Source: "src",
		Node:   "n1",
		Values: map[string]any{key: value},
	}, "payload")
	if err != nil {
		t.Fatalf("building the interface-site fixture failed: %v", err)
	}

	return packet
}

// TestTheCheckpointSubstrateDropsSilentlyAtTheConcreteSite is the leg the whole
// derivation exists for.
//
// A checkpointed packet's payload rides envelope[T].Payload, a TYPED field, so it
// is the concrete site. gob carries the struct, discards the chan and the func, and
// reports success at every layer — there is no runtime signal of any kind, which is
// why this class is a generation-time REFUSAL rather than a warning.
func TestTheCheckpointSubstrateDropsSilentlyAtTheConcreteSite(t *testing.T) {
	codec := GobCodec[dropping]{}

	packet, err := RebuildPacket(FrameData{ID: "id", Source: "src", Node: "n1"},
		dropping{A: 42, C: make(chan int), F: func() {}})
	if err != nil {
		t.Fatalf("building the concrete-site fixture failed: %v", err)
	}

	// THE CONTROL COMES FIRST: a fixture whose chan and func were already nil would
	// demonstrate nothing, and every assertion below would pass on it.
	if packet.Value().C == nil || packet.Value().F == nil {
		t.Fatal("CONTROL FAILED: the fixture went in with a nil chan or func, so a drop cannot be observed")
	}

	data, err := codec.Marshal(packet)
	restored, uerr := codec.Unmarshal(data)
	if uerr != nil {
		t.Fatalf("unmarshalling the concrete-site fixture failed: %v", uerr)
	}

	got := restored.Value()

	t.Logf("concrete site: err=%v A=%d C==nil:%t F==nil:%t", err, got.A, got.C == nil, got.F == nil)

	if err != nil {
		t.Fatalf("the codec REFUSED the chan-and-func-bearing payload, so this is not the silent class: %v", err)
	}

	if got.A != 42 {
		t.Errorf("the ordinary field came back as %d, want 42", got.A)
	}

	if got.C != nil || got.F != nil {
		t.Error("the chan and func SURVIVED the round trip, so the substrate no longer drops silently and " +
			"every diagnostic this lane emits about that class describes a codec that no longer exists")
	}
}

// TestTheCheckpointSubstrateNeedsRegistrationAtTheInterfaceSite asserts BOTH axes on
// one value, which is what proves they are independent.
//
// Registration is what the interface site needs and it is NO repair for the drop: the
// same value, once registered, goes through with a nil error and comes back with its
// chan and func gone.
func TestTheCheckpointSubstrateNeedsRegistrationAtTheInterfaceSite(t *testing.T) {
	codec := GobCodec[string]{}
	value := dropping{A: 7, C: make(chan int), F: func() {}}

	// UNREGISTERED FIRST, and the order is load-bearing: gob.Register is
	// process-global and irreversible, so the refusal can only be observed before it.
	_, err := codec.Marshal(interfaceSitePacket(t, checkpointDropping.Name(), value))
	if err == nil {
		t.Fatal("an UNREGISTERED named type crossed the interface site, so registration is not required there")
	}

	t.Logf("interface site, unregistered: %v", err)

	// THE DISCRIMINATING CONTROL: registration must CHANGE the answer. Without it a
	// codec that refused everything would satisfy the leg above.
	gob.Register(dropping{})

	data, merr := codec.Marshal(interfaceSitePacket(t, checkpointDropping.Name(), value))
	if merr != nil {
		t.Fatalf("CONTROL FAILED: registration did not change the answer, so the refusal above was not "+
			"about registration: %v", merr)
	}

	restored, uerr := codec.Unmarshal(data)
	if uerr != nil {
		t.Fatalf("unmarshalling the registered interface-site value failed: %v", uerr)
	}

	got, ok := restored.Data().Values[checkpointDropping.Name()].(dropping)
	if !ok {
		t.Fatalf("the stack value came back as %T, not a dropping", restored.Data().Values[checkpointDropping.Name()])
	}

	t.Logf("interface site, registered: err=%v A=%d C==nil:%t F==nil:%t", merr, got.A, got.C == nil, got.F == nil)

	if got.A != 7 {
		t.Errorf("the ordinary field came back as %d, want 7", got.A)
	}

	if got.C != nil || got.F != nil {
		t.Error("registration REPAIRED the drop, so the two axes are not independent and a registration " +
			"requirement would be a sufficient answer for a chan-bearing type")
	}
}

// TestTheGobHatchCoversStructureAndNotRegistration pins the corrected hatch rule on
// the SHIPPED substrate rather than on raw gob.
//
// A type supplying both halves owns its own bytes, so its chan is no longer a drop —
// and it is refused at the interface site exactly as an ordinary named type is,
// because registration names the concrete type on the wire and is orthogonal to who
// produced the bytes.
func TestTheGobHatchCoversStructureAndNotRegistration(t *testing.T) {
	payload := hatched{A: 9, C: make(chan int)}

	data, err := (GobCodec[hatched]{}).Marshal(interfaceSiteFreePacket(t, payload))
	if err != nil {
		t.Fatalf("the hatch did not carry its own type at the concrete site: %v", err)
	}

	restored, uerr := (GobCodec[hatched]{}).Unmarshal(data)
	if uerr != nil {
		t.Fatalf("unmarshalling the hatched payload failed: %v", uerr)
	}

	got := restored.Value()

	t.Logf("hatch at the concrete site: err=%v A=%d C-restored=%t", err, got.A, got.C != nil)

	if got.A != 9 {
		t.Errorf("the hatched value came back as %d, want 9", got.A)
	}

	if got.C == nil {
		t.Error("the hatch did NOT carry the field it owns, so it suppresses nothing and the structural " +
			"exemption this contract grants it would be wrong")
	}

	assertHatchStillNeedsRegistration(t, payload)
}

// assertHatchStillNeedsRegistration is the half the hatch does not buy.
func assertHatchStillNeedsRegistration(t *testing.T, payload hatched) {
	t.Helper()

	_, err := (GobCodec[string]{}).Marshal(interfaceSitePacket(t, checkpointHatched.Name(), payload))
	if err == nil {
		t.Fatal("the hatched type crossed the interface site UNREGISTERED, so the hatch exempts registration")
	}

	t.Logf("hatch at the interface site, unregistered: %v", err)

	if !strings.Contains(err.Error(), "not registered for interface") {
		t.Errorf("the hatched type was refused for some reason OTHER than registration: %v", err)
	}
}

// interfaceSiteFreePacket builds a packet carrying the value as its PAYLOAD and
// nothing on the stack, so the concrete site is measured on its own.
func interfaceSiteFreePacket(t *testing.T, payload hatched) Packet[hatched] {
	t.Helper()

	packet, err := RebuildPacket(FrameData{ID: "id", Source: "src", Node: "n1"}, payload)
	if err != nil {
		t.Fatalf("building the concrete-site hatch fixture failed: %v", err)
	}

	return packet
}
