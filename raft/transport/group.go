// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package transport

import "github.com/hashicorp/raft"

// Group is one raft group's binding on the shared listener: the stream layer
// raft accepts and dials through, and the NetworkTransport built over it.
//
// The lifecycle a caller conforms to is Bind then Close. Close is idempotent
// and remains safe after raft has closed the transport itself, which raft does
// when the caller drains the future returned by Raft.Shutdown.
type Group struct {
	stream *groupStream
	trans  *raft.NetworkTransport
}

// Bind registers a group and returns its transport binding. A group id may be
// bound once at a time; Close frees it for rebinding. It refuses a duplicate id
// with ErrGroupBound, an out-of-range id with ErrGroupIDRange and a closed mux
// with ErrClosed.
func (m *Mux) Bind(id GroupID) (*Group, error) {
	s, err := m.bindStream(id)
	if err != nil {
		return nil, err
	}
	return newGroup(m, s), nil
}

// newGroup builds the NetworkTransport over an already-registered stream.
func newGroup(m *Mux, s *groupStream) *Group {
	return &Group{
		stream: s,
		trans: raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
			Stream:  s,
			MaxPool: m.cfg.MaxPool,
			Timeout: m.cfg.RPCTimeout,
			Logger:  m.logger,
		}),
	}
}

// ID reports the group this binding carries.
func (g *Group) ID() GroupID { return g.stream.id }

// Transport returns the raft Transport for this group. The concrete type is
// returned so raft's optional WithClose and WithPreVote upgrades still resolve
// through it.
func (g *Group) Transport() *raft.NetworkTransport { return g.trans }

// Close closes this group's transport, which unbinds it from the mux and leaves
// every other group on the shared listener running. It is idempotent:
// NetworkTransport.Close is guarded by its own shutdown flag and the stream's
// release is a sync.Once, so a caller may call it unconditionally after
// draining raft's shutdown future, which closes the transport itself.
func (g *Group) Close() error { return g.trans.Close() }
