package main

// Ledger is the SILENT-DROP seed. gob encodes it with a nil error and returns it
// with C and F gone, so nothing at run time says the value arrived incomplete.
// It sits in a state block, which is the interface-slot boundary.
type Ledger struct {
	Total int
	C     chan int
	F     func()
}

// Note is the REGISTRATION seed: a plain named struct that needs a gob.Register
// at an interface slot and nothing else.
type Note struct {
	Text string
}
