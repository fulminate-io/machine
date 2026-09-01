// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package transport

import (
	"net"
	"time"
)

// membershipID is the group id the control channel binds under.
//
// IT IS A CONSTANT because there is one control channel per node, not one per
// flow, and it is NON-EMPTY because bindKey refuses a zero-length id and that
// refusal stays. It cannot collide with a user-chosen flow name: the stream
// table is keyed by the kind and the id together, so this binding and a raft
// group that happens to be called "membership" occupy different slots.
const membershipID GroupID = "membership"

// MembershipLink is the node's control channel over the shared listener: the
// door the join, redirect, stats and leave exchange arrives at, which raft has
// no RPC for.
//
// It is a thin handle over the same groupStream a raft group binds, so the
// bounded accept queue, the done channel, the once-guarded release and the
// unbind through the mux are the landed ones rather than a second
// implementation. What it does NOT carry is the NetworkTransport construction a
// raft group needs, which is the one raft-specific part of that path.
type MembershipLink struct {
	stream *groupStream
}

// BindMembership registers this node's control channel. It refuses a second
// binding with ErrGroupBound and a closed mux with ErrClosed, on exactly the
// terms Bind does, because both reach the same registration path.
func (m *Mux) BindMembership() (*MembershipLink, error) {
	s, err := m.bindKey(streamKey{kind: KindMembership, id: membershipID})
	if err != nil {
		return nil, err
	}
	return &MembershipLink{stream: s}, nil
}

// Accept returns the next control connection. Once the link is closed it returns
// ErrClosed, which is what lets the serve loop reading it exit rather than
// parking forever on a channel nobody will send to.
func (l *MembershipLink) Accept() (net.Conn, error) { return l.stream.Accept() }

// Addr reports the shared mux address: every binding on a node answers at one
// address, and the handshake is what tells them apart.
func (l *MembershipLink) Addr() net.Addr { return l.stream.Addr() }

// Close unbinds the control channel and leaves every raft group on the shared
// listener running. It is idempotent, because the stream's release is a
// sync.Once — exactly as Group.Close is.
func (l *MembershipLink) Close() error { return l.stream.Close() }

// Dial opens a control connection to address. It goes through the one dial path,
// so a control connection is authenticated and encrypted on exactly the same
// terms as a raft one rather than on terms this file could drift away from.
func (l *MembershipLink) Dial(address string, timeout time.Duration) (net.Conn, error) {
	return dialSession(l.stream.mux, KindMembership, membershipID, address, timeout)
}
