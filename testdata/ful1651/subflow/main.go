package main

import (
	"context"
	"fmt"
	"os"
	"time"

	machine "github.com/whitaker-io/machine/v4"
)

func main() {
	m := machine.New("flow-sub")
	ingests, err := WireMain(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wiring:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}
	if err := ingests.Ingest(ctx, Order{ID: "o1"}); err != nil {
		fmt.Fprintln(os.Stderr, "push:", err)
		os.Exit(1)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		fmt.Fprintln(os.Stderr, "the flow drove no data through within its deadline")
		os.Exit(1)
	}
}
