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

// Detector turns a flow's membership into the datums whose worker is gone.
type Detector struct {
	ledger  *ledger.Ledger
	manager Membership
	flow    string
	cursor  uint64
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
	return &Detector{ledger: l, manager: manager, flow: flow}
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

// Claim takes ownership of a datum and reports whether THIS owner holds it.
//
// A LOST RACE IS NOT AN ERROR HERE. The journal's apply arm refuses every later
// claimant with its own sentinel, and that refusal means another survivor won —
// ordinary, expected, and reported as false rather than as a failure. Anything else
// is a real error and reaches the caller.
func (d *Detector) Claim(ctx context.Context, flow, datum, owner string) (bool, error) {
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
			return nil, fmt.Errorf("recovery: flow %q has no committed membership to read: %w",
				flow, ledger.ErrNotLeader)
		}

		orphans, err := d.scan(ctx, flow, live(configuration))
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

// await parks until the flow's membership moves.
//
// A REFUSED CURSOR IS NOT AN ERROR TO RETRY. The signal log refuses a cursor that
// fell off its retention window and one minted by a different incarnation with the
// SAME sentinel, and the defined response to both is one thing: reset to a zero
// cursor and rebuild the view from the committed membership on the next round.
// Under ephemeral identity a reconnect to a different incarnation is the designed
// steady state, not an exception.
func (d *Detector) await(ctx context.Context) error {
	_, cursor, err := d.manager.Watch(ctx, d.cursor)
	if err == nil {
		d.cursor = cursor

		return nil
	}
	if errors.Is(err, membership.ErrCursorTooOld) {
		d.cursor = 0

		return nil
	}

	return fmt.Errorf("recovery: reading membership signals for flow %q: %w", d.flow, err)
}

// live reports the server ids in a committed configuration.
func live(configuration raft.Configuration) map[string]struct{} {
	out := make(map[string]struct{}, len(configuration.Servers))
	for _, server := range configuration.Servers {
		out[string(server.ID)] = struct{}{}
	}

	return out
}

// requireLeader refuses a detection round on a node that does not lead the flow.
func (d *Detector) requireLeader() error {
	if d.ledger.Raft().State() == raft.Leader {
		return nil
	}

	return fmt.Errorf(
		"recovery: detection on flow %q runs on the leader, and this node does not lead it: %w",
		d.flow, ledger.ErrNotLeader)
}

// datumOf strips the checkpoint prefix from a journaled path.
func datumOf(path string) string {
	prefix := checkpoint.Path("")

	if len(path) >= len(prefix) && path[:len(prefix)] == prefix {
		return path[len(prefix):]
	}

	return path
}
