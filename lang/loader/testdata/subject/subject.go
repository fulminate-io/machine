// Package subject is a fixture module the loader's own tests load for real.
//
// It exists so the tests measure go/types against a genuine module on disk
// rather than against a synthesized scope: a fixture that builds the input the
// code will read tests the code against the test author's assumption of what a
// loaded package looks like, which is the assumption most worth not trusting.
package subject

import "encoding/gob"

// Codecs names the two gob interfaces by their spellings in this package's own
// scope, so a test can resolve them THROUGH the loader rather than hand-building
// an interface type and measuring its own construction.
type Codecs struct {
	Encoder gob.GobEncoder
	Decoder gob.GobDecoder
}

// Payload carries the receiver split the method-set query exists to report.
//
// GobEncode is declared on the VALUE receiver, so it belongs to both method
// sets; GobDecode is declared on the POINTER receiver, so it belongs only to the
// pointer's. A query that answers once for both is wrong about exactly one of
// them, and this type is shaped so that being wrong is visible.
type Payload struct {
	Name string
}

// GobEncode is declared on the value receiver deliberately.
func (p Payload) GobEncode() ([]byte, error) {
	return []byte(p.Name), nil
}

// GobDecode is declared on the pointer receiver deliberately.
func (p *Payload) GobDecode(data []byte) error {
	p.Name = string(data)

	return nil
}
