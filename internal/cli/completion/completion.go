// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package completion generates the static shell completion scripts of
// `ruralz completion bash|zsh|fish|powershell` (spec 10 section 2.14).
// Scripts are built at run time from the command table (cli.Registry): no
// build-time generation and no callback command. They complete nouns,
// verbs, every flag in --name form per command, enumerated values (the
// --output formats per command and mode, the SHELL names), file names for
// FILE and PATH values and FROM and TO, and directory names for DIR,
// --output-dir and --data-dir. Planned and deprecated entries are left out.
package completion

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// Shell names a supported shell.
type Shell string

// The supported shells.
const (
	Bash       Shell = "bash"
	Zsh        Shell = "zsh"
	Fish       Shell = "fish"
	PowerShell Shell = "powershell"
)

// Shells returns the supported shells in help order.
func Shells() []Shell { return []Shell{Bash, Zsh, Fish, PowerShell} }

// ShellNames returns the names of Shells, the values of SHELL.
func ShellNames() []string {
	out := make([]string, 0, 4)
	for _, s := range Shells() {
		out = append(out, string(s))
	}
	return out
}

// Entry is one completable command, or the description of a noun.
type Entry struct {
	// Path is the command path without the program name.
	Path []string
	// Summary is the description shells show beside the word.
	Summary string
	// Noun marks a noun description (one-word Path, no Args or Flags).
	Noun bool
	// Args are the positionals in order.
	Args []command.Arg
	// Flags are the completable flags, --help included.
	Flags []command.FlagSpec
}

// Entries returns the completable entries of a command table: every
// runnable command that is neither Planned nor deprecated, with its
// non-deprecated flags plus --help, and the descriptions of nouns that
// keep at least one verb; sorted by path.
func Entries(specs []command.Spec) []Entry {
	var out []Entry
	nouns := map[string]bool{}
	for _, s := range specs {
		if s.IsNoun() || s.Planned != "" || s.Deprecated != "" || s.New == nil {
			continue
		}
		if len(s.Path) == 2 {
			nouns[s.Path[0]] = true
		}
		flags := slices.DeleteFunc(command.FlagSpecsOf(s), func(f command.FlagSpec) bool { return f.Deprecated != "" })
		flags = append(flags, command.HelpFlag())
		slices.SortFunc(flags, func(a, b command.FlagSpec) int { return strings.Compare(a.Name, b.Name) })
		out = append(out, Entry{
			Path: slices.Clone(s.Path), Summary: s.Summary,
			Args: slices.Clone(s.Args), Flags: flags,
		})
	}
	for _, s := range specs {
		if s.IsNoun() && nouns[s.Path[0]] {
			out = append(out, Entry{Path: slices.Clone(s.Path), Summary: s.Summary, Noun: true})
		}
	}
	slices.SortFunc(out, func(a, b Entry) int { return slices.Compare(a.Path, b.Path) })
	return out
}

// Write writes the completion script of sh for program over cmds. The
// script is built in memory first, so w receives nothing on error.
func Write(w io.Writer, sh Shell, program string, cmds []Entry) error {
	m, err := buildModel(program, cmds)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	switch sh {
	case Bash:
		writeBash(&b, m)
	case Zsh:
		writeZsh(&b, m)
	case Fish:
		writeFish(&b, m)
	case PowerShell:
		writePowerShell(&b, m)
	default:
		return fmt.Errorf("unsupported shell %q (want bash, zsh, fish or powershell)", string(sh))
	}
	_, err = w.Write(b.Bytes())
	return err
}

// InstallHelp returns the install line of every shell for program (spec
// 10 req 108).
func InstallHelp(program string) string {
	return "Install:\n" +
		"  bash        source <(" + program + " completion bash)\n" +
		"  zsh         " + program + " completion zsh > \"${fpath[1]}/_" + program + "\"\n" +
		"  fish        " + program + " completion fish > ~/.config/fish/completions/" + program + ".fish\n" +
		"  powershell  " + program + " completion powershell | Out-String | Invoke-Expression"
}

// Spec returns the `completion SHELL` command of program. registry is
// called when the command runs, so the script covers the table the
// binary dispatches over, this command included.
func Spec(program string, registry func() []command.Spec) command.Spec {
	return command.Spec{
		Path:    []string{"completion"},
		Summary: "Print a shell completion script",
		Help:    InstallHelp(program),
		Args: []command.Arg{{
			Name: "SHELL", Usage: "Shell to complete for",
			Complete: command.CompleteValues, Values: ShellNames(),
		}},
		New: func() command.Command { return &completionCommand{program: program, registry: registry} },
	}
}

// completionCommand is one run of `completion SHELL`.
type completionCommand struct {
	program  string
	registry func() []command.Spec
}

// Flags binds nothing: completion takes only SHELL.
func (c *completionCommand) Flags(*command.Flags) {}

// Run writes the script of args[0] to stdout.
func (c *completionCommand) Run(_ context.Context, sio command.IO, args []string) error {
	if err := Write(sio.Stdout, Shell(args[0]), c.program, Entries(c.registry())); err != nil {
		return command.NoResult(err)
	}
	return nil
}
