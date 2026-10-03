// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package completion_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/completion"
)

// psLiteral single-quotes s for PowerShell.
func psLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// TestPowerShellBehaviorReq107 loads the generated PowerShell script and
// drives it through TabExpansion2, as an editor or PSReadLine does: nouns,
// verbs, flags per command, --output values per mode, SHELL values,
// directory completion, flag values after "=", and path completion for
// FILE values. Where a value has no completion the script returns an
// empty string to keep PowerShell from completing paths; CompleteInput
// then throws because a completion text must not be empty, and
// PSReadLine swallows that. The test pins this trade-off: such a case
// yields an exception naming completionText, or no match, and never a
// path. It skips without pwsh.
func TestPowerShellBehaviorReq107(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed")
	}
	work := t.TempDir()
	for _, d := range []string{"dir1", "dir2"} {
		if err := os.Mkdir(filepath.Join(work, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "file1.yaml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "ruralz.ps1") // dot-sourcing needs .ps1
	if err := os.WriteFile(scriptPath, []byte(script(t, completion.PowerShell)), 0o600); err != nil {
		t.Fatal(err)
	}
	const blocked = "<blocked>" // '' returned: no completion and no path
	files := []string{"dir1", "dir2", "file1.yaml"}
	cases := []struct {
		line string // the input up to the cursor
		want []string
	}{
		{"ruralz ", []string{"bundle", "completion", "dev", "help", "node", "version"}},
		{"ruralz b", []string{"bundle"}},
		{"ruralz bundle ", []string{"build", "diff", "render", "validate"}},
		{"ruralz bundle re", []string{"render"}},
		{"ruralz node ", []string{"drain", "dump"}},
		{"ruralz bundle render --out", []string{"--output", "--output-dir", "--output-file"}},
		{"ruralz bundle render --output ", []string{"json", "yaml"}},
		{"ruralz bundle render --effective --output ", []string{"json", "text"}},
		{"ruralz bundle render --output=j", []string{"--output=json"}},
		{"ruralz bundle render --output-dir ", []string{"dir1", "dir2"}},
		{"ruralz bundle render --output-file ", files},
		{"ruralz bundle render --route ", []string{blocked}},
		{"ruralz bundle validate ", []string{"dir1", "dir2"}},
		{"ruralz bundle validate d", []string{"dir1", "dir2"}},
		{"ruralz bundle validate dir1 ", []string{"--env", "--environments", "--help", "--online", "--output"}},
		{"ruralz bundle validate x", []string{blocked}},
		{"ruralz bundle diff ", files},
		{"ruralz completion ", []string{"bash", "fish", "powershell", "zsh"}},
		{"ruralz completion z", []string{"zsh"}},
		{"ruralz completion x", []string{blocked}},
		{"ruralz version --output ", []string{"json", "text"}},
		{"ruralz version ", []string{"--help", "--output"}},
		{"ruralz dev tap --route ", []string{blocked}},
		{"ruralz help ", []string{"bundle", "completion", "dev", "node", "version"}},
		{"ruralz help bundle ", []string{"build", "diff", "render", "validate"}},
		{"ruralz help bundle build ", []string{blocked}},
		{"ruralz frob ", []string{blocked}},
		{"ruralz rollout ", []string{blocked}},
	}
	var probe strings.Builder
	probe.WriteString(". " + psLiteral(scriptPath) + "\n")
	for _, c := range cases {
		probe.WriteString(`try {
    $r = TabExpansion2 -inputScript ` + psLiteral(c.line) + ` -cursorColumn ` + psLiteral(c.line) + `.Length
    'ok ' + (($r.CompletionMatches | ForEach-Object { $_.CompletionText }) -join "` + "`t" + `")
} catch {
    'error ' + $_.Exception.Message.Replace("` + "`n" + `", ' ').Replace("` + "`r" + `", '')
}
`)
	}
	probePath := filepath.Join(dir, "probe.ps1")
	if err := os.WriteFile(probePath, []byte(probe.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), pwsh, "-NoProfile", "-NonInteractive", "-File", probePath) //nolint:gosec // G204: pwsh from PATH runs the generated script under test.
	cmd.Dir = work
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("pwsh: %v\n%s", err, out)
	}
	var results []string
	for _, l := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
		if l == "ok" || strings.HasPrefix(l, "ok ") || strings.HasPrefix(l, "error ") {
			results = append(results, l)
		}
	}
	if len(results) != len(cases) {
		t.Fatalf("pwsh printed %d results for %d cases:\n%s", len(results), len(cases), out)
	}
	for i, c := range cases {
		t.Run(c.line, func(t *testing.T) {
			res := results[i]
			if msg, ok := strings.CutPrefix(res, "error "); ok {
				if !slices.Equal(c.want, []string{blocked}) || !strings.Contains(msg, "completionText") {
					t.Fatalf("TabExpansion2 threw %q, want %q", msg, c.want)
				}
				return
			}
			got := psMatches(strings.TrimPrefix(strings.TrimPrefix(res, "ok"), " "))
			want := c.want
			if slices.Equal(want, []string{blocked}) {
				want = nil
			}
			if !slices.Equal(got, want) {
				t.Errorf("CompletionMatches = %q, want %q", got, want)
			}
		})
	}
}

// psMatches splits the tab-separated completion texts, drops the "./" or
// ".\" PowerShell puts before a path in the working directory and any
// trailing separator, and sorts.
func psMatches(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, m := range strings.Split(s, "\t") {
		m = strings.TrimPrefix(strings.TrimPrefix(m, "./"), `.\`)
		m = strings.TrimRight(m, `/\`)
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}
