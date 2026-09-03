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
// A DETECTOR ON A NON-LEADER REFUSES LOUDLY. It returns an error wrapping BOTH the
// ledger's not-leader refusal and the root module's, never an empty set and a nil
// error — those are indistinguishable from "no orphans" at every call site above, and
// a recovery path that reports healthy while doing nothing is the exact failure this
// package exists to prevent. It carries both sentinels because the two consumers can
// name different things: this module's own callers key on the ledger's, and the root
// module may not import this package, so the root's is the only one a machine can
// name. Naming it is what lets the machine's resume loop treat the refusal as a WAIT
// — see AwaitLeadership — rather than as the fatal error it cannot classify.
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
// A DEPARTED HOLDER'S CLAIM IS RETIRED BY THE LEADER, NOT STOLEN BY A SURVIVOR. A
// worker that dies while HOLDING a claim would otherwise strand its datum forever: the
// journal refuses every later claimant, and the state machine has no liveness view with
// which to judge that the holder is gone. The leader observes the departure through the
// membership signals it already reads, appends a retire-claim entry that drops the claim
// and leaves the checkpoint, and re-offers the datum; a live worker then claims it
// through the ordinary path. First-writer-wins is untouched, and the datum re-enters the
// duplicate window above rather than a third one of its own.
//
// A CLAIM IS WRITTEN UNDER THE LEDGER'S OWN IDENTITY, and the retire mechanism above
// rests entirely on it. A claim's claimant, a record's Owner and the alive set the
// leader builds from the committed configuration are ONE namespace, which is what
// lets the leader ask whether a claim's holder is still a member and get a meaningful
// answer. The seam therefore takes no owner from its caller: while it did, every
// replica passed the same machine name, so no claim excluded another survivor and the
// leader read every claimant as departed and retired it in the same scan.
//
// # Liveness, and what this design does not prevent
//
// RECOVERY NO LONGER WAITS ON EVICTION. An owner that is still in the committed
// configuration but that the leader's health view marks unreachable is offered at
// once, and an eviction removing it later changes nothing. Orphanhood used to be
// decided by the configuration alone, so a datum stayed stranded until its dead
// owner was removed; it is now decided by the configuration MINUS the members the
// leader has published as unreachable, and the removal is no longer on the path.
//
// EVICTION BOUND 2 STILL HAS NO TIMEOUT AND NO ESCALATION. The membership package
// refuses a removal that would take the configuration below the live count, and this
// package neither owns that bound nor changes it. What survives is narrower than it
// used to be: the dead voter stays configured until an operator removes it, but its
// datums are recovered regardless, because orphanhood no longer depends on that
// removal happening.
//
// A LIVE MEMBER'S EVICTION SUSPENDS ITS DATUMS RATHER THAN ORPHANING THEM, and it is
// worth saying plainly which departures do NOT suspend and why. An eviction of an
// UNREACHABLE member is a death, and a graceful departure through the leave path
// emits no signal at all; both are gone at once, because stranding them is the
// failure the absence arm exists to prevent. Only a live member's eviction pays a
// wait, and it pays in LATENCY rather than in stranding: the suspension expires at a
// deadline the signal itself carries, after which the datums are offered.
//
// THE LIVENESS FACT IS AN OBSERVATION, AND A PARTITIONED MEMBER READS DEAD. This is
// the honest statement of the whole design's cost and it belongs here beside the
// duplicate windows rather than buried. The health view is what the LEADER can see,
// so a member that is alive but partitioned from the leader is treated as dead and
// its datums are offered while it is still running them; the eviction mark is a
// single observation taken at removal, so that same member's eviction reads
// not-reachable and is not suspended either. Both follow from one choice — the
// leader's view is the only liveness authority available — and the compensating
// mechanism is the journal's first-writer-wins claim, which admits exactly one writer
// even when two try. THAT EXCLUSION IS REAL ONLY BECAUSE THE CLAIM CARRIES THE
// LEDGER'S IDENTITY, described above: while the claim was written under the machine's
// name, one string shared by every replica, it admitted every survivor and the
// compensation named here did not exist. Name the residual plainly: THIS DESIGN
// PREVENTS A SECOND CLAIM, NOT A SECOND ATTEMPT.
//
// A REFUSED CURSOR DROPS THE HEALTH VIEW AND THE SUSPENSIONS WITH IT. A cursor the
// signal log refuses means signals were dropped between this reader and the log, so
// a view accumulated from a prefix of the stream is not trustworthy; both are
// rebuilt from the next batch. The two losses err in opposite directions and the
// second is the one to know about. Until a peer's next transition an owner that was
// unreachable reads alive, so its datums are offered LATE rather than wrongly. A
// dropped SUSPENSION goes the other way: the owner reads absent and its datums may
// be offered inside a window that is no longer known, which is the one place this
// design trades toward the duplicate rather than away from it.
//
// # Streams
//
// EVERY STREAM THIS PACKAGE OPENS CARRIES A READ DEADLINE. It opens none of its own
// today — it rides the ledger's and the membership manager's — and that is stated
// here so a later author adding one knows the rule applies to it.
package recovery
