// Package analysis - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package analysis

// The module's analyzers are registered here, in one place and in dependency
// order, rather than from an init function in each analyzer's own file.
//
// Two reasons. All() reports registration order, and a per-file init would make
// that order the compiler's file ordering — deterministic, but arbitrary and
// invisible at the call site. And the set a consumer gets from All() is a fact
// about this module worth reading in one place: an analyzer that exists but was
// never registered is unreachable through the public seam, and that is far
// easier to notice as a missing line here than as a missing init elsewhere.
func init() {
	Register(SymbolsAnalyzer)
	Register(FlowgraphAnalyzer)
	Register(ResolveAnalyzer)
	Register(ReachabilityAnalyzer)
	Register(CyclesAnalyzer)
	Register(SignatureAnalyzer)
	Register(StateAnalyzer)
	Register(SwitchesAnalyzer)
	Register(ErrorRoutingAnalyzer)
	Register(TypeflowAnalyzer)
	Register(GuidanceAnalyzer)
	Register(CheckpointAnchorAnalyzer)
}
