// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// Package recovery turns a flow's membership signals into the set of datums whose
// worker is gone, and lets a survivor claim and finish one.
//
// DETECTION IS LEADER-GATED; CLAIMING IS NOT. The two are different operations on
// different nodes and conflating them is the trap this package is shaped around.
// Detection reads leader-only inputs — the peer signals autopilot publishes only
// where a flow is led, and a claim-state read that barriers — so it runs on the node
// that leads the flow, and it re-checks leadership itself rather than trusting the
// state that woke it. Claiming is a replicated append that FORWARDS, so any worker
// can claim from anywhere: the leader offers the orphan set and the workers race for
// it, which is what compare-and-claim through log ordering means.
//
// A DETECTOR ON A NON-LEADER REFUSES LOUDLY. It returns an error wrapping the
// ledger's not-leader refusal, never an empty set and a nil error — those are
// indistinguishable from "no orphans" at every call site above, and a recovery path
// that reports healthy while doing nothing is the exact failure this package exists
// to prevent.
//
// THE ALREADY-CLAIMED FILTER IS AN OPTIMIZATION, NOT THE CORRECTNESS MECHANISM.
// Single-writer-per-datum is enforced by the journal's first-writer-wins apply arm,
// which refuses every later claimant regardless of who read what beforehand. The
// filter exists so a leader does not offer work already taken; skipping it entirely
// would leave the system correct and merely noisier.
//
// # The duplicate window
//
// RECOVERY IS AT-LEAST-ONCE AND THE WINDOW IS DISCLOSED RATHER THAN MASKED. Work
// performed after a datum's last checkpoint and before its worker died is executed a
// SECOND time on resume, because the journal has no record of it. The window's size
// is the interval between checkpoints, and it is a property of the design rather
// than a defect to be narrowed by a second mechanism.
//
// WHAT IS DUPLICATED DEPENDS ON THE ANCHOR. For a node marked idempotent the record
// is written on ARRIVAL, so the duplicate is the node itself running again — harmless
// by the author's own declaration, which is what the marker declares. For an unmarked
// node the record is written on COMPLETION, so the node is NOT re-run; the duplicate
// is only the span between its last completed checkpoint and the death.
//
// THERE IS A SECOND, SMALLER WINDOW AT RETIREMENT. A worker that dies between a datum
// completing and its retire landing leaves a record for work already done, and
// recovery will claim and re-run it. That is the same at-least-once semantic arriving
// at the last checkpoint rather than at an intermediate one.
//
// # Streams
//
// EVERY STREAM THIS PACKAGE OPENS CARRIES A READ DEADLINE. It opens none of its own
// today — it rides the ledger's and the membership manager's — and that is stated
// here so a later author adding one knows the rule applies to it.
package recovery
