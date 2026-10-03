// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Tests for the IO of spec 10 req 13 and the command table model and
// checks of reqs 3, 4 and 8.

func TestNewIOReq13(t *testing.T) {
	env := []string{"A=1", "B=x=y", "A=2", "NOEQ", "=C:=C:\\dir", "Path=/bin"}
	now := func() time.Time { return time.Unix(0, 0) }
	sio := NewIO(nil, nil, env, now, "linux")
	env[0] = "A=changed" // the snapshot is a copy
	cases := []struct {
		key, val string
		ok       bool
	}{
		{"A", "1", true},
		{"B", "x=y", true},
		{"NOEQ", "", false},
		{"=C:", "C:\\dir", true},
		{"PATH", "", false},
		{"Path", "/bin", true},
		{"missing", "", false},
	}
	for _, c := range cases {
		if v, ok := sio.LookupEnv(c.key); v != c.val || ok != c.ok {
			t.Errorf("LookupEnv(%q) = %q, %v; want %q, %v", c.key, v, ok, c.val, c.ok)
		}
	}
	if sio.GOOS != "linux" || !sio.Now().Equal(time.Unix(0, 0)) || len(sio.Environ) != 6 {
		t.Errorf("NewIO fields: %+v", sio)
	}
	win := NewIO(nil, nil, env, now, "windows")
	if v, ok := win.LookupEnv("PATH"); !ok || v != "/bin" {
		t.Errorf("windows LookupEnv(PATH) = %q, %v", v, ok)
	}
	if d := NewIO(nil, nil, nil, now, ""); d.GOOS != runtime.GOOS || d.platform() != runtime.GOOS {
		t.Errorf("default GOOS = %q", d.GOOS)
	}
	if (IO{}).platform() != runtime.GOOS {
		t.Error("zero IO platform")
	}
}

func TestNotPlannedOnReq10(t *testing.T) {
	reasons := map[string]string{"windows": "no ruralzd"}
	gate := NotPlannedOn(reasons)
	reasons["linux"] = "later change" // copied
	if err := gate("windows"); err == nil || err.Error() != "no ruralzd" {
		t.Errorf("windows: %v", err)
	}
	if err := gate("linux"); err != nil {
		t.Errorf("linux: %v", err)
	}
}

func TestCompletionString(t *testing.T) {
	for c, want := range map[Completion]string{
		CompleteNone: "none", CompleteFile: "file", CompleteDir: "dir", CompleteValues: "values", Completion(9): "unknown",
	} {
		if c.String() != want {
			t.Errorf("%d.String() = %q", c, c.String())
		}
	}
}

func TestExitErrorReq18(t *testing.T) {
	if ExitOK != 0 || ExitNegative != 1 || ExitNoResult != 2 || ExitWaiting != 3 || ExitInterrupted != 130 {
		t.Fatal("exit codes changed; they never change once released")
	}
	if got := (&ExitError{Code: 3}).Error(); got != "exit status 3" {
		t.Errorf("Error() = %q", got)
	}
	cause := errors.New("cause")
	e := &ExitError{Code: 2, Err: cause}
	if e.Error() != "cause" || !errors.Is(e, cause) {
		t.Errorf("ExitError does not wrap its cause")
	}
	u := Usagef("bad %s: %w", "flag", cause)
	if !errors.Is(u, cause) || u.Error() != "bad flag: cause" {
		t.Errorf("Usagef = %v", u)
	}
	if ue, ok := errors.AsType[*UsageError](u); !ok || ue.Unwrap() == nil {
		t.Error("Usagef is not a UsageError")
	}
	if Code(context.Background(), NoResult(cause)) != 2 || !errors.Is(NoResult(cause), cause) {
		t.Error("NoResult")
	}
	if Code(context.Background(), Exit(1)) != 1 {
		t.Error("Exit")
	}
}

func noop() Command { return &nopCommand{} }

type nopCommand struct{ bind func(*Flags) }

func (n *nopCommand) Flags(f *Flags) {
	if n.bind != nil {
		n.bind(f)
	}
}

func (n *nopCommand) Run(context.Context, IO, []string) error { return nil }

// TestCheck covers the table rules Check enforces for cli.Registry().
func TestCheck(t *testing.T) {
	var s string
	cases := []struct {
		name  string
		specs []Spec
		want  string
	}{
		{"valid", []Spec{
			{Path: []string{"bundle"}, Summary: "Bundles"},
			{
				Path: []string{"bundle", "build"}, Summary: "Build", New: noop,
				Args: []Arg{{Name: "DIR", Optional: true, Default: "."}},
			},
			{Path: []string{"bundle", "push"}, Summary: "Push", Planned: "M2"},
			{Path: []string{"version"}, Summary: "Version", New: noop},
		}, ""},
		{"empty path", []Spec{{Summary: "x", New: noop}}, "one or two words"},
		{"long path", []Spec{{Path: []string{"a", "b", "c"}, Summary: "x", New: noop}}, "one or two words"},
		{"bad word", []Spec{{Path: []string{"Bundle"}, Summary: "x", New: noop}}, "invalid path word"},
		{"help word", []Spec{{Path: []string{"help"}, Summary: "x", New: noop}}, "invalid path word"},
		{"duplicate", []Spec{{Path: []string{"v"}, Summary: "x", New: noop}, {Path: []string{"v"}, Summary: "x", New: noop}}, "duplicate path"},
		{"noun without verbs", []Spec{{Path: []string{"dev"}, Summary: "x"}}, "noun description without verbs"},
		{"noun with args", []Spec{
			{Path: []string{"dev"}, Summary: "x", Args: []Arg{{Name: "X"}}},
			{Path: []string{"dev", "run"}, Summary: "x", New: noop},
		}, "only a summary"},
		{"command and noun", []Spec{
			{Path: []string{"dev"}, Summary: "x", New: noop},
			{Path: []string{"dev", "run"}, Summary: "x", New: noop},
		}, "both a command and a noun"},
		{"missing summary", []Spec{{Path: []string{"v"}, New: noop}}, "missing summary"},
		{"planned with New", []Spec{{Path: []string{"v"}, Summary: "x", Planned: "M2", New: noop}}, "has no New"},
		{"runnable without New", []Spec{{Path: []string{"a", "b"}, Summary: "x"}}, "needs New"},
		{"optional before required", []Spec{{
			Path: []string{"v"}, Summary: "x", New: noop,
			Args: []Arg{{Name: "A", Optional: true}, {Name: "B"}},
		}}, "follows an optional"},
		{"repeat not last", []Spec{{
			Path: []string{"v"}, Summary: "x", New: noop,
			Args: []Arg{{Name: "A", Repeat: true}, {Name: "B"}},
		}}, "only the last positional"},
		{"unnamed", []Spec{{Path: []string{"v"}, Summary: "x", New: noop, Args: []Arg{{}}}}, "has no name"},
		{"required default", []Spec{{
			Path: []string{"v"}, Summary: "x", New: noop,
			Args: []Arg{{Name: "A", Default: "."}},
		}}, "has a default"},
		{"default outside values", []Spec{{
			Path: []string{"v"}, Summary: "x", New: noop,
			Args: []Arg{{Name: "A", Optional: true, Default: "c", Values: []string{"a", "b"}}},
		}}, "not one of its values"},
		{"flag panic", []Spec{{Path: []string{"v"}, Summary: "x", New: func() Command {
			return &nopCommand{bind: func(f *Flags) { f.Bool(new(bool), "Bad", "") }}
		}}}, "invalid flag name"},
		{"mode condition", []Spec{{Path: []string{"v"}, Summary: "x", New: func() Command {
			return &nopCommand{bind: func(f *Flags) {
				f.Output(&s, "text", "text", "json")
				f.EnumMode("output", []string{"effective"}, "json", "json")
			}}
		}}}, "names undefined flag --effective"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Check(c.specs)
			if c.want == "" {
				if err != nil {
					t.Fatalf("Check = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Check = %v, want %q", err, c.want)
			}
		})
	}
}

// TestFlagSpecsOf covers the flag view help and completion read.
func TestFlagSpecsOf(t *testing.T) {
	if FlagSpecsOf(Spec{Path: []string{"x"}, Planned: "M2"}) != nil {
		t.Error("Planned spec has flags")
	}
	var s string
	var b bool
	spec := Spec{Path: []string{"v"}, New: func() Command {
		return &nopCommand{bind: func(f *Flags) {
			f.Bool(&b, "zeta", "Z")
			f.String(&s, "alpha", "A", "", "A", CompleteFile)
		}}
	}}
	got := FlagSpecsOf(spec)
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zeta" || !got[1].Bool {
		t.Errorf("FlagSpecsOf = %+v", got)
	}
	if h := HelpFlag(); h.Name != "help" || !h.Bool {
		t.Errorf("HelpFlag = %+v", h)
	}
}

type failAfter struct{ n int }

func (f *failAfter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("closed pipe")
	}
	f.n--
	return len(p), nil
}

// TestHelpWriteFailure covers a help destination that fails.
func TestHelpWriteFailure(t *testing.T) {
	specs := []Spec{{Path: []string{"a", "b"}, Summary: "x", New: noop}}
	for _, args := range [][]string{{"--help"}, {"a", "--help"}, {"a", "b", "--help"}, {"help", "a", "b"}} {
		var errOut bytes.Buffer
		sio := IO{Stdout: &failAfter{}, Stderr: &errOut}
		if code := Execute(context.Background(), sio, "ruralz", specs, args); code != ExitNoResult ||
			!strings.HasPrefix(errOut.String(), "write help: closed pipe") {
			t.Errorf("%q: exit %d, stderr %q", args, code, errOut.String())
		}
	}
}

func TestIsHelpToken(t *testing.T) {
	for tok, want := range map[string]bool{
		"-h": true, "--h": true, "-help": true, "--help": true, "--help=true": true,
		"help": false, "-": false, "--": false, "---help": false, "--helpx": false, "-x": false,
	} {
		if isHelpToken(tok) != want {
			t.Errorf("isHelpToken(%q) = %v", tok, !want)
		}
	}
}

func TestOneLine(t *testing.T) {
	if got := oneLine("a\nb\n"); got != "a; b" {
		t.Errorf("oneLine = %q", got)
	}
	if !isCancellation(context.Canceled) || isCancellation(errors.New("x")) {
		t.Error("isCancellation")
	}
}

// nilCause unwraps to nil.
type nilCause struct{}

func (nilCause) Error() string { return "no cause" }
func (nilCause) Unwrap() error { return nil }

// TestPrintable covers the stderr line of spec 10 req 22 when an
// ExitError without a cause sits in the chain; print false means silent.
func TestPrintable(t *testing.T) {
	neg := Negative()
	cases := []struct {
		name  string
		err   error
		want  string
		print bool
	}{
		{"plain", errors.New("boom"), "boom", true},
		{"bare exit", neg, "", false},
		{"exit with cause", NoResultf("cannot read %s", "dir"), "cannot read dir", true},
		{"prefix wrapper", fmt.Errorf("check: %w", neg), "", false},
		{"two prefix wrappers", fmt.Errorf("a: %w", fmt.Errorf("b: %w", neg)), "", false},
		{"wrapper of a cause", fmt.Errorf("a: %w", errors.New("b")), "a: b", true},
		{"join", errors.Join(errors.New("real failure"), neg), "real failure", true},
		{"join, failure last", errors.Join(neg, errors.New("real failure")), "real failure", true},
		{"join of bare exits", errors.Join(neg, Exit(ExitWaiting)), "", false},
		{"wrapped join", fmt.Errorf("x: %w", errors.Join(neg, errors.New("y"))), "x: y", true},
		{"join inside a cause", NoResult(errors.Join(errors.New("a"), neg)), "a", true},
		{"suffix wrapper kept whole", fmt.Errorf("%w (while checking)", neg), "exit status 1 (while checking)", true},
		{"several %w kept whole", fmt.Errorf("%w; %w", errors.New("a"), neg), "a; exit status 1", true},
		{"nil cause", nilCause{}, "no cause", true},
		{"usage", Usagef("bad %s", "flag"), "bad flag", true},
		{"empty cause kept", fmt.Errorf("failed: %w", errors.New("")), "failed: ", true},
		{"empty member kept", errors.Join(errors.New(""), neg), "", true},
	}
	for _, c := range cases {
		if got, ok := printable(c.err); got != c.want || ok != c.print {
			t.Errorf("printable(%s) = %q, %v, want %q, %v", c.name, got, ok, c.want, c.print)
		}
	}
}
