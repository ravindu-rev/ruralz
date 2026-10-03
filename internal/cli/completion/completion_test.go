// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package completion_test

import (
	"bytes"
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/cli/completion"
)

// Tests for spec 10 section 2.14 (reqs 106 to 108): golden scripts for the
// four shells, the registry property of section 6.1 and the completion
// command.

// update rewrites the golden files: go test ./internal/cli/completion -update.
var update = flag.Bool("update", false, "rewrite testdata golden files")

func script(t *testing.T, sh completion.Shell) string {
	t.Helper()
	var buf bytes.Buffer
	if err := completion.Write(&buf, sh, "ruralz", completion.Entries(m1Table())); err != nil {
		t.Fatalf("Write(%s) = %v", sh, err)
	}
	return buf.String()
}

func TestM1TableIsValid(t *testing.T) {
	if err := command.Check(m1Table()); err != nil {
		t.Fatal(err)
	}
}

// TestGoldenScriptsReq108 pins the four scripts generated from the M1 table.
func TestGoldenScriptsReq108(t *testing.T) {
	for _, sh := range completion.Shells() {
		t.Run(string(sh), func(t *testing.T) {
			got := script(t, sh)
			path := filepath.Join("testdata", string(sh)+".golden")
			if *update {
				if err := os.MkdirAll("testdata", 0o750); err != nil {
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
				t.Errorf("%s script differs from %s (run with -update to accept)\n%s", sh, path, got)
			}
		})
	}
}

// TestScriptsCoverRegistryReq107 is section 6.1's property: every script
// holds every registered path and flag, and no Planned or deprecated entry.
func TestScriptsCoverRegistryReq107(t *testing.T) {
	specs := m1Table()
	for _, sh := range completion.Shells() {
		s := script(t, sh)
		for _, spec := range specs {
			switch {
			case spec.IsNoun() && spec.Path[0] == "rollout":
				if strings.Contains(s, "rollout") {
					t.Errorf("%s: the Planned-only noun rollout is completed", sh)
				}
			case spec.IsNoun():
				// bash shows no descriptions.
				if sh != completion.Bash && !strings.Contains(s, spec.Summary) {
					t.Errorf("%s: noun %s lacks its description", sh, spec.Path[0])
				}
			case spec.Planned != "" || spec.Deprecated != "":
				verb := spec.Path[len(spec.Path)-1]
				if containsWord(s, verb) {
					t.Errorf("%s: excluded command %q is completed", sh, spec.Name())
				}
			default:
				for _, w := range spec.Path {
					if !containsWord(s, w) {
						t.Errorf("%s: path word %q of %q missing", sh, w, spec.Name())
					}
				}
				for _, f := range command.FlagSpecsOf(spec) {
					has := strings.Contains(s, "--"+f.Name) || strings.Contains(s, "-l "+f.Name+" ")
					if f.Deprecated != "" && has {
						t.Errorf("%s: deprecated --%s is completed", sh, f.Name)
					}
					if f.Deprecated == "" && !has {
						t.Errorf("%s: flag --%s of %q missing", sh, f.Name, spec.Name())
					}
				}
			}
		}
		for _, v := range []string{"yaml", "text", "json", "bash", "zsh", "fish", "powershell"} {
			if !containsWord(s, v) {
				t.Errorf("%s: value %q missing", sh, v)
			}
		}
	}
}

// containsWord reports whether w appears in s delimited by non-word bytes.
func containsWord(s, w string) bool {
	isWord := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
	}
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(w)
		if (j == 0 || !isWord(s[j-1])) && (end == len(s) || !isWord(s[end])) {
			return true
		}
		i = j + 1
	}
}

func TestEntries(t *testing.T) {
	entries := completion.Entries(m1Table())
	var paths []string
	for _, e := range entries {
		paths = append(paths, strings.Join(e.Path, " "))
		if !e.Noun && !slices.ContainsFunc(e.Flags, func(f command.FlagSpec) bool { return f.Name == "help" }) {
			t.Errorf("%v lacks --help", e.Path)
		}
		if !slices.IsSortedFunc(e.Flags, func(a, b command.FlagSpec) int { return strings.Compare(a.Name, b.Name) }) {
			t.Errorf("%v flags not sorted", e.Path)
		}
	}
	want := []string{
		"bundle", "bundle build", "bundle diff", "bundle render", "bundle validate", "completion",
		"dev", "dev run", "dev tap", "node", "node drain", "node dump", "version",
	}
	if !slices.Equal(paths, want) {
		t.Errorf("Entries paths = %q, want %q", paths, want)
	}
	drain := entries[slices.IndexFunc(entries, func(e completion.Entry) bool { return strings.Join(e.Path, " ") == "node drain" })]
	if slices.ContainsFunc(drain.Flags, func(f command.FlagSpec) bool { return f.Name == "lock-dir" }) {
		t.Error("deprecated --lock-dir kept")
	}
}

func TestShells(t *testing.T) {
	if got := completion.ShellNames(); !slices.Equal(got, []string{"bash", "zsh", "fish", "powershell"}) {
		t.Errorf("ShellNames = %q", got)
	}
	help := completion.InstallHelp("ruralz")
	for _, line := range []string{
		"source <(ruralz completion bash)",
		`ruralz completion zsh > "${fpath[1]}/_ruralz"`,
		"ruralz completion fish > ~/.config/fish/completions/ruralz.fish",
		"ruralz completion powershell | Out-String | Invoke-Expression",
	} {
		if !strings.Contains(help, line) {
			t.Errorf("install help lacks %q", line)
		}
	}
}

// TestCompletionCommandReq106 runs `completion SHELL` through Execute.
func TestCompletionCommandReq106(t *testing.T) {
	specs := m1Table()
	for _, sh := range completion.Shells() {
		var out, errOut bytes.Buffer
		sio := command.NewIO(&out, &errOut, nil, time.Now, "linux")
		code := command.Execute(context.Background(), sio, "ruralz", specs, []string{"completion", string(sh)})
		if code != 0 || errOut.Len() != 0 || out.String() != script(t, sh) {
			t.Errorf("completion %s: exit %d, stderr %q", sh, code, errOut.String())
		}
	}
	// A table the generator refuses is exit 2 with nothing on stdout.
	bad := func() []command.Spec {
		return []command.Spec{
			{
				Path: []string{"x"}, Summary: "x", Args: []command.Arg{{Name: "V", Values: []string{"a b"}}},
				New: cmd(nil),
			},
			completion.Spec("ruralz", nil),
		}
	}
	specs = []command.Spec{completion.Spec("ruralz", bad)}
	var out, errOut bytes.Buffer
	sio := command.NewIO(&out, &errOut, nil, time.Now, "linux")
	code := command.Execute(context.Background(), sio, "ruralz", specs, []string{"completion", "bash"})
	if code != command.ExitNoResult || out.Len() != 0 || !strings.Contains(errOut.String(), "not a plain word") {
		t.Errorf("bad table: exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
}

func TestWriteErrors(t *testing.T) {
	flags := func(fs ...command.FlagSpec) []command.FlagSpec { return fs }
	cases := []struct {
		name    string
		program string
		shell   completion.Shell
		entries []completion.Entry
		want    string
	}{
		{"shell", "ruralz", "tcsh", nil, `unsupported shell "tcsh"`},
		{"program", "ru ralz", completion.Bash, nil, "invalid program name"},
		{"empty program", "", completion.Bash, nil, "invalid program name"},
		{"path", "ruralz", completion.Bash, []completion.Entry{{Path: []string{"a", "b", "c"}}}, "invalid path"},
		{"noun path", "ruralz", completion.Bash, []completion.Entry{{Path: []string{"a", "b"}, Noun: true}}, "invalid path"},
		{"word", "ruralz", completion.Bash, []completion.Entry{{Path: []string{"A"}}}, "invalid path word"},
		{"help word", "ruralz", completion.Bash, []completion.Entry{{Path: []string{"help"}}}, "invalid path word"},
		{"duplicate", "ruralz", completion.Bash, []completion.Entry{{Path: []string{"a"}}, {Path: []string{"a"}}}, "duplicate path"},
		{
			"noun and command", "ruralz", completion.Bash,
			[]completion.Entry{{Path: []string{"a"}}, {Path: []string{"a", "b"}}},
			"both a command and a noun",
		},
		{"noun without verbs", "ruralz", completion.Bash, []completion.Entry{{Path: []string{"a"}, Noun: true}}, "has no verbs"},
		{
			"flag name", "ruralz", completion.Bash,
			[]completion.Entry{{Path: []string{"a"}, Flags: flags(command.FlagSpec{Name: "A"})}},
			"invalid flag name",
		},
		{
			"duplicate flag", "ruralz", completion.Bash,
			[]completion.Entry{{Path: []string{"a"}, Flags: flags(command.FlagSpec{Name: "x"}, command.FlagSpec{Name: "x"})}},
			"duplicate flag",
		},
		{
			"flag value", "ruralz", completion.Bash,
			[]completion.Entry{{Path: []string{"a"}, Flags: flags(command.FlagSpec{Name: "x", Values: []string{"it's"}})}},
			"not a plain word",
		},
		{
			"dash value", "ruralz", completion.Bash,
			[]completion.Entry{{Path: []string{"a"}, Flags: flags(command.FlagSpec{Name: "x", Values: []string{"-x"}})}},
			"not a plain word",
		},
		{
			"mode flag", "ruralz", completion.Bash,
			[]completion.Entry{{Path: []string{"a"}, Flags: flags(command.FlagSpec{
				Name: "x", Values: []string{"a"}, Modes: []command.Mode{{When: []string{"y"}, Values: []string{"b"}}},
			})}},
			"names unknown flag --y",
		},
		{
			"mode value", "ruralz", completion.Bash,
			[]completion.Entry{{Path: []string{"a"}, Flags: flags(command.FlagSpec{Name: "y", Bool: true}, command.FlagSpec{
				Name: "x", Values: []string{"a"}, Modes: []command.Mode{{When: []string{"y"}, Values: []string{"b c"}}},
			})}},
			"not a plain word",
		},
		{
			"repeat", "ruralz", completion.Bash,
			[]completion.Entry{{Path: []string{"a"}, Args: []command.Arg{{Name: "A", Repeat: true}, {Name: "B"}}}},
			"may repeat",
		},
		{
			"arg value", "ruralz", completion.Bash,
			[]completion.Entry{{Path: []string{"a"}, Args: []command.Arg{{Name: "A", Values: []string{""}}}}},
			"not a plain word",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := completion.Write(&buf, c.shell, c.program, c.entries)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Write = %v, want %q", err, c.want)
			}
			if buf.Len() != 0 {
				t.Errorf("wrote %d bytes on error", buf.Len())
			}
		})
	}
}

// TestNounWithoutDescription covers the fallback description and a
// program name that is not an identifier.
func TestNounWithoutDescription(t *testing.T) {
	entries := []completion.Entry{
		{Path: []string{"ai", "cost"}, Summary: "Cost", Args: []command.Arg{
			{Name: "FILE", Optional: true, Repeat: true, Complete: command.CompleteFile},
		}, Flags: []command.FlagSpec{command.HelpFlag(), {Name: "group-by", Placeholder: "", Complete: command.CompleteValues}}},
		{Path: []string{"ai", "models"}, Summary: "Models"},
	}
	for _, sh := range completion.Shells() {
		var buf bytes.Buffer
		if err := completion.Write(&buf, sh, "my-cli", entries); err != nil {
			t.Fatal(err)
		}
		s := buf.String()
		if sh != completion.Bash && !strings.Contains(s, "cost, models") {
			t.Errorf("%s: noun ai lacks the verb-list description", sh)
		}
		if sh == completion.Bash && !strings.Contains(s, "complete -o bashdefault -o default -F _my_cli my-cli") {
			t.Errorf("bash: registration line wrong:\n%s", s)
		}
	}
}

// scriptEnv names the environment variable through which pwsh checks
// receive the script path: pwsh appends arguments after a string -Command
// to the command text instead of binding them to $args.
const scriptEnv = "RURALZ_TEST_COMPLETION_SCRIPT"

// TestShellSyntax checks the scripts with the shells installed here:
// bash -n, zsh -n, fish --no-execute and the PowerShell parser (section
// 6.6, req 108).
func TestShellSyntax(t *testing.T) {
	checks := []struct {
		shell completion.Shell
		bin   string
		args  []string
		byEnv bool // the path goes in scriptEnv, not as the last argument
	}{
		{completion.Bash, "bash", []string{"-n"}, false},
		{completion.Zsh, "zsh", []string{"-n"}, false},
		{completion.Fish, "fish", []string{"--no-execute"}, false},
		{completion.PowerShell, "pwsh", []string{
			"-NoProfile", "-NonInteractive", "-Command",
			"$e = $null; [System.Management.Automation.Language.Parser]::ParseFile($env:" + scriptEnv +
				", [ref]$null, [ref]$e) | Out-Null; if ($e.Count) { $e; exit 1 }",
		}, true},
	}
	for _, c := range checks {
		t.Run(string(c.shell), func(t *testing.T) {
			bin, err := exec.LookPath(c.bin)
			if err != nil {
				t.Skipf("%s not installed", c.bin)
			}
			path := filepath.Join(t.TempDir(), "script")
			if err := os.WriteFile(path, []byte(script(t, c.shell)), 0o600); err != nil {
				t.Fatal(err)
			}
			args := c.args
			if !c.byEnv {
				args = append(slices.Clone(args), path)
			}
			cmd := exec.CommandContext(t.Context(), bin, args...) //nolint:gosec // G204: an installed shell checks the generated script.
			if c.byEnv {
				cmd.Env = append(os.Environ(), scriptEnv+"="+path)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s syntax check: %v\n%s", c.bin, err, out)
			}
		})
	}
}
