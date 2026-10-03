// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// Tests for spec 10 section 2.1 (reqs 3 to 12) and 2.2 (reqs 13, 15,
// 18 to 23) through Execute over the M1 table, plus the golden help of
// section 6.2: root, each noun and each of the ten commands.

// update rewrites the golden files: go test ./internal/cli/command -update.
var update = flag.Bool("update", false, "rewrite testdata golden files")

func fixedNow() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// execute runs one invocation over specs on goos.
func execute(t *testing.T, specs []command.Spec, goos string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return executeCtx(t.Context(), specs, goos, args...)
}

func executeCtx(ctx context.Context, specs []command.Spec, goos string, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	sio := command.NewIO(&out, &errOut, []string{"HOME=/home/test"}, fixedNow, goos)
	code = command.Execute(ctx, sio, "ruralz", specs, args)
	return code, out.String(), errOut.String()
}

// golden compares got with testdata/<name>, rewriting it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", filepath.FromSlash(name))
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: a golden file under testdata.
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch (run with -update to accept)\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// TestM1TableIsValid checks the fixture with Check and pins the ten M1
// commands of req 3.
func TestM1TableIsValid(t *testing.T) {
	specs := m1Table(echo)
	if err := command.Check(specs); err != nil {
		t.Fatalf("Check(m1Table) = %v", err)
	}
	got := runnable(specs)
	want := m1Paths()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("runnable commands = %v, want %v", got, want)
	}
}

// TestGoldenHelp covers req 6 and section 6.2: every help form prints the
// same text to stdout and exits 0.
func TestGoldenHelp(t *testing.T) {
	specs := m1Table(echo)
	type form struct {
		name  string
		forms [][]string
	}
	cases := []form{{"root", [][]string{{"--help"}, {"-h"}, {"-help"}, {"help"}, {"help", "--help"}, {"help", "help"}}}}
	for _, noun := range []string{"bundle", "dev", "node"} {
		cases = append(cases, form{noun, [][]string{{noun, "--help"}, {noun, "-h"}, {"help", noun}}})
	}
	for _, p := range m1Paths() {
		name := strings.Join(p, "-")
		forms := [][]string{
			append(append([]string{}, p...), "--help"),
			append(append([]string{}, p...), "-h"),
			append([]string{"help"}, p...),
		}
		cases = append(cases, form{name, forms})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var first string
			for i, args := range c.forms {
				code, out, errOut := execute(t, specs, "linux", args...)
				if code != command.ExitOK || errOut != "" {
					t.Fatalf("%q: exit %d, stderr %q", args, code, errOut)
				}
				if i == 0 {
					first = out
					golden(t, "help/"+c.name+".golden", out)
				} else if out != first {
					t.Errorf("%q printed different help:\n%s", args, out)
				}
			}
		})
	}
}

// TestHelpOmitsPlanned covers req 8: Planned commands are absent from help.
func TestHelpOmitsPlanned(t *testing.T) {
	specs := m1Table(echo)
	planned := map[string]bool{"push": true, "audit": true, "rollout": true, "test": true, "ai": true, "list": true}
	for _, args := range [][]string{{"--help"}, {"bundle", "--help"}, {"node", "--help"}} {
		_, out, _ := execute(t, specs, "linux", args...)
		for _, line := range strings.Split(out, "\n") {
			if !strings.HasPrefix(line, "  ") {
				continue
			}
			fields := strings.Fields(line)
			for _, w := range fields[:min(2, len(fields))] {
				if planned[w] {
					t.Errorf("help %q lists Planned %q: %q", args, w, line)
				}
			}
		}
	}
}

// TestBareInvocation covers req 7: bare ruralz prints the root usage to
// stderr and exits 2.
func TestBareInvocation(t *testing.T) {
	specs := m1Table(echo)
	code, out, errOut := execute(t, specs, "linux")
	_, help, _ := execute(t, specs, "linux", "--help")
	if code != command.ExitNoResult || out != "" || errOut != help {
		t.Fatalf("bare: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// TestUsageErrors covers reqs 7, 8, 10 and 15: exact stderr, empty stdout,
// exit 2.
func TestUsageErrors(t *testing.T) {
	specs := m1Table(echo)
	cases := []struct {
		name   string
		goos   string
		args   []string
		stderr string
	}{
		{
			"noun without verb (req 7)", "linux",
			[]string{"bundle"},
			"ruralz bundle: missing verb (want build, diff, render or validate)\nRun 'ruralz bundle --help' for usage.\n",
		},
		{
			"flag instead of verb", "linux",
			[]string{"dev", "--env", "x"},
			"ruralz dev: missing verb (want run or tap)\nRun 'ruralz dev --help' for usage.\n",
		},
		{
			"unknown verb with hint", "linux",
			[]string{"bundle", "valdate"},
			"ruralz bundle: unknown verb \"valdate\" (did you mean \"validate\"?)\nRun 'ruralz bundle --help' for usage.\n",
		},
		{
			"unknown verb", "linux",
			[]string{"bundle", "frobnicate"},
			"ruralz bundle: unknown verb \"frobnicate\"\nRun 'ruralz bundle --help' for usage.\n",
		},
		{
			"unknown noun with hint", "linux",
			[]string{"bundel", "validate"},
			"ruralz: unknown command \"bundel\" (did you mean \"bundle\"?)\nRun 'ruralz --help' for usage.\n",
		},
		{
			"unknown noun", "linux",
			[]string{"frobnicate"},
			"ruralz: unknown command \"frobnicate\"\nRun 'ruralz --help' for usage.\n",
		},
		{
			"flag before the command", "linux",
			[]string{"--output", "json", "version"},
			"ruralz: unknown flag --output (flags follow the command)\nRun 'ruralz --help' for usage.\n",
		},
		{
			"planned verb (req 8)", "linux",
			[]string{"bundle", "push", "--env", "prod"},
			"ruralz bundle push is Planned (M2) and not in this build\n",
		},
		{
			"planned verb help (req 8)", "linux",
			[]string{"bundle", "push", "--help"},
			"ruralz bundle push is Planned (M2) and not in this build\n",
		},
		{
			"planned noun verb (req 8)", "linux",
			[]string{"rollout", "status"},
			"ruralz rollout status is Planned (M2) and not in this build\n",
		},
		{
			"planned noun (req 8)", "linux",
			[]string{"rollout"},
			"ruralz rollout is Planned (M2) and not in this build\n",
		},
		{
			"planned noun, unknown verb", "linux",
			[]string{"rollout", "frob"},
			"ruralz rollout is Planned (M2) and not in this build\n",
		},
		{
			"planned noun help", "linux",
			[]string{"rollout", "--help"},
			"ruralz rollout is Planned (M2) and not in this build\n",
		},
		{
			"planned M3 noun", "linux",
			[]string{"ai", "cost"},
			"ruralz ai cost is Planned (M3) and not in this build\n",
		},
		{
			"help of a planned verb", "linux",
			[]string{"help", "rollout", "status"},
			"ruralz rollout status is Planned (M2) and not in this build\n",
		},
		{
			"help of a planned noun", "linux",
			[]string{"help", "test"},
			"ruralz test is Planned (M2) and not in this build\n",
		},
		{
			"help of a planned bundle verb", "linux",
			[]string{"help", "bundle", "push"},
			"ruralz bundle push is Planned (M2) and not in this build\n",
		},
		{
			"help of an unknown command", "linux",
			[]string{"help", "frob"},
			"ruralz: unknown command \"frob\"\nRun 'ruralz --help' for usage.\n",
		},
		{
			"help with an extra argument", "linux",
			[]string{"help", "bundle", "diff", "extra"},
			"ruralz help: unexpected argument \"extra\"\nRun 'ruralz help --help' for usage.\n",
		},
		{
			"help help with an argument", "linux",
			[]string{"help", "help", "bundle"},
			"ruralz help: unexpected argument \"bundle\"\nRun 'ruralz help --help' for usage.\n",
		},
		{
			"help of a noun with a flag", "linux",
			[]string{"help", "bundle", "--env"},
			"ruralz help: unexpected argument \"--env\"\nRun 'ruralz help --help' for usage.\n",
		},
		{
			"unexpected argument", "linux",
			[]string{"version", "extra"},
			"ruralz version: unexpected argument \"extra\"\nRun 'ruralz version --help' for usage.\n",
		},
		{
			"unknown flag", "linux",
			[]string{"version", "--no-such-flag"},
			"ruralz version: unknown flag --no-such-flag\nRun 'ruralz version --help' for usage.\n",
		},
		{
			"unknown flag with hint", "linux",
			[]string{"version", "--outptu", "json"},
			"ruralz version: unknown flag --outptu (did you mean --output?)\nRun 'ruralz version --help' for usage.\n",
		},
		{
			"enum value (req 15)", "linux",
			[]string{"version", "--output", "yaml"},
			"ruralz version: unsupported --output \"yaml\" (want text or json)\nRun 'ruralz version --help' for usage.\n",
		},
		{
			"enum value per mode (req 15)", "linux",
			[]string{"bundle", "render", "--output", "text"},
			"ruralz bundle render: unsupported --output \"text\" (want yaml or json)\nRun 'ruralz bundle render --help' for usage.\n",
		},
		{
			"enum value in a mode (req 15)", "linux",
			[]string{"bundle", "render", "--output", "yaml", "--effective"},
			"ruralz bundle render: unsupported --output \"yaml\" (want text or json)\nRun 'ruralz bundle render --help' for usage.\n",
		},
		{
			"missing flag value", "linux",
			[]string{"bundle", "validate", "--env"},
			"ruralz bundle validate: flag needs an argument: --env\nRun 'ruralz bundle validate --help' for usage.\n",
		},
		{
			"bad bool value", "linux",
			[]string{"bundle", "validate", "--online=maybe"},
			"ruralz bundle validate: invalid value \"maybe\" for --online: want true or false\nRun 'ruralz bundle validate --help' for usage.\n",
		},
		{
			"unparsable duration (req 7)", "linux",
			[]string{"node", "drain", "--timeout", "5x"},
			"ruralz node drain: invalid value \"5x\" for --timeout: want a duration such as 30s or 5m\nRun 'ruralz node drain --help' for usage.\n",
		},
		{
			"zero duration (req 94)", "linux",
			[]string{"node", "drain", "--timeout", "0"},
			"ruralz node drain: invalid value \"0\" for --timeout: must be greater than zero\nRun 'ruralz node drain --help' for usage.\n",
		},
		{
			"missing positional", "linux",
			[]string{"completion"},
			"ruralz completion: missing argument SHELL\nRun 'ruralz completion --help' for usage.\n",
		},
		{
			"missing positionals", "linux",
			[]string{"bundle", "diff"},
			"ruralz bundle diff: missing arguments FROM and TO\nRun 'ruralz bundle diff --help' for usage.\n",
		},
		{
			"one positional short", "linux",
			[]string{"bundle", "diff", "a"},
			"ruralz bundle diff: missing argument TO\nRun 'ruralz bundle diff --help' for usage.\n",
		},
		{
			"too many positionals", "linux",
			[]string{"bundle", "diff", "a", "b", "c"},
			"ruralz bundle diff: unexpected argument \"c\"\nRun 'ruralz bundle diff --help' for usage.\n",
		},
		{
			"unknown shell (req 106)", "linux",
			[]string{"completion", "tcsh"},
			"ruralz completion: unsupported SHELL \"tcsh\" (want bash, zsh, fish or powershell)\nRun 'ruralz completion --help' for usage.\n",
		},
		{
			"two shells (req 106)", "linux",
			[]string{"completion", "bash", "zsh"},
			"ruralz completion: unexpected argument \"zsh\"\nRun 'ruralz completion --help' for usage.\n",
		},
		{
			"dev run on windows (req 10, 70)", "windows",
			[]string{"dev", "run"},
			"ruralz dev run is not planned on windows: there is no ruralzd build; use WSL\n",
		},
		{
			"node drain on darwin (req 10, 97)", "darwin",
			[]string{"node", "drain"},
			"ruralz node drain is not planned on darwin: it needs /proc/locks\n",
		},
		{
			"node drain on windows (req 10, 97)", "windows",
			[]string{"node", "drain", "--timeout", "1m"},
			"ruralz node drain is not planned on windows: there is no ruralzd and no SIGTERM\n",
		},
		{
			"platform before usage (req 10)", "darwin",
			[]string{"node", "drain", "--bogus"},
			"ruralz node drain is not planned on darwin: it needs /proc/locks\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := execute(t, specs, c.goos, c.args...)
			if code != command.ExitNoResult {
				t.Errorf("exit %d, want 2", code)
			}
			if out != "" {
				t.Errorf("stdout %q, want empty", out)
			}
			if errOut != c.stderr {
				t.Errorf("stderr\n%q\nwant\n%q", errOut, c.stderr)
			}
		})
	}
}

// TestPlatformGateAllowsOthers covers req 10: gated commands run on the
// other platforms, and help still works where they do not.
func TestPlatformGateAllowsOthers(t *testing.T) {
	specs := m1Table(echo)
	for _, c := range []struct {
		goos string
		args []string
	}{
		{"linux", []string{"dev", "run"}},
		{"darwin", []string{"dev", "run"}},
		{"linux", []string{"node", "drain"}},
		{"windows", []string{"bundle", "validate"}},
		{"windows", []string{"dev", "tap"}},
	} {
		if code, _, errOut := execute(t, specs, c.goos, c.args...); code != command.ExitOK {
			t.Errorf("%s %q: exit %d, stderr %q", c.goos, c.args, code, errOut)
		}
	}
	code, out, _ := execute(t, specs, "windows", "dev", "run", "--help")
	if code != command.ExitOK || !strings.HasPrefix(out, "Usage: ruralz dev run") {
		t.Errorf("dev run --help on windows: exit %d, %q", code, out)
	}
}

// TestInterspersedDispatch covers req 5 end to end: positionals and flag
// values reach the command whatever the order.
func TestInterspersedDispatch(t *testing.T) {
	specs := m1Table(echo)
	cases := []struct {
		name  string
		args  []string
		want  []string
		flags map[string]string
	}{
		{
			"flags after positionals",
			[]string{"bundle", "diff", "a", "b", "--env", "prod", "--environments", "e.yaml"},
			[]string{"a", "b"},
			map[string]string{"env": "prod", "environments": "e.yaml", "output": "text"},
		},
		{
			"flags between positionals",
			[]string{"bundle", "diff", "a", "--env=prod", "b", "--output", "json"},
			[]string{"a", "b"},
			map[string]string{"env": "prod", "output": "json"},
		},
		{
			"flags before positionals",
			[]string{"bundle", "diff", "-env", "prod", "--from-env", "s", "a", "b"},
			[]string{"a", "b"},
			map[string]string{"env": "prod", "from-env": "s"},
		},
		{
			"double dash",
			[]string{"bundle", "diff", "a", "--", "--env"},
			[]string{"a", "--env"},
			map[string]string{"env": ""},
		},
		{
			"default DIR",
			[]string{"bundle", "validate", "--online"},
			[]string{"."},
			map[string]string{"online": "true"},
		},
		{
			"lone dash DIR",
			[]string{"bundle", "validate", "-"},
			[]string{"-"},
			map[string]string{"online": "false"},
		},
		{
			"bool false",
			[]string{"bundle", "validate", "d", "--online=false"},
			[]string{"d"},
			map[string]string{"online": "false"},
		},
		{
			"repeated flag keeps the last",
			[]string{"bundle", "build", "--env", "a", "--env", "b"},
			[]string{"."},
			map[string]string{"env": "b"},
		},
		{
			"mode default",
			[]string{"bundle", "render", "--effective", "--route", "r"},
			[]string{"."},
			map[string]string{"output": "text", "effective": "true", "route": "r"},
		},
		{
			"mode set later on the line",
			[]string{"bundle", "render", "--output", "json", "d", "--api-version", "v"},
			[]string{"d"},
			map[string]string{"output": "json", "api-version": "v"},
		},
		{
			"plain default",
			[]string{"bundle", "render"},
			[]string{"."},
			map[string]string{"output": "yaml"},
		},
		{
			"duration",
			[]string{"dev", "run", "--ready-timeout", "1m30s"},
			[]string{"."},
			map[string]string{"ready-timeout": "1m30s"},
		},
		{
			"duration default",
			[]string{"node", "drain"},
			[]string{},
			map[string]string{"timeout": "5m0s"},
		},
		{"shell", []string{"completion", "zsh"}, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := execute(t, specs, "linux", c.args...)
			if code != command.ExitOK {
				t.Fatalf("exit %d, stderr %q", code, errOut)
			}
			if c.want == nil {
				return
			}
			var got struct {
				Args  []string          `json:"args"`
				Flags map[string]string `json:"flags"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("stdout is not JSON: %v\n%s", err, out)
			}
			if fmt.Sprint(got.Args) != fmt.Sprint(c.want) {
				t.Errorf("args = %q, want %q", got.Args, c.want)
			}
			for k, v := range c.flags {
				if got.Flags[k] != v {
					t.Errorf("--%s = %q, want %q", k, got.Flags[k], v)
				}
			}
		})
	}
}

// problemError is an admin error carrying its problem document (req 23).
type problemError struct{ doc string }

func (e problemError) Error() string {
	return "401 Unauthorized: token required (code RZ-AUTH-001, requestId 0123)"
}
func (e problemError) ProblemJSON() []byte { return []byte(e.doc) }

// TestExitCodeTable is the exit-code table test of the work package: each
// result a command can return, its exit code (req 18) and its streams
// (reqs 13, 22, 23).
func TestExitCodeTable(t *testing.T) {
	sigint, stopINT := context.WithCancelCause(context.Background())
	stopINT(command.Interrupted{Signal: os.Interrupt})
	sigterm, stopTERM := context.WithCancelCause(context.Background())
	stopTERM(command.Interrupted{Signal: syscall.SIGTERM})
	plainCancel, stopPlain := context.WithCancel(context.Background())
	stopPlain()
	doc := `{"title":"Unauthorized","status":401,"code":"RZ-AUTH-001","requestId":"0123"}`

	cases := []struct {
		name   string
		ctx    context.Context
		args   []string
		result func(ctx context.Context) error
		code   int
		stdout string
		stderr string
	}{
		{"success", nil, nil, func(context.Context) error { return nil }, 0, "", ""},
		{"negative result", nil, nil, func(context.Context) error { return command.Negative() }, 1, "", ""},
		{"negative wrapped", nil, nil, func(context.Context) error { return fmt.Errorf("check: %w", command.Negative()) }, 1, "", ""},
		{
			"negative joined with a failure (req 22)", nil, nil,
			func(context.Context) error { return errors.Join(errors.New("real failure"), command.Negative()) }, 1, "",
			"ruralz version: real failure\n",
		},
		{
			"wrapped join with a negative (req 22)", nil, nil,
			func(context.Context) error {
				return fmt.Errorf("check: %w", errors.Join(command.Negative(), errors.New("real failure")))
			}, 1, "",
			"ruralz version: check: real failure\n",
		},
		{
			"join with a wrapped negative (req 22)", nil, nil,
			func(context.Context) error {
				return errors.Join(errors.New("first"), fmt.Errorf("check: %w", command.Negative()), errors.New("second"))
			}, 1, "",
			"ruralz version: first; second\n",
		},
		{
			"join of results already printed", nil, nil,
			func(context.Context) error { return errors.Join(command.Negative(), command.NoResult(nil)) }, 1, "", "",
		},
		{
			"usage error joined with a negative", nil, nil,
			func(context.Context) error { return errors.Join(command.Negative(), command.Usagef("bad --env")) }, 1, "",
			"ruralz version: bad --env\nRun 'ruralz version --help' for usage.\n",
		},
		{
			"cause wrapped in an ExitError", nil, nil,
			func(context.Context) error {
				return command.NoResult(fmt.Errorf("read: %w", errors.Join(errors.New("a"), command.Negative())))
			}, 2, "",
			"ruralz version: read: a\n",
		},
		{
			"negative with message", nil, nil,
			func(context.Context) error { return &command.ExitError{Code: 1, Err: errors.New("2 changes")} }, 1, "",
			"ruralz version: 2 changes\n",
		},
		{
			"no result", nil, nil, func(context.Context) error { return command.NoResultf("cannot read %s", "dir") }, 2, "",
			"ruralz version: cannot read dir\n",
		},
		{"no result, printed", nil, nil, func(context.Context) error { return command.NoResult(nil) }, 2, "", ""},
		{"plain error", nil, nil, func(context.Context) error { return errors.New("boom") }, 2, "", "ruralz version: boom\n"},
		{
			"RZ code (req 22)", nil, nil,
			func(context.Context) error {
				return fmt.Errorf("verify dump: %w", errcode.Errorf("RZ-CFG-027", "content does not hash to its digest"))
			}, 2, "", "ruralz version: verify dump: RZ-CFG-027: content does not hash to its digest\n",
		},
		{
			"multi-line error (req 22)", nil, nil,
			func(context.Context) error { return errors.Join(errors.New("first"), errors.New("second")) }, 2, "",
			"ruralz version: first; second\n",
		},
		{
			"usage error (req 7)", nil, nil, func(context.Context) error { return command.Usagef("--effective needs --route") }, 2, "",
			"ruralz version: --effective needs --route\nRun 'ruralz version --help' for usage.\n",
		},
		{"waiting on a person", nil, nil, func(context.Context) error { return command.Exit(command.ExitWaiting) }, 3, "", ""},
		{"interrupted by SIGINT (req 21)", sigint, nil, context.Cause, 130, "", ""},
		{"interrupted by SIGTERM (req 21)", sigterm, nil, func(ctx context.Context) error { return ctx.Err() }, 130, "", ""},
		{"interrupted, success", sigint, nil, func(context.Context) error { return nil }, 130, "", ""},
		{
			"interrupted, other error printed", sigint, nil, func(context.Context) error { return errors.New("cleanup failed") }, 130, "",
			"ruralz version: cleanup failed\n",
		},
		{
			"canceled without a signal", plainCancel, nil, func(ctx context.Context) error { return ctx.Err() }, 2, "",
			"ruralz version: context canceled\n",
		},
		{
			"Interrupted returned", nil, nil,
			func(context.Context) error { return fmt.Errorf("wait: %w", command.Interrupted{Signal: os.Interrupt}) }, 130, "", "",
		},
		{
			"problem document, text (req 23)", nil, nil,
			func(context.Context) error { return command.NoResult(problemError{doc}) }, 2, "",
			"ruralz version: 401 Unauthorized: token required (code RZ-AUTH-001, requestId 0123)\n",
		},
		{
			"problem document, json (req 23)", nil,
			[]string{"--output", "json"},
			func(context.Context) error { return command.NoResult(problemError{doc}) }, 2, doc + "\n",
			"ruralz version: 401 Unauthorized: token required (code RZ-AUTH-001, requestId 0123)\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := c.ctx
			if ctx == nil {
				ctx = t.Context()
			}
			run := func(ctx context.Context, _ command.IO, _ []string, _ *values) error { return c.result(ctx) }
			specs := []command.Spec{{
				Path: []string{"version"}, Summary: "Version",
				New: newFixture(func(f *command.Flags, v *values) { f.Output(v.str("output"), "text", "text", "json") }, run),
			}}
			code, out, errOut := executeCtx(ctx, specs, "linux", append([]string{"version"}, c.args...)...)
			if code != c.code {
				t.Errorf("exit %d, want %d", code, c.code)
			}
			if out != c.stdout {
				t.Errorf("stdout %q, want %q", out, c.stdout)
			}
			if errOut != c.stderr {
				t.Errorf("stderr %q, want %q", errOut, c.stderr)
			}
		})
	}
}

// TestCode covers the Code mapping of req 18 directly.
func TestCode(t *testing.T) {
	sigint, stop := context.WithCancelCause(context.Background())
	stop(command.Interrupted{Signal: os.Interrupt})
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want int
	}{
		{"nil", context.Background(), nil, 0},
		{"nil context", nil, nil, 0},
		{"negative", context.Background(), command.Negative(), 1},
		{"no result", context.Background(), command.NoResultf("x"), 2},
		{"plain", context.Background(), errors.New("x"), 2},
		{"waiting", context.Background(), &command.ExitError{Code: 3}, 3},
		{"joined", context.Background(), errors.Join(errors.New("a"), command.Negative()), 1},
		{"wrapped", context.Background(), fmt.Errorf("a: %w", &command.ExitError{Code: 3}), 3},
		{"interrupted context", sigint, nil, 130},
		{"interrupted context wins", sigint, command.Negative(), 130},
		{"interrupted error", context.Background(), command.Interrupted{Signal: syscall.SIGTERM}, 130},
	}
	for _, c := range cases {
		if got := command.Code(c.ctx, c.err); got != c.want {
			t.Errorf("%s: Code = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestDeprecation covers req 9: deprecated flags and commands warn on
// stderr and still run.
func TestDeprecation(t *testing.T) {
	specs := []command.Spec{{
		Path: []string{"node", "old"}, Summary: "An old verb", Deprecated: "use ruralz node new",
		New: newFixture(func(f *command.Flags, v *values) {
			f.String(v.str("dir"), "dir", "DIR", "", "Old directory flag", command.CompleteDir)
			f.Deprecate("dir", "use --data-dir")
		}, echo),
	}}
	code, _, errOut := execute(t, specs, "linux", "node", "old", "--dir", "x")
	want := "warning: ruralz node old is deprecated: use ruralz node new\nwarning: --dir is deprecated: use --data-dir\n"
	if code != 0 || errOut != want {
		t.Fatalf("exit %d, stderr %q, want %q", code, errOut, want)
	}
	_, help, _ := execute(t, specs, "linux", "node", "old", "--help")
	golden(t, "help/deprecated.golden", help)
	_, list, _ := execute(t, specs, "linux", "node", "--help")
	if !strings.Contains(list, "An old verb (deprecated)") {
		t.Errorf("noun help lacks the deprecation mark:\n%s", list)
	}
}

// TestHelpWithoutNounDescription covers noun help when the table has no
// noun description and a verb with a repeated positional.
func TestHelpWithoutNounDescription(t *testing.T) {
	specs := []command.Spec{{
		Path: []string{"ai", "cost"}, Summary: "Cost attribution.",
		Args: []command.Arg{{Name: "FILE", Usage: "Usage records", Optional: true, Repeat: true, Complete: command.CompleteFile}},
		New:  newFixture(nil, echo),
	}}
	_, noun, _ := execute(t, specs, "linux", "ai", "--help")
	golden(t, "help/repeat-noun.golden", noun)
	_, cmd, _ := execute(t, specs, "linux", "ai", "cost", "--help")
	golden(t, "help/repeat-command.golden", cmd)
	code, out, _ := execute(t, specs, "linux", "ai", "cost", "a", "b", "c")
	var got struct {
		Args []string `json:"args"`
	}
	if err := json.Unmarshal([]byte(out), &got); code != 0 || err != nil || strings.Join(got.Args, ",") != "a,b,c" {
		t.Errorf("repeated positional: exit %d, %s", code, out)
	}
}

// TestStreamsOfTheCommand covers req 13: a command writes data to stdout
// and progress to stderr through IO.
func TestStreamsOfTheCommand(t *testing.T) {
	run := func(_ context.Context, sio command.IO, _ []string, _ *values) error {
		_, _ = fmt.Fprintln(sio.Stderr, "progress")
		_, _ = fmt.Fprintln(sio.Stdout, command.FormatTime(sio.Now()))
		v, ok := sio.LookupEnv("HOME")
		_, _ = fmt.Fprintln(sio.Stdout, v, ok, sio.GOOS)
		return nil
	}
	specs := []command.Spec{{Path: []string{"version"}, Summary: "Version", New: newFixture(nil, run)}}
	code, out, errOut := execute(t, specs, "darwin", "version")
	if code != 0 || out != "2026-09-26T12:00:00.000Z\n/home/test true darwin\n" || errOut != "progress\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}
