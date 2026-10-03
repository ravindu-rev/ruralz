// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Execute runs one invocation of program (the binary name, "ruralz") over
// the command table specs and returns the process exit code (spec 10
// reqs 3 to 10, 13, 18 to 23).
//
// The first argument names a noun or a one-word command, the second the
// noun's verb; everything after the path goes to the command's Flags.Parse.
// -h, -help and --help at any level, "help", "help help" and
// "help <noun> [<verb>]" print that level's help to stdout and exit 0
// ("help help" prints the root help, which explains help); a bare
// invocation prints
// the root help to stderr and exits 2. Usage errors print
// "<program> <path>: <problem>" and "Run '<program> <path> --help' for
// usage." to stderr and exit 2. Planned commands exit 2 naming their
// milestone; a Platform refusal exits 2 before any other work. A command's
// error prints one line "<program> <path>: <message>" and maps to its exit
// code through Code; an ExitError without a cause adds nothing to that
// line and alone prints nothing (see printable). With --output json the
// problem document of a ProblemDocument error also goes to stdout.
func Execute(ctx context.Context, sio IO, program string, specs []Spec, args []string) int {
	t := newTable(program, specs)
	if len(args) == 0 {
		_ = t.writeRootHelp(sio.Stderr)
		return ExitNoResult
	}
	if isHelpToken(args[0]) {
		return writeHelp(sio, t.writeRootHelp(sio.Stdout))
	}
	if args[0] == "help" {
		return t.help(sio, args[1:])
	}
	tg, fail := t.resolve(args)
	if fail != nil {
		return fail.print(sio.Stderr)
	}
	if tg.spec == nil {
		n := tg.noun
		if len(tg.rest) > 0 && isHelpToken(tg.rest[0]) {
			return writeHelp(sio, t.writeNounHelp(sio.Stdout, n))
		}
		return (&failure{
			prefix: t.program + " " + n.name, usage: true,
			msg: fmt.Sprintf("missing verb (want %s)", joinOr(verbNames(n.built()))),
		}).print(sio.Stderr)
	}
	return t.run(ctx, sio, tg.spec, tg.rest)
}

// writeHelp maps the result of writing requested help to an exit code.
func writeHelp(sio IO, err error) int {
	if err != nil {
		_, _ = fmt.Fprintf(sio.Stderr, "write help: %v\n", err)
		return ExitNoResult
	}
	return ExitOK
}

// run executes one resolved command.
func (t *table) run(ctx context.Context, sio IO, s *Spec, rest []string) int {
	path := t.program + " " + s.Name()
	if s.Planned != "" {
		return plannedFailure(path, s.Planned).print(sio.Stderr)
	}
	cmd := s.New()
	f := NewFlags(path)
	cmd.Flags(f)
	positionals, perr := f.Parse(rest)
	if errors.Is(perr, ErrHelp) {
		return writeHelp(sio, t.writeCommandHelp(sio.Stdout, s, f.Specs()))
	}
	if s.Platform != nil {
		goos := sio.platform()
		if err := s.Platform(goos); err != nil {
			_, _ = fmt.Fprintf(sio.Stderr, "%s is not planned on %s: %v\n", path, goos, err)
			return ExitNoResult
		}
	}
	if perr != nil {
		return report(ctx, sio, path, f, perr)
	}
	if s.Deprecated != "" {
		_, _ = fmt.Fprintf(sio.Stderr, "warning: %s is deprecated: %s\n", path, s.Deprecated)
	}
	for _, w := range f.Warnings() {
		_, _ = fmt.Fprintln(sio.Stderr, w)
	}
	positionals, err := checkArgs(s.Args, positionals)
	if err != nil {
		return report(ctx, sio, path, f, err)
	}
	return report(ctx, sio, path, f, cmd.Run(ctx, sio, positionals))
}

// report prints err per spec 10 reqs 22 and 23 and returns its exit code.
func report(ctx context.Context, sio IO, path string, f *Flags, err error) int {
	code := Code(ctx, err)
	if err == nil {
		return code
	}
	if code == ExitInterrupted && isCancellation(err) {
		return code
	}
	if pd, ok := errors.AsType[ProblemDocument](err); ok && f != nil {
		if v, _ := f.value("output"); v == "json" {
			doc := pd.ProblemJSON()
			_, _ = sio.Stdout.Write(doc)
			if len(doc) > 0 && doc[len(doc)-1] != '\n' {
				_, _ = io.WriteString(sio.Stdout, "\n")
			}
		}
	}
	msg, ok := printable(err)
	if !ok {
		return code
	}
	_, _ = fmt.Fprintf(sio.Stderr, "%s: %s\n", path, oneLine(msg))
	if _, ok := errors.AsType[*UsageError](err); ok {
		_, _ = fmt.Fprintf(sio.Stderr, "Run '%s --help' for usage.\n", path)
	}
	return code
}

// printable returns the message of err without the "exit status N" of
// ExitErrors that carry no cause: such an error says that the command
// already printed its result. It reports false when nothing else remains,
// that is when the bare ExitError is reached through wrappers that only
// prefix its message (fmt.Errorf("check: %w", Negative()) stays silent).
// errors.Join keeps its other members, so errors.Join(errors.New("real
// failure"), Negative()) prints "real failure". Any other message is kept
// whole (spec 10 req 22).
func printable(err error) (string, bool) {
	if ee, ok := err.(*ExitError); ok { //nolint:errorlint // the wrapper chain is walked here one level at a time.
		if ee.Err == nil {
			return "", false
		}
		return printable(ee.Err)
	}
	msg := err.Error()
	switch u := err.(type) { //nolint:errorlint // the wrapper chain is walked here one level at a time.
	case interface{ Unwrap() error }:
		inner := u.Unwrap()
		if inner == nil {
			return msg, true
		}
		prefix, ok := strings.CutSuffix(msg, inner.Error())
		if !ok {
			return msg, true
		}
		rest, ok := printable(inner)
		if !ok {
			return "", false
		}
		return prefix + rest, true
	case interface{ Unwrap() []error }:
		members := slices.DeleteFunc(slices.Clone(u.Unwrap()), func(e error) bool { return e == nil })
		texts := make([]string, 0, len(members))
		for _, e := range members {
			texts = append(texts, e.Error())
		}
		if msg != strings.Join(texts, "\n") {
			return msg, true
		}
		kept := make([]string, 0, len(members))
		for _, e := range members {
			if t, ok := printable(e); ok {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			return "", false
		}
		return strings.Join(kept, "\n"), true
	default:
		return msg, true
	}
}

// isCancellation reports whether err only says that the invocation was
// canceled, which needs no message after a signal.
func isCancellation(err error) bool {
	if _, ok := errors.AsType[Interrupted](err); ok {
		return true
	}
	return errors.Is(err, context.Canceled)
}

// oneLine keeps an error message on one line (spec 10 req 22).
func oneLine(msg string) string {
	msg = strings.TrimRight(msg, "\n")
	return strings.ReplaceAll(msg, "\n", "; ")
}

// checkArgs checks the positional count and enumerated values, and
// replaces omitted optional arguments by their defaults.
func checkArgs(args []Arg, pos []string) ([]string, error) {
	required := 0
	for _, a := range args {
		if !a.Optional {
			required++
		}
	}
	repeat := len(args) > 0 && args[len(args)-1].Repeat
	if !repeat && len(pos) > len(args) {
		return nil, Usagef("unexpected argument %q", pos[len(args)])
	}
	if len(pos) < required {
		missing := make([]string, 0, required-len(pos))
		for _, a := range args[len(pos):required] {
			missing = append(missing, a.Name)
		}
		noun := "argument"
		if len(missing) > 1 {
			noun = "arguments"
		}
		return nil, Usagef("missing %s %s", noun, joinAnd(missing))
	}
	for i, p := range pos {
		a := args[min(i, len(args)-1)]
		if len(a.Values) > 0 && !slices.Contains(a.Values, p) {
			return nil, Usagef("unsupported %s %q (want %s)", a.Name, p, joinOr(a.Values))
		}
	}
	for i := len(pos); i < len(args) && args[i].Default != ""; i++ {
		pos = append(pos, args[i].Default)
	}
	return pos, nil
}

// joinAnd joins words as "a", "a and b", "a, b and c".
func joinAnd(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// isHelpToken reports whether tok asks for help: -h, --h, -help, --help,
// optionally with "=value" as the flag package accepts.
func isHelpToken(tok string) bool {
	name, ok := strings.CutPrefix(tok, "-")
	if !ok {
		return false
	}
	name = strings.TrimPrefix(name, "-")
	name, _, _ = strings.Cut(name, "=")
	return name == "h" || name == "help"
}

// failure is a dispatch problem printed before any command runs.
type failure struct {
	prefix string // "ruralz" or "ruralz bundle"; empty for a bare message
	msg    string
	usage  bool
}

// print writes the failure to w and returns exit code 2.
func (f *failure) print(w io.Writer) int {
	if f.prefix == "" {
		_, _ = fmt.Fprintln(w, f.msg)
		return ExitNoResult
	}
	_, _ = fmt.Fprintf(w, "%s: %s\n", f.prefix, f.msg)
	if f.usage {
		_, _ = fmt.Fprintf(w, "Run '%s --help' for usage.\n", f.prefix)
	}
	return ExitNoResult
}

// plannedFailure is spec 10 req 8's message for a command not built yet.
func plannedFailure(path, milestone string) *failure {
	return &failure{msg: fmt.Sprintf("%s is Planned (%s) and not in this build", path, milestone)}
}

// table indexes a command table for dispatch, help and completion.
type table struct {
	program string
	tops    map[string]*Spec // one-word commands
	nouns   map[string]*noun
}

// noun groups the verbs of one noun.
type noun struct {
	name  string
	doc   *Spec   // noun description, or nil
	verbs []*Spec // sorted by verb; runnable and Planned
}

// built returns the runnable verbs.
func (n *noun) built() []*Spec {
	out := make([]*Spec, 0, len(n.verbs))
	for _, v := range n.verbs {
		if v.Planned == "" {
			out = append(out, v)
		}
	}
	return out
}

// planned returns the earliest milestone of a noun without runnable
// verbs, or "" when some verb is built.
func (n *noun) planned() string {
	m := ""
	for _, v := range n.verbs {
		if v.Planned == "" {
			return ""
		}
		if m == "" || v.Planned < m {
			m = v.Planned
		}
	}
	return m
}

func (n *noun) verb(name string) *Spec {
	for _, v := range n.verbs {
		if v.Path[1] == name {
			return v
		}
	}
	return nil
}

func verbNames(specs []*Spec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Path[len(s.Path)-1])
	}
	return out
}

func newTable(program string, specs []Spec) *table {
	t := &table{program: program, tops: map[string]*Spec{}, nouns: map[string]*noun{}}
	get := func(name string) *noun {
		n := t.nouns[name]
		if n == nil {
			n = &noun{name: name}
			t.nouns[name] = n
		}
		return n
	}
	for i := range specs {
		s := &specs[i]
		switch {
		case len(s.Path) == 2:
			n := get(s.Path[0])
			n.verbs = append(n.verbs, s)
		case s.IsNoun():
			get(s.Path[0]).doc = s
		case len(s.Path) == 1:
			t.tops[s.Path[0]] = s
		}
	}
	for name, n := range t.nouns {
		if len(n.verbs) == 0 {
			delete(t.nouns, name)
			continue
		}
		slices.SortStableFunc(n.verbs, func(a, b *Spec) int { return strings.Compare(a.Path[1], b.Path[1]) })
	}
	return t
}

// target is what the leading arguments name: a command or only a noun.
type target struct {
	spec *Spec
	noun *noun
	rest []string
}

// resolve finds the command path at the start of args (len(args) > 0).
func (t *table) resolve(args []string) (target, *failure) {
	tok := args[0]
	if strings.HasPrefix(tok, "-") && tok != "-" {
		return target{}, &failure{
			prefix: t.program, usage: true,
			msg: fmt.Sprintf("unknown flag %s (flags follow the command)", tok),
		}
	}
	if s, ok := t.tops[tok]; ok {
		return target{spec: s, rest: args[1:]}, nil
	}
	n, ok := t.nouns[tok]
	if !ok {
		msg := fmt.Sprintf("unknown command %q", tok)
		if near := nearest(tok, t.topWords()); near != "" {
			msg += fmt.Sprintf(" (did you mean %q?)", near)
		}
		return target{}, &failure{prefix: t.program, usage: true, msg: msg}
	}
	if m := n.planned(); m != "" {
		if len(args) > 1 {
			if v := n.verb(args[1]); v != nil {
				return target{spec: v, rest: args[2:]}, nil
			}
		}
		return target{}, plannedFailure(t.program+" "+n.name, m)
	}
	if len(args) == 1 || strings.HasPrefix(args[1], "-") {
		return target{noun: n, rest: args[1:]}, nil
	}
	if v := n.verb(args[1]); v != nil {
		return target{spec: v, rest: args[2:]}, nil
	}
	msg := fmt.Sprintf("unknown verb %q", args[1])
	if near := nearest(args[1], verbNames(n.built())); near != "" {
		msg += fmt.Sprintf(" (did you mean %q?)", near)
	}
	return target{}, &failure{prefix: t.program + " " + n.name, usage: true, msg: msg}
}

// topWords returns the runnable nouns and one-word commands, sorted.
func (t *table) topWords() []string {
	out := make([]string, 0, len(t.tops)+len(t.nouns))
	for name, s := range t.tops {
		if s.Planned == "" {
			out = append(out, name)
		}
	}
	for name, n := range t.nouns {
		if n.planned() == "" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// help serves "help [<noun> [<verb>]]". "help help" asks about help
// itself, which the root help describes.
func (t *table) help(sio IO, args []string) int {
	args = slices.DeleteFunc(slices.Clone(args), isHelpToken)
	if len(args) > 0 && args[0] == "help" {
		if len(args) > 1 {
			return (&failure{
				prefix: t.program + " help", usage: true,
				msg: fmt.Sprintf("unexpected argument %q", args[1]),
			}).print(sio.Stderr)
		}
		args = nil
	}
	if len(args) == 0 {
		return writeHelp(sio, t.writeRootHelp(sio.Stdout))
	}
	tg, fail := t.resolve(args)
	if fail != nil {
		return fail.print(sio.Stderr)
	}
	if len(tg.rest) > 0 {
		return (&failure{
			prefix: t.program + " help", usage: true,
			msg: fmt.Sprintf("unexpected argument %q", tg.rest[0]),
		}).print(sio.Stderr)
	}
	if tg.spec == nil {
		return writeHelp(sio, t.writeNounHelp(sio.Stdout, tg.noun))
	}
	s := tg.spec
	if s.Planned != "" {
		return plannedFailure(t.program+" "+s.Name(), s.Planned).print(sio.Stderr)
	}
	return writeHelp(sio, t.writeCommandHelp(sio.Stdout, s, FlagSpecsOf(*s)))
}
