// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/cli/completion"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
)

// TestSpecsAreValid: the rows pass command.Check and name the bundle noun,
// its four M1 commands and the M2 verbs as Planned, with the summaries of
// the command table (docs/reference/01-cli-and-api-surface.md, "Command
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
		{"bundle", "Validate, render, diff and build Bundles", ""},
		{"bundle validate", "Offline validation, source-mapped diagnostics", ""},
		{"bundle render", "Render one Environment's one-file Bundle", ""},
		{"bundle diff", "Compare FROM with TO", ""},
		{"bundle build", "Build a Revision, print its digest", ""},
		{"bundle push", "Send source to Ruralz Control, or a signed Revision to OCI", "M2"},
		{"bundle import", "Routes and Upstreams from OpenAPI 3.x", "M2"},
		{"bundle export", "OpenAPI 3.x, Postman collection or DOT graph of a Bundle", "M2"},
		{"bundle audit", "Security and best-practice findings", "M2"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Specs = %+v, want %+v", got, want)
	}
}

// TestArguments: validate, render and build take an optional DIR, "." by
// default, that may be a file or "-"; diff takes FROM and TO.
func TestArguments(t *testing.T) {
	for _, s := range []command.Spec{ValidateSpec(Options{}), RenderSpec(Options{}), BuildSpec(Options{})} {
		if len(s.Args) != 1 || s.Args[0].Name != "DIR" || !s.Args[0].Optional || s.Args[0].Default != "." ||
			s.Args[0].Complete != command.CompleteFile {
			t.Errorf("%s args = %+v, want one optional DIR defaulting to . that completes files", s.Name(), s.Args)
		}
	}
	d := DiffSpec(Options{})
	if len(d.Args) != 2 || d.Args[0].Name != "FROM" || d.Args[1].Name != "TO" || d.Args[0].Optional || d.Args[1].Optional {
		t.Errorf("diff args = %+v, want FROM and TO, both required", d.Args)
	}
}

// TestFlags: every command takes the Bundle flags and its own: validate
// --online, render --effective, --route, --api-version, --output-dir and
// --output-file, diff --from-env, --to-env, the shared and per-side admin
// credentials and --timeout (5m, 0 meaning none), build --offline and
// --output-file; with their --output formats.
func TestFlags(t *testing.T) {
	type flag struct{ name, def string }
	flags := func(s command.Spec) []flag {
		var out []flag
		for _, f := range command.FlagSpecsOf(s) {
			out = append(out, flag{f.Name, f.Default})
		}
		return out
	}
	cases := []struct {
		spec command.Spec
		want []flag
	}{
		{ValidateSpec(Options{}), []flag{{"env", ""}, {"environments", ""}, {"online", ""}, {"output", "text"}}},
		{RenderSpec(Options{}), []flag{
			{"api-version", ""},
			{"effective", ""},
			{"env", ""},
			{"environments", ""},
			{"output", "yaml"},
			{"output-dir", ""},
			{"output-file", ""},
			{"route", ""},
		}},
		{DiffSpec(Options{}), []flag{
			{"admin-token-file", ""},
			{"ca-file", ""},
			{"client-cert", ""},
			{"client-key", ""},
			{"env", ""},
			{"environments", ""},
			{"from-admin-token-file", ""},
			{"from-client-cert", ""},
			{"from-client-key", ""},
			{"from-env", ""},
			{"output", "text"},
			{"timeout", "5m"},
			{"to-admin-token-file", ""},
			{"to-client-cert", ""},
			{"to-client-key", ""},
			{"to-env", ""},
		}},
		{BuildSpec(Options{}), []flag{{"env", ""}, {"environments", ""}, {"offline", ""}, {"output", "text"}, {"output-file", ""}}},
	}
	for _, tc := range cases {
		if got := flags(tc.spec); !slices.Equal(got, tc.want) {
			t.Errorf("%s flags = %v, want %v", tc.spec.Name(), got, tc.want)
		}
	}
	for _, f := range command.FlagSpecsOf(RenderSpec(Options{})) {
		if f.Name != "output" {
			continue
		}
		if !slices.Equal(f.Values, []string{"yaml", "json"}) || len(f.Modes) != 1 ||
			!slices.Equal(f.Modes[0].When, []string{"effective", "api-version"}) ||
			f.Modes[0].Default != "text" || !slices.Equal(f.Modes[0].Values, []string{"text", "json"}) {
			t.Errorf("render --output = %+v, want yaml|json, and text|json defaulting to text with --effective or --api-version", f)
		}
	}
	for _, f := range command.FlagSpecsOf(DiffSpec(Options{})) {
		if f.Name == "timeout" && !f.ZeroNone {
			t.Error("diff --timeout does not take 0 for none")
		}
	}
}

// TestPlannedVerbs: push, import, export and audit exit 2 naming M2, and
// stay out of help and completion.
func TestPlannedVerbs(t *testing.T) {
	planned := []string{"push", "import", "export", "audit"}
	for _, verb := range planned {
		r := execute(t, Options{}, nil, "bundle", verb, "--env", "prod")
		r.want(t, "bundle "+verb, command.ExitNoResult, "", "ruralz bundle "+verb+" is Planned (M2) and not in this build\n")
	}
	r := execute(t, Options{}, nil, "bundle", "--help")
	for _, verb := range planned {
		if strings.Contains(r.stdout, "  "+verb+" ") {
			t.Errorf("bundle help lists Planned %s:\n%s", verb, r.stdout)
		}
	}
	for _, verb := range []string{"build", "diff", "render", "validate"} {
		if !strings.Contains(r.stdout, "  "+verb+" ") {
			t.Errorf("bundle help lacks %s:\n%s", verb, r.stdout)
		}
	}
	var paths []string
	for _, e := range completion.Entries(Specs(Options{})) {
		paths = append(paths, strings.Join(e.Path, " "))
	}
	if want := []string{"bundle", "bundle build", "bundle diff", "bundle render", "bundle validate"}; !slices.Equal(paths, want) {
		t.Errorf("completion entries = %q, want %q", paths, want)
	}
}

// TestHelp: each command's help names its arguments and flags and says
// what it does, on every platform.
func TestHelp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"bundle", "validate", "--help"}, []string{
			"Usage: ruralz bundle validate [DIR] [FLAGS]", "Offline validation, source-mapped diagnostics.",
			"once per Environment", "--env NAME", "--environments FILE", "--online", "--output text|json", "(default .)",
		}},
		{[]string{"bundle", "render", "--help"}, []string{
			"Usage: ruralz bundle render [DIR] [FLAGS]", "--effective", "--route NAME", "--api-version VERSION",
			"--output-dir DIR", "--output-file PATH", "with --effective or --api-version: text or json, default text",
		}},
		{[]string{"bundle", "diff", "--help"}, []string{
			"Usage: ruralz bundle diff FROM TO [FLAGS]", "Compare FROM with TO.", "Planned (M2)", "--from-env NAME", "--to-env NAME",
			"--from-admin-token-file PATH", "--to-client-key PATH", "--timeout DURATION", "(default 5m; 0 means none)",
		}},
		{[]string{"bundle", "build", "--help"}, []string{
			"Usage: ruralz bundle build [DIR] [FLAGS]", "Build a Revision, print its digest.", "--offline",
			"ruralz.canonical.v1",
		}},
	} {
		for _, goos := range []string{"linux", "darwin", "windows"} {
			var out, errOut bytes.Buffer
			sio := command.NewIO(&out, &errOut, nil, epoch, goos)
			code := command.Execute(t.Context(), sio, "ruralz", Specs(Options{}), tc.args)
			if code != command.ExitOK || errOut.Len() != 0 {
				t.Fatalf("%s %q = exit %d, stderr %q", goos, tc.args, code, errOut.String())
			}
			if missing := containsAll(out.String(), tc.want...); len(missing) > 0 {
				t.Errorf("%s %q lacks %q:\n%s", goos, tc.args, missing, out.String())
			}
		}
	}
}

// TestOptionsDefaults: the zero Options run on the real clock, the
// process's standard input and the real terminal check, and set ones
// replace them.
func TestOptionsDefaults(t *testing.T) {
	var zero Options
	if zero.timer() == nil || zero.stdin() != os.Stdin || zero.isTerminalFunc() == nil {
		t.Fatal("the zero Options lack a clock, standard input or terminal check")
	}
	clk := clocktest.New(epoch())
	in := strings.NewReader("x")
	called := false
	o := Options{Clock: clk, Stdin: in, terminal: func(io.Writer) bool { called = true; return true }}
	if o.timer() != clk || o.stdin() != in {
		t.Error("set clock or standard input not used")
	}
	if !o.isTerminalFunc()(nil) || !called {
		t.Error("set terminal check not used")
	}
}

// TestRealPipeline: a run without a test pipeline builds the pipeline
// every binary builds, checks.Table() included: a jwksUrl that is not
// https is the auth.jwt check's RZ-CFG-037.
func TestRealPipeline(t *testing.T) {
	files := simpleBundle()
	files["policies.yaml"] = policyDoc("jwt", "auth.jwt", `  config:
    issuers:
      - issuer: https://idp.example
        audiences: [shop]
        jwksUrl: http://idp.example/jwks.json
`)
	dir := writeBundle(t, files)
	r := execute(t, Options{Clock: clocktest.New(epoch())}, nil, "bundle", "validate", dir)
	r.wantCode(t, "validate with the real pipeline", command.ExitNegative)
	if !strings.Contains(r.stdout, " error RZ-CFG-037 Policy/jwt ") {
		t.Errorf("validate stdout lacks the auth.jwt check's RZ-CFG-037:\n%s", r.stdout)
	}
}

// TestPipelineFailure: a pipeline that cannot be built exits 2 before
// anything is read.
func TestPipelineFailure(t *testing.T) {
	o := testOptions()
	o.pipelineFunc = func(Options) (*pipeline.Pipeline, error) { return nil, errors.New("no schema") }
	o.Stdin = failingReader{}
	for _, verb := range []string{"validate", "render", "build"} {
		r := execute(t, o, nil, "bundle", verb, "-")
		r.want(t, verb, command.ExitNoResult, "", "ruralz bundle "+verb+": build the configuration pipeline: no schema\n")
	}
}
