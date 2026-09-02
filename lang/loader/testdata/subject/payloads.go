// Package subject's serialization fixtures.
//
// EVERY TYPE HERE SPANS A THRESHOLD THE DERIVATION ASSERTS, at BOTH sites rather
// than one: a fixture whose input range cannot express the property it is meant
// to gate lets a broken implementation pass, which is the failure this set is
// shaped to avoid.
package subject

// Plain is serializable at concrete position and a registration REQUIREMENT at
// interface position. It is the pair that separates the two sites: a derivation
// that answered once for both is wrong about exactly one of them.
type Plain struct {
	A int
	B string
}

// Mixed is where the derivation must DISAGREE with gob. gob encodes this with a
// nil error and silently drops C and F; the derivation reports both, because a
// class with no runtime signal at all is the one worth refusing at generation
// time.
type Mixed struct {
	A int
	C chan int
	F func()
}

// Inner exists so Nested can bury its drops one level down.
type Inner struct {
	A int
	C chan int
	F func()
}

// Nested separates a walk that recurses from one that inspects only top-level
// fields: its own fields are all fine and the problems sit inside Inner.
type Nested struct {
	A     int
	Inner Inner
}

// Escaped carries the gob escape hatch over a chan field, which is what makes it
// the test of the hatch's exact scope. The codec owns its bytes, so the chan is
// no longer a drop at concrete position — and it is STILL a registration
// requirement at interface position, because registration names the concrete
// type on the wire and is orthogonal to who produced the bytes.
type Escaped struct {
	A int
	C chan int
}

// GobEncode gives Escaped the encoding half of the hatch, on the value receiver.
func (e Escaped) GobEncode() ([]byte, error) {
	return []byte{byte(e.A)}, nil
}

// GobDecode gives Escaped the decoding half, on the pointer receiver — the
// conventional split, and the reason the hatch is detected through the POINTER's
// method set, which is the only one containing both halves.
func (e *Escaped) GobDecode(data []byte) error {
	if len(data) > 0 {
		e.A = int(data[0])
	}

	return nil
}

// Rec is self-referential, so a walk with no memo re-enters the cycle forever.
// Its chan field sits inside that cycle, so terminating is not enough — the walk
// must terminate AND still report the drop.
type Rec struct {
	Next *Rec
	A    int
	C    chan int
}

// Collections exercises the container arms of the walk: a slice, a fixed array,
// a map value and a map KEY each hiding the same silent-drop class. Without it
// those arms are specified behavior with no fixture, and a walk that forgot to
// descend into containers would look correct.
type Collections struct {
	Chans  []chan int
	Fixed  [2]chan int
	ByName map[string]chan int
	Keyed  map[chan int]string
}

// NoFields carries nothing the codec is allowed to see. gob refuses such a
// struct outright rather than encoding it partially, which is a different
// outcome from a dropped field and gets its own reason.
type NoFields struct {
	hidden int
}
