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
// THE FIFTH FLAG IS -pkgpath, AND ITS DEFAULT IS EMPTY ON PURPOSE. An empty value
// selects the derivation — the import path of the output directory, read from the
// nearest enclosing go.mod — so the ordinary invocation above needs no new
// knowledge. Pass it only for the case the derivation cannot serve: generating
// into a directory whose enclosing module is not the one the generated code will
// be compiled in.
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
// WHAT REFUSES AND WHAT IS ONLY REPORTED. Before anything is written, flowc runs
// every analyzer the language has — the twelve registered ones plus type
// inference and the serialization derivation, which are constructed because they
// need a loaded package set. A finding at analysis.SeverityError REFUSES the run:
// nothing is written and the exit status is non-zero. Everything below that line
// is printed to STDERR and the run CONTINUES, because the language calls a
// warning "suspicious but not provably wrong" and a hint "an observation an
// author may reasonably ignore" — refusing on those would refuse legal programs,
// and printing nothing would hide findings the analyzers already paid to compute.
// So a run that printed findings and still wrote files is behaving as designed.
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
	pkgPath := flag.String("pkgpath", "",
		"import path of the generated package; derived from the output directory when empty")
	flag.Parse()

	if *qualifier == "" {
		_, _ = fmt.Fprintln(os.Stderr, "flowc: -qualifier is required: it prefixes every process-global state "+
			"handle name, and two programs sharing a prefix collide in the runtime's single namespace")
		os.Exit(2)
	}

	driver := &assembler.Driver{
		Config:      assembler.Config{Package: *pkg, Qualifier: *qualifier},
		PackagePath: *pkgPath,
		Disclose:    func(diags []assembler.Diagnostic) { disclose(*in, diags) },
	}
	if err := driver.Generate(*in, *out); err != nil {
		report(*in, err)
		os.Exit(1)
	}
}

// disclose prints the analysis findings that do NOT refuse the run.
//
// THEY GO TO STDERR AND THE RUN CONTINUES. They are rendered through the same
// assembler.Render every refusal goes through, so a warning and a refusal read
// identically and the exit status is what distinguishes them — an author reading
// one line has no reason to learn a second format.
func disclose(dir string, diags []assembler.Diagnostic) {
	for _, d := range diags {
		_, _ = fmt.Fprintln(os.Stderr, assembler.Render(dir, d))
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
