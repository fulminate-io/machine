// Command flowc generates Go from .flow sources.
//
// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
//
// USAGE
//
//	flowc -in ./flows -out ./flows -package generated -qualifier acme
//
// It discovers .flow files by extension, the way the Go toolchain discovers .go
// files: nothing marks a directory as flow-bearing, because a marker is one more
// thing to forget and its absence would silently generate nothing.
//
// GENERATED CODE IS A BUILD ARTIFACT. Commit .flow sources, not the .go this
// writes; the repository's .gitignore excludes the pattern. Regeneration is
// always WHOLE — every previously generated file is removed before the new set is
// put in place — so deleting a .flow leaves no orphaned .go behind to go on
// compiling forever.
//
// NOTHING IS WRITTEN UNTIL EVERYTHING EMITS. Files are staged and renamed into
// place only after the whole run succeeds, so a failure partway through cannot
// leave a directory that is neither the old program nor the new one.
//
// THE HOST OWNS THE JOURNAL. A flow containing a checkpoint clause needs a
// recovery journal, and generated code does not build one: the host constructs
// its Machine and passes machine.OptionJournal. Every generated Wire function
// returns error uniformly, whether or not its flow checkpoints, so a call site
// never depends on a property of the .flow source it cannot see. For a flow that
// DOES checkpoint, Wire checks for a journal BEFORE declaring any node and
// returns an error naming the flow and the option, leaving the machine untouched.
// The RUNTIME's own refusal is a different one and arrives later: a checkpointed
// node declared on a journal-less machine records a declaration error that Start
// returns, and Wire returns nothing on the runtime's behalf.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/whitaker-io/machine/lang/assembler"
)

func main() {
	in := flag.String("in", ".", "directory holding the .flow sources")
	out := flag.String("out", ".", "directory the generated Go is written to")
	pkg := flag.String("package", "generated", "package clause for the generated files")
	qualifier := flag.String("qualifier", "", "prefix for process-global state handle names")
	flag.Parse()

	if *qualifier == "" {
		_, _ = fmt.Fprintln(os.Stderr, "flowc: -qualifier is required: it prefixes every process-global state "+
			"handle name, and two programs sharing a prefix collide in the runtime's single namespace")
		os.Exit(2)
	}

	driver := &assembler.Driver{
		Config: assembler.Config{Package: *pkg, Qualifier: *qualifier},
	}
	if err := driver.Generate(*in, *out); err != nil {
		report(*in, err)
		os.Exit(1)
	}
}

// report renders a failure, expanding an *assembler.Error into one line per
// diagnostic.
//
// EVERY PROBLEM IS PRINTED, not just the first. An author fixing one diagnostic
// per run is paying for the tool's convenience with their own time.
func report(dir string, err error) {
	var assembly *assembler.Error
	if !asAssemblyError(err, &assembly) {
		_, _ = fmt.Fprintf(os.Stderr, "flowc: %v\n", err)

		return
	}
	for _, d := range assembly.Diagnostics {
		_, _ = fmt.Fprintln(os.Stderr, assembler.Render(dir, d))
	}
}

// asAssemblyError unwraps an assembly error without pulling errors.As's
// reflection into the reporting path's readability.
func asAssemblyError(err error, target **assembler.Error) bool {
	assembly, ok := err.(*assembler.Error)
	if ok {
		*target = assembly
	}

	return ok
}
