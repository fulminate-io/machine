// Package lint - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lint

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/whitaker-io/machine/lang/analysis"
	"github.com/whitaker-io/machine/lang/ast"
)

// Extension is the suffix a directory walk collects. A file named directly is
// taken whatever it is called.
const Extension = ".flow"

// cannotRead prefixes every refusal that names a path the loader could not fill.
const cannotRead = "lint: cannot read "

// Batch is one run's input, already partitioned: the sources the analyzers can
// be given, the diagnostics the parser produced, and the files withheld.
//
// Damaged files are carried by NAME rather than dropped because a caller owes
// its reader an account of what was not analyzed. A file silently missing from
// Sources is a file nobody decided to stop checking.
type Batch struct {
	Sources []analysis.Source
	Parse   []analysis.Diagnostic
	Damaged []string
}

// Load resolves paths to parsed sources, refusing any input it cannot fill.
//
// A path naming a file is taken as that file whatever its extension, so the
// operator lints what the operator named. A path naming a directory is walked
// RECURSIVELY and every Extension file under it is taken — no vendor rule and no
// ignore file, because a silent exclusion is how a file stops being checked
// without anyone deciding it should.
//
// THE THREE REFUSALS ARE THE POINT. No paths, a path that cannot be stat-ed, and
// a non-empty path set holding no flow sources are all errors. A linter handed an
// input set it could not fill would otherwise answer "clean" to every question it
// was never asked, and that vacuous green is indistinguishable at the exit status
// from a real one.
//
// Arguments are the operator's own input here — a person or a CI job runs this
// against paths its own configuration names — so they are validated against what
// they are allowed to be and REJECTED rather than coerced. They are deliberately
// NOT confined by os.Root: a linter is pointed at arbitrary trees by its
// operator, and a root would break `flowlint ../other/tree`, which is a primary
// use.
//
// The walk is SERIAL, which is a measurement rather than a preference: an
// twelve-analyzer pass over a strawman costs tens of microseconds against a
// parse an order of magnitude larger, so file I/O and parsing dominate and both
// are per-file and cheap. A worker pool would spend more on coordination than it
// saves.
func Load(paths []string) (Batch, error) {
	if len(paths) == 0 {
		return Batch{}, errors.New("lint: no paths named; name at least one flow file or directory")
	}

	found, err := gather(paths)
	if err != nil {
		return Batch{}, err
	}
	if len(found) == 0 {
		return Batch{}, errors.New("lint: no " + Extension + " sources under " + strings.Join(paths, ", "))
	}

	// Sorted before parsing so a run's output does not depend on directory order.
	sort.Strings(found)
	return parseAll(found)
}

// gather resolves every named path to the files it stands for.
func gather(paths []string) ([]string, error) {
	var found []string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, errors.New(cannotRead + path + ": " + err.Error())
		}
		if !info.IsDir() {
			found = append(found, path)
			continue
		}
		under, err := walk(path)
		if err != nil {
			return nil, err
		}
		found = append(found, under...)
	}
	return found, nil
}

// walk collects every Extension file beneath dir, at any depth.
func walk(dir string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Ext(path) == Extension {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		return nil, errors.New(cannotRead + dir + ": " + err.Error())
	}
	return found, nil
}

// parseAll reads and parses each file, partitioning parse damage away from the
// sources the analyzers are given.
//
// A file that did not parse is withheld because analyzer findings over a damaged
// tree describe the damage rather than the program. A parse failure that yielded
// NO diagnostics is an error naming the file: the parser failed in a way this
// partition does not model, and absorbing it would drop a file silently.
func parseAll(found []string) (Batch, error) {
	var batch Batch
	for _, path := range found {
		src, err := os.ReadFile(path)
		if err != nil {
			return Batch{}, errors.New(cannotRead + path + ": " + err.Error())
		}

		file, perr := ast.Parse(src)
		if perr != nil {
			diags := analysis.ParseDiagnostics(path, perr)
			if len(diags) == 0 {
				return Batch{}, errors.New("lint: " + path +
					" did not parse and reported no diagnostics: " + perr.Error())
			}
			batch.Parse = append(batch.Parse, diags...)
			batch.Damaged = append(batch.Damaged, path)
			continue
		}
		batch.Sources = append(batch.Sources, analysis.Source{Path: path, Src: src, File: file})
	}
	return batch, nil
}
