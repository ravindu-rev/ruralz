// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/cli/completion"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// TestSpecsAreValid: the rows pass command.Check, name the node noun, its
// two M1 commands and the M2 verbs as Planned, with the summaries of the
// command table (docs/reference/01-cli-and-api-surface.md, "Command
// table").
func TestSpecsAreValid(t *testing.T) {
	specs := Specs(Options{})
	if err := command.Check(specs); err != nil {
		t.Fatalf("Check(Specs) = %v", err)
	}
	type row struct{ path, summary, planned string }
	var got []row
	for _, s := range specs {
		got = append(got, row{s.Name(), s.Summary, s.Planned})
	}
	want := []row{
		{"node", "Operate Nodes", ""},
		{"node drain", "Drain the local ruralzd", ""},
		{"node dump", "Save /config/dump, secrets omitted", ""},
		{"node list", "Nodes with active and Last-Known-Good digests", "M2"},
		{"node token", "One-time Enrollment token", "M2"},
		{"node revoke", "Revoke an enrollment identity", "M2"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Specs = %+v, want %+v", got, want)
	}
}

// TestOptionsDefaults: the zero Options run on the real clock and host,
// and set ones replace them.
func TestOptionsDefaults(t *testing.T) {
	if (Options{}).timer() == nil || (Options{}).machine() == nil {
		t.Fatal("the zero Options have no clock or host")
	}
	clk := clocktest.New(epoch())
	if got := (Options{Clock: clk}).timer(); got != clk {
		t.Errorf("timer() = %v, want the configured clock", got)
	}
	h := newFakeHost(t.TempDir())
	if got := (Options{host: h}).machine(); got != h {
		t.Errorf("machine() = %v, want the configured host", got)
	}
}

// TestFlags: drain takes --data-dir, --timeout (5m, positive) and --output
// text or json; dump takes the admin flags, --output-file and --timeout
// (5m, 0 meaning none), and no --output, its output being the document.
func TestFlags(t *testing.T) {
	type flag struct{ name, def string }
	flags := func(s command.Spec) []flag {
		var out []flag
		for _, f := range command.FlagSpecsOf(s) {
			out = append(out, flag{f.Name, f.Default})
		}
		return out
	}
	drain := []flag{{"data-dir", ""}, {"output", "text"}, {"timeout", "5m"}}
	if got := flags(DrainSpec(Options{})); !slices.Equal(got, drain) {
		t.Errorf("drain flags = %v, want %v", got, drain)
	}
	dump := []flag{
		{"admin", ""}, {"admin-token-file", ""}, {"ca-file", ""}, {"client-cert", ""}, {"client-key", ""},
		{"output-file", ""}, {"timeout", "5m"},
	}
	if got := flags(DumpSpec(Options{})); !slices.Equal(got, dump) {
		t.Errorf("dump flags = %v, want %v", got, dump)
	}
	for _, f := range command.FlagSpecsOf(DumpSpec(Options{})) {
		if f.Name == "timeout" && !f.ZeroNone {
			t.Error("dump --timeout does not take 0 for none")
		}
	}
	for _, f := range command.FlagSpecsOf(DrainSpec(Options{})) {
		switch f.Name {
		case "timeout":
			if f.ZeroNone {
				t.Error("drain --timeout takes 0")
			}
		case "output":
			if !slices.Equal(f.Values, []string{"text", "json"}) {
				t.Errorf("drain --output values = %v", f.Values)
			}
		case "data-dir":
			if f.Complete != command.CompleteDir {
				t.Errorf("drain --data-dir completes %v, want dir", f.Complete)
			}
		}
	}
}

// TestPlannedVerbs: list, token and revoke exit 2 naming M2, and stay out
// of help and completion.
func TestPlannedVerbs(t *testing.T) {
	for _, verb := range []string{"list", "token", "revoke"} {
		r := execute(t.Context(), Options{}, "linux", nil, "node", verb)
		want := "ruralz node " + verb + " is Planned (M2) and not in this build\n"
		if r.code != command.ExitNoResult || r.stdout != "" || r.stderr != want {
			t.Errorf("node %s = exit %d, stdout %q, stderr %q; want 2, %q", verb, r.code, r.stdout, r.stderr, want)
		}
	}
	r := execute(t.Context(), Options{}, "linux", nil, "node", "--help")
	for _, verb := range []string{"list", "token", "revoke"} {
		if strings.Contains(r.stdout, "  "+verb+" ") {
			t.Errorf("node help lists Planned %s:\n%s", verb, r.stdout)
		}
	}
	for _, verb := range []string{"drain", "dump"} {
		if !strings.Contains(r.stdout, "  "+verb+" ") {
			t.Errorf("node help lacks %s:\n%s", verb, r.stdout)
		}
	}
	var paths []string
	for _, e := range completion.Entries(Specs(Options{})) {
		paths = append(paths, strings.Join(e.Path, " "))
	}
	if want := []string{"node", "node drain", "node dump"}; !slices.Equal(paths, want) {
		t.Errorf("completion entries = %q, want %q", paths, want)
	}
}

// TestHelp: each command's help names its flags and explains what it
// does; drain's help prints on every platform.
func TestHelp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"node", "drain", "--help"}, []string{
			"Usage: ruralz node drain [FLAGS]", "Drain the local ruralzd.", "SIGTERM",
			"--data-dir PATH", "(default $RURALZ_DATA_DIR, else /var/lib/ruralz)", "--timeout DURATION",
			"(default 5m)", "--output text|json",
		}},
		{[]string{"node", "dump", "--help"}, []string{
			"Usage: ruralz node dump [FLAGS]", "Save /config/dump, secrets omitted.", "ruralz bundle diff source",
			"--admin URL", "--admin-token-file PATH", "--ca-file PATH", "--client-cert PATH", "--client-key PATH",
			"--output-file PATH", "(default 5m; 0 means none)",
		}},
	} {
		for _, goos := range []string{"linux", "darwin", "windows"} {
			r := execute(t.Context(), Options{}, goos, nil, tc.args...)
			if r.code != command.ExitOK || r.stderr != "" {
				t.Fatalf("%s %q = exit %d, stderr %q", goos, tc.args, r.code, r.stderr)
			}
			for _, w := range tc.want {
				if !strings.Contains(r.stdout, w) {
					t.Errorf("%s %q lacks %q:\n%s", goos, tc.args, w, r.stdout)
				}
			}
		}
	}
}
