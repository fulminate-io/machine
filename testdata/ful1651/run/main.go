package main

import (
	"context"
	"fmt"
	"os"
	"time"

	machine "github.com/whitaker-io/machine/v4"
)

type stubJournal struct{}

func (stubJournal) Checkpoint(ctx context.Context, r machine.CheckpointRecord) error { return nil }
func (stubJournal) Claim(ctx context.Context, flow, datum, owner string) (bool, error) {
	return true, nil
}
func (stubJournal) Retire(ctx context.Context, flow, datum string) error { return nil }
func (stubJournal) Orphans(ctx context.Context, flow string) ([]machine.CheckpointRecord, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func main() {
	withJournal := len(os.Args) < 2 || os.Args[1] != "-nojournal"

	var m *machine.Machine
	if withJournal {
		m = machine.New("flow-smoke", machine.OptionJournal(stubJournal{}))
	} else {
		m = machine.New("flow-smoke")
	}

	ingests, err := WireSmoke(m)
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
	if err := ingests.Ingest(ctx, Order{ID: "o1", Kind: "CARD"}); err != nil {
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
