// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"context"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	autopilot "github.com/hashicorp/raft-autopilot"
)

// Promotion thresholds, each with its number. An operator raises the
// stabilization time more often than any other.
const (
	defaultLastContactThreshold    = 500 * time.Millisecond
	defaultMaxTrailingLogs         = 100
	defaultMinQuorum               = 1
	defaultServerStabilizationTime = 10 * time.Second
)

// AutopilotTuning overrides the promoter's thresholds and the reconcile
// cadence. A zero field takes the default beside it.
type AutopilotTuning struct {
	LastContactThreshold    time.Duration
	MaxTrailingLogs         uint64
	MinQuorum               uint
	ServerStabilizationTime time.Duration
	UpdateInterval          time.Duration
	ReconcileInterval       time.Duration
}

// flowPilot is one flow's autopilot integration. ONE INSTANCE RUNS PER FLOW, and
// only where that flow is led.
type flowPilot struct {
	mgr   *Manager
	flow  string
	pilot *autopilot.Autopilot
	// done releases this flow's supervisor when the flow itself goes away,
	// which a manager-wide cancel cannot express: a single flow leaving must
	// stop reading a raft handle that is about to be closed, while every other
	// flow keeps running.
	done     chan struct{}
	doneOnce sync.Once

	mu      sync.Mutex
	state   *autopilot.State
	failed  []raft.ServerID
	healthy map[raft.ServerID]bool
}

// release ends this flow's supervision, once.
func (f *flowPilot) release() { f.doneOnce.Do(func() { close(f.done) }) }

// flowPilot must satisfy autopilot's integration contract exactly; a drift in
// that interface must break the build here rather than at a call site.
var _ autopilot.ApplicationIntegration = (*flowPilot)(nil)

// newFlowPilot builds the integration and the autopilot instance for one flow.
func newFlowPilot(m *Manager, flow string, r *raft.Raft) *flowPilot {
	f := &flowPilot{mgr: m, flow: flow, done: make(chan struct{}), healthy: map[raft.ServerID]bool{}}
	tuning := m.cfg.Autopilot
	f.pilot = autopilot.New(r, f,
		autopilot.WithLogger(m.logger.Named("autopilot").With("flow", flow)),
		autopilot.WithUpdateInterval(orDuration(tuning.UpdateInterval, autopilot.DefaultUpdateInterval)),
		autopilot.WithReconcileInterval(orDuration(tuning.ReconcileInterval, autopilot.DefaultReconcileInterval)),
	)
	return f
}

// AutopilotConfig supplies the thresholds every reconcile round reads.
//
// CleanupDeadServers IS FALSE, AND THE REASON IS MEASURED RATHER THAN
// JURISDICTIONAL. It is not that removal belongs elsewhere — dead-member
// eviction is this lane's. It is that autopilot PROVABLY WILL NOT ACT under
// ephemeral identities: with it true and KnownServers fed from the live peers
// probe, autopilot identified the dead member correctly and then declined every
// reconcile interval with "will not remove server node as removal of a majority
// of voting servers is not safe", because its removal budget counts only servers
// present in BOTH the raft configuration and KnownServers. That guard is right
// for stable identities and wrong for ours.
func (f *flowPilot) AutopilotConfig() *autopilot.Config {
	tuning := f.mgr.cfg.Autopilot
	return &autopilot.Config{
		CleanupDeadServers:      false,
		LastContactThreshold:    orDuration(tuning.LastContactThreshold, defaultLastContactThreshold),
		MaxTrailingLogs:         orUint64(tuning.MaxTrailingLogs, defaultMaxTrailingLogs),
		MinQuorum:               orUint(tuning.MinQuorum, defaultMinQuorum),
		ServerStabilizationTime: orDuration(tuning.ServerStabilizationTime, defaultServerStabilizationTime),
	}
}

// NotifyState records the state the membership signals read, and turns the
// health it carries into unreachable and returned signals.
func (f *flowPilot) NotifyState(state *autopilot.State) {
	f.mu.Lock()
	f.state = state
	f.mu.Unlock()
	f.noteHealth(healthOf(state))
}

// healthOf projects autopilot's published state down to the one thing the
// signals read: whether each member is healthy.
func healthOf(state *autopilot.State) *autopilotState {
	out := &autopilotState{health: map[raft.ServerID]bool{}}
	if state == nil {
		return out
	}
	for id, server := range state.Servers {
		out.health[id] = server.Health.Healthy
	}
	return out
}

// FetchServerStats answers from the PER-NODE stats view, never by dialing here.
//
// THAT IS A PERFORMANCE REQUIREMENT, NOT A CONVENIENCE. One autopilot instance
// runs per flow, each on its own schedule, so an integration that dialed peers
// inside this method would issue N independent stats rounds per interval for N
// led flows — at fifty flows and three peers, three hundred round trips per two
// seconds where fifteen would do, defeating the per-call batching the control
// client is built around.
func (f *flowPilot) FetchServerStats(
	_ context.Context, servers map[raft.ServerID]*autopilot.Server,
) map[raft.ServerID]*autopilot.ServerStats {
	byAddress := f.mgr.Stats(f.flow)
	out := make(map[raft.ServerID]*autopilot.ServerStats, len(servers))
	for id, server := range servers {
		if stats, ok := byAddress[string(server.Address)]; ok {
			out[id] = statsFor(stats)
		}
	}
	// This node answers for ITSELF locally rather than dialing its own address,
	// which no peer set contains anyway.
	if self, ok := f.mgr.localFlowStats(f.flow); ok {
		out[raft.ServerID(f.mgr.cfg.Node)] = statsFor(self)
	}
	return out
}

// statsFor renders one member's report in autopilot's vocabulary.
func statsFor(stats FlowStats) *autopilot.ServerStats {
	return &autopilot.ServerStats{
		LastContact: stats.LastContact,
		LastTerm:    stats.Term,
		LastIndex:   stats.LastIndex,
	}
}

// KnownServers is this node's view of the flow's members, taken from the
// committed configuration.
func (f *flowPilot) KnownServers() map[raft.ServerID]*autopilot.Server {
	l, ok := f.mgr.Ledger(f.flow)
	if !ok {
		return nil
	}
	future := l.Raft().GetConfiguration()
	if future.Error() != nil {
		return nil
	}
	leaderID := raft.ServerID("")
	if _, id := l.Raft().LeaderWithID(); id != "" {
		leaderID = id
	}
	out := make(map[raft.ServerID]*autopilot.Server)
	for _, server := range future.Configuration().Servers {
		out[server.ID] = &autopilot.Server{
			ID:          server.ID,
			Name:        string(server.ID),
			Address:     server.Address,
			NodeStatus:  autopilot.NodeAlive,
			RaftVersion: int(raft.ProtocolVersionMax),
			IsLeader:    server.ID == leaderID,
		}
	}
	return out
}

// RemoveFailedServer RECORDS AND RETURNS. Autopilot's contract asks this to come
// back nearly immediately, and the eviction path is what acts on the signal —
// which matters here beyond politeness, because autopilot's own cleanup provably
// declines to act under ephemeral identity.
func (f *flowPilot) RemoveFailedServer(server *autopilot.Server) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.failed {
		if id == server.ID {
			return
		}
	}
	f.failed = append(f.failed, server.ID)
	f.mgr.logger.Warn("autopilot reported a failed member", "flow", f.flow, "node", string(server.ID))
}

// start runs this flow's reconcile loop.
func (f *flowPilot) start(ctx context.Context) { f.pilot.Start(ctx) }

// stop halts the loop and reports the channel that closes once both of
// autopilot's goroutines have exited, so a caller can join them before tearing
// down the ledger they reconcile against.
func (f *flowPilot) stop() <-chan struct{} { return f.pilot.Stop() }

func orDuration(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

func orUint64(value, fallback uint64) uint64 {
	if value == 0 {
		return fallback
	}
	return value
}

func orUint(value, fallback uint) uint {
	if value == 0 {
		return fallback
	}
	return value
}
