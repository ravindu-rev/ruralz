// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package command is the ruralz command framework (spec 10 sections 2.1
// and 2.2): the command table model (Spec, Arg, Command), interspersed
// standard library flag parsing with flag specs for help and completion,
// the IO streams of one invocation, exit codes, ExitError and Interrupted,
// the dispatcher Execute with help and usage errors, the JSON and NDJSON
// writers and the atomic file write behind --output-file.
//
// The package holds no state: the root package builds the command table
// per call (cli.Registry) and hands it to Execute with the IO of the
// invocation.
package command

import (
	"context"
	"io"
	"runtime"
	"strings"
	"time"
)

// Completion says how a shell completes a flag value or a positional.
type Completion uint8

const (
	// CompleteNone offers nothing: the value is free text.
	CompleteNone Completion = iota
	// CompleteFile completes file names (FILE, PATH, FROM, TO).
	CompleteFile
	// CompleteDir completes directory names (DIR, --output-dir, --data-dir).
	CompleteDir
	// CompleteValues completes the enumerated Values.
	CompleteValues
)

// String returns the lower-case name of c.
func (c Completion) String() string {
	switch c {
	case CompleteNone:
		return "none"
	case CompleteFile:
		return "file"
	case CompleteDir:
		return "dir"
	case CompleteValues:
		return "values"
	default:
		return "unknown"
	}
}

// Arg describes one positional argument of a command.
type Arg struct {
	// Name is the placeholder shown in usage lines: "DIR", "FROM", "SHELL".
	Name string
	// Usage is the one-line description shown by help.
	Usage string
	// Optional marks an argument that may be omitted; only trailing
	// arguments are optional.
	Optional bool
	// Default replaces an omitted optional argument before Run ("." for DIR).
	Default string
	// Repeat lets the last argument take any number of further values.
	Repeat bool
	// Complete says how shells complete the argument.
	Complete Completion
	// Values are the allowed values; when set, Execute rejects others with
	// exit 2 and shells complete them.
	Values []string
}

// Spec is one row of the command table: a runnable command, a Planned
// command recognized but not built (spec 10 req 8), or the description of
// a noun (a one-word Path with neither New nor Planned).
type Spec struct {
	// Path is the command path without the program name: {"bundle",
	// "validate"}, {"version"}; {"bundle"} for a noun description.
	Path []string
	// Summary is the one-line description shown in command lists.
	Summary string
	// Help holds further paragraphs printed by the command's help after
	// the summary, such as install lines.
	Help string
	// Args are the positional arguments in order.
	Args []Arg
	// Planned names the milestone ("M2") of a command that is recognized
	// but not in this build; such a command has no New and is absent from
	// help and completion.
	Planned string
	// Platform, when set, returns the reason the command is not planned on
	// goos, or nil when it runs there (spec 10 req 10).
	Platform func(goos string) error
	// When set, the Deprecated text is printed as a warning each time the
	// command is used (spec 10 req 9).
	Deprecated string
	// New returns a fresh Command per invocation; nil when Planned is set
	// and for a noun description.
	New func() Command
}

// Name returns the space-separated command path.
func (s Spec) Name() string { return strings.Join(s.Path, " ") }

// IsNoun reports whether s only describes a noun.
func (s Spec) IsNoun() bool { return len(s.Path) == 1 && s.New == nil && s.Planned == "" }

// Command is one invocation of a runnable Spec.
type Command interface {
	// Flags binds the command's flags to its fields. It is called once per
	// invocation, and also on a fresh value for help and completion.
	Flags(f *Flags)
	// Run executes the command with the positionals left after flag
	// parsing (omitted optional arguments replaced by their Default). A nil
	// result exits 0; see Code for the mapping of errors.
	Run(ctx context.Context, io IO, args []string) error
}

// IO is what one invocation may touch outside its arguments (spec 10
// req 13): data goes to Stdout; progress, warnings, usage and errors go to
// Stderr.
type IO struct {
	// Stdout receives data.
	Stdout io.Writer
	// Stderr receives progress, warnings, prompts, usage and errors.
	Stderr io.Writer
	// Environ is the process environment snapshot taken once per
	// invocation, in os.Environ form.
	Environ []string
	// LookupEnv reads Environ.
	LookupEnv func(key string) (string, bool)
	// Now returns the current time; the root wires clock.Real().Now and
	// tests inject a fixed time.
	Now func() time.Time
	// GOOS is runtime.GOOS in the binary; tests inject it to exercise
	// platform gates.
	GOOS string
}

// NewIO returns the IO of one invocation. environ is copied, so later
// changes to the process environment do not reach the command; an empty
// goos means runtime.GOOS.
func NewIO(stdout, stderr io.Writer, environ []string, now func() time.Time, goos string) IO {
	if goos == "" {
		goos = runtime.GOOS
	}
	env := append([]string(nil), environ...)
	return IO{
		Stdout:    stdout,
		Stderr:    stderr,
		Environ:   env,
		LookupEnv: envLookup(env, goos == "windows"),
		Now:       now,
		GOOS:      goos,
	}
}

// platform returns GOOS, or runtime.GOOS when unset.
func (s IO) platform() string {
	if s.GOOS == "" {
		return runtime.GOOS
	}
	return s.GOOS
}

// envLookup returns a lookup over env in which the first entry of a name
// wins, as in the process environment; names ignore case on Windows.
func envLookup(env []string, foldCase bool) func(string) (string, bool) {
	return func(key string) (string, bool) {
		for _, kv := range env {
			// A leading '=' belongs to the name (Windows "=C:" entries).
			i := strings.IndexByte(kv[min(1, len(kv)):], '=')
			if i < 0 {
				continue
			}
			i += min(1, len(kv))
			name := kv[:i]
			if name == key || (foldCase && strings.EqualFold(name, key)) {
				return kv[i+1:], true
			}
		}
		return "", false
	}
}

// NotPlannedOn returns a Spec.Platform function that refuses the listed
// operating systems with their reasons, such as {"windows": "there is no
// ruralzd build; use WSL"}. Execute prints "<program> <path> is not planned
// on <goos>: <reason>" and exits 2.
func NotPlannedOn(reasons map[string]string) func(goos string) error {
	m := make(map[string]string, len(reasons))
	for k, v := range reasons {
		m[k] = v
	}
	return func(goos string) error {
		if r, ok := m[goos]; ok {
			return platformError(r)
		}
		return nil
	}
}

// platformError is the reason a command does not run on a platform.
type platformError string

func (e platformError) Error() string { return string(e) }
