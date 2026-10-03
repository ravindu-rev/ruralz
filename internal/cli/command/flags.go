// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrHelp is returned by Flags.Parse when -h, -help or --help appears
// among the flags.
var ErrHelp = flag.ErrHelp

// FlagSpec describes one flag for help, completion and validation.
type FlagSpec struct {
	// Name is the flag name without dashes: "output".
	Name string
	// Placeholder is the value placeholder in help: "PATH", "yaml|json".
	// Empty for a bool flag.
	Placeholder string
	// Default is the default value as help shows it; empty for none.
	Default string
	// Usage is the one-line description.
	Usage string
	// When set, the Deprecated text marks the flag deprecated (spec 10
	// req 9).
	Deprecated string
	// Bool marks a flag that takes no value (--flag or --flag=true|false).
	Bool bool
	// Values are the allowed values of an enum flag outside every Mode.
	Values []string
	// Modes are the alternative defaults and values of an enum flag that
	// apply when other flags are set.
	Modes []Mode
	// Complete says how shells complete the value.
	Complete Completion
}

// Mode is an alternative set of values of an enum flag, active when any of
// When is set, such as render's --output with --effective (spec 10
// req 12). The first matching Mode wins.
type Mode struct {
	// When lists flag names; the mode applies when any is set.
	When []string
	// Default replaces the flag's value when the flag is not set.
	Default string
	// Values are the allowed values in this mode.
	Values []string
}

// Flags is the flag set of one invocation: one *flag.FlagSet with
// flag.ContinueOnError and output discarded (spec 10 req 4), plus the
// specs that help, completion and enum validation read.
type Flags struct {
	path     string
	fs       *flag.FlagSet
	specs    []FlagSpec
	enums    map[string]*enumValue
	set      map[string]bool
	setErr   error
	warnings []string
	parsed   bool
}

// NewFlags returns an empty flag set for the command path, such as
// "ruralz bundle validate".
func NewFlags(path string) *Flags {
	fs := flag.NewFlagSet(path, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return &Flags{path: path, fs: fs, enums: map[string]*enumValue{}, set: map[string]bool{}}
}

// String binds a string flag shown as --name PLACEHOLDER with default
// value. An empty value shows no default.
func (f *Flags) String(p *string, name, placeholder, value, usage string, c Completion) {
	*p = value
	f.define(FlagSpec{Name: name, Placeholder: placeholder, Default: value, Usage: usage, Complete: c},
		&stringValue{p: p})
}

// Enum binds a string flag whose value must be one of values; its
// placeholder lists them (yaml|json). The check runs after parsing, so
// modes added by EnumMode can depend on flags given later on the line.
func (f *Flags) Enum(p *string, name, value string, values []string, usage string) {
	if !slices.Contains(values, value) {
		panic(fmt.Sprintf("command: default %q of --%s is not one of its values", value, name))
	}
	*p = value
	v := &enumValue{stringValue: stringValue{p: p}}
	f.enums[name] = v
	f.define(FlagSpec{
		Name: name, Placeholder: strings.Join(values, "|"), Default: value, Usage: usage,
		Values: slices.Clone(values), Complete: CompleteValues,
	}, v)
}

// EnumMode adds a mode to the enum flag name: when any flag in when is
// set, the default is value and the allowed values are values.
func (f *Flags) EnumMode(name string, when []string, value string, values ...string) {
	i := f.index(name)
	if i < 0 || f.enums[name] == nil {
		panic(fmt.Sprintf("command: EnumMode on --%s, which is not an enum flag", name))
	}
	if !slices.Contains(values, value) {
		panic(fmt.Sprintf("command: mode default %q of --%s is not one of its values", value, name))
	}
	s := &f.specs[i]
	s.Modes = append(s.Modes, Mode{When: slices.Clone(when), Default: value, Values: slices.Clone(values)})
	s.Placeholder = strings.Join(enumUnion(*s), "|")
}

// Output binds --output FORMAT, the one helper for the output format of
// every command (spec 10 reqs 11, 15): value is the default and values
// the allowed formats.
func (f *Flags) Output(p *string, value string, values ...string) {
	f.Enum(p, "output", value, values, "Output format")
}

// OutputFile binds --output-file PATH (spec 10 req 17); write the data
// with WriteData.
func (f *Flags) OutputFile(p *string, usage string) {
	f.String(p, "output-file", "PATH", "", usage, CompleteFile)
}

// Bool binds a flag set by --name or --name=true|false.
func (f *Flags) Bool(p *bool, name, usage string) {
	*p = false
	f.define(FlagSpec{Name: name, Usage: usage, Bool: true}, &boolValue{f: f, name: name, p: p})
}

// Duration binds --name DURATION in time.ParseDuration form; the value
// must be greater than zero (spec 10 reqs 7, 94).
func (f *Flags) Duration(p *time.Duration, name string, value time.Duration, usage string) {
	*p = value
	def := ""
	if value != 0 {
		def = FormatDuration(value)
	}
	f.define(FlagSpec{Name: name, Placeholder: "DURATION", Default: def, Usage: usage},
		&durationValue{f: f, name: name, p: p})
}

// Deprecate marks the defined flag name as deprecated: using it prints
// "warning: --<name> is deprecated: <text>" (spec 10 req 9).
func (f *Flags) Deprecate(name, text string) {
	i := f.index(name)
	if i < 0 {
		panic(fmt.Sprintf("command: Deprecate on undefined flag --%s", name))
	}
	f.specs[i].Deprecated = text
}

// define registers a flag; a bad or repeated name is a programming error.
func (f *Flags) define(s FlagSpec, v flag.Value) {
	if !validWord(s.Name) || s.Name == "h" || s.Name == "help" {
		panic(fmt.Sprintf("command: invalid flag name %q", s.Name))
	}
	if f.index(s.Name) >= 0 {
		panic(fmt.Sprintf("command: flag --%s defined twice on %s", s.Name, f.path))
	}
	if f.parsed {
		panic("command: flag defined after Parse")
	}
	f.specs = append(f.specs, s)
	f.fs.Var(v, s.Name, s.Usage)
}

func (f *Flags) index(name string) int {
	return slices.IndexFunc(f.specs, func(s FlagSpec) bool { return s.Name == name })
}

// Specs returns the flag specs sorted by name.
func (f *Flags) Specs() []FlagSpec {
	out := slices.Clone(f.specs)
	slices.SortFunc(out, func(a, b FlagSpec) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// IsSet reports whether the command line set the flag name.
func (f *Flags) IsSet(name string) bool { return f.set[name] }

// Warnings returns the deprecation warnings of the parsed command line,
// one line each without a newline.
func (f *Flags) Warnings() []string { return slices.Clone(f.warnings) }

// value returns the current value of the flag name.
func (f *Flags) value(name string) (string, bool) {
	fl := f.fs.Lookup(name)
	if fl == nil {
		return "", false
	}
	return fl.Value.String(), true
}

// Parse parses args with interspersed flags (spec 10 req 5) and returns
// the positionals. Tokens after the first "--" are positionals verbatim;
// before it, flags may precede, follow or sit between positionals; a
// lone "-" is a positional; a repeated flag keeps its last value. It
// returns ErrHelp for -h, -help or --help, and a usage error (exit 2) for
// an unknown flag, a missing or invalid value and an enum value outside
// the allowed set. Parse is called once.
func (f *Flags) Parse(args []string) ([]string, error) {
	if f.parsed {
		panic("command: Parse called twice")
	}
	f.parsed = true
	head, tail := args, []string(nil)
	if i := slices.Index(args, "--"); i >= 0 {
		head, tail = args[:i], args[i+1:]
	}
	positionals := []string{}
	rest := head
	for {
		f.setErr = nil
		if err := f.fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, ErrHelp
			}
			return nil, &ExitError{Code: ExitNoResult, Err: &UsageError{Err: f.translate(err)}}
		}
		if f.fs.NArg() == 0 {
			break
		}
		positionals = append(positionals, f.fs.Arg(0))
		rest = f.fs.Args()[1:]
	}
	positionals = append(positionals, tail...)
	f.fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
	if err := f.checkEnums(); err != nil {
		return nil, err
	}
	for _, s := range f.Specs() {
		if s.Deprecated != "" && f.set[s.Name] {
			f.warnings = append(f.warnings, fmt.Sprintf("warning: --%s is deprecated: %s", s.Name, s.Deprecated))
		}
	}
	return positionals, nil
}

// checkEnums applies the active mode of every enum flag: its default when
// the flag is not set, else the allowed-values check (spec 10 req 15).
func (f *Flags) checkEnums() error {
	for _, s := range f.Specs() {
		v := f.enums[s.Name]
		if v == nil {
			continue
		}
		def, values := s.Default, s.Values
		for _, m := range s.Modes {
			if slices.ContainsFunc(m.When, f.IsSet) {
				def, values = m.Default, m.Values
				break
			}
		}
		if !f.set[s.Name] {
			*v.p = def
			continue
		}
		if !slices.Contains(values, *v.p) {
			return Usagef("unsupported --%s %q (want %s)", s.Name, *v.p, joinOr(values))
		}
	}
	return nil
}

// translate rewrites a flag package error in the --name form help uses.
func (f *Flags) translate(err error) error {
	if f.setErr != nil {
		return f.setErr
	}
	msg := err.Error()
	if name, ok := strings.CutPrefix(msg, "flag provided but not defined: -"); ok {
		names := make([]string, 0, len(f.specs))
		for _, s := range f.specs {
			names = append(names, s.Name)
		}
		if near := nearest(name, names); near != "" {
			return fmt.Errorf("unknown flag --%s (did you mean --%s?)", name, near)
		}
		return fmt.Errorf("unknown flag --%s", name)
	}
	if name, ok := strings.CutPrefix(msg, "flag needs an argument: -"); ok {
		return fmt.Errorf("flag needs an argument: --%s", name)
	}
	return errors.New(msg)
}

// enumUnion returns the values of s and of its modes in first-seen order.
func enumUnion(s FlagSpec) []string {
	out := slices.Clone(s.Values)
	for _, m := range s.Modes {
		for _, v := range m.Values {
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out
}

// joinOr joins words as "a", "a or b", "a, b or c".
func joinOr(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	default:
		return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
	}
}

// FormatDuration formats d without zero trailing units: 5m, 30s, 1h30m,
// 1h, 1.5s.
func FormatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// validWord reports whether s is a lower-case command or flag word:
// a letter, then letters, digits or single dashes, not ending in a dash.
func validWord(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' || s[len(s)-1] == '-' || strings.Contains(s, "--") {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// stringValue is a string flag.
type stringValue struct {
	p *string
}

func (v *stringValue) String() string {
	if v.p == nil {
		return ""
	}
	return *v.p
}

func (v *stringValue) Set(s string) error {
	*v.p = s
	return nil
}

// enumValue is a string flag checked against its values after parsing.
type enumValue struct {
	stringValue
}

// boolValue is a bool flag.
type boolValue struct {
	f    *Flags
	name string
	p    *bool
}

func (v *boolValue) String() string {
	if v.p == nil {
		return "false"
	}
	return strconv.FormatBool(*v.p)
}

func (v *boolValue) Set(s string) error {
	b, err := strconv.ParseBool(s)
	if err != nil {
		v.f.setErr = fmt.Errorf("invalid value %q for --%s: want true or false", s, v.name)
		return v.f.setErr
	}
	*v.p = b
	return nil
}

// IsBoolFlag lets the flag package accept --name without a value.
func (v *boolValue) IsBoolFlag() bool { return true }

// durationValue is a positive duration flag.
type durationValue struct {
	f    *Flags
	name string
	p    *time.Duration
}

func (v *durationValue) String() string {
	if v.p == nil {
		return ""
	}
	return FormatDuration(*v.p)
}

func (v *durationValue) Set(s string) error {
	d, err := time.ParseDuration(s)
	switch {
	case err != nil:
		v.f.setErr = fmt.Errorf("invalid value %q for --%s: want a duration such as 30s or 5m", s, v.name)
	case d <= 0:
		v.f.setErr = fmt.Errorf("invalid value %q for --%s: must be greater than zero", s, v.name)
	default:
		*v.p = d
		return nil
	}
	return v.f.setErr
}
