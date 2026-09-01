// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package checkpoint

import (
	"strings"
	"testing"
)

func TestTheCheckpointAndClaimSpacesAreDisjoint(t *testing.T) {
	// THE DISJOINTNESS IS WHAT LETS RECOVERY ENUMERATE CHECKPOINTS WITHOUT CLAIMS.
	// Were one prefix reachable from the other, an enumeration would return claims
	// among the checkpoints and each would be read as a datum's own progress.
	datums := []string{"datum-1", "", "checkpoint/looks-like-a-prefix", "claim/looks-like-the-other", "a/b/c", "  spaced  "}

	for _, datum := range datums {
		checkpoint, claim := Path(datum), ClaimPath(datum)

		if checkpoint == claim {
			t.Fatalf("datum %q maps to the same path %q in both spaces", datum, checkpoint)
		}
		if strings.HasPrefix(checkpoint, claimPrefix) {
			t.Fatalf("datum %q checkpoints to %q, which is inside the CLAIM space", datum, checkpoint)
		}
		if strings.HasPrefix(claim, checkpointPrefix) {
			t.Fatalf("datum %q claims at %q, which is inside the CHECKPOINT space", datum, claim)
		}
	}

	// CONTROL: an enumeration of one prefix must actually MATCH its own members, or
	// the exclusions above would be satisfied by paths in neither space.
	if !strings.HasPrefix(Path("datum-1"), checkpointPrefix) {
		t.Fatal("CONTROL FAILED: a checkpoint path does not carry the checkpoint prefix, so no enumeration would find it at all")
	}
	if !strings.HasPrefix(ClaimPath("datum-1"), claimPrefix) {
		t.Fatal("CONTROL FAILED: a claim path does not carry the claim prefix")
	}
}

func TestADatumIdIsOpaqueToThePathSpaces(t *testing.T) {
	// Nothing here parses, validates or normalizes an id: it is whatever the packet
	// reported as its own. An id carrying a slash, a space or the other space's own
	// prefix still lands in the space its function names.
	for _, datum := range []string{"claim/evil", "../escape", "with space", "ünïcødé"} {
		if got, want := Path(datum), checkpointPrefix+datum; got != want {
			t.Fatalf("Path(%q) = %q, want %q; the id was rewritten rather than carried through", datum, got, want)
		}
		if got, want := ClaimPath(datum), claimPrefix+datum; got != want {
			t.Fatalf("ClaimPath(%q) = %q, want %q", datum, got, want)
		}
	}
}
