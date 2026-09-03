// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package recovery

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/whitaker-io/machine/raft/checkpoint"
	"github.com/whitaker-io/machine/raft/ledger"
	"github.com/whitaker-io/machine/raft/membership"
	machine "github.com/whitaker-io/machine/v4"
)

// Membership is the slice of the membership manager this package reads.
//
// IT IS AN INTERFACE DECLARED HERE rather than the concrete manager, following the
// same direction the root module's own seams run: the consumer states what it needs
// and the provider satisfies it. *membership.Manager satisfies this as written, and
// stating it narrowly also makes the detector's behavior observable against a
// membership view a test can control, which a concrete dependency would not be.
type Membership interface {
	Watch(ctx context.Context, since uint64) ([]membership.Signal, uint64, error)
	Membership(flow string) (raft.Configuration, uint64, bool)
}

// leadershipPollInterval is how often a parked detector re-reads whether this node
// leads its flow. IT IS THE CADENCE THE MEMBERSHIP SUPERVISOR ALREADY USES —
// raft/membership/manager.go names the same number for the same read — and a poll
// rather than a channel because raft's NotifyCh is owned by the ledger's drain,
// which must never park, and LeaderCh drops notifications a parked reader misses.
const leadershipPollInterval = 50 * time.Millisecond

// Detector turns a flow's membership into the datums whose worker is gone.
type Detector struct {
	ledger  *ledger.Ledger
	manager Membership
	flow    string

	// mu guards cursor, unreachable and suspended. ONE DETECTOR SERVES EVERY
	// CHECKPOINTED WORKER on a node, because a machine has one journal, and the
	// root starts a resume loop PER WORKER — so a flow with two checkpointed nodes
	// has two concurrent Orphans calls walking all three fields.
	mu     sync.Mutex
	cursor uint64
	// unreachable is the leader's health view, accumulated from the peer-health
	// signals await already receives. THE CONTRACT IT DEPENDS ON, named here so
	// two plans cannot drift apart on it: SignalPeerUnreachable is published
	// STABILIZED-THEN-LOST — a first sighting publishes nothing in either
	// direction, and a peer unhealthy since its first sighting is reported once
	// the stabilization window has passed — and SignalPeerReturned closes only an
	// episode that was actually opened. Without that contract a first-time leader
	// would open by naming every peer unreachable and this set would read every
	// live peer as dead.
	unreachable map[string]struct{}
	// suspended holds owners EVICTED WHILE REACHABLE, against the instant their
	// suspension expires. A live member's eviction is not a death: lane D's
	// re-place round readmits it a round later, and until then offering its datums
	// would put a second writer beside a running one.
	suspended map[string]time.Time
}

// New builds a detector over one flow's ledger.
//
// IT TAKES NO IDENTITY PARAMETER, and that is the point rather than an omission. The
// node's identity is read from the ledger it was handed, so there is no second field
// to disagree with the first — a divergence this surface cannot express is strictly
// better than one it refuses. The two-stamper defect that motivated this is guarded
// upstream at the membership open sites; this shape is what keeps recovery correct
// even for a ledger opened directly, outside the manager, where that guard never runs.
func New(l *ledger.Ledger, manager Membership, flow string) *Detector {
	return &Detector{
		ledger: l, manager: manager, flow: flow,
		unreachable: map[string]struct{}{},
		suspended:   map[string]time.Time{},
	}
}

// LocalID reports the identity this detector compares an orphan's owner against.
//
// It is the LEDGER's, never a copy: the configuration and the signals are stamped
// with that same authority, so a comparison against anything else would be comparing
// against a different fact.
func (d *Detector) LocalID() string { return d.ledger.LocalID() }

// Checkpoint journals one record.
//
// THE WHOLE RECORD IS PERSISTED, NOT ONLY ITS PAYLOAD, and that is load-bearing in
// two directions. Detection needs the OWNER to decide whether a datum's worker is
// still in the configuration — an orphan is a checkpoint whose writer is gone, and a
// payload alone cannot say who wrote it. Resume needs the ANCHOR and the NODE to
// decide where the record goes back: an arrival record returns to its own node and
// re-runs it, a completion record is re-injected into that node's successors. A
// journal that stored bytes alone would leave both questions unanswerable at the
// only moment they matter.
//
// THE OWNER IS STAMPED HERE rather than by the caller, from the ledger's own
// identity — the same authority the configuration and the signals carry, so the
// comparison at detection time is against the same fact rather than a copy of it.
func (d *Detector) Checkpoint(ctx context.Context, record machine.CheckpointRecord) error {
	if record.Owner == "" {
		record.Owner = d.ledger.LocalID()
	}

	value, err := encodeRecord(record)
	if err != nil {
		return err
	}
	if _, err := d.ledger.Append(ctx, ledger.Entry{
		Kind: ledger.KindSet, Path: checkpoint.Path(record.Datum), Value: value,
	}); err != nil {
		return fmt.Errorf("recovery: journaling datum %q on flow %q: %w", record.Datum, record.Flow, err)
	}

	return nil
}

// encodeRecord renders a record as the bytes the journal replicates.
func encodeRecord(record machine.CheckpointRecord) ([]byte, error) {
	var sink bytes.Buffer
	if err := gob.NewEncoder(&sink).Encode(record); err != nil {
		return nil, fmt.Errorf("recovery: encoding the record for datum %q: %w", record.Datum, err)
	}

	return sink.Bytes(), nil
}

// decodeRecord rebuilds a record from replicated bytes.
//
// A record that will not decode is an ERROR rather than a skipped entry: silently
// dropping it would hide a datum from recovery, which is the same silence this
// package exists to prevent.
func decodeRecord(path string, data []byte) (machine.CheckpointRecord, error) {
	var record machine.CheckpointRecord
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&record); err != nil {
		return machine.CheckpointRecord{}, fmt.Errorf("recovery: decoding the record at %q: %w", path, err)
	}

	return record, nil
}

// Claim takes ownership of a datum and reports whether THIS NODE holds it.
//
// THE OWNER IS STAMPED HERE, from the ledger's own identity, and the caller does not
// supply one — the same reasoning New records for taking no identity parameter, and
// the same thing Checkpoint already does for a record's Owner. It is not a
// stylistic echo: the claimant is compared against the alive set, which live builds
// from the committed configuration's server ids, so a claim written under anything
// else is a claim in a namespace the comparison can never match. Passing an owner in
// is how this seam shipped a durability defect — every replica handed it one shared
// machine name, so the journal's first-writer-wins arm read every later claim as the
// winner retrying and admitted them all, while claimWithholds read every claimant as
// departed and retired the claim in the same scan.
//
// A LOST RACE IS NOT AN ERROR HERE. The journal's apply arm refuses every later
// claimant with its own sentinel, and that refusal means another survivor won —
// ordinary, expected, and reported as false rather than as a failure. Anything else
// is a real error and reaches the caller.
func (d *Detector) Claim(ctx context.Context, flow, datum string) (bool, error) {
	owner := d.ledger.LocalID()

	_, err := d.ledger.Append(ctx, ledger.Entry{
		Kind: ledger.KindClaim, Path: checkpoint.ClaimPath(datum), Value: []byte(owner),
	})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ledger.ErrClaimHeld):
		return false, nil
	default:
		return false, fmt.Errorf("recovery: claiming datum %q on flow %q for %q: %w", datum, flow, owner, err)
	}
}

// Retire drops a completed datum's checkpoint and its claim together.
//
// IT NAMES BOTH PATHS. The journal's retire arm deletes Entry.Path from the
// checkpoints and Entry.Value from the claims, because the two spaces are disjoint
// and the ledger cannot derive one from the other without importing this module's
// path vocabulary back through a cycle.
// Sending only the checkpoint path retires no claim at all.
func (d *Detector) Retire(ctx context.Context, flow, datum string) error {
	if _, err := d.ledger.Append(ctx, ledger.Entry{
		Kind: ledger.KindRetire, Path: checkpoint.Path(datum),
		Value: []byte(checkpoint.ClaimPath(datum)),
	}); err != nil {
		return fmt.Errorf("recovery: retiring datum %q on flow %q: %w", datum, flow, err)
	}

	return nil
}

// retireStrandedClaim drops the claim of a holder that has left the flow.
//
// NOBODY STEALS; THE LEADER RETIRES. Permitting a survivor to take a claim from its
// holder would contradict the first-writer-wins arm that makes single-writer-per-datum
// safe. Instead the leader — the one node with the authoritative membership view, and
// the only node this runs on — appends a retire-claim entry, and the survivor then
// claims through the ordinary compare-and-claim path against no incumbent.
func (d *Detector) retireStrandedClaim(ctx context.Context, flow, datum string) error {
	if _, err := d.ledger.Append(ctx, ledger.Entry{
		Kind: ledger.KindRetireClaim, Path: checkpoint.ClaimPath(datum),
	}); err != nil {
		return fmt.Errorf("recovery: retiring the stranded claim on datum %q of flow %q: %w", datum, flow, err)
	}

	return nil
}

// Orphans reports the datums whose owner is no longer in the flow's configuration
// and which nobody already holds.
//
// IT REFUSES ON A NON-LEADER RATHER THAN REPORTING NOTHING. The claim-state read it
// filters with is leader-local, so on a follower this cannot answer — and an empty
// slice with a nil error is indistinguishable from "no orphans" at every call site
// above it, which would leave recovery looking healthy while doing nothing.
//
// A REFUSED CURSOR IS NOT AN ERROR TO RETRY. The signal log refuses a cursor that
// fell off its retention window and one minted by a different incarnation with the
// same sentinel, and the defined response to both is one thing: rebuild the view from
// the manager's committed membership and resume from a zero cursor. Under ephemeral
// identity a reconnect to a different incarnation is the designed steady state.
func (d *Detector) Orphans(ctx context.Context, flow string) ([]machine.CheckpointRecord, error) {
	for {
		// LEADERSHIP IS RE-CHECKED EVERY ROUND rather than once on entry, because
		// this call parks between rounds and a term can end while it waits.
		if err := d.requireLeader(); err != nil {
			return nil, err
		}

		configuration, _, ok := d.manager.Membership(d.flow)
		if !ok {
			return nil, fmt.Errorf(
				"recovery: flow %q has no committed membership to read: %w, %w",
				flow, ledger.ErrNotLeader, machine.ErrNotLeader)
		}

		// SIGNALS ARE FOLDED BEFORE THE SCAN, never after. The configuration read
		// above is always current, while a signal sits unread until this detector
		// asks for it — so a round that scanned first would judge an absence it had
		// not yet been told the reason for. That is not a narrow race: a node that
		// has just won leadership starts with a zero cursor and scans before it has
		// consumed anything, which is exactly the failover path this lane exists for.
		if err := d.drain(ctx); err != nil {
			return nil, err
		}

		orphans, err := d.scan(ctx, flow, d.live(configuration))
		if err != nil {
			return nil, err
		}
		if len(orphans) > 0 {
			return orphans, nil
		}

		// NOTHING CLAIMABLE YET. Park on the membership stream rather than
		// spinning: the next configuration commit is what can turn a live owner
		// into a dead one.
		if err := d.await(ctx); err != nil {
			return nil, err
		}
	}
}

// scan reads the checkpoint space and keeps the datums whose owner is gone and
// which nobody already holds.
func (d *Detector) scan(
	ctx context.Context, flow string, alive map[string]struct{},
) ([]machine.CheckpointRecord, error) {
	entries, err := d.ledger.List(ctx, checkpoint.Path(""))
	if err != nil {
		return nil, fmt.Errorf("recovery: enumerating checkpoints on flow %q: %w", flow, err)
	}

	out := make([]machine.CheckpointRecord, 0, len(entries))
	for _, entry := range entries {
		datum := datumOf(entry.Path)

		record, err := decodeRecord(entry.Path, entry.Value)
		if err != nil {
			return nil, err
		}
		record.Flow = flow
		record.Datum = datum

		withheld, err := d.claimWithholds(ctx, flow, datum, alive)
		if err != nil {
			return nil, err
		}
		if withheld {
			continue
		}
		// THE WRITER decides orphanhood. A checkpoint whose writer is still in the
		// configuration is live work, not an orphan.
		if _, up := alive[record.Owner]; up {
			continue
		}
		out = append(out, record)
	}

	return out, nil
}

// claimWithholds reports whether a datum's claim keeps it out of this round, and
// retires the claim when its holder has left the flow.
//
// It is a method rather than an inline branch in scan only because scan measures past
// the package's per-function statement limit with it inlined; the decision it makes is
// scan's own and the ordering below is load bearing.
func (d *Detector) claimWithholds(
	ctx context.Context, flow, datum string, alive map[string]struct{},
) (bool, error) {
	// The claim read goes through the CLAIM ACCESSOR, which is the only thing that can
	// see a held claim at all: written against the value read it observes nothing and
	// drops nothing, which is the inertness the audit reproduced.
	claimant, held, err := d.ledger.Claimant(ctx, checkpoint.ClaimPath(datum))
	if err != nil {
		return false, fmt.Errorf("recovery: reading claim state for datum %q on flow %q: %w",
			datum, flow, err)
	}
	if !held {
		return false, nil
	}
	// A CLAIM HELD BY A LIVE MEMBER is work a survivor has already taken, so it is not
	// offered again.
	if _, up := alive[claimant]; up {
		return true, nil
	}

	// A claim held by a member that has LEFT is not a reason to withhold anything: it is
	// a dead worker's own claim, and the datum under it is exactly what recovery is for.
	// THE HOLDER IS GONE, so the claim is STRANDED: first-writer-wins would refuse every
	// survivor forever. Retire it HERE, before the datum is offered — a datum offered
	// while its dead holder's claim still stands is a datum every survivor loses the
	// race for.
	return false, d.retireStrandedClaim(ctx, flow, datum)
}

// drain folds every signal already retained past the cursor WITHOUT parking.
//
// IT NEEDS NO SECOND METHOD ON THE MEMBERSHIP SEAM, and that is worth stating because
// the obvious reading is that a non-blocking read is missing from it. Watch selects on
// its context ONLY when it has nothing to give, so handed an already-expired one it
// returns any retained batch immediately and otherwise reports the context at once.
// An expired context is therefore "tell me what you have", and the cancellation it
// reports back is this method's own rather than a failure.
func (d *Detector) drain(ctx context.Context) error {
	// THE CALLER'S CONTEXT IS CHECKED FIRST so the cancellation below is
	// unambiguously ours: with a live caller context, a Canceled from Watch can only
	// have come from the cancel on the next line.
	if err := ctx.Err(); err != nil {
		return err
	}

	d.mu.Lock()
	since := d.cursor
	d.mu.Unlock()

	expired, cancel := context.WithCancel(ctx)
	cancel()

	signals, cursor, err := d.manager.Watch(expired, since)
	switch {
	case err == nil:
		d.noteHealth(signals)
		d.setCursor(cursor)

		return nil
	case errors.Is(err, membership.ErrCursorTooOld):
		d.resetView()

		return nil
	case errors.Is(err, context.Canceled):
		// Nothing retained past the cursor. Not a failure.
		return nil
	}

	return fmt.Errorf("recovery: draining membership signals for flow %q: %w", d.flow, err)
}

// await parks until the flow's membership moves.
//
// A REFUSED CURSOR IS NOT AN ERROR TO RETRY. The signal log refuses a cursor that
// fell off its retention window and one minted by a different incarnation with the
// SAME sentinel, and the defined response to both is one thing: reset to a zero
// cursor and rebuild the view from the committed membership on the next round.
// Under ephemeral identity a reconnect to a different incarnation is the designed
// steady state, not an exception.
func (d *Detector) await(ctx context.Context) error {
	// THE LOCK IS NOT HELD ACROSS Watch. Watch parks until the membership moves, so
	// a detector holding the lock across it would block every other worker's round
	// behind one park rather than merely serializing a field read.
	d.mu.Lock()
	since := d.cursor
	d.mu.Unlock()

	signals, cursor, err := d.manager.Watch(ctx, since)
	if err == nil {
		d.noteHealth(signals)
		d.setCursor(cursor)

		return nil
	}
	if errors.Is(err, membership.ErrCursorTooOld) {
		d.resetView()

		return nil
	}

	return fmt.Errorf("recovery: reading membership signals for flow %q: %w", d.flow, err)
}

// setCursor records the position the next round reads from.
func (d *Detector) setCursor(cursor uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cursor = cursor
}

// noteHealth folds one batch of membership signals into the leader's health view.
//
// THIS IS THE BATCH await USED TO DISCARD. The leader publishes a peer's health
// transitions where it leads, and recovery is leader-only, so the fact and the
// consumer are on the same node.
//
// ONLY THE TWO HEALTH KINDS CARRY HEALTH. SignalMembershipChanged names the
// PUBLISHING node rather than a peer, by design, so reading its Node as a peer
// would have the leader mark ITSELF unreachable. SignalPeerEvicted is deliberately
// NOT terminal for an owner: an evicted member leaves the committed configuration
// and the configuration arm decides it from there, and a member evicted while LIVE
// re-announces and is readmitted, at which point it must read alive again.
func (d *Detector) noteHealth(signals []membership.Signal) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, signal := range signals {
		if signal.Flow != d.flow {
			continue
		}
		switch signal.Kind {
		case membership.SignalPeerUnreachable:
			d.unreachable[signal.Node] = struct{}{}
		case membership.SignalPeerReturned:
			delete(d.unreachable, signal.Node)
		case membership.SignalPeerEvicted:
			// AN EVICTION OF A REACHABLE MEMBER IS A SUSPENSION, NOT A DEATH. An
			// eviction of an UNREACHABLE one is already decided by the health arm
			// and needs nothing here, and a graceful SetFlows departure emits no
			// signal at all, so both stay immediately gone — which is the failure
			// the absence arm exists to prevent.
			//
			// THE DEADLINE ARRIVES ON THE SIGNAL, never computed from a window this
			// package holds. Duplicating a duration across packages is the defect
			// the debounce alternative was rejected for, and this quantity is not
			// the stabilization time anyway: it is when a readmission stops being
			// in flight, which the re-place round's cadence governs.
			//
			// A ZERO DEADLINE IS ALREADY PAST, so an eviction that carries none
			// suspends nothing and the absence arm decides it — the safe direction.
			if signal.EvictedWhileReachable {
				d.suspended[signal.Node] = signal.ReadmissionExpectedBy
			}
		case membership.SignalMembershipChanged:
		}
	}
}

// resetView drops what a refused cursor invalidates.
//
// THE HEALTH VIEW GOES WITH THE CURSOR. A refused cursor means signals were
// dropped between this reader and the log, so an accumulated view built from a
// prefix of the stream is not trustworthy — and the honest response is to rebuild
// it from the stream the next Watch returns. Losing it errs toward NOT offering a
// datum, which is the safe direction: a datum offered late is recovered late, a
// datum offered wrongly is executed twice.
func (d *Detector) resetView() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cursor = 0
	d.unreachable = map[string]struct{}{}
	d.suspended = map[string]time.Time{}
}

// AwaitLeadership parks until this node leads the flow.
//
// IT IS THE OTHER HALF OF THE LOUD REFUSAL. Orphans still refuses on a non-leader
// rather than reporting an empty set, and this is what the caller does with that
// refusal: a node that does not lead has nothing to detect until it does, so the
// resume loop waits here instead of exiting for the flow's lifetime.
func (d *Detector) AwaitLeadership(ctx context.Context, flow string) error {
	ticker := time.NewTicker(leadershipPollInterval)
	defer ticker.Stop()

	for {
		if d.requireLeader() == nil {
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("recovery: awaiting leadership of flow %q: %w", flow, ctx.Err())
		}
	}
}

// live reports the members a detection round treats as alive: the committed
// configuration MINUS the members the leader's health view marks unreachable.
//
// AN OWNER IS DEAD ON EITHER ARM. Absent from the configuration, it is gone and
// its checkpoints are orphans, which is what this package has always done. Present
// in the configuration and marked unreachable, it is dead too — and that arm is
// what makes recovery independent of eviction, which knows nothing about datums
// and refuses under a bound that has no timeout.
//
// THE HEALTH SET IS PRUNED TO THE CONFIGURATION HERE, and that one line carries
// two obligations at once. It BOUNDS the set at the size of the configuration,
// where an unpruned one would accumulate an entry per departed node forever under
// ephemeral identity. And it is what makes an eviction NON-TERMINAL: a member
// evicted while live is pruned out while it is absent, so when it re-announces and
// is readmitted it reads alive again rather than staying dead on a stale entry.
func (d *Detector) live(configuration raft.Configuration) map[string]struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()

	configured := make(map[string]struct{}, len(configuration.Servers))
	for _, server := range configuration.Servers {
		configured[string(server.ID)] = struct{}{}
	}
	for id := range d.unreachable {
		if _, ok := configured[id]; !ok {
			delete(d.unreachable, id)
		}
	}

	out := make(map[string]struct{}, len(configured))
	for id := range configured {
		if _, down := d.unreachable[id]; down {
			continue
		}
		out[id] = struct{}{}
	}

	// A SUSPENDED OWNER READS ALIVE WHILE IT IS ABSENT, and leaves the set on
	// exactly two events: READMISSION, which is lane D's re-place round returning
	// it under the same id, and EXPIRY, after which no readmission is coming and
	// the absence arm decides it. Both clears also bound the map.
	now := time.Now()
	for id, until := range d.suspended {
		if _, readmitted := configured[id]; readmitted || !now.Before(until) {
			delete(d.suspended, id)

			continue
		}
		out[id] = struct{}{}
	}

	return out
}

// requireLeader refuses a detection round on a node that does not lead the flow.
func (d *Detector) requireLeader() error {
	if d.ledger.Raft().State() == raft.Leader {
		return nil
	}

	// IT WRAPS BOTH SENTINELS, and that is the seam rather than belt-and-braces.
	// The ledger's is what this module's own callers key on; the root module's is
	// the only one the machine can name, because it may not import this package.
	return fmt.Errorf(
		"recovery: detection on flow %q runs on the leader, and this node does not lead it: %w, %w",
		d.flow, ledger.ErrNotLeader, machine.ErrNotLeader)
}

// datumOf strips the checkpoint prefix from a journaled path.
func datumOf(path string) string {
	prefix := checkpoint.Path("")

	if len(path) >= len(prefix) && path[:len(prefix)] == prefix {
		return path[len(prefix):]
	}

	return path
}
