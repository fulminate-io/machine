// Package subject is the serialization fixtures' real Go module.
//
// EVERY TYPE HERE IS ONE ROW OF THE CLASS THE DERIVATION SPLITS ON, and the
// counter-intuitive rows are the reason the census is a class rather than a
// sample: an array is refused at an interface slot even of a basic element,
// while a slice of a basic element is carried, and a slice's element is tested
// DIRECTLY rather than through its underlying type, so []float64 is carried and
// []Celsius is not.
package subject

// Plain is a named struct with nothing wrong with it. At an interface slot it
// needs registration and nothing else; at a concrete site it is clean.
type Plain struct {
	A int
}

// Mixed carries a chan and a func beside an exported field, which is the class
// with no runtime signal at all: gob encodes it with a nil error and returns it
// with those fields gone.
type Mixed struct {
	A int
	C chan int
	F func()
}

// Sealed has no field the codec is allowed to see, which the codec refuses
// outright rather than encoding partially.
type Sealed struct {
	a int
}

// Escaped supplies BOTH halves of the gob escape hatch over a chan-bearing
// struct: GobEncode on the value receiver and GobDecode on the pointer, which is
// the conventional split. The hatch suppresses the STRUCTURAL walk of the type
// it sits on and suppresses nothing about registration.
type Escaped struct {
	A int
	C chan int
}

// GobEncode owns this type's bytes, which is what makes the chan field no longer
// a drop.
func (e Escaped) GobEncode() ([]byte, error) { return []byte{byte(e.A)}, nil }

// GobDecode reads back what GobEncode wrote. The POINTER carries it, because a
// decoder must be able to fill the value.
func (e *Escaped) GobDecode(b []byte) error {
	if len(b) > 0 {
		e.A = int(b[0])
	}

	return nil
}

// Nested reaches the drop two levels down, which is what makes the reported
// field chain .Inner.C rather than .C.
type Nested struct {
	Inner Mixed
}

// Celsius is a named basic type. It needs registration at an interface slot and
// is structurally clean.
type Celsius float64

// Cels is the NAMED twin of []Celsius. It reaches the walk through the named arm
// while its unnamed twin reaches it through the default one.
type Cels []Celsius

// NamedMap is the NAMED twin of map[string]int, which is the shape the shipped
// corpus actually declares as a state entry.
type NamedMap map[string]int

// Trio is the NAMED twin of [3]int. An array is refused at an interface slot
// even though its element is basic, which is the row a reasoned rule gets wrong.
type Trio [3]int

// _ keeps the unexported field of Sealed from reading as dead weight to a
// reader: it is the whole point of the type.
var _ = Sealed{a: 0}
