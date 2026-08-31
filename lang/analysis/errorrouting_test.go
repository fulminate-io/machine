// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

import (
	"path/filepath"
	"testing"
)

// TestErrorRoutingResolvesDestinationsWithoutErroringOnHandlerFreeFlows pins
// both halves of the ticket's premise, that a failure destination is statically
// knowable, and the severity ruling that keeps it from flagging legal programs.
//
// enrichment.flow is the load-bearing case: it declares NO error handler at flow
// level and none per node, so every one of its statements resolves to no
// handler. At error severity this analyzer would red a canonical program, which
// is exactly the mistake the hint severity exists to prevent.
func TestErrorRoutingResolvesDestinationsWithoutErroringOnHandlerFreeFlows(t *testing.T) {
	var resolved, unhandled int

	for _, name := range strawmanFiles {
		src := loadSource(t, filepath.Join(strawmanDir, name))
		got, diags := resultOf(t, ErrorRoutingAnalyzer, src)
		routes, ok := got.(*ErrorRoutes)
		if !ok {
			t.Fatalf("the errorrouting analyzer produced %T, want *ErrorRoutes", got)
		}

		if errs := errorsIn(withCode(diags, ErrorRoutingAnalyzer.Name)); len(errs) != 0 {
			t.Errorf("strawman %s produced errorrouting ERRORS: %v", name, messages(errs))
		}

		// EVERY node-bearing statement is resolved to one of the three
		// destinations, with none left blank. A route with an empty Kind would
		// mean the resolver skipped a shape it should have read.
		if len(routes.Routes) == 0 {
			t.Fatalf("%s resolved no failure destinations at all", name)
		}
		byKind := map[string]int{}
		for _, route := range routes.Routes {
			if route.Kind == "" {
				t.Errorf("%s left the failure destination of %s unresolved", name, route.Label)
			}
			byKind[route.Kind]++
			resolved++
			if route.Kind == handlerNone {
				unhandled++
			}
		}
		t.Logf("%s: %d statements routed %v", name, len(routes.Routes), byKind)
	}

	// THE KNOWN POSITIVE FOR THE HINT PATH. Without it, "no errors on any
	// strawman" is equally satisfied by an analyzer that reports nothing —
	// enrichment declares no handler at all, so its statements must attract
	// hints while attracting no errors.
	if unhandled == 0 {
		t.Error("no strawman statement resolved to no handler, so the hint path was never exercised")
	}
	t.Logf("resolved %d statements across the corpus, %d of them to no handler", resolved, unhandled)
}

// TestErrorRoutingPrefersTheNodeClauseOverTheFlowDeclaration pins the resolution
// order, which the strawman sweep alone does not separate.
func TestErrorRoutingPrefersTheNodeClauseOverTheFlowDeclaration(t *testing.T) {
	src := loadSource(t, filepath.Join(strawmanDir, "payments.flow"))
	got, _ := resultOf(t, ErrorRoutingAnalyzer, src)
	routes, ok := got.(*ErrorRoutes)
	if !ok {
		t.Fatalf("the errorrouting analyzer produced %T, want *ErrorRoutes", got)
	}

	byLabel := map[string]ErrorRoute{}
	for _, route := range routes.Routes {
		byLabel[route.Label] = route
	}

	// payments declares `on error ops.PageOnCall` at flow level, and `transform
	// try` overrides it with its own `on error stripe.VoidCharge`.
	try, found := byLabel["try"]
	if !found {
		t.Fatalf("payments.flow resolved no route for try; routes are %v", sortedKeys(byLabel))
	}
	if try.Kind != handlerNode || try.Handler != "stripe.VoidCharge" {
		t.Errorf("try resolved to %s/%q, want its own clause stripe.VoidCharge", try.Kind, try.Handler)
	}

	events, found := byLabel["events"]
	if !found {
		t.Fatalf("payments.flow resolved no route for events; routes are %v", sortedKeys(byLabel))
	}
	if events.Kind != handlerFlow || events.Handler != "ops.PageOnCall" {
		t.Errorf("events resolved to %s/%q, want the flow declaration ops.PageOnCall", events.Kind, events.Handler)
	}
}
