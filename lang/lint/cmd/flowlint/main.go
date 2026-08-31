// Package main - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// Command flowlint runs every registered flow analyzer over the files and
// directories it is given, and reports what they found.
//
// Usage:
//
//	flowlint [-format text|json] [-fail-on error|warning|hint] [-rules] path...
//
// A path naming a file is linted whatever its extension. A path naming a
// directory is walked recursively and every .flow file under it is linted.
//
// THE EXIT STATUS IS THE PRODUCT, and its three values are kept apart
// deliberately:
//
//	0  nothing at or above the fail-on level was reported (or -rules printed)
//	1  something at or above the fail-on level was reported
//	2  the run could not be performed at all
//
// The third is the one that usually gets collapsed into the first, and
// collapsing it is how a build ends up gated on nothing: a linter handed a path
// set it could not fill would answer "clean" to every question it was never
// asked. An unparsed flag, an unknown -format or -fail-on value, no paths, an
// unreadable path, a path set holding no flow sources, an analyzer failure and a
// write failure on the output stream are all 2.
//
// There is no display filter, no rule selection, no configuration file and no
// suppression syntax. Each would be a way to make a finding disappear without
// fixing it. -fail-on can be made stricter than its default and no looser.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/whitaker-io/machine/lang/lint"
)

const (
	// exitClean reports that nothing at or above the fail-on level was found.
	exitClean = 0
	// exitFound reports that something at or above the fail-on level was found.
	exitFound = 1
	// exitCannotRun reports that the run could not be performed at all. It is
	// never collapsed into exitClean: a run that did not happen is not a run
	// that found nothing.
	exitCannotRun = 2
)

// options holds the three accepted flags.
//
// The flag set is built in newFlags rather than inline so the surface is
// ENUMERABLE by a test: without the extraction it is reachable only by parsing
// help text, which is not an assertion anybody should build a gate on.
type options struct {
	format *string
	failOn *string
	rules  *bool
}

// newFlags builds the flag set and the options it fills.
//
// EXACTLY THREE FLAGS ARE REGISTERED, and a test asserts set equality against
// that list. A fourth is a way to make a finding disappear without fixing it.
func newFlags(errOut io.Writer) (*flag.FlagSet, *options) {
	flags := flag.NewFlagSet("flowlint", flag.ContinueOnError)
	flags.SetOutput(errOut)

	opts := &options{
		format: flags.String("format", lint.FormatText,
			"output format, one of: "+strings.Join(lint.Formats, ", ")),
		// Thresholds is in severity order, so its first element is the loudest
		// level and the default: a run fails on an error and nothing less.
		failOn: flags.String("fail-on", lint.Thresholds[0],
			"lowest severity that fails the run, one of: "+strings.Join(lint.Thresholds, ", ")),
		rules: flags.Bool("rules", false,
			"print every registered rule and its own stated limits, then exit"),
	}
	return flags, opts
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main's body with the streams and the arguments passed in, so the exit
// contract is exercisable by an in-process test rather than only by a subprocess.
func run(args []string, out, errOut io.Writer) int {
	flags, opts := newFlags(errOut)
	if err := flags.Parse(args); err != nil {
		return refuse(errOut, err.Error())
	}
	if *opts.rules {
		return listRules(out, errOut)
	}
	return check(flags.Args(), opts, out, errOut)
}

// listRules prints every registered rule and its own stated limits.
func listRules(out, errOut io.Writer) int {
	if err := lint.WriteRules(out); err != nil {
		return refuse(errOut, err.Error())
	}
	return exitClean
}

// check resolves the options, performs the run and returns its exit status.
func check(paths []string, opts *options, out, errOut io.Writer) int {
	format, err := parseFormat(*opts.format)
	if err != nil {
		return refuse(errOut, err.Error())
	}
	threshold, err := lint.ParseThreshold(*opts.failOn)
	if err != nil {
		return refuse(errOut, err.Error())
	}
	batch, err := lint.Load(paths)
	if err != nil {
		return refuse(errOut, err.Error())
	}
	result, err := lint.Check(batch, threshold)
	if err != nil {
		return refuse(errOut, err.Error())
	}
	if err := writeResult(out, format, result); err != nil {
		return refuse(errOut, err.Error())
	}

	if result.Failing > 0 {
		return exitFound
	}
	return exitClean
}

// parseFormat resolves a format name, refusing an unknown one by naming both the
// value and the vocabulary rather than falling back to a default.
func parseFormat(name string) (string, error) {
	for _, format := range lint.Formats {
		if format == name {
			return format, nil
		}
	}
	return "", errors.New("lint: unknown format " + name +
		"; the formats are " + strings.Join(lint.Formats, ", "))
}

// writeResult renders the result in the selected format. Choosing a format
// cannot change a verdict; it changes only how the verdict is spelled.
func writeResult(out io.Writer, format string, result lint.Result) error {
	if format == lint.FormatJSON {
		return lint.WriteJSON(out, result)
	}
	return lint.WriteText(out, result)
}

// refuse writes a message and returns the could-not-run status.
//
// ITS OWN WRITE ERROR IS DISCARDED HERE AND NOWHERE ELSE, and the reason is that
// this is the last thing the process does with a failure: a stderr that will not
// accept the message leaves no channel through which to report the failure to
// report the failure. Every other write error in this module is returned.
func refuse(errOut io.Writer, message string) int {
	_, _ = fmt.Fprintln(errOut, message)
	return exitCannotRun
}
