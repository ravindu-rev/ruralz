// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"errors"
	"fmt"
	"slices"
)

// Check validates a command table and returns every problem joined: path
// words are lower-case words; paths have one or two words and are unique;
// a one-word path is a command or a noun, not both; a runnable command
// has New and no Planned, a Planned one no New; only trailing positionals
// are optional and only the last repeats; every enum default is allowed;
// and binding each command's flags succeeds with valid, unique names
// whose mode conditions name defined flags. The root package's tests run
// it over cli.Registry().
func Check(specs []Spec) error {
	var errs []error
	seen := map[string]bool{}
	nouns := map[string]bool{}
	for _, s := range specs {
		if len(s.Path) == 2 {
			nouns[s.Path[0]] = true
		}
	}
	for i, s := range specs {
		name := s.Name()
		where := fmt.Sprintf("spec %d (%q)", i, name)
		if len(s.Path) < 1 || len(s.Path) > 2 {
			errs = append(errs, fmt.Errorf("%s: path must have one or two words", where))
			continue
		}
		for _, w := range s.Path {
			if !validWord(w) || w == "help" {
				errs = append(errs, fmt.Errorf("%s: invalid path word %q", where, w))
			}
		}
		if seen[name] {
			errs = append(errs, fmt.Errorf("%s: duplicate path", where))
		}
		seen[name] = true
		switch {
		case s.IsNoun():
			if !nouns[s.Path[0]] {
				errs = append(errs, fmt.Errorf("%s: noun description without verbs", where))
			}
			if len(s.Args) > 0 || s.Platform != nil || s.Deprecated != "" {
				errs = append(errs, fmt.Errorf("%s: a noun description has only a summary", where))
			}
			continue
		case len(s.Path) == 1:
			if nouns[name] {
				errs = append(errs, fmt.Errorf("%s: %q is both a command and a noun", where, name))
			}
		}
		if s.Summary == "" {
			errs = append(errs, fmt.Errorf("%s: missing summary", where))
		}
		if s.Planned != "" {
			if s.New != nil {
				errs = append(errs, fmt.Errorf("%s: a Planned command has no New", where))
			}
			continue
		}
		if s.New == nil {
			errs = append(errs, fmt.Errorf("%s: a runnable command needs New", where))
			continue
		}
		errs = append(errs, checkArgSpecs(where, s.Args)...)
		errs = append(errs, checkFlags(where, s)...)
	}
	return errors.Join(errs...)
}

func checkArgSpecs(where string, args []Arg) []error {
	var errs []error
	optional := false
	for i, a := range args {
		if a.Name == "" {
			errs = append(errs, fmt.Errorf("%s: positional %d has no name", where, i))
		}
		if optional && !a.Optional {
			errs = append(errs, fmt.Errorf("%s: required %s follows an optional positional", where, a.Name))
		}
		optional = optional || a.Optional
		if a.Repeat && i != len(args)-1 {
			errs = append(errs, fmt.Errorf("%s: only the last positional may repeat", where))
		}
		if a.Default != "" && !a.Optional {
			errs = append(errs, fmt.Errorf("%s: required %s has a default", where, a.Name))
		}
		if len(a.Values) > 0 && a.Default != "" && !slices.Contains(a.Values, a.Default) {
			errs = append(errs, fmt.Errorf("%s: default %q of %s is not one of its values", where, a.Default, a.Name))
		}
	}
	return errs
}

// checkFlags binds a fresh Command, turning definition panics into errors.
func checkFlags(where string, s Spec) (errs []error) {
	defer func() {
		if r := recover(); r != nil {
			errs = append(errs, fmt.Errorf("%s: %v", where, r))
		}
	}()
	specs := FlagSpecsOf(s)
	for _, f := range specs {
		for _, m := range f.Modes {
			for _, w := range m.When {
				if !slices.ContainsFunc(specs, func(o FlagSpec) bool { return o.Name == w }) {
					errs = append(errs, fmt.Errorf("%s: mode of --%s names undefined flag --%s", where, f.Name, w))
				}
			}
		}
	}
	return errs
}
