package main

import (
	"fmt"
	"os"

	machine "github.com/whitaker-io/machine/v4"
)

// main ROUND-TRIPS a Memo through the SHIPPED GobCodec at an interface site.
//
// The value rides FrameData.Values, which is map[string]any, so gob must know
// the concrete type to put it on the wire and to take it off again. Nothing
// here registers it: the GENERATED package is what must, and gob's own error is
// the enforcement if it did not.
func main() {
	data := machine.FrameData{
		ID:     "d1",
		Source: "ingest",
		Node:   "tally",
		Values: map[string]any{"flowregister.Register.memo": Memo{Text: "carried"}},
	}
	packet, err := machine.RebuildPacket(data, Order{ID: "o1"})
	if err != nil {
		fmt.Fprintln(os.Stderr, "rebuild:", err)
		os.Exit(1)
	}
	codec := machine.GobCodec[Order]{}
	raw, err := codec.Marshal(packet)
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal:", err)
		os.Exit(1)
	}
	back, err := codec.Unmarshal(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unmarshal:", err)
		os.Exit(1)
	}
	got, ok := back.Data().Values["flowregister.Register.memo"].(Memo)
	if !ok {
		fmt.Fprintln(os.Stderr, "the recovered stack value is not a Memo")
		os.Exit(1)
	}
	fmt.Printf("flow-register: round-tripped=%s\n", got.Text)
}
