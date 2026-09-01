// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package membership

import (
	"context"
	"fmt"
	"net"
	"sort"

	"github.com/whitaker-io/machine/raft/ledger"
)

// OpenFunc opens one flow's ledger.
//
// IT IS A FUNCTION RATHER THAN A ledger.Config so this package does not restate
// the ledger's configuration surface. A caller that already knows how it wants
// its ledgers built passes that knowledge in one place.
type OpenFunc func(flow string) (*ledger.Ledger, error)

// resolvePeers turns the one configured address into every instance behind it.
//
// ONE ADDRESS, RESOLVED, IS THE WHOLE DISCOVERY CONTRACT. A headless Service
// resolves to every ready pod's IP; a service-discovery name and a DNS
// round-robin over plain VMs behave the same way. Nothing in this package knows
// what a pod is, which is what keeps the design out of any one deployment
// environment — the operator supplies an address that reaches the other
// instances and how it comes to reach them is theirs to decide.
func resolvePeers(ctx context.Context, peers string) ([]string, error) {
	host, port, err := net.SplitHostPort(peers)
	if err != nil {
		return nil, fmt.Errorf("membership: Config.Peers %q is not host:port: %w", peers, err)
	}
	found, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("membership: resolving Config.Peers %q failed: %w", peers, err)
	}
	out := make([]string, 0, len(found))
	for _, addr := range found {
		out = append(out, net.JoinHostPort(addr, port))
	}
	// Sorted so the lowest-id rule and the logs read the same on every instance.
	sort.Strings(out)
	return out, nil
}
