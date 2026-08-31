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
// may never do. Adding a journal kind means declaring a Kind constant and an arm
// in the state machine's apply; it is a coordinated upgrade by design, not an
// accident of deployment order.
package ledger
