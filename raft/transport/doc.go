// Package transport - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
//
// Package transport multiplexes N hashicorp/raft groups over ONE listener.
//
// hashicorp/raft has no notion of a group: NewRaft takes exactly one Transport,
// the Transport interface carries no group parameter on any of its ten methods,
// and every inbound RPC arrives on a single Consumer channel. NewTCPTransport
// therefore binds its own listener per instance, so N groups in one process
// would need N ports. The only seam below raft that can carry a group identity
// is raft.StreamLayer (net.Listener plus Dial), which is what NetworkTransport
// is built on, so the group tag rides a fixed-shape handshake written by Dial
// and read by the shared accept loop before the connection is handed to raft.
package transport
