// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Tests for spec 10 reqs 4 to 7, 9, 11 and 15: Flags.Parse and the flag
// definitions, following the Flags.Parse rows of section 6.1.

// testFlags is a flag set with one flag of each kind.
type testFlags struct {
	f       *Flags
	env     string
	reason  string
	online  bool
	output  string
	timeout time.Duration
	effect  bool
}

func newTestFlags() *testFlags {
	t := &testFlags{f: NewFlags("ruralz test cmd")}
	t.f.String(&t.env, "env", "NAME", "", "Environment", CompleteNone)
	t.f.String(&t.reason, "reason", "TEXT", "none", "Reason", CompleteNone)
	t.f.Bool(&t.online, "online", "Online check")
	t.f.Bool(&t.effect, "effective", "Effective chains")
	t.f.Output(&t.output, "yaml", "yaml", "json")
	t.f.EnumMode("output", []string{"effective"}, "text", "text", "json")
	t.f.Duration(&t.timeout, "timeout", 5*time.Minute, "Timeout")
	return t
}

func TestParseReq5(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		pos     []string
		env     string
		reason  string
		online  bool
		output  string
		timeout time.Duration
	}{
		{"no arguments", nil, []string{}, "", "none", false, "yaml", 5 * time.Minute},
		{"equals form", []string{"--env=prod"}, []string{}, "prod", "none", false, "yaml", 5 * time.Minute},
		{"separate value", []string{"--env", "prod"}, []string{}, "prod", "none", false, "yaml", 5 * time.Minute},
		{"single dash", []string{"-env", "prod"}, []string{}, "prod", "none", false, "yaml", 5 * time.Minute},
		{"single dash equals", []string{"-env=prod"}, []string{}, "prod", "none", false, "yaml", 5 * time.Minute},
		{"bool", []string{"--online"}, []string{}, "", "none", true, "yaml", 5 * time.Minute},
		{"bool false", []string{"--online=false"}, []string{}, "", "none", false, "yaml", 5 * time.Minute},
		{"bool true", []string{"-online=true"}, []string{}, "", "none", true, "yaml", 5 * time.Minute},
		{
			"before, between and after",
			[]string{"--env", "a", "p1", "--online", "p2", "--timeout", "1s", "p3"},
			[]string{"p1", "p2", "p3"},
			"a", "none", true, "yaml", time.Second,
		},
		{
			"double dash keeps flags as positionals",
			[]string{"p1", "--", "--env", "x", "--"},
			[]string{"p1", "--env", "x", "--"},
			"", "none", false, "yaml", 5 * time.Minute,
		},
		{"literal double dash value", []string{"--reason=--", "p"}, []string{"p"}, "", "--", false, "yaml", 5 * time.Minute},
		{"lone dash is positional", []string{"-", "--env", "x", "-"}, []string{"-", "-"}, "x", "none", false, "yaml", 5 * time.Minute},
		{"repeated flag keeps the last", []string{"--env", "a", "--env=b", "-env", "c"}, []string{}, "c", "none", false, "yaml", 5 * time.Minute},
		{"value that looks like a flag", []string{"--reason", "--online"}, []string{}, "", "--online", false, "yaml", 5 * time.Minute},
		{"empty value", []string{"--env="}, []string{}, "", "none", false, "yaml", 5 * time.Minute},
		{"value with equals", []string{"--reason=a=b"}, []string{}, "", "a=b", false, "yaml", 5 * time.Minute},
		{"enum", []string{"--output", "json"}, []string{}, "", "none", false, "json", 5 * time.Minute},
		{"enum mode default", []string{"--effective"}, []string{}, "", "none", false, "text", 5 * time.Minute},
		{"enum mode set before its condition", []string{"--output=text", "x", "--effective"}, []string{"x"}, "", "none", false, "text", 5 * time.Minute},
		{"fractional duration", []string{"--timeout", "1.5s"}, []string{}, "", "none", false, "yaml", 1500 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tf := newTestFlags()
			pos, err := tf.f.Parse(c.args)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", c.args, err)
			}
			if !slices.Equal(pos, c.pos) {
				t.Errorf("positionals = %q, want %q", pos, c.pos)
			}
			got := fmt.Sprint(tf.env, tf.reason, tf.online, tf.output, tf.timeout)
			want := fmt.Sprint(c.env, c.reason, c.online, c.output, c.timeout)
			if got != want {
				t.Errorf("values = %s, want %s", got, want)
			}
		})
	}
}

func TestParseErrorsReq7(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--nope"}, "unknown flag --nope"},
		{[]string{"-onlin"}, "unknown flag --onlin (did you mean --online?)"},
		{[]string{"p", "--env"}, "flag needs an argument: --env"},
		{[]string{"--env", "a", "--", "x"}, ""},
		{[]string{"--online=yes"}, `invalid value "yes" for --online: want true or false`},
		{[]string{"--timeout", "5x"}, `invalid value "5x" for --timeout: want a duration such as 30s or 5m`},
		{[]string{"--timeout=0"}, `invalid value "0" for --timeout: must be greater than zero`},
		{[]string{"--timeout=-1s"}, `invalid value "-1s" for --timeout: must be greater than zero`},
		{[]string{"--output", "csv"}, `unsupported --output "csv" (want yaml or json)`},
		{[]string{"--output", "yaml", "--effective"}, `unsupported --output "yaml" (want text or json)`},
		{[]string{"---x"}, "bad flag syntax: ---x"},
		{[]string{"-=x"}, "bad flag syntax: -=x"},
	}
	for _, c := range cases {
		tf := newTestFlags()
		_, err := tf.f.Parse(c.args)
		if c.want == "" {
			if err != nil {
				t.Errorf("Parse(%q) = %v, want nil", c.args, err)
			}
			continue
		}
		ee, ok := errors.AsType[*ExitError](err)
		if !ok || ee.Code != ExitNoResult {
			t.Errorf("Parse(%q) = %v, want an exit 2 error", c.args, err)
			continue
		}
		if _, ok := errors.AsType[*UsageError](err); !ok {
			t.Errorf("Parse(%q) = %v, want a usage error", c.args, err)
		}
		if err.Error() != c.want {
			t.Errorf("Parse(%q) = %q, want %q", c.args, err.Error(), c.want)
		}
	}
}

func TestParseHelpReq6(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--h"}, {"-help"}, {"--help"}, {"p", "--env", "x", "--help"}, {"--help=1"}} {
		tf := newTestFlags()
		if _, err := tf.f.Parse(args); !errors.Is(err, ErrHelp) {
			t.Errorf("Parse(%q) = %v, want ErrHelp", args, err)
		}
	}
	tf := newTestFlags()
	if pos, err := tf.f.Parse([]string{"--", "--help"}); err != nil || !slices.Equal(pos, []string{"--help"}) {
		t.Errorf("Parse(-- --help) = %q, %v", pos, err)
	}
}

func TestIsSetSpecsAndWarningsReq9(t *testing.T) {
	tf := newTestFlags()
	tf.f.Deprecate("reason", "use --why")
	if _, err := tf.f.Parse([]string{"--reason", "x", "--online=false"}); err != nil {
		t.Fatal(err)
	}
	if !tf.f.IsSet("reason") || !tf.f.IsSet("online") || tf.f.IsSet("env") || tf.f.IsSet("nope") {
		t.Errorf("IsSet wrong: reason %v online %v env %v", tf.f.IsSet("reason"), tf.f.IsSet("online"), tf.f.IsSet("env"))
	}
	if got := tf.f.Warnings(); !slices.Equal(got, []string{"warning: --reason is deprecated: use --why"}) {
		t.Errorf("Warnings = %q", got)
	}
	var names []string
	for _, s := range tf.f.Specs() {
		names = append(names, s.Name)
	}
	if !slices.IsSorted(names) || len(names) != 6 {
		t.Errorf("Specs names = %q, want 6 sorted", names)
	}
	out := tf.f.Specs()[slices.IndexFunc(tf.f.Specs(), func(s FlagSpec) bool { return s.Name == "output" })]
	if out.Placeholder != "yaml|json|text" || out.Default != "yaml" || len(out.Modes) != 1 || out.Complete != CompleteValues {
		t.Errorf("output spec = %+v", out)
	}
	if v, ok := tf.f.value("output"); !ok || v != "yaml" {
		t.Errorf("value(output) = %q, %v", v, ok)
	}
	if _, ok := tf.f.value("nope"); ok {
		t.Errorf("value(nope) found")
	}
}

func TestFlagDefinitionPanics(t *testing.T) {
	var s string
	var b bool
	cases := map[string]func(f *Flags){
		"invalid name":        func(f *Flags) { f.String(&s, "Bad", "X", "", "", CompleteNone) },
		"dash end":            func(f *Flags) { f.String(&s, "bad-", "X", "", "", CompleteNone) },
		"double dash":         func(f *Flags) { f.String(&s, "a--b", "X", "", "", CompleteNone) },
		"help":                func(f *Flags) { f.Bool(&b, "help", "") },
		"h":                   func(f *Flags) { f.Bool(&b, "h", "") },
		"twice":               func(f *Flags) { f.Bool(&b, "x", ""); f.Bool(&b, "x", "") },
		"enum default":        func(f *Flags) { f.Enum(&s, "output", "csv", []string{"text"}, "") },
		"mode on non-enum":    func(f *Flags) { f.String(&s, "x", "X", "", "", CompleteNone); f.EnumMode("x", nil, "a", "a") },
		"mode default":        func(f *Flags) { f.Output(&s, "text", "text"); f.EnumMode("output", nil, "yaml", "json") },
		"deprecate undefined": func(f *Flags) { f.Deprecate("x", "gone") },
		"define after parse":  func(f *Flags) { _, _ = f.Parse(nil); f.Bool(&b, "x", "") },
		"parse twice":         func(f *Flags) { _, _ = f.Parse(nil); _, _ = f.Parse(nil) },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("no panic")
				}
			}()
			fn(NewFlags("ruralz x"))
		})
	}
}

func TestValueStrings(t *testing.T) {
	// The flag package calls String on zero values for its defaults.
	if (&stringValue{}).String() != "" || (&boolValue{}).String() != "false" || (&durationValue{}).String() != "" {
		t.Error("zero values do not print their zero")
	}
	d := 90 * time.Second
	if (&durationValue{p: &d}).String() != "1m30s" {
		t.Error("duration String")
	}
	b := true
	if (&boolValue{p: &b}).String() != "true" || !(&boolValue{}).IsBoolFlag() {
		t.Error("bool String or IsBoolFlag")
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		5 * time.Minute:                   "5m",
		30 * time.Second:                  "30s",
		time.Hour:                         "1h",
		90 * time.Minute:                  "1h30m",
		time.Hour + 30*time.Second:        "1h0m30s",
		1500 * time.Millisecond:           "1.5s",
		100 * time.Millisecond:            "100ms",
		2*time.Hour + 5*time.Minute + 3e9: "2h5m3s",
	}
	for d, want := range cases {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
		if back, err := time.ParseDuration(FormatDuration(d)); err != nil || back != d {
			t.Errorf("FormatDuration(%v) does not parse back: %v, %v", d, back, err)
		}
	}
}

func TestJoinOrAnd(t *testing.T) {
	cases := []struct {
		in      []string
		or, and string
	}{
		{nil, "", ""},
		{[]string{"a"}, "a", "a"},
		{[]string{"a", "b"}, "a or b", "a and b"},
		{[]string{"a", "b", "c"}, "a, b or c", "a, b and c"},
	}
	for _, c := range cases {
		if got := joinOr(c.in); got != c.or {
			t.Errorf("joinOr(%q) = %q", c.in, got)
		}
		if got := joinAnd(c.in); got != c.and {
			t.Errorf("joinAnd(%q) = %q", c.in, got)
		}
	}
}

func TestNearest(t *testing.T) {
	names := []string{"output", "output-file", "env", "environments", "online"}
	cases := map[string]string{
		"outptu":                 "output",
		"output-fil":             "output-file",
		"en":                     "env",
		"envirnments":            "environments",
		"x":                      "",
		"zzzzzz":                 "",
		"":                       "",
		strings.Repeat("a", 100): "",
	}
	for in, want := range cases {
		if got := nearest(in, names); got != want {
			t.Errorf("nearest(%q) = %q, want %q", in, got, want)
		}
	}
	if d := editDistance("kitten", "sitting"); d != 3 {
		t.Errorf("editDistance = %d, want 3", d)
	}
}

// FuzzFlagsParse checks section 6.3's parse round trip: Parse never
// panics, and a successful parse yields the same positionals and values
// as the canonical "flags first" order.
func FuzzFlagsParse(f *testing.F) {
	for _, seed := range []string{
		"a --env x b", "--env=x -- --online", "- --reason=-- p", "--output json --effective",
		"--timeout 1m30s p q", "--online=false --online", "-h", "--nope", "--env", "---x",
		"--output=text", "p -- -- --", "--reason --online", "-env=a -env b",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		args := strings.Fields(line)
		a := newTestFlags()
		pos, err := a.f.Parse(args)
		if err != nil {
			if errors.Is(err, ErrHelp) {
				return
			}
			if ee, ok := errors.AsType[*ExitError](err); !ok || ee.Code != ExitNoResult {
				t.Fatalf("Parse(%q) = %v, want ErrHelp or an exit 2 error", args, err)
			}
			return
		}
		var canon []string
		for _, s := range a.f.Specs() {
			if a.f.IsSet(s.Name) {
				v, _ := a.f.value(s.Name)
				canon = append(canon, "--"+s.Name+"="+v)
			}
		}
		canon = append(canon, "--")
		canon = append(canon, pos...)
		b := newTestFlags()
		pos2, err := b.f.Parse(canon)
		if err != nil {
			t.Fatalf("canonical Parse(%q) of %q = %v", canon, args, err)
		}
		if !slices.Equal(pos, pos2) {
			t.Fatalf("positionals %q, canonical %q (args %q)", pos, pos2, args)
		}
		got := fmt.Sprint(a.env, a.reason, a.online, a.effect, a.output, a.timeout)
		want := fmt.Sprint(b.env, b.reason, b.online, b.effect, b.output, b.timeout)
		if got != want {
			t.Fatalf("values %s, canonical %s (args %q)", got, want, args)
		}
		for _, s := range a.f.Specs() {
			if a.f.IsSet(s.Name) != b.f.IsSet(s.Name) {
				t.Fatalf("IsSet(%s) differs (args %q)", s.Name, args)
			}
		}
	})
}

// BenchmarkParse measures interspersed parsing of a typical command line.
func BenchmarkParse(b *testing.B) {
	args := []string{"./shop", "--env", "prod", "--environments", "env.yaml", "--output", "json", "--online"}
	for b.Loop() {
		tf := newTestFlags()
		if _, err := tf.f.Parse(args); err != nil {
			b.Fatal(err)
		}
	}
}
