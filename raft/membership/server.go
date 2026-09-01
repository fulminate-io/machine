// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"fmt"

	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/ledger"
)

// Ledger reports the open ledger for a flow this node hosts.
func (m *Manager) Ledger(flow string) (*ledger.Ledger, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.flows[flow]
	return l, ok
}

// addFlow records an open ledger for a flow.
func (m *Manager) addFlow(flow string, l *ledger.Ledger) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flows[flow] = l
}

// answerAnnounce answers a join request PER FLOW, because a receiver may lead
// one announced flow, follow another, and host none of a third. There is one
// answer per flow and never a single verdict for the request.
func (m *Manager) answerAnnounce(req announce) announceReply {
	reply := announceReply{
		Node:      m.cfg.Node,
		Redirects: make(map[string]string, len(req.Flows)),
		Refused:   make(map[string]string, len(req.Flows)),
	}
	for _, flow := range req.Flows {
		m.answerOneFlow(flow, req, &reply)
	}
	return reply
}

// answerOneFlow settles one flow of an announce.
//
// REFUSALS ARE NAMED, NEVER DROPPED. A flow this node does not host comes back
// in Refused naming the flow, because a silent omission is indistinguishable
// from a lost message and would leave the joiner believing it had joined.
func (m *Manager) answerOneFlow(flow string, req announce, reply *announceReply) {
	l, ok := m.Ledger(flow)
	if !ok {
		reply.Refused[flow] = fmt.Sprintf("this node does not host flow %q", flow)
		return
	}
	r := l.Raft()
	if r.State() != raft.Leader {
		m.redirect(flow, r, reply)
		return
	}
	if err := m.stage(r, req); err != nil {
		reply.Refused[flow] = fmt.Sprintf("staging %q into flow %q failed: %v", req.Node, flow, err)
		return
	}
	reply.Staged = append(reply.Staged, flow)
}

// redirect names the address the joiner should ask instead.
//
// REDIRECTION IS CLIENT-SIDE, which is the shape already ruled for a non-leader
// write, and honest for the same reason: a node that does not lead cannot admit
// anyone, so saying so and naming who can is the whole of what it owes.
func (*Manager) redirect(flow string, r *raft.Raft, reply *announceReply) {
	addr, _ := r.LeaderWithID()
	if addr == "" {
		reply.Refused[flow] = fmt.Sprintf("flow %q has no known leader to redirect to", flow)
		return
	}
	reply.Redirects[flow] = string(addr)
}

// stage admits a joiner as a NONVOTER and then confirms it landed.
//
// THE JOIN FUTURE IS NOT THE EVIDENCE. The measured trap is that the voter form
// returns a nil future error in zero seconds on exactly the path that costs the
// leader its leadership, so a join path that trusted the future would report
// success on the path that just broke the group. A flow counts as staged when
// the joiner appears in the leader's COMMITTED configuration, which is what
// confirmStaged reads.
func (m *Manager) stage(r *raft.Raft, req announce) error {
	if err := m.admit(r, raft.ServerID(req.Node), raft.ServerAddress(req.Address)).Error(); err != nil {
		return err
	}
	return confirmStaged(r, raft.ServerID(req.Node))
}

// confirmStaged reports whether id is in the leader's committed configuration.
func confirmStaged(r *raft.Raft, id raft.ServerID) error {
	future := r.GetConfiguration()
	if err := future.Error(); err != nil {
		return err
	}
	for _, server := range future.Configuration().Servers {
		if server.ID == id {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrNotStaged, id)
}
