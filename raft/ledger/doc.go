// Package ledger - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
//
// Package ledger gives a flow a raft-replicated recovery ledger: a deterministic
// state machine over a kind-tagged journal, a linearizable read built from the
// primitives hashicorp/raft actually exposes, and a machine.Store implementation
// whose contract states its own guarantees rather than inheriting the in-memory
// store's.
//
// THE READ IS BUILT, NOT BORROWED. hashicorp/raft v1.7.3 exposes no ReadIndex, so
// a linearizable read here is VerifyLeader — which proves this node still holds
// the term against a quorum — followed by a wait until the state machine's OWN
// applied index reaches the commit index observed after that proof. Reading raft's
// commit index alone is not enough: the commit index counts entries this state
// machine may not have applied yet, and a value read before they land is stale.
//
// A FRESHLY ELECTED LEADER COMMITS AN ENTRY THIS PACKAGE CAN SEE. raft commits a
// no-op on election to establish its term, and that entry never reaches Apply, so
// a first read of a term would wait forever on an index the state machine can
// never reach. Every leadership term therefore appends one KindEpoch entry, which
// carries no data and exists only to move the applied index past whatever the
// election committed. It is appended per TERM rather than once per ledger, because
// a node that leads, loses the term and leads again needs one for each of them.
//
// EVERY MEMBER OF A GROUP MUST RUN A BUILD THAT KNOWS EVERY KIND IN ITS JOURNAL.
// An entry whose kind is outside the declared set poisons the ledger loudly rather
// than being skipped or defaulted, because skipping it would let two peers that
// disagree about the vocabulary diverge — the one thing a replicated state machine
// may never do. That holds on BOTH arrival paths: a kind reaching a node inside a
// snapshot is no more interpretable than the same kind reaching it in the log, so a
// restore validates what it installs exactly as apply does, and a restore that
// discards a journal also clears the poison recorded against it. Both paths consult
// ONE declared-kind predicate, so a kind can never be admitted by one and refused by
// the other. Adding a journal kind therefore means declaring a Kind constant, adding
// it to that predicate, and giving it an arm in the state machine's apply; it is a
// coordinated upgrade by design, not an accident of deployment order.
//
// A NON-LEADER FORWARDS RATHER THAN REFUSING. raft refuses both an append and a
// leadership verification on a node that is not the leader, so the leader-local
// primitives beneath this package's surface return ErrNotLeader there. The surface
// itself — Append, Get, and every Store method over them — resolves the flow group's
// leader and carries the operation to it, so a flow author calls the same methods on
// any member of the group and gets the same answer.
//
// THE REFUSAL DID NOT DISAPPEAR; IT BECAME THE SIGNAL THE FORWARDING RUNS ON. The loop
// reads it twice: to decide an operation must leave this node, and to decide that a
// forwarded operation which landed on a node that no longer leads must be re-resolved
// by its client rather than relayed onward — relaying it would let two peers whose
// leader resolution disagrees bounce it between them with no bound governing the chain.
//
// THE FORWARDING IS BOUNDED, and the bound is wall-clock rather than a count of
// attempts, because the attempts one leadership event costs depend entirely on the
// retry interval. Config.ForwardTimeout sets it and the default covers raft's own
// worst compliant detection-plus-election window twice over. An operation no leader
// serves within it fails with ErrForwardBoundExceeded, naming the flow, the attempts
// made, the bound and the last condition observed — so a caller can tell a leadership
// change in progress from a group that has lost quorum. The caller's own context is the
// outer bound and is reported as itself.
//
// A FORWARDED READ IS STILL LINEARIZABLE, because the barrier above runs ON THE NODE
// THAT SERVES IT. The leader proves its term against a quorum and waits for its own
// state machine before answering, so traveling costs the read none of its guarantee.
package ledger
