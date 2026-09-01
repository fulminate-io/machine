// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// Package membership gives a worker flow-scoped raft membership over the shared
// transport: the join exchange raft itself has no RPC for, the not-leader
// redirect, the per-peer progress the promoter needs, and the leave.
//
// RAFT HAS NO JOIN RPC, AND THAT IS WHY THIS PACKAGE EXISTS. A node absent from
// a configuration is dialed by nobody, and hashicorp/raft exports nothing a
// joiner could call to ask for admission — its surface is operations on a group
// you are already in. The transport routes an announced group id straight to
// that group's NetworkTransport, so a joiner dialing a flow id would be handed
// to raft's RPC decoder, which has no message meaning "add me". The join, the
// redirect, the leave and the per-peer stats therefore ride a control channel of
// our own, on a separate handshake kind over the same listener.
//
// THE STATS RIDE HERE TOO, for a reason worth stating: raft exposes no
// per-follower progress on the leader. Stats() is local and there is no exported
// match index, so a leader cannot compute whether a joiner has caught up. Every
// member answers for itself over this channel instead, and one request carries a
// flow LIST so a single round trip serves every flow a peer shares with us.
//
// ENCODING. Each exchange is one kind byte followed by one length-prefixed gob
// value in each direction. gob because the ledger already uses it for the
// journal and consistency inside the module is worth more than an encoding
// argument. Unlike journal entries these messages are NOT replicated and never
// reach disk, so a later retarget of the journal's serialization does not reach
// them.
package membership
