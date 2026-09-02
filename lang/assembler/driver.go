// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/whitaker-io/machine/lang/ast"
	"github.com/whitaker-io/machine/lang/loader"
)

// generatedSuffix is what a generated file is named with, and what whole
// regeneration removes.
const generatedSuffix = ".flow.go"

// generationConcurrency is the ceiling on CONCURRENT FILE EMISSIONS.
//
// ITS DIMENSION IS FILES BEING EMITTED AT ONCE, and its value is the machine's
// own parallelism rather than a hand-picked number: parse, graph, lower and emit
// are CPU-bound and purely local, so past GOMAXPROCS extra goroutines add
// scheduling and peak memory without adding throughput. A generation run over a
// large .flow directory must not spawn one goroutine per file.
func generationConcurrency() int {
	if n := runtime.GOMAXPROCS(0); n > 0 {
		return n
	}

	return 1
}

// Check is one pre-generation check the driver runs before writing anything.
//
// ITS TYPE IS DECLARED HERE AND NAMES NO ANALYSIS CONCEPT. That is the whole of
// the analysis seam and it is deliberately this small: the analysis API is
// unruled, and a driver hook shaped around it would freeze a design nobody has
// made. A check reports problems; the driver refuses on any of them.
type Check func(sources []Source) []Diagnostic

// Driver runs a whole generation.
type Driver struct {
	// Config is the generated package name and handle qualifier.
	Config Config
	// Checks run BEFORE any file is written. The list is empty in production
	// today; it is where analysis's own verdicts arrive.
	Checks []Check
	// Load loads the type-checked package set. It is a field so a test can count
	// the calls; production leaves it nil and the real loader is used.
	Load func(dir string, patterns []string) (*loader.Packages, error)
	// Inferred is the analysis type table, or nil.
	Inferred Inference
	// Boundary is the per-flow bindable-output fact.
	Boundary map[string]Boundary
	// PackagePath is the import path of the package the generated files belong
	// to, which is the scope every type spelling is resolved in. EMPTY selects
	// the derivation from the output directory's enclosing go.mod.
	PackagePath string
	// Disclose receives the analysis findings that do NOT refuse the run.
	//
	// A LIBRARY CALLER LEAVING IT NIL IS CHOOSING NOT TO SEE THEM, and nothing
	// else in this package reads them. They are handed out rather than dropped
	// because the analyzers already paid to compute them; they are handed out
	// rather than refused on because the module's own vocabulary calls a warning
	// "suspicious but not provably wrong" and a hint "an observation an author
	// may reasonably ignore", and refusing on those refuses legal programs.
	Disclose func(diags []Diagnostic)
	// Observe reports the live count of concurrent file emissions, so the
	// declared ceiling is OBSERVABLE rather than asserted. Production passes nil.
	Observe func(live int)
}

// Generate runs the whole pipeline over a directory of .flow sources.
//
// THE ORDER IS THE COST MODEL. Per-file work is concurrent and bounded; the
// package load is ONE call for the whole run; then the facts are read, then one
// synthesis and emission pass, then one atomic rename.
//
// THE ANALYSIS GATE RUNS BETWEEN THE LOAD AND THE EMISSION, and it is the same
// contract runChecks carries one step earlier: a refusal returns before anything
// is staged. What it refuses on is analysis's SeverityError alone; findings below
// that line reach Disclose rather than the exit status, because refusing on a
// warning or a hint would refuse programs the language calls legal.
//
// REGENERATION IS ALWAYS WHOLE. Every previously generated file in the output
// directory is removed before the new set is put in place, so a deleted .flow
// leaves no orphaned .go behind to be compiled forever.
//
// THE CRASH WINDOW IS CLOSED BY WRITING TO A TEMP DIRECTORY AND RENAMING ONLY
// AFTER EVERY FILE HAS EMITTED. A failure partway through otherwise leaves a
// half-regenerated output directory that a subsequent build would compile without
// complaint — a tree that is neither the old program nor the new one.
func (d *Driver) Generate(inputDir, outputDir string) error {
	sources, err := d.discover(inputDir)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return fmt.Errorf("no .flow sources under %s", inputDir)
	}
	if diags := d.runChecks(sources); len(diags) != 0 {
		return &Error{Diagnostics: diags}
	}
	// THE OUTPUT DIRECTORY IS CREATED BEFORE THE PATH IS DERIVED, because the
	// derivation reads it: a first run into a directory that does not exist yet
	// would otherwise have nothing to walk upward from. commit creates it too and
	// MkdirAll is idempotent, so this hoist adds a directory and removes nothing.
	if mkErr := os.MkdirAll(outputDir, 0o750); mkErr != nil {
		return fmt.Errorf("creating %s: %w", outputDir, mkErr)
	}
	pkgPath, err := d.packagePath(outputDir)
	if err != nil {
		return err
	}

	pkgs, err := d.load(inputDir, loadPatterns(pkgPath, sources))
	if err != nil {
		return err
	}
	facts, refused, err := d.facts(sources, pkgs, pkgPath)
	if err != nil {
		return err
	}
	// THE REFUSAL IS BEFORE ANY FILE IS WRITTEN, which is the contract runChecks
	// already carries and the reason neither can move after emission: a rejected
	// program left on disk compiles.
	if len(refused) != 0 {
		return &Error{Diagnostics: refused}
	}
	if pkgs != nil {
		facts.Types = NewTypes(pkgs, pkgPath, map[int]ast.Position{})
	}

	generated, err := d.emitAll(sources, facts)
	if err != nil {
		return err
	}

	return d.commit(outputDir, generated)
}

// facts gathers the answers this package consumes and never derives.
//
// A NIL PACKAGE SET ANSWERS WITH THE CALLER'S OWN FACTS and no diagnostics. That
// is what keeps a test driving the Driver with a counting loader working: it has
// no packages to analyze and supplies the one fact its rule is about.
//
// OTHERWISE THE ANALYSIS RUN ANSWERS, AND A CALLER-SUPPLIED FACT WINS PER FACT. A
// non-nil Boundary replaces the gate's boundary map and a non-nil Inferred
// replaces its table, each independently. A test injects ONE fact to drive ONE
// rule; production injects nothing and every fact comes from the single run.
func (d *Driver) facts(sources []Source, pkgs *loader.Packages, pkgPath string) (Facts, []Diagnostic, error) {
	if pkgs == nil {
		return Facts{Boundary: d.Boundary, Inferred: d.Inferred}, nil, nil
	}

	facts, refused, disclosed, err := gate(sources, pkgs, pkgPath)
	if err != nil {
		return Facts{}, nil, err
	}
	if d.Disclose != nil && len(disclosed) != 0 {
		d.Disclose(disclosed)
	}
	if d.Boundary != nil {
		facts.Boundary = d.Boundary
	}
	if d.Inferred != nil {
		facts.Inferred = d.Inferred
	}

	return facts, refused, nil
}

// packagePath answers the import path the generated files belong to.
//
// A CALLER WHO SAYS WHICH PACKAGE IT IS GENERATING IS BELIEVED, and everything
// else derives. Production sets nothing and the derivation answers; a test, and
// the -pkgpath flag, exist for the case the derivation cannot serve — generating
// into a directory whose enclosing module is not the one the generated code will
// be compiled in.
//
// IT READS THE OUTPUT DIRECTORY, because PackagePath is the import path of the
// package the generated files belong to and that is where they are written. For
// flowc's own defaults, and for every fixture in this repository, the input and
// output directories are the same one.
//
// THE RESULT IS RETURNED RATHER THAN WRITTEN BACK to the field, so a Driver
// reused across two output directories does not carry the first one's answer
// into the second.
func (d *Driver) packagePath(outputDir string) (string, error) {
	if d.PackagePath != "" {
		return d.PackagePath, nil
	}

	return DerivePackagePath(outputDir)
}

// loadPatterns names everything the run's SINGLE load has to resolve.
//
// THREE THINGS, and each was observed missing before it was added:
//
//   - "./..." — the packages under the load root, which is where the Go symbols a
//     flow references live.
//   - THE GENERATED PACKAGE ITSELF. When the output directory is not the input
//     one, "./..." rooted at the input directory does not reach the package the
//     generated files belong to, and Resolve then refuses by name.
//   - EVERY MODULE A .flow IMPORTS. A cross-module `use` names a flow in a module
//     the consumer's GO code need not import at all, so "./..." indexes no package
//     belonging to it and ResolveFlow refuses with `no loaded package has a module
//     for import path ...` — a refusal that reads like a missing flow and is
//     actually a missing load.
//
// THIS WIDENS ONE CALL RATHER THAN ADDING ONE. Loading is the seconds-scale
// operation in this toolchain and the run still performs exactly one load.
func loadPatterns(pkgPath string, sources []Source) []string {
	patterns := []string{"./..."}
	seen := map[string]bool{"./...": true}
	add := func(pattern string) {
		if pattern == "" || seen[pattern] {
			return
		}
		seen[pattern] = true
		patterns = append(patterns, pattern)
	}
	add(pkgPath)
	for _, source := range sources {
		for _, path := range importPaths(source.File) {
			add(path)
		}
	}

	return patterns
}

// importPaths maps each of a file's import QUALIFIERS to the path it names.
//
// THE QUALIFIER IS THE ALIAS WHEN ONE IS WRITTEN and the last segment of the path
// otherwise, which is Go's own rule. Two callers need this mapping and they need
// the same one: the load names every imported module as a pattern, and a dotted
// `use` resolves its qualifier to the module the reference reaches.
//
// The parser keeps an import path WITH its quotes, so they are stripped here
// rather than at each call site.
func importPaths(file *ast.File) map[string]string {
	out := map[string]string{}
	if file == nil {
		return out
	}
	for _, decl := range file.Decls {
		imp, ok := decl.(ast.ImportDecl)
		if !ok {
			continue
		}
		path := strings.Trim(imp.Path, `"`)
		if path == "" {
			continue
		}
		qualifier := path
		if at := strings.LastIndex(qualifier, "/"); at >= 0 {
			qualifier = qualifier[at+1:]
		}
		if imp.Alias != nil {
			qualifier = imp.Alias.Name
		}
		out[qualifier] = path
	}

	return out
}

// runChecks runs the pre-generation gate.
//
// IT RUNS BEFORE ANY FILE IS WRITTEN, which is the whole of its contract: a
// reporting check means the run writes NOTHING and fails. Moving it after
// emission would leave a rejected program on disk.
func (d *Driver) runChecks(sources []Source) []Diagnostic {
	var diags []Diagnostic
	for _, check := range d.Checks {
		diags = append(diags, check(sources)...)
	}

	return diags
}

// load performs the run's SINGLE package load.
//
// ONCE PER RUN, NEVER PER FILE OR PER FLOW. The loader holds no process-global
// cache and says so — the lifetime of a load belongs to the caller — and this
// driver is that caller. Loading is the seconds-scale operation in this
// toolchain, so a per-unit call turns a one-off cost into a per-unit one.
//
// THE ROOT IS THE INPUT DIRECTORY, and that is measured rather than preferred:
// the Go symbols a flow references live beside the .flow, and rooting the load at
// the OUTPUT directory instead indexes no package at all when the two differ.
// What the output directory contributes is a PATTERN, not a root.
func (d *Driver) load(dir string, patterns []string) (*loader.Packages, error) {
	load := d.Load
	if load == nil {
		load = loader.Load
	}
	pkgs, err := load(dir, patterns)
	if err != nil {
		return nil, fmt.Errorf("loading the package set under %s: %w", dir, err)
	}

	return pkgs, nil
}

// discover finds .flow sources BY EXTENSION, the way Go finds .go files.
//
// Nothing marks a directory as flow-bearing, deliberately: a marker file is one
// more thing to forget, and its absence would silently generate nothing.
func (*Driver) discover(dir string) ([]Source, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	var sources []Source
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".flow") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("reading %s: %w", path, readErr)
		}
		file, parseErr := ast.Parse(body)
		if parseErr != nil {
			return nil, fmt.Errorf("%s: %w", path, parseErr)
		}
		sources = append(sources, Source{Path: entry.Name(), Src: body, File: file})
	}
	// A STABLE ORDER, so a run over one directory produces one result.
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })

	return sources, nil
}

// emitAll runs the per-file work concurrently, bounded by the declared ceiling.
func (d *Driver) emitAll(sources []Source, facts Facts) ([]Generated, error) {
	ceiling := generationConcurrency()
	var (
		semaphore = make(chan struct{}, ceiling)
		mu        sync.Mutex
		live      int
		wg        sync.WaitGroup
		results   = make([]Generated, len(sources))
		failures  = make([][]Diagnostic, len(sources))
	)
	for i, source := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			d.enter(&mu, &live)
			results[i], failures[i] = assembleOne(source, d.Config, facts)
			d.leave(&mu, &live)
		}()
	}
	wg.Wait()

	return collect(results, failures)
}

// enter records one emission starting and reports the live count.
func (d *Driver) enter(mu *sync.Mutex, live *int) {
	mu.Lock()
	*live++
	observed := *live
	mu.Unlock()
	if d.Observe != nil {
		d.Observe(observed)
	}
}

// leave records one emission finishing.
func (*Driver) leave(mu *sync.Mutex, live *int) {
	mu.Lock()
	*live--
	mu.Unlock()
}

// collect folds the per-file results into one answer.
func collect(results []Generated, failures [][]Diagnostic) ([]Generated, error) {
	var (
		generated []Generated
		diags     []Diagnostic
	)
	for i := range results {
		diags = append(diags, failures[i]...)
		if len(results[i].Source) > 0 {
			generated = append(generated, results[i])
		}
	}
	if len(diags) != 0 {
		return nil, &Error{Diagnostics: diags, Partial: generated}
	}

	return generated, nil
}

// commit writes the generated set atomically.
func (*Driver) commit(outputDir string, generated []Generated) error {
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", outputDir, err)
	}
	staging, err := os.MkdirTemp(outputDir, ".flowc-staging-")
	if err != nil {
		return fmt.Errorf("creating a staging directory under %s: %w", outputDir, err)
	}
	defer os.RemoveAll(staging)

	for _, file := range generated {
		path := filepath.Join(staging, file.Name)
		if writeErr := os.WriteFile(path, file.Source, 0o600); writeErr != nil {
			return fmt.Errorf("writing %s: %w", path, writeErr)
		}
	}
	if err := removeGenerated(outputDir); err != nil {
		return err
	}
	for _, file := range generated {
		from := filepath.Join(staging, file.Name)
		to := filepath.Join(outputDir, file.Name)
		if renameErr := os.Rename(from, to); renameErr != nil {
			return fmt.Errorf("moving %s into place: %w", file.Name, renameErr)
		}
	}

	return nil
}

// removeGenerated deletes every previously generated file.
//
// THIS IS WHAT MAKES REGENERATION WHOLE. A deleted .flow whose .go survived would
// go on compiling forever, declaring a flow whose source nobody can find.
func removeGenerated(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("reading %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), generatedSuffix) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("removing the stale %s: %w", entry.Name(), err)
		}
	}

	return nil
}

// Render formats one diagnostic as `<file>:<line>:<col>: <message>`.
//
// THE COLUMN IS A BYTE COUNT, NOT A RUNE INDEX. lang/ast documents Position.Col
// that way deliberately, so an editor maps an offset without re-scanning the
// line. Do not "fix" it to runes when rendering: every consumer downstream reads
// it as bytes.
//
// A NON-EMPTY d.Path WINS OVER THE CALLER'S file. A refusal this package raised
// carries no Path and the caller's name is right; one that crossed in from the
// analysis gate names the file that caused it, which may be any file in the run
// or a dependency in another module entirely.
func Render(file string, d Diagnostic) string {
	if d.Path != "" {
		file = d.Path
	}

	return fmt.Sprintf("%s:%d:%d: %s", file, d.Pos.Line, d.Pos.Col, d.Message)
}
