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
type groupStream struct {
	mux      *Mux
	id       GroupID
	acceptCh chan net.Conn
	doneCh   chan struct{}
	once     sync.Once
}

// newGroupStream builds the binding for id with the mux's queue depth.
func newGroupStream(m *Mux, id GroupID) *groupStream {
	return &groupStream{
		mux:      m,
		id:       id,
		acceptCh: make(chan net.Conn, m.cfg.AcceptQueueDepth),
		doneCh:   make(chan struct{}),
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
	s.mux.unbind(s.id)
	return nil
}

// Addr reports the shared mux address: every group on a node answers at one
// address, and the handshake is what tells them apart.
func (s *groupStream) Addr() net.Addr { return s.mux.Addr() }

// Dial opens a connection to address and announces this group on it. The
// connection handed back is the raw dialed connection with the handshake
// already written and no deadline of ours left set, so raft owns it unwrapped
// and the mux contributes nothing to the per-RPC path.
func (s *groupStream) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", string(address), timeout)
	if err != nil {
		return nil, err
	}
	if err := writePreamble(conn, s.id, timeout); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// release wakes everything parked on this binding, once.
func (s *groupStream) release() {
	s.once.Do(func() { close(s.doneCh) })
}
