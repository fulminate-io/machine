// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"context"
	"strings"
	"testing"
)

// TestAnAnnounceCrossingDeploymentGenerationsIsRefusedByName drives the acceptor
// in all three directions and over the real wire.
//
// THE EQUAL LEG IS THE CONTROL THAT MAKES THE OTHER TWO MEAN SOMETHING: an
// acceptor that refused every announce would satisfy both refusal legs on its
// own.
func TestAnAnnounceCrossingDeploymentGenerationsIsRefusedByName(t *testing.T) {
	leader := newClusterNode(t, "a-leader", []string{"alpha"}, 0)
	leader.start(t)
	leader.awaitLeader(t, "alpha")

	refusal := func(t *testing.T, mine, theirs uint64) string {
		t.Helper()
		leader.mgr.cfg.Generation = mine
		reply := leader.mgr.answerAnnounce(announce{
			Node: "b-joiner", Address: unreachableAddress(t),
			Flows: []string{"alpha"}, Generation: theirs,
		})
		if reply.Generation != mine {
			t.Fatalf("the reply carries generation %d, want the answering node's %d: an announcer "+
				"cannot tell a foreign answer from its own without it", reply.Generation, mine)
		}
		reason, refused := reply.Refused["alpha"]
		if !refused {
			t.Fatalf("an announce at generation %d was NOT refused by a node at generation %d: "+
				"staged=%v", theirs, mine, reply.Staged)
		}
		return reason
	}

	// THE RULED DIRECTION: a dying older group must not admit its successor.
	older := refusal(t, 5, 9)
	if !strings.Contains(older, "newer generation 9") || !strings.Contains(older, "generation 5") {
		t.Fatalf("the refusal %q does not name both generations and the direction", older)
	}
	// THE OTHER DIRECTION, decided here rather than left open: admitting a member
	// that is being terminated puts a doomed identity in the configuration.
	newer := refusal(t, 9, 5)
	if !strings.Contains(newer, "older generation 5") || !strings.Contains(newer, "generation 9") {
		t.Fatalf("the refusal %q does not name both generations and the direction", newer)
	}

	// THE PROPERTY PAIR'S OTHER HALF: equal admits.
	leader.mgr.cfg.Generation = testGeneration
	ghost := unreachableAddress(t)
	same := leader.mgr.answerAnnounce(announce{
		Node: "b-equal", Address: ghost, Flows: []string{"alpha"}, Generation: testGeneration,
	})
	if len(same.Staged) != 1 || same.Staged[0] != "alpha" {
		t.Fatalf("CONTROL FAILED: an announce at the SAME generation was not staged: "+
			"staged=%v refused=%v", same.Staged, same.Refused)
	}
	t.Log("equal generations admit: the refusals above are about the generation, not about an acceptor " +
		"that refuses everything")

	// THE FIELD ROUND-TRIPS ON THE REAL WIRE rather than through a direct call,
	// because a field the encoder drops would still pass every leg above.
	asker := newClusterNode(t, "c-asker", nil, 0)
	asker.mgr.cfg.Generation = 9
	asker.mgr.peers.setAddresses([]string{leader.addr})
	reply, err := asker.mgr.announceTo(context.Background(), leader.addr, "alpha")
	if err != nil {
		t.Fatalf("announceTo over the mux: %v", err)
	}
	if reply.Generation != testGeneration {
		t.Fatalf("the reply came back carrying generation %d, want %d: the field did not survive the wire",
			reply.Generation, testGeneration)
	}
	if _, refused := reply.Refused["alpha"]; !refused {
		t.Fatalf("an announce sent over the wire at generation 9 was admitted by a node at %d: "+
			"the announcer's generation did not survive the wire; reply=%+v", testGeneration, reply)
	}
	t.Logf("both generations survived the wire: request %d, reply %d", 9, reply.Generation)
}
