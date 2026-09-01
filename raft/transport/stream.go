// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package transport

import (
	"net"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

// groupStream must satisfy raft's StreamLayer exactly: NetworkTransport is
// built over it, and a drift in that interface must break the build here.
var _ raft.StreamLayer = (*groupStream)(nil)

// groupStream is one group's raft.StreamLayer over the shared listener. It is
// deliberately separate from Group: raft's NetworkTransport.Close calls
// StreamLayer.Close, and that call must unbind ONE group rather than take the
// node's whole listener — or every other group — down with it.
// forwardCh is the second delivery queue, holding connections the handshake
// announced as KindForward. It is separate from acceptCh so a backlog on one
// arm cannot starve the other, and it is released by the SAME doneCh: the
// group owns both streams' lifetime, so there is one door rather than two.
type groupStream struct {
	mux       *Mux
	kind      StreamKind
	id        GroupID
	acceptCh  chan net.Conn
	forwardCh chan net.Conn
	doneCh    chan struct{}
	once      sync.Once
}

// key is the pair this stream is registered under.
func (s *groupStream) key() streamKey { return streamKey{kind: s.kind, id: s.id} }

// newGroupStream builds the binding for id with the mux's queue depth, which
// both delivery queues take: neither arm is privileged over the other.
func newGroupStream(m *Mux, k streamKey) *groupStream {
	return &groupStream{
		mux:       m,
		kind:      k.kind,
		id:        k.id,
		acceptCh:  make(chan net.Conn, m.cfg.AcceptQueueDepth),
		forwardCh: make(chan net.Conn, m.cfg.AcceptQueueDepth),
		doneCh:    make(chan struct{}),
	}
}

// Accept implements net.Listener for raft's listen loop. Once the group is
// unbound it returns an error, which is what lets that goroutine exit instead
// of parking forever on a channel nobody will send to: raft's listen loop
// returns only when its own shutdown channel is closed, so an Accept that
// blocked forever after Close would leak one goroutine per group ever created.
func (s *groupStream) Accept() (net.Conn, error) {
	select {
	case c := <-s.acceptCh:
		return c, nil
	case <-s.doneCh:
		return nil, ErrClosed
	}
}

// Close unbinds this group and leaves the shared listener open. raft's
// NetworkTransport.Close calls straight through to here, so closing the shared
// listener would take every other group on the node down with one group.
func (s *groupStream) Close() error {
	s.mux.unbind(s.key())
	return nil
}

// Addr reports the shared mux address: every group on a node answers at one
// address, and the handshake is what tells them apart.
func (s *groupStream) Addr() net.Addr { return s.mux.Addr() }

// Dial opens a connection to address, announces this group on it and completes
// the session exchange. The connection handed back is the session conn over the
// raw dialed connection, with no deadline of ours left set.
//
// EXACTLY ONE WRAPPER SITS BETWEEN RAFT AND THE SOCKET, which is the successor
// to lane B's unwrapped-connection rule rather than a retreat from it: the mux
// still contributes no second layer, no buffered reader and no copy, and the one
// wrapper it does contribute is the encryption decision ce79d7e2 ruled.
func (s *groupStream) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	return dialSession(s.mux, KindRaft, s.id, string(address), timeout)
}

// release wakes everything parked on this binding, once.
func (s *groupStream) release() {
	s.once.Do(func() { close(s.doneCh) })
}
