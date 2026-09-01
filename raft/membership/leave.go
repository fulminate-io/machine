// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/hashicorp/raft"
)

// Leave refusals, declared beside the code that returns them.
var (
	// ErrLeaveUnreachable reports a leave whose flow leader could not be
	// reached. It is an ERROR AND NOT A LOCAL CLOSE: reporting it leaves the
	// group with a member the leader can still see and a recovery path can act
	// on, while closing anyway would manufacture exactly the orphan that
	// recovery exists to clean up, and would do it silently.
	ErrLeaveUnreachable = errors.New("membership: a flow's leader could not be reached to leave through")
	// ErrLeaveRefused reports a leave the leader declined, naming the flow.
	ErrLeaveRefused = errors.New("membership: a leave was refused")
)

// SetFlows IS THE ONLY ENTRY POINT FOR A FLOW-SET CHANGE, and it computes the
// difference ITSELF. Two call sites computing the difference is how the added
// and removed halves drift apart.
//
// Removals run first: a node that is shedding a flow should stop being a member
// of it before it takes on more work.
func (m *Manager) SetFlows(ctx context.Context, flows []string) error {
	added, removed := m.diffFlows(flows)
	for _, flow := range removed {
		if err := m.leaveFlow(ctx, flow); err != nil {
			return err
		}
	}
	for _, flow := range added {
		if err := m.joinFlow(ctx, flow); err != nil {
			return err
		}
	}
	m.peers.setMembership(m.peers.addresses(), m.hostedFlows())
	return nil
}

// diffFlows reports which of want this node does not host and which it hosts
// that want does not name.
func (m *Manager) diffFlows(want []string) (added, removed []string) {
	wanted := make(map[string]bool, len(want))
	for _, flow := range want {
		wanted[flow] = true
	}
	held := m.hostedFlows()
	for _, flow := range held {
		if !wanted[flow] {
			removed = append(removed, flow)
		}
	}
	for _, flow := range want {
		if _, ok := m.Ledger(flow); !ok {
			added = append(added, flow)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// hostedFlows reports the flows this node currently holds a ledger for.
func (m *Manager) hostedFlows() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.flows))
	for flow := range m.flows {
		out = append(out, flow)
	}
	sort.Strings(out)
	return out
}

// joinFlow opens a flow's ledger and settles it against the cluster, on exactly
// the terms Start uses for the flows it starts with — through openFlow, which is
// what makes "exactly the terms" a shared call rather than a claim in prose that
// the next edit to either site can quietly falsify.
func (m *Manager) joinFlow(ctx context.Context, flow string) error {
	l, err := m.openFlow(flow)
	if err != nil {
		return err
	}
	m.addFlow(flow, l)
	if err := m.placeFlow(ctx, flow); err != nil {
		return err
	}
	m.superviseFlow(flow)
	return nil
}

// leaveFlow removes this node from a flow and tears its ledger down, IN THIS
// ORDER AND NO OTHER:
//
//  1. ask that flow's leader to remove this node
//  2. close this node's ledger for that flow, UNCONDITIONALLY
//  3. drop the flow from the manager's map
//
// REMOVAL FIRST. Closing first would leave the group carrying a member that has
// stopped answering, which is indistinguishable from a failure and would raise
// the failure signal for a departure that was perfectly orderly.
//
// THE CLOSE IS UNCONDITIONAL FOR TWO REASONS, and only one of them is the reason
// the ledger documents. That one holds where it was written: a LEADER that
// removes itself calls raft's Shutdown internally and discards the future, and
// only a drained shutdown future closes the transport, so the group id stays
// held. The second is this lane's own measurement: a removed FOLLOWER is told
// NOTHING AT ALL. ShutdownOnRemove is consulted at exactly one site inside
// leaderLoop, under a step-down guard that fires when the LEADER commits a
// configuration giving its own id no vote — so a probe's removed follower sat in
// state Follower with its raft instance up and its group id still bound. The
// departing node closes its own ledger because nothing else will.
func (m *Manager) leaveFlow(ctx context.Context, flow string) error {
	if err := m.requestRemoval(ctx, flow); err != nil {
		return err
	}
	return m.closeFlow(flow)
}

// requestRemoval asks the flow's leader to drop this node.
func (m *Manager) requestRemoval(ctx context.Context, flow string) error {
	l, ok := m.Ledger(flow)
	if !ok {
		return fmt.Errorf("membership: flow %q is not hosted here", flow)
	}
	r := l.Raft()
	if r.State() == raft.Leader {
		return r.RemoveServer(raft.ServerID(m.cfg.Node), 0, 0).Error()
	}
	addr, _ := r.LeaderWithID()
	if addr == "" {
		return fmt.Errorf("%w: flow %q knows no leader", ErrLeaveUnreachable, flow)
	}
	return m.leaveVia(ctx, string(addr), flow)
}

// leaveVia sends one leave to the address that leads the flow.
func (m *Manager) leaveVia(ctx context.Context, addr, flow string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	reply, err := m.peers.call(addr, leave{Node: m.cfg.Node, Flows: []string{flow}})
	if err != nil {
		return fmt.Errorf("%w: flow %q through %s: %v", ErrLeaveUnreachable, flow, addr, err)
	}
	answer, ok := reply.(*leaveReply)
	if !ok {
		return fmt.Errorf("%w: a leave was answered with %T", ErrUnknownMessage, reply)
	}
	if reason, refused := answer.Refused[flow]; refused {
		return fmt.Errorf("%w: flow %q by %s: %s", ErrLeaveRefused, flow, addr, reason)
	}
	if !contains(answer.Removed, flow) {
		return fmt.Errorf("%w: flow %q was neither removed nor refused by %s", ErrLeaveRefused, flow, addr)
	}
	return nil
}

// closeFlow stops the flow's supervision and closes its ledger, releasing the
// group id for rebinding.
func (m *Manager) closeFlow(flow string) error {
	m.mu.Lock()
	l, held := m.flows[flow]
	pilot := m.pilots[flow]
	delete(m.flows, flow)
	delete(m.pilots, flow)
	m.mu.Unlock()
	if pilot != nil {
		pilot.release()
	}
	if !held {
		return nil
	}
	return l.Close()
}

// answerLeave removes a departing member from each flow it names, answering PER
// FLOW for the reason the announce arm gives.
func (m *Manager) answerLeave(req leave) leaveReply {
	reply := leaveReply{Refused: make(map[string]string, len(req.Flows))}
	for _, flow := range req.Flows {
		m.answerOneLeave(flow, req, &reply)
	}
	return reply
}

// answerOneLeave settles one flow of a leave. REFUSALS ARE NAMED, never dropped.
func (m *Manager) answerOneLeave(flow string, req leave, reply *leaveReply) {
	l, ok := m.Ledger(flow)
	if !ok {
		reply.Refused[flow] = fmt.Sprintf("this node does not host flow %q", flow)
		return
	}
	r := l.Raft()
	if r.State() != raft.Leader {
		reply.Refused[flow] = fmt.Sprintf("this node does not lead flow %q", flow)
		return
	}
	if err := r.RemoveServer(raft.ServerID(req.Node), 0, 0).Error(); err != nil {
		reply.Refused[flow] = fmt.Sprintf("removing %q from flow %q failed: %v", req.Node, flow, err)
		return
	}
	reply.Removed = append(reply.Removed, flow)
}
