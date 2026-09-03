package main

// HOST SCAFFOLDING, SUPPLIED BY THE IMPLEMENTER — NOT PART OF THE MODEL'S OUTPUT.
//
// The model that wrote orders.flow and main.go was given the 185 lines of
// lang/ast/SPEC.md and nothing else, and that document describes the LANGUAGE
// only: it never mentions a module file, a Wire function, machine.New, an option,
// package main or Start. A model given the spec alone therefore cannot write the
// program that runs its own flow, and the acid test would otherwise be measuring
// the union of the language and a host contract the spec does not carry.
//
// The orchestrator's ruling under the ticket's step (7) resolves that: the acid
// test evaluates the FLOW half as the model wrote it — parse with the real
// parser, flowlint at the strictest threshold, and flowc generation — while the
// host program is supplied here as fixture scaffolding, exactly as the run,
// subflow and dotted stagings supply theirs. The model's own main.go is used
// AS-IS for the funcs the flow references, and whatever the generator or the
// compiler says about its guessed calling convention (plain values, and a *int
// for the state cell) is the result and is never repaired.
//
// The Wire and ingest names are the generator's, not a guess: emit_wire.go builds
// them as "Wire" + exported(flow) and exported(flow) + "Ingests", so a flow named
// `orders` yields WireOrders and OrdersIngests whatever the source's own casing.
//
// THE TOKEN IS PRINTED HERE RATHER THAN BY A NODE BODY, and that is a disclosure
// rather than a convenience. Every other fixture in this plan prints its locked
// token from the fixture's own func body so the token cannot be produced by the
// harness. Here the model's two sinks return an error and print nothing, and the
// ruling forbids editing them, so the host is the only place left. The token
// therefore proves the datum reached the end of the flow and NOT that a node body
// ran, which is a weaker claim than the other fixtures make.

import (
	"context"
	"fmt"
	"os"
	"time"

	machine "github.com/whitaker-io/machine/v4"
)

func main() {
	m := machine.New("flow-llm")

	ingests, err := WireOrders(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "flow-llm: wiring failed:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := m.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "flow-llm: start failed:", err)
		os.Exit(1)
	}

	if err := ingests.Ingest(ctx, Order{Kind: "card", Total: 100}); err != nil {
		fmt.Fprintln(os.Stderr, "flow-llm: ingest failed:", err)
		os.Exit(1)
	}

	fmt.Println("flow-llm: ingested=1")
}
