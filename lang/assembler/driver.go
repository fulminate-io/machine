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
	// to, which is the scope every type spelling is resolved in.
	PackagePath string
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

	pkgs, err := d.load(inputDir)
	if err != nil {
		return err
	}
	facts := Facts{
		Boundary: d.Boundary,
		Inferred: d.Inferred,
	}
	if pkgs != nil {
		facts.Types = NewTypes(pkgs, d.PackagePath, map[int]ast.Position{})
	}

	generated, err := d.emitAll(sources, facts)
	if err != nil {
		return err
	}

	return d.commit(outputDir, generated)
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
func (d *Driver) load(dir string) (*loader.Packages, error) {
	load := d.Load
	if load == nil {
		load = loader.Load
	}
	pkgs, err := load(dir, []string{"./..."})
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
		sources = append(sources, Source{Path: entry.Name(), File: file})
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
func Render(file string, d Diagnostic) string {
	return fmt.Sprintf("%s:%d:%d: %s", file, d.Pos.Line, d.Pos.Col, d.Message)
}
