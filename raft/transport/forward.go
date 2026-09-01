// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package transport

import (
	"net"
	"time"
)

// forwardListener must satisfy net.Listener exactly: the forwarding server is
// written against that interface rather than against this package's types, and
// a drift must break the build here.
var _ net.Listener = (*forwardListener)(nil)

// forwardListener hands out the connections a group's handshake announced as
// KindForward. It is the forwarding arm's counterpart to groupStream's raft
// arm, over the same binding and the same shared listener.
type forwardListener struct {
	stream *groupStream
}

// Accept returns the next forwarding connection for this group. Once the group
// is unbound it returns an error, which is what lets the server loop reading it
// exit instead of parking forever on a channel nobody will send to — the same
// reason groupStream.Accept gives for raft's loop.
func (l *forwardListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.stream.forwardCh:
		return c, nil
	case <-l.stream.doneCh:
		return nil, ErrClosed
	}
}

// Close is a NO-OP, and that is deliberate.
//
// THE FORWARDING STREAM'S LIFETIME IS THE GROUP'S, NOT THIS LISTENER'S. Group.Close
// is the one door that releases both of a group's streams, so a Close here that
// unbound the group would let a forwarding server's own shutdown take raft's
// delivery arm down with it. A caller that wants this listener to stop closes
// the group.
func (*forwardListener) Close() error { return nil }

// Addr reports the shared mux address, for the reason groupStream.Addr gives:
// every group on a node answers at one address, and group and stream identity
// ride the handshake rather than the address.
func (l *forwardListener) Addr() net.Addr { return l.stream.mux.Addr() }

// Forward returns the listener carrying the operations peers forwarded to this
// group. Its lifetime is this group's; see forwardListener.Close.
func (g *Group) Forward() net.Listener { return &forwardListener{stream: g.stream} }

// DialForward opens a forwarding connection to address and announces this group
// on it. It mirrors groupStream.Dial and inherits its discipline: the connection
// handed back is the RAW dialed connection with the handshake already written
// and no deadline of ours left on it, so the caller owns it unwrapped and this
// package contributes nothing to the per-operation path.
func (g *Group) DialForward(address string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return nil, err
	}
	if err := writePreamble(conn, g.stream.id, KindForward, timeout); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}
