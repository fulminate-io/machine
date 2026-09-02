// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// Package assembler turns parsed .flow sources into generated Go that builds and
// runs against the machine runtime.
//
// # What it consumes
//
// One or more parsed ast.File values, plus FACTS the caller gathered: the
// type-checked package set from lang/loader, the per-flow bindable-output
// boundary from lang/analysis, and that package's inferred types. It derives none
// of those itself. Package loading is owner-exclusive to lang/loader, and which
// outputs a flow exposes has ONE derivation, in lang/analysis; this package
// resolves, lowers and emits against those answers. Where its own graph disagrees
// with the exported boundary it refuses loudly, naming both views, rather than
// preferring either — two implementations of one rule drift apart invisibly,
// changing no build result and no runtime behavior.
//
// # What it emits
//
// One Go file per .flow source, named <source>.flow.go, carrying:
//
//   - a generated-code marker on line 1 and a source stamp on line 2;
//   - a package doc stating the host journal contract;
//   - type-carrying option helpers;
//   - the source's own funcs, imports, consts and params, pasted verbatim;
//   - synthesized deep-copy functions and tee duplicators;
//   - process-global state handle declarations;
//   - one Wire<Flow>(m *machine.Machine) error per flow.
//
// GENERATED CODE IS A BUILD ARTIFACT. Users commit .flow sources; the Go here is
// regenerated whole on every run and is never committed. Regeneration removes the
// previous output first, so a deleted .flow leaves no orphan behind.
//
// # Nothing degrades silently
//
// Every construct the runtime cannot express is REFUSED with a positioned
// diagnostic naming the .flow line that wrote it. The refusal set is closed, and
// each member is refused for a stated reason:
//
//   - A LOOP LABEL NOTHING SENDS TO. The loop the author wrote does not exist:
//     nothing would ever feed the statement consuming the label, and the flow
//     reads as if it cycles.
//   - A DOTTED `use` REFERENCE. It names a flow in another module, and resolving
//     one is lang/loader's surface. Inventing a second resolution path here is how
//     two answers to one question appear.
//   - A RECURSIVE `use` CHAIN. Inlining substitutes a referenced flow's statements
//     into its caller, so a cycle is a generator that does not terminate. It is
//     refused with the cycle named rather than discovered as a hang.
//   - AN OUTPUT NOTHING CONSUMES. The runtime already refuses this at Start;
//     refusing at generation reports the same fact against the line that wrote it
//     instead of against a node name at run time.
//   - A DUPLICATE NODE NAME. Node names are unique within a flow, and the runtime
//     records a duplicate as a declaration error later than it needs to be.
//   - AN UNKNOWN NAME in a from, send or drop reference.
//   - A STATEMENT THE PARSER COULD NOT READ. It reaches the graph as a recovered
//     placeholder, and assembling around it would generate a program missing
//     whatever the author meant.
//   - A FUNC COLLIDING WITH A PREAMBLE HELPER NAME. The emitted helpers are
//     declared in every generated file, so a source-declared func under either
//     name produces Go that does not compile; refusing names the .flow line rather
//     than leaving a redeclaration error against a generated one.
//
// Two further refusals belong to type resolution rather than to the graph. A
// checkpoint operand whose codec FAMILY cannot be named is refused, because
// re-instantiating it at the successor's type means naming a generic head and
// swapping a written type argument, and only two written forms carry one. And a
// tee or a non-trivially-copyable var with no type information available is
// refused, because the only alternative is a shallow copy that leaves two
// branches sharing backing memory with nothing said about it.
//
// # Derived names
//
// Some lowerings need more nodes than the source wrote: an N-arm switch becomes a
// chain of N runtime If calls, an N-target tee becomes N-1 chained Tee calls, and
// a sink becomes a Map followed by a Drop of its drain. The extra nodes are named
// from the source-written name plus a suffix, separated by "#".
//
// THAT SEPARATOR IS THE WHOLE GUARANTEE, and it is chosen because the grammar
// admits it in no identifier: an author CANNOT write a name the generator derives,
// so a derived name can never collide with one in the source. The property is
// checked by handing the parser a flow that tries and requiring it to refuse.
//
// # State handle names
//
// The runtime keeps every key and cell in ONE process-global namespace and panics
// on a duplicate at declaration. Two generated flows in one binary that both
// declare a var called `attempt` would therefore kill the process at startup.
//
// So an emitted handle name carries a caller-supplied qualifier and the flow's
// own name: <qualifier>.<flow>.<var>. A subflow inlined under an instance carries
// the instance too, <qualifier>.<flow>.<instance>.<var>, which is what gives two
// instances of one subflow independent state rather than a collision.
//
// # The host owns the journal
//
// A flow containing a checkpoint clause needs a recovery journal, and generated
// code does not build one: the host constructs its Machine and passes
// machine.OptionJournal. Generated code can ask WHETHER a journal is wired and
// deliberately cannot reach the journal itself, because a generated file must not
// learn a deployment's replication configuration.
//
// Every Wire function returns error uniformly, whether or not its flow
// checkpoints. That is one contract rather than two, so a host's call site never
// depends on a property of the .flow source it cannot see and a regeneration
// cannot silently change how it is called. For a flow that does checkpoint, Wire
// checks for a journal BEFORE declaring any node and returns an error naming the
// flow and the option, leaving the machine untouched.
//
// The RUNTIME's own refusal is a different one and arrives later: a checkpointed
// node declared on a journal-less machine records a declaration error that Start
// returns, and Wire returns nothing on the runtime's behalf.
package assembler
