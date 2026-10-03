// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
)

// HelpFlag is the spec of the built-in -h, -help, --help flag that help
// and completion list beside a command's own flags.
func HelpFlag() FlagSpec { return FlagSpec{Name: "help", Usage: "Print this help", Bool: true} }

// FlagSpecsOf returns the flag specs of a runnable command, sorted by
// name, by binding a fresh Command to an empty flag set. It returns nil
// for a Planned command and a noun description.
func FlagSpecsOf(s Spec) []FlagSpec {
	if s.New == nil {
		return nil
	}
	f := NewFlags(s.Name())
	s.New().Flags(f)
	return f.Specs()
}

// writeRootHelp lists every runnable command (spec 10 req 6).
func (t *table) writeRootHelp(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Usage: %s <noun> <verb> [ARGS] [FLAGS]\n\nCommands:\n", t.program)
	var rows []*Spec
	for _, s := range t.tops {
		if s.Planned == "" {
			rows = append(rows, s)
		}
	}
	for _, n := range t.nouns {
		rows = append(rows, n.built()...)
	}
	slices.SortFunc(rows, func(a, b *Spec) int { return strings.Compare(a.Name(), b.Name()) })
	tw := tabwriter.NewWriter(&b, 0, 8, 2, ' ', 0)
	for _, s := range rows {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\n", s.Name(), listSummary(s))
	}
	_ = tw.Flush()
	fmt.Fprintf(&b, "\nRun '%[1]s help <command>' or '%[1]s <command> --help' for details.\n", t.program)
	_, err := io.WriteString(w, b.String())
	return err
}

// writeNounHelp lists the runnable verbs of n.
func (t *table) writeNounHelp(w io.Writer, n *noun) error {
	path := t.program + " " + n.name
	var b strings.Builder
	fmt.Fprintf(&b, "Usage: %s <verb> [ARGS] [FLAGS]\n\n", path)
	if n.doc != nil && n.doc.Summary != "" {
		fmt.Fprintf(&b, "%s\n\n", sentence(n.doc.Summary))
	}
	b.WriteString("Verbs:\n")
	tw := tabwriter.NewWriter(&b, 0, 8, 2, ' ', 0)
	for _, s := range n.built() {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\n", s.Path[1], listSummary(s))
	}
	_ = tw.Flush()
	fmt.Fprintf(&b, "\nRun '%s <verb> --help' for details.\n", path)
	_, err := io.WriteString(w, b.String())
	return err
}

// writeCommandHelp prints usage, summary, positionals and flags in
// --name PLACEHOLDER form sorted by name, with defaults and allowed values.
func (t *table) writeCommandHelp(w io.Writer, s *Spec, flags []FlagSpec) error {
	path := t.program + " " + s.Name()
	var b strings.Builder
	fmt.Fprintf(&b, "Usage: %s", path)
	for _, a := range s.Args {
		fmt.Fprintf(&b, " %s", argUsage(a))
	}
	b.WriteString(" [FLAGS]\n")
	if s.Summary != "" {
		fmt.Fprintf(&b, "\n%s\n", sentence(s.Summary))
	}
	if s.Deprecated != "" {
		fmt.Fprintf(&b, "\nDeprecated: %s\n", s.Deprecated)
	}
	if h := strings.Trim(s.Help, "\n"); h != "" {
		fmt.Fprintf(&b, "\n%s\n", h)
	}
	if len(s.Args) > 0 {
		b.WriteString("\nArguments:\n")
		tw := tabwriter.NewWriter(&b, 0, 8, 2, ' ', 0)
		for _, a := range s.Args {
			_, _ = fmt.Fprintf(tw, "  %s\t%s\n", a.Name, argDetail(a))
		}
		_ = tw.Flush()
	}
	all := append(slices.Clone(flags), HelpFlag())
	slices.SortFunc(all, func(a, b FlagSpec) int { return strings.Compare(a.Name, b.Name) })
	b.WriteString("\nFlags:\n")
	tw := tabwriter.NewWriter(&b, 0, 8, 2, ' ', 0)
	for _, f := range all {
		name := "--" + f.Name
		if !f.Bool && f.Placeholder != "" {
			name += " " + f.Placeholder
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\n", name, flagDetail(f))
	}
	_ = tw.Flush()
	_, err := io.WriteString(w, b.String())
	return err
}

// listSummary is a command's summary in a command list.
func listSummary(s *Spec) string {
	if s.Deprecated != "" {
		return s.Summary + " (deprecated)"
	}
	return s.Summary
}

// sentence ends s with a period unless it already ends a sentence.
func sentence(s string) string {
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, "?") || strings.HasSuffix(s, "!") {
		return s
	}
	return s + "."
}

// argUsage is the usage-line form of a positional: DIR, [DIR], FILE...
func argUsage(a Arg) string {
	s := a.Name
	if a.Repeat {
		s += "..."
	}
	if a.Optional {
		s = "[" + s + "]"
	}
	return s
}

// argDetail is a positional's help text with its values and default.
func argDetail(a Arg) string {
	var parts []string
	if len(a.Values) > 0 {
		parts = append(parts, joinOr(a.Values))
	}
	if a.Default != "" {
		parts = append(parts, "default "+a.Default)
	}
	return withDetails(a.Usage, parts)
}

// flagDetail is a flag's help text with its values, modes, default and
// deprecation.
func flagDetail(f FlagSpec) string {
	var parts []string
	switch {
	case len(f.Values) > 0:
		parts = append(parts, joinOr(f.Values)+", default "+f.Default)
		for _, m := range f.Modes {
			when := make([]string, 0, len(m.When))
			for _, n := range m.When {
				when = append(when, "--"+n)
			}
			parts = append(parts, fmt.Sprintf("with %s: %s, default %s", joinOr(when), joinOr(m.Values), m.Default))
		}
	case f.Default != "":
		parts = append(parts, "default "+f.Default)
	}
	if f.Deprecated != "" {
		parts = append(parts, "deprecated: "+f.Deprecated)
	}
	return withDetails(f.Usage, parts)
}

func withDetails(usage string, parts []string) string {
	if len(parts) == 0 {
		return usage
	}
	d := "(" + strings.Join(parts, "; ") + ")"
	if usage == "" {
		return d
	}
	return usage + " " + d
}
