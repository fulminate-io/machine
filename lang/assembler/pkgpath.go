// Package assembler - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package assembler

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"golang.org/x/mod/modfile"
)

// DerivePackagePath answers the import path of the package a directory holds.
//
// IT DERIVES THE PATH THE WAY THE GO TOOLCHAIN DOES: the nearest enclosing
// go.mod's module path, joined with that directory's path relative to the module
// root. Nothing else in this package can answer the question, and leaving it
// unanswered is what made every typed source fail — with no package path the
// type table resolves every spelling in an empty scope.
//
// IT REFUSES RATHER THAN GUESSES, in three directions: no enclosing go.mod, a
// go.mod that cannot be read, and a go.mod carrying no module line. An empty
// string returned from any of them would reintroduce the empty scope this
// function exists to prevent, and it would do it silently.
func DerivePackagePath(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolving %s to an absolute path: %w", dir, err)
	}
	root, module, err := enclosingModule(abs)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", fmt.Errorf("relating %s to the module root %s: %w", abs, root, err)
	}
	if rel == "." {
		return module, nil
	}

	// THE SEPARATOR IS THE IMPORT PATH'S, NOT THE FILESYSTEM'S. filepath.Rel
	// answers in the platform's separator, and a Windows checkout would otherwise
	// produce a backslash inside an import path.
	return path.Join(module, filepath.ToSlash(rel)), nil
}

// enclosingModule walks upward from an absolute directory to the nearest go.mod
// and reads its module path.
//
// THE WALK STOPS WHEN filepath.Dir STOPS CHANGING, which is the root of the
// volume on every platform this builds for. Testing for a specific separator
// would be a second definition of "root" that disagrees with the first one
// somewhere.
func enclosingModule(abs string) (root, module string, err error) {
	for dir := abs; ; {
		file := filepath.Join(dir, "go.mod")
		body, readErr := os.ReadFile(file)
		switch {
		case readErr == nil:
			// modfile.ModulePath is the toolchain's own extractor and is
			// documented to tolerate unrelated problems in the file. It answers
			// the EMPTY STRING when it finds no module line, and that empty
			// string is refused here rather than passed on.
			declared := modfile.ModulePath(body)
			if declared == "" {
				return "", "", fmt.Errorf(
					"%s declares no module path, so the import path of %s cannot be derived", file, abs)
			}

			return dir, declared, nil
		case !errors.Is(readErr, os.ErrNotExist):
			return "", "", fmt.Errorf("reading %s: %w", file, readErr)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", fmt.Errorf(
				"no go.mod encloses %s, so the import path of the generated package cannot be derived", abs)
		}
		dir = parent
	}
}
