package main

// Memo is the REGISTRATION seed: a plain named struct with nothing wrong with
// it. Declared as a flow var it becomes a frame stack key, which the shipped
// GobCodec carries as an interface value inside FrameData.Values — so it needs
// a gob.Register before it can cross, and gob's own error is the enforcement.
type Memo struct {
	Text string
}
