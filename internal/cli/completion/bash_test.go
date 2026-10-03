// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package completion_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/completion"
)

// modernBash returns a bash of version 4.2 or newer, which the script
// needs (req 108: mapfile, compopt, local -A). The bash on PATH comes
// first; macOS ships bash 3.2 as /bin/bash, so the Homebrew locations
// follow. It skips the test when there is none.
func modernBash(t *testing.T) string {
	t.Helper()
	var candidates, found []string
	if p, err := exec.LookPath("bash"); err == nil {
		candidates = append(candidates, p)
	}
	candidates = append(candidates, "/opt/homebrew/bin/bash", "/usr/local/bin/bash")
	for _, p := range candidates {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		out, err := exec.CommandContext(t.Context(), p, "--norc", "--noprofile", "-c", //nolint:gosec // G204: a bash from PATH or Homebrew prints its version.
			`echo "${BASH_VERSINFO[0]} ${BASH_VERSINFO[1]}"`).Output()
		if err != nil {
			continue
		}
		version := strings.TrimSpace(string(out))
		if bashAtLeast(version, 4, 2) {
			return p
		}
		found = append(found, p+" "+version)
	}
	if len(found) == 0 {
		t.Skip("bash not installed")
	}
	t.Skipf("the script needs bash 4.2 or newer; found %s", strings.Join(found, ", "))
	return ""
}

// bashAtLeast reports whether "MAJOR MINOR" is at least major.minor.
func bashAtLeast(version string, major, minor int) bool {
	fields := strings.Fields(version)
	if len(fields) != 2 {
		return false
	}
	gotMajor, err1 := strconv.Atoi(fields[0])
	gotMinor, err2 := strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return gotMajor > major || (gotMajor == major && gotMinor >= minor)
}

// TestBashAtLeast covers the version gate of TestBashBehaviorReq107.
func TestBashAtLeast(t *testing.T) {
	cases := map[string]bool{
		"3 2": false, "4 1": false, "4 2": true, "4 4": true, "5 0": true, "5 2": true,
		"": false, "5": false, "x 2": false, "5 y": false, "5 2 1": false,
	}
	for in, want := range cases {
		if got := bashAtLeast(in, 4, 2); got != want {
			t.Errorf("bashAtLeast(%q, 4, 2) = %v, want %v", in, got, want)
		}
	}
}

// TestBashBehaviorReq107 drives the generated bash function the way bash
// completion does (COMP_WORDS, COMP_CWORD) and checks COMPREPLY: nouns,
// verbs, flags per command, --output values per mode, SHELL values, file
// and directory completion, flag values after "=", and "--". It needs
// bash 4.2 or newer (req 108) and skips otherwise.
func TestBashBehaviorReq107(t *testing.T) {
	bash := modernBash(t)
	work := t.TempDir()
	for _, d := range []string{"dir1", "dir2"} {
		if err := os.Mkdir(filepath.Join(work, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "file1.yaml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ruralz.bash")
	if err := os.WriteFile(path, []byte(script(t, completion.Bash)), 0o600); err != nil {
		t.Fatal(err)
	}
	dirs := []string{"dir1", "dir2"}
	files := []string{"dir1", "dir2", "file1.yaml"}
	renderFlags := []string{
		"--api-version", "--effective", "--env", "--environments", "--help", "--output",
		"--output-dir", "--output-file", "--route",
	}
	cases := []struct {
		line string // words; a trailing space completes an empty word
		want []string
	}{
		{"ruralz ", []string{"bundle", "completion", "dev", "help", "node", "version"}},
		{"ruralz b", []string{"bundle"}},
		{"ruralz bundle ", []string{"build", "diff", "render", "validate"}},
		{"ruralz bundle re", []string{"render"}},
		{"ruralz node ", []string{"drain", "dump"}},
		{"ruralz bundle render --", renderFlags},
		{"ruralz bundle render --out", []string{"--output", "--output-dir", "--output-file"}},
		{"ruralz bundle render --output ", []string{"json", "yaml"}},
		{"ruralz bundle render --effective --output ", []string{"json", "text"}},
		{"ruralz bundle render -api-version v --output ", []string{"json", "text"}},
		{"ruralz bundle render --output = ", []string{"json", "yaml"}},
		{"ruralz bundle render --output =", []string{"json", "yaml"}},
		{"ruralz bundle render --output = j", []string{"json"}},
		{"ruralz bundle render --output=j", []string{"--output=json"}},
		{"ruralz bundle render --output-dir ", dirs},
		{"ruralz bundle render --output-file ", files},
		{"ruralz bundle render --route ", nil},
		{"ruralz bundle validate ", dirs},
		{"ruralz bundle validate d", dirs},
		{"ruralz bundle validate dir1 ", []string{"--env", "--environments", "--help", "--online", "--output"}},
		{"ruralz bundle validate --environments ", files},
		{"ruralz bundle validate --environments f", []string{"file1.yaml"}},
		{"ruralz bundle validate --online ", dirs},
		{"ruralz bundle validate --output json ", dirs},
		{"ruralz bundle validate --output = json ", dirs},
		{"ruralz bundle validate -- ", dirs},
		{"ruralz bundle validate -- --", nil},
		{"ruralz bundle diff ", files},
		{"ruralz bundle diff --env prod a ", files},
		{"ruralz bundle diff a b ", []string{
			"--admin-token-file", "--ca-file", "--client-cert", "--client-key", "--env", "--environments",
			"--from-env", "--help", "--output", "--to-env",
		}},
		{"ruralz completion ", []string{"bash", "fish", "powershell", "zsh"}},
		{"ruralz completion z", []string{"zsh"}},
		{"ruralz completion bash ", []string{"--help"}},
		{"ruralz version --output ", []string{"json", "text"}},
		{"ruralz version ", []string{"--help", "--output"}},
		{"ruralz node drain --data-dir ", dirs},
		{"ruralz node drain --lock-dir ", []string{"--data-dir", "--help", "--timeout"}},
		{"ruralz dev tap --route ", nil},
		{"ruralz dev run --ready-timeout ", nil},
		{"ruralz help ", []string{"bundle", "completion", "dev", "node", "version"}},
		{"ruralz help bundle ", []string{"build", "diff", "render", "validate"}},
		{"ruralz help bundle build ", nil},
		{"ruralz frob ", nil},
		{"ruralz bundle frob ", nil},
		{"ruralz bundle push ", nil},
		{"ruralz rollout ", nil},
	}
	for _, c := range cases {
		t.Run(c.line, func(t *testing.T) {
			words := strings.Fields(c.line)
			if strings.HasSuffix(c.line, " ") {
				words = append(words, "")
			}
			quoted := make([]string, 0, len(words))
			for _, w := range words {
				quoted = append(quoted, "'"+w+"'")
			}
			prog := `source "$1" || exit 9
COMP_WORDS=(` + strings.Join(quoted, " ") + `)
COMP_CWORD=` + strconv.Itoa(len(words)-1) + `
_ruralz
printf '%s\n' "${COMPREPLY[@]}"`
			cmd := exec.CommandContext(t.Context(), bash, "--norc", "--noprofile", "-c", prog, "bash", path) //nolint:gosec // G204: bash from PATH runs the generated script under test.
			cmd.Dir = work
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("bash: %v", err)
			}
			var got []string
			for _, l := range strings.Split(string(out), "\n") {
				if l != "" {
					got = append(got, l)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, c.want) {
				t.Errorf("COMPREPLY = %q, want %q", got, c.want)
			}
		})
	}
}
