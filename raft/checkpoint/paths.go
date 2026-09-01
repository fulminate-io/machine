// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// Package checkpoint writes a datum's progress into a flow's replicated journal,
// pipelined so that many appends are in flight for one group at once.
//
// IT DECLARES NO RECORD TYPE OF ITS OWN, and that is deliberate rather than an
// omission. The record a checkpoint carries is the root module's, and this package
// takes the flow, the datum and the already-marshaled bytes as plain parameters. A
// second record type here, identical in content and converted at the seam, would be
// two shapes for one thing, each free to drift from the other.
//
// IT NEVER DECODES THOSE BYTES EITHER. The journal's entry vocabulary keeps values
// opaque so that decoding happens at the READING node, where an unregistered type
// fails one reader loudly instead of poisoning replicated state across peers.
package checkpoint

// The two path spaces a datum occupies in the journal. They are DISJOINT BY
// CONSTRUCTION, which is what lets a recovery pass enumerate every checkpoint
// without also enumerating the claims: an enumeration under one prefix cannot reach
// the other.
const (
	checkpointPrefix = "checkpoint/"
	claimPrefix      = "claim/"
)

// Path is where a datum's progress is journaled.
//
// THE DATUM ID IS OPAQUE HERE. It is whatever the packet reported as its own id, and
// nothing in this package parses, validates or normalizes it — a path is a prefix
// and an id, so an id containing anything at all still lands in its own space rather
// than in the other one.
//
// It is EXPORTED because its consumers are in other packages: the recovery detector
// enumerates this space to find orphans, and the root module's journal seam names it
// too. An unexported spelling would be unreachable from either.
func Path(datum string) string { return checkpointPrefix + datum }

// ClaimPath is where a datum's recovery ownership is journaled, on the same terms.
//
// It is the path a claim entry is keyed by, which is what a recovery pass reads back
// through the journal's claim accessor to decide whether a datum is already owned.
func ClaimPath(datum string) string { return claimPrefix + datum }
