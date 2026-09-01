// Package machine - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package machine

import "context"

// Anchor names WHEN a checkpoint was journaled relative to the node function that
// produced it. It rides the record rather than being re-derived at resume time.
//
// IT IS A STRING AND NOT A bool, for the reason the grammar gives about the
// checkpoint clause being a position rather than a flag: the reader wants to know
// WHICH anchor, and a bool throws that away the moment a third anchor is
// contemplated. A resuming worker must not have to infer the anchor from a marker
// that may differ between the build that WROTE the record and the build READING it.
const (
	// AnchorArrival marks a record journaled BEFORE the node function ran. It holds
	// the node's INPUT, and resume hands it back to that same node, which runs
	// again — safe precisely because the author declared the node idempotent.
	AnchorArrival = "arrival"
	// AnchorCompletion marks a record journaled AFTER the node function returned. It
	// holds the node's OUTPUT, and resume re-injects it into the node's SUCCESSORS
	// without re-running the node, which is what keeps a non-idempotent node's side
	// effects from happening twice.
	AnchorCompletion = "completion"
)

// CheckpointRecord is one datum's journaled progress.
//
// EVERY FIELD IS A string OR []byte, and that is forced rather than stylistic. The
// root module's structural gate strips an allowlist of declarations and fails on any
// surviving bare `any` or type assertion, so a seam carrying an erased payload would
// red it. The payload therefore crosses this seam as bytes the node's own codec
// marshaled, which also keeps decoding at the READING node — where an unregistered
// type fails one reader loudly instead of poisoning replicated state.
type CheckpointRecord struct {
	// Flow names the flow whose journal holds this record.
	Flow string
	// Datum is the packet id this record describes.
	Datum string
	// Owner is the worker that claimed the datum, empty on a record nobody holds.
	Owner string
	// Node names the node whose progress this is. Resume keeps only the records
	// naming the node it is resuming.
	Node string
	// Anchor is AnchorArrival or AnchorCompletion — which side of the node function
	// this record was written on, and therefore where resume must hand it.
	Anchor string
	// Data is the marshaled packet, opaque here and decoded by the node's codec.
	Data []byte
}

// Journal is the durable progress store a machine checkpoints into.
//
// THE ROOT MODULE DECLARES IT AND DOES NOT IMPLEMENT IT. The replicated
// implementation lives in the raft module, which depends on this one; declaring the
// interface here and implementing it there is the same direction the heap Store
// already runs, and it is what keeps this module's dependencies unchanged.
type Journal interface {
	// Checkpoint journals one record. It does not await durability: the
	// implementation submits into its own bounded window, because awaiting each
	// datum in turn is what caps a flow at one checkpoint per disk sync.
	Checkpoint(ctx context.Context, record CheckpointRecord) error
	// Claim takes ownership of a datum for recovery and reports whether THIS owner
	// holds it. A retry by the winner returns true rather than false, so a claim
	// replayed across a leadership change does not deny a worker the datum it
	// already owns.
	Claim(ctx context.Context, flow, datum, owner string) (bool, error)
	// Retire drops a completed datum's record and its claim together. Retiring a
	// datum that was never checkpointed is a no-op rather than an error, because
	// completion fires for every datum whether or not its flow declared a
	// checkpoint.
	Retire(ctx context.Context, flow, datum string) error
	// Orphans BLOCKS until at least one claimable record exists or the context
	// ends, mirroring the membership watch it is driven beside. The cursor it reads
	// from lives on the implementation's side; this module holds none.
	Orphans(ctx context.Context, flow string) ([]CheckpointRecord, error)
}

// OptionJournal registers the journal a machine checkpoints into.
//
// IT IS AN OPTION AND NEVER A Machine METHOD. The Machine's exported method set is
// deliberately closed, and adding a public accessor there is a gate failure rather
// than a review miss — so the journal is wired the way every other machine-wide
// dependency is, through the option that writes the config.
func OptionJournal(j Journal) Option {
	return func(c *config) { c.journal = j }
}
