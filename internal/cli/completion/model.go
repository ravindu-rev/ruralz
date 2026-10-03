// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package completion

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// helpWord is the framework's `help [<noun> [<verb>]]` command, which
// every script completes although it is not a table row.
const helpWord = "help"

// model is the shell-neutral view of a command table that every
// generator renders.
type model struct {
	program string
	ident   string // program as a shell identifier
	tops    []word // nouns, one-word commands and help, sorted
	nouns   []nounModel
	cmds    []cmdModel
}

// word is a completable word with its description.
type word struct{ name, desc string }

type nounModel struct {
	name  string
	verbs []word
}

type cmdModel struct {
	id    string // "bundle build"
	path  []string
	flags []flagModel // sorted by name, --help included
	args  []argModel
}

type flagModel struct {
	name        string
	desc        string
	placeholder string
	takesValue  bool
	value       valueKind
	modes       []modeModel
}

type modeModel struct {
	when  []string
	value valueKind
}

type argModel struct {
	name     string
	desc     string
	optional bool
	repeat   bool
	value    valueKind
}

// valueKind says how a value is completed.
type valueKind struct {
	complete command.Completion
	values   []string
}

// kindOf maps a completion and its values; values without CompleteValues
// still enumerate, and CompleteValues without values offers nothing.
func kindOf(c command.Completion, values []string) valueKind {
	if len(values) > 0 {
		return valueKind{complete: command.CompleteValues, values: values}
	}
	if c == command.CompleteValues {
		return valueKind{complete: command.CompleteNone}
	}
	return valueKind{complete: c}
}

// token is the bash and PowerShell encoding: none, file, dir or
// "words:<space-separated values>".
func (v valueKind) token() string {
	switch v.complete {
	case command.CompleteFile:
		return "file"
	case command.CompleteDir:
		return "dir"
	case command.CompleteValues:
		return "words:" + strings.Join(v.values, " ")
	default:
		return "none"
	}
}

// commandWords reports whether s is a lower-case command or flag word.
func commandWord(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for _, c := range []byte(s) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// plainValue reports whether s needs no quoting in any supported shell
// and cannot be mistaken for syntax of a completion specification.
func plainValue(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for _, c := range []byte(s) {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			strings.IndexByte("._/@+-", c) >= 0
		if !ok {
			return false
		}
	}
	return true
}

// programName reports whether s is usable as the program name.
func programName(s string) bool {
	if s == "" || (s[0] < 'a' || s[0] > 'z') && (s[0] < 'A' || s[0] > 'Z') {
		return false
	}
	for _, c := range []byte(s) {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// oneLine folds a description onto one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// buildModel validates the entries and indexes them.
func buildModel(program string, entries []Entry) (*model, error) {
	if !programName(program) {
		return nil, fmt.Errorf("completion: invalid program name %q", program)
	}
	m := &model{program: program, ident: strings.ReplaceAll(program, "-", "_")}
	nounDesc := map[string]string{}
	verbs := map[string][]word{}
	topCmds := map[string]string{}
	seen := map[string]bool{}
	for _, e := range entries {
		id := strings.Join(e.Path, " ")
		if len(e.Path) < 1 || len(e.Path) > 2 || (e.Noun && len(e.Path) != 1) {
			return nil, fmt.Errorf("completion: invalid path %q", id)
		}
		for _, w := range e.Path {
			if !commandWord(w) || w == helpWord {
				return nil, fmt.Errorf("completion: invalid path word %q in %q", w, id)
			}
		}
		if seen[id] {
			return nil, fmt.Errorf("completion: duplicate path %q", id)
		}
		seen[id] = true
		if e.Noun {
			nounDesc[id] = oneLine(e.Summary)
			continue
		}
		cmd, err := buildCommand(e)
		if err != nil {
			return nil, err
		}
		m.cmds = append(m.cmds, cmd)
		if len(e.Path) == 2 {
			verbs[e.Path[0]] = append(verbs[e.Path[0]], word{e.Path[1], oneLine(e.Summary)})
		} else {
			topCmds[id] = oneLine(e.Summary)
		}
	}
	for name, desc := range topCmds {
		if _, ok := verbs[name]; ok {
			return nil, fmt.Errorf("completion: %q is both a command and a noun", name)
		}
		m.tops = append(m.tops, word{name, desc})
	}
	for name, vs := range verbs {
		slices.SortFunc(vs, func(a, b word) int { return strings.Compare(a.name, b.name) })
		desc := nounDesc[name]
		if desc == "" {
			names := make([]string, 0, len(vs))
			for _, v := range vs {
				names = append(names, v.name)
			}
			desc = strings.Join(names, ", ")
		}
		m.tops = append(m.tops, word{name, desc})
		m.nouns = append(m.nouns, nounModel{name: name, verbs: vs})
	}
	for name := range nounDesc {
		if _, ok := verbs[name]; !ok {
			return nil, fmt.Errorf("completion: noun %q has no verbs", name)
		}
	}
	m.tops = append(m.tops, word{helpWord, "Print help for a command"})
	slices.SortFunc(m.tops, func(a, b word) int { return strings.Compare(a.name, b.name) })
	slices.SortFunc(m.nouns, func(a, b nounModel) int { return strings.Compare(a.name, b.name) })
	slices.SortFunc(m.cmds, func(a, b cmdModel) int { return strings.Compare(a.id, b.id) })
	return m, nil
}

// buildCommand validates and converts one command entry.
func buildCommand(e Entry) (cmdModel, error) {
	id := strings.Join(e.Path, " ")
	c := cmdModel{id: id, path: e.Path}
	names := map[string]bool{}
	for _, f := range e.Flags {
		if !commandWord(f.Name) {
			return c, fmt.Errorf("completion: invalid flag name %q on %q", f.Name, id)
		}
		if names[f.Name] {
			return c, fmt.Errorf("completion: duplicate flag --%s on %q", f.Name, id)
		}
		names[f.Name] = true
	}
	for _, f := range e.Flags {
		fm := flagModel{
			name: f.Name, desc: oneLine(f.Usage), placeholder: f.Placeholder,
			takesValue: !f.Bool, value: kindOf(f.Complete, f.Values),
		}
		if fm.placeholder == "" {
			fm.placeholder = "VALUE"
		}
		if err := checkValues(id, "--"+f.Name, f.Values); err != nil {
			return c, err
		}
		for _, md := range f.Modes {
			for _, w := range md.When {
				if !names[w] {
					return c, fmt.Errorf("completion: mode of --%s on %q names unknown flag --%s", f.Name, id, w)
				}
			}
			if err := checkValues(id, "--"+f.Name, md.Values); err != nil {
				return c, err
			}
			when := slices.Sorted(slices.Values(md.When))
			fm.modes = append(fm.modes, modeModel{when: when, value: kindOf(command.CompleteValues, md.Values)})
		}
		c.flags = append(c.flags, fm)
	}
	slices.SortFunc(c.flags, func(a, b flagModel) int { return strings.Compare(a.name, b.name) })
	for i, a := range e.Args {
		if a.Repeat && i != len(e.Args)-1 {
			return c, fmt.Errorf("completion: only the last positional of %q may repeat", id)
		}
		if err := checkValues(id, a.Name, a.Values); err != nil {
			return c, err
		}
		desc := oneLine(a.Usage)
		if desc == "" {
			desc = a.Name
		}
		c.args = append(c.args, argModel{
			name: a.Name, desc: desc, optional: a.Optional, repeat: a.Repeat, value: kindOf(a.Complete, a.Values),
		})
	}
	return c, nil
}

func checkValues(id, what string, values []string) error {
	for _, v := range values {
		if !plainValue(v) {
			return fmt.Errorf("completion: value %q of %s on %q is not a plain word", v, what, id)
		}
	}
	return nil
}
